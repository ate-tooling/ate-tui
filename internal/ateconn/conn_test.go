package ateconn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The minted token is minted again once half its lifetime has passed, and a
// failed refresh keeps the token that still works.
func TestMintedTokenRefreshes(t *testing.T) {
	kc := fake.NewClientset()
	var mu sync.Mutex
	n, fail := 0, false
	kc.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return true, nil, errors.New("apiserver down")
		}
		n++
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{
			Token:               fmt.Sprintf("t%d", n),
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour)),
		}}, nil
	})
	tok := &mintedToken{kube: kc}
	get := func() (string, error) {
		md, err := tok.GetRequestMetadata(context.Background())
		return md["authorization"], err
	}
	if got, err := get(); err != nil || got != "Bearer t1" {
		t.Fatalf("first = %q, %v", got, err)
	}
	if got, _ := get(); got != "Bearer t1" {
		t.Fatalf("cached = %q, want t1", got)
	}
	// Past refreshAt: the current token is used while the next is minted
	// in the background.
	tok.mu.Lock()
	tok.refreshAt = time.Now().Add(-time.Second)
	tok.mu.Unlock()
	if got, _ := get(); got != "Bearer t1" {
		t.Fatalf("while refreshing = %q, want t1", got)
	}
	waitFor(t, func() bool { got, _ := get(); return got == "Bearer t2" })
	// A failed refresh keeps the token that still works.
	mu.Lock()
	fail = true
	mu.Unlock()
	tok.mu.Lock()
	tok.refreshAt = time.Now().Add(-time.Second)
	tok.mu.Unlock()
	if got, err := get(); err != nil || got != "Bearer t2" {
		t.Fatalf("failed refresh = %q, %v; want the current token", got, err)
	}
	time.Sleep(50 * time.Millisecond) // let the background mint fail
	if got, err := get(); err != nil || got != "Bearer t2" {
		t.Fatalf("after a failed refresh = %q, %v; want the current token", got, err)
	}
	tok.mu.Lock()
	tok.expires = time.Now().Add(-time.Second)
	tok.refreshAt = tok.expires
	tok.mu.Unlock()
	if _, err := get(); err == nil {
		t.Fatal("an expired token with a failing refresh should fail")
	}
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newCA(t *testing.T, name string) testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return testCA{c, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (ca testCA) leaf(t *testing.T, dns string) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{dns},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func bundle(name, pem string) *certv1.ClusterTrustBundle {
	return &certv1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"podcert.ate.dev/canarying": "live"}},
		Spec:       certv1.ClusterTrustBundleSpec{SignerName: serviceDNSSignerName, TrustBundle: pem},
	}
}

// A certificate from a rotated CA verifies once the bundles are read again;
// the server name is still checked.
func TestServerTLSConfigFollowsRotation(t *testing.T) {
	old, rotated := newCA(t, "old"), newCA(t, "new")
	kc := fake.NewClientset(bundle("b", old.pem))
	ts, err := newTrustStore(context.Background(), kc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ts.tlsConfig("api.ate-system.svc")
	if !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil {
		t.Fatal("verification must go through VerifyConnection")
	}
	verify := func(c *x509.Certificate) error {
		return cfg.VerifyConnection(tlsState(c))
	}
	if err := verify(old.leaf(t, "api.ate-system.svc")); err != nil {
		t.Fatalf("old CA: %v", err)
	}
	if err := verify(old.leaf(t, "evil.example")); err == nil {
		t.Fatal("a certificate for another name verified")
	}
	if _, err := kc.CertificatesV1().ClusterTrustBundles().Update(context.Background(), bundle("b", rotated.pem), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	newLeaf := rotated.leaf(t, "api.ate-system.svc")
	// Read less than a minute ago: not read again yet.
	if err := verify(newLeaf); err == nil {
		t.Fatal("rotated CA verified before the bundles were read again")
	}
	ts.fetched = time.Now().Add(-2 * trustRefetchEvery)
	if err := verify(newLeaf); err != nil {
		t.Fatalf("rotated CA after a re-read: %v", err)
	}
	if err := verify(rotated.leaf(t, "evil.example")); err == nil {
		t.Fatal("a certificate for another name verified after rotation")
	}
	if err := cfg.VerifyConnection(tlsState()); err == nil {
		t.Fatal("no certificate verified")
	}
}

func tlsState(certs ...*x509.Certificate) tls.ConnectionState {
	return tls.ConnectionState{PeerCertificates: certs}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in 5s")
}
