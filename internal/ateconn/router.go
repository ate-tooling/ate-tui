package ateconn

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	certsv1 "k8s.io/api/certificates/v1"
	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// The router dialer reproduces go.ate.dev/substrate/internal/ateclient's
// RouterDialer and internal/podidentity, which `kubectl ate proxy actor`
// uses: a CONNECT tunnel over TLS to atenet-router, authenticated with a
// podidentity client certificate minted through a PodCertificateRequest.
const (
	routerService = "atenet-router"
	// routerConnectTLSPort is atenet-router's CONNECT-TLS Service port.
	routerConnectTLSPort = 8444
	// routerCertTimeout bounds the initial wait for the podidentity signer.
	routerCertTimeout = 2 * time.Minute
	// routerCertRefreshTimeout bounds a refresh made while a cached
	// certificate is still valid.
	routerCertRefreshTimeout = 15 * time.Second
	// routerCertRequester names this client on its PodCertificateRequests.
	routerCertRequester = "ate-tui"
	// routerClientSA is the ServiceAccount the client certificate is for.
	// The router allows its SPIFFE ID by default.
	routerClientSA = "atenet-client"

	podIdentitySigner = "podidentity.podcert.ate.dev/identity"
	targetActorHeader = "ate-target-actor"
)

func routerServerName() string {
	return fmt.Sprintf("%s.%s.svc", routerService, SystemNamespace())
}

// routerDialer opens CONNECT tunnels to actors through atenet-router.
type routerDialer struct {
	tlsConfig *tls.Config
}

// PrepareProxy mints the router client certificate, so a caller without
// RBAC on PodCertificateRequests fails before it listens on a port. It is
// done once per connection; later calls return at once.
func (c *Conn) PrepareProxy(ctx context.Context) error {
	_, err := c.router(ctx)
	return err
}

func (c *Conn) router(ctx context.Context) (*routerDialer, error) {
	c.routerMu.Lock()
	defer c.routerMu.Unlock()
	if c.routerDialer != nil {
		return c.routerDialer, nil
	}
	tlsCfg, err := serverTLSConfig(ctx, c.Kube, routerServerName())
	if err != nil {
		return nil, err
	}
	certs := &routerClientCerts{kube: c.Kube}
	if _, err := certs.get(ctx); err != nil {
		return nil, err
	}
	tlsCfg.GetClientCertificate = func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return certs.get(info.Context())
	}
	c.routerDialer = &routerDialer{tlsConfig: tlsCfg}
	return c.routerDialer, nil
}

// DialActor opens a CONNECT tunnel through atenet-router to port on the
// actor. The router resumes a suspended actor. Closing the connection also
// stops its port-forward.
func (c *Conn) DialActor(ctx context.Context, ref *ateapi.ObjectRef, port int) (net.Conn, error) {
	d, err := c.router(ctx)
	if err != nil {
		return nil, err
	}
	// client-go tears down a whole port-forward when one of its streams
	// errors, so each tunnel gets its own, which also picks a ready router
	// pod again after one goes away.
	localPort, stop, err := servicePortForward(ctx, c.config, c.Kube, SystemNamespace(), routerService, routerConnectTLSPort)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		stop()
		return nil, fmt.Errorf("connecting to router's CONNECT listener: %w", err)
	}
	actor := ref.GetAtespace() + "/" + ref.GetName()
	fc, err := connectActor(ctx, raw, d.tlsConfig, ref.GetName(), port, http.Header{targetActorHeader: []string{actor}})
	if err != nil {
		stop()
		return nil, err
	}
	fc.stop = stop
	return fc, nil
}

func connectActor(ctx context.Context, raw net.Conn, tlsCfg *tls.Config, name string, port int, header http.Header) (*forwardedConn, error) {
	tlsConn := tls.Client(raw, tlsCfg.Clone())
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("TLS handshake with router's CONNECT listener: %w", err)
	}

	stopOnCancel := context.AfterFunc(ctx, func() { _ = tlsConn.Close() })
	defer stopOnCancel()

	dest := net.JoinHostPort(name, strconv.Itoa(port))
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Host: dest}, Host: dest, Header: header}
	if err := req.Write(tlsConn); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("writing CONNECT request: %w", err)
	}
	reader := bufio.NewReader(tlsConn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("reading CONNECT response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		_ = tlsConn.Close()
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("router rejected CONNECT to %s with %s: %s", dest, resp.Status, msg)
	}
	if !stopOnCancel() {
		_ = tlsConn.Close()
		return nil, ctx.Err()
	}
	return &forwardedConn{Conn: tlsConn, reader: reader}, nil
}

// forwardedConn keeps the bytes http.ReadResponse buffered past the header
// and stops its port-forward when closed.
type forwardedConn struct {
	net.Conn
	reader   *bufio.Reader
	stop     func()
	stopOnce sync.Once
}

func (c *forwardedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *forwardedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *forwardedConn) Close() error {
	err := c.Conn.Close()
	if c.stop != nil {
		c.stopOnce.Do(c.stop)
	}
	return err
}

// routerClientCerts holds the current router client certificate and
// requests a new one once half its lifetime has passed.
type routerClientCerts struct {
	kube kubernetes.Interface

	mu        sync.Mutex
	cert      *tls.Certificate
	refreshAt time.Time
}

func (c *routerClientCerts) get(ctx context.Context) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.cert != nil && now.Before(c.refreshAt) {
		return c.cert, nil
	}
	timeout := routerCertTimeout
	if c.cert != nil && now.Before(c.cert.Leaf.NotAfter) {
		timeout = routerCertRefreshTimeout
	}
	ns := SystemNamespace()
	cert, err := requestCertificate(ctx, c.kube, routerCertRequester, ns, routerClientSA, timeout)
	if err != nil {
		now = time.Now()
		// kubectl-ate warns on stderr here; a TUI cannot, so it keeps the
		// cached certificate quietly and retries in a minute.
		if c.cert != nil && now.Before(c.cert.Leaf.NotAfter) {
			c.refreshAt = now.Add(time.Minute)
			if c.cert.Leaf.NotAfter.Before(c.refreshAt) {
				c.refreshAt = c.cert.Leaf.NotAfter
			}
			return c.cert, nil
		}
		return nil, fmt.Errorf("requesting a router client certificate for %s/%s: %w", ns, routerClientSA, err)
	}
	c.cert = &cert
	c.refreshAt = cert.Leaf.NotBefore.Add(cert.Leaf.NotAfter.Sub(cert.Leaf.NotBefore) / 2)
	return c.cert, nil
}

// requestCertificate creates a PodCertificateRequest for serviceAccount,
// waits for the podidentity signer to issue it, and deletes it again. The
// key never leaves this process. The pod, ServiceAccount and node it names
// do not exist: kube-apiserver checks them only for kubelet requests.
func requestCertificate(ctx context.Context, kc kubernetes.Interface, requester, namespace, serviceAccount string, timeout time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("creating stub CSR: %w", err)
	}
	pcr := &certsv1.PodCertificateRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: requester + "-", Namespace: namespace},
		Spec: certsv1.PodCertificateRequestSpec{
			SignerName:           podIdentitySigner,
			PodName:              requester,
			PodUID:               uuid.NewUUID(),
			ServiceAccountName:   serviceAccount,
			ServiceAccountUID:    uuid.NewUUID(),
			NodeName:             types.NodeName(requester),
			NodeUID:              uuid.NewUUID(),
			MaxExpirationSeconds: ptr.To(int32(24 * 60 * 60)),
			StubPKCS10Request:    csr,
		},
	}
	pcrs, err := podCertificateRequests(kc, namespace)
	if err != nil {
		return tls.Certificate{}, err
	}
	name, err := pcrs.create(ctx, pcr)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("creating PodCertificateRequest in %s: %w", namespace, err)
	}
	defer func() {
		// A leftover request is harmless: kube-apiserver garbage collects
		// old PodCertificateRequests.
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = pcrs.delete(delCtx, name)
	}()

	var chainPEM string
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		st, err := pcrs.status(ctx, name)
		if err != nil {
			return false, err
		}
		for _, c := range st.Conditions {
			if c.Status != metav1.ConditionTrue {
				continue
			}
			switch c.Type {
			case certsv1.PodCertificateRequestConditionTypeIssued:
				chainPEM = st.CertificateChain
				return true, nil
			case certsv1.PodCertificateRequestConditionTypeDenied, certsv1.PodCertificateRequestConditionTypeFailed:
				return false, fmt.Errorf("%s: %s: %s", c.Type, c.Reason, c.Message)
			}
		}
		return false, nil
	})
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("waiting for PodCertificateRequest %s/%s to be issued: %w", namespace, name, err)
	}

	cert := tls.Certificate{PrivateKey: key}
	for rest := []byte(chainPEM); ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert.Certificate = append(cert.Certificate, block.Bytes)
	}
	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, fmt.Errorf("PodCertificateRequest %s/%s was issued with no certificates", namespace, name)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parsing the issued leaf certificate: %w", err)
	}
	if !key.PublicKey.Equal(leaf.PublicKey) {
		return tls.Certificate{}, fmt.Errorf("the issued certificate does not carry the requested public key")
	}
	cert.Leaf = leaf
	return cert, nil
}

// pcrClient uses whichever of certificates.k8s.io v1 and v1beta1 the
// cluster serves, preferring v1.
type pcrClient struct {
	kc        kubernetes.Interface
	namespace string
	v1        bool
}

func podCertificateRequests(kc kubernetes.Interface, namespace string) (*pcrClient, error) {
	for _, version := range []string{"v1", "v1beta1"} {
		res, err := kc.Discovery().ServerResourcesForGroupVersion("certificates.k8s.io/" + version)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("discovering PodCertificateRequest %s: %w", version, err)
		}
		for _, r := range res.APIResources {
			if r.Name == "podcertificaterequests" {
				return &pcrClient{kc: kc, namespace: namespace, v1: version == "v1"}, nil
			}
		}
	}
	return nil, fmt.Errorf("the cluster serves neither v1 nor v1beta1 PodCertificateRequest")
}

func (c *pcrClient) create(ctx context.Context, pcr *certsv1.PodCertificateRequest) (string, error) {
	if c.v1 {
		created, err := c.kc.CertificatesV1().PodCertificateRequests(c.namespace).Create(ctx, pcr, metav1.CreateOptions{})
		if err != nil {
			return "", err
		}
		return created.Name, nil
	}
	s := pcr.Spec
	beta := &certsv1beta1.PodCertificateRequest{
		ObjectMeta: pcr.ObjectMeta,
		Spec: certsv1beta1.PodCertificateRequestSpec{
			SignerName: s.SignerName, PodName: s.PodName, PodUID: s.PodUID,
			ServiceAccountName: s.ServiceAccountName, ServiceAccountUID: s.ServiceAccountUID,
			NodeName: s.NodeName, NodeUID: s.NodeUID,
			MaxExpirationSeconds: s.MaxExpirationSeconds, StubPKCS10Request: s.StubPKCS10Request,
		},
	}
	created, err := c.kc.CertificatesV1beta1().PodCertificateRequests(c.namespace).Create(ctx, beta, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return created.Name, nil
}

func (c *pcrClient) status(ctx context.Context, name string) (certsv1.PodCertificateRequestStatus, error) {
	if c.v1 {
		pcr, err := c.kc.CertificatesV1().PodCertificateRequests(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return certsv1.PodCertificateRequestStatus{}, err
		}
		return pcr.Status, nil
	}
	pcr, err := c.kc.CertificatesV1beta1().PodCertificateRequests(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return certsv1.PodCertificateRequestStatus{}, err
	}
	return certsv1.PodCertificateRequestStatus(pcr.Status), nil
}

func (c *pcrClient) delete(ctx context.Context, name string) error {
	if c.v1 {
		return c.kc.CertificatesV1().PodCertificateRequests(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}
	return c.kc.CertificatesV1beta1().PodCertificateRequests(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
}
