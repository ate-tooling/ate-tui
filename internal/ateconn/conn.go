// Package ateconn connects to a Substrate install the same way kubectl-ate
// does: it port-forwards to the ateapi Service (or dials --endpoint), verifies
// the serving cert against the cluster's live ClusterTrustBundle, and
// authenticates with a minted ate-client ServiceAccount token or a token file.
//
// The upstream helpers live under go.ate.dev/substrate/internal, so the
// pieces needed here are reproduced in this package.
package ateconn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	atev1 "go.ate.dev/substrate/pkg/client/clientset/versioned"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Defaults match go.ate.dev/substrate/internal/installdefaults. The env vars
// are the ones kubectl-ate honors for installs that rename these resources.
const (
	defaultSystemNamespace = "ate-system"
	defaultAPIService      = "api"
	defaultClientSA        = "ate-client"

	namespaceEnv  = "ATE_NAMESPACE"
	apiServiceEnv = "ATE_API_SERVICE_NAME"
	clientSAEnv   = "ATE_CLIENT_SERVICE_ACCOUNT"

	serviceDNSSignerName = "servicedns.podcert.ate.dev/identity"
	liveBundleSelector   = "podcert.ate.dev/canarying=live"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// SystemNamespace is the namespace substrate's control plane runs in.
func SystemNamespace() string { return envOr(namespaceEnv, defaultSystemNamespace) }

func apiServerName() string {
	return fmt.Sprintf("%s.%s.svc", envOr(apiServiceEnv, defaultAPIService), SystemNamespace())
}

// Options selects the cluster and credentials.
type Options struct {
	Kubeconfig string
	Context    string
	// Endpoint dials ateapi directly instead of port-forwarding.
	Endpoint string
	// TokenFile is a bearer token for ateapi. Empty mints an ate-client token.
	TokenFile string
}

// Conn holds the ateapi client and the Kubernetes clients used next to it.
type Conn struct {
	Control  ateapi.ControlClient
	Kube     kubernetes.Interface
	Ate      atev1.Interface
	Context  string
	Host     string
	Endpoint string

	config      *rest.Config
	grpcConn    *grpc.ClientConn
	stopForward func()

	routerMu     sync.Mutex
	routerDialer *routerDialer
}

// Close tears down the gRPC connection and the port-forward.
func (c *Conn) Close() {
	if c.grpcConn != nil {
		c.grpcConn.Close()
	}
	if c.stopForward != nil {
		c.stopForward()
	}
}

// KubeContext is one context in the kubeconfig.
type KubeContext struct {
	Name, Server string
	Current      bool
}

// Contexts lists the kubeconfig's contexts, sorted by name.
func Contexts(kubeconfig string) ([]KubeContext, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfig
	raw, err := loadingRules.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	out := make([]KubeContext, 0, len(raw.Contexts))
	for name, c := range raw.Contexts {
		kc := KubeContext{Name: name, Current: name == raw.CurrentContext}
		if cl := raw.Clusters[c.Cluster]; cl != nil {
			kc.Server = cl.Server
		}
		out = append(out, kc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Connect dials ateapi.
func Connect(ctx context.Context, opts Options) (*Conn, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = opts.Kubeconfig
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{CurrentContext: opts.Context})
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	config, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	// Older clusters serve only the deprecated beta ClusterTrustBundle API.
	config.WarningHandlerWithContext = &rest.NoWarnings{}

	kc, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create k8s client: %w", err)
	}
	ac, err := atev1.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create ate.dev client: %w", err)
	}

	conn := &Conn{Kube: kc, Ate: ac, Host: config.Host, Context: opts.Context, config: config}
	if conn.Context == "" {
		conn.Context = raw.CurrentContext
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		port, stop, err := supervisedPortForward(ctx, config, kc, SystemNamespace(), envOr(apiServiceEnv, defaultAPIService), 443)
		if err != nil {
			return nil, err
		}
		conn.stopForward = stop
		endpoint = fmt.Sprintf("127.0.0.1:%d", port)
	}
	conn.Endpoint = endpoint

	// Verify the server before attaching the bearer token: the token must
	// never go over an unauthenticated channel.
	tlsCfg, err := serverTLSConfig(ctx, kc, apiServerName())
	if err != nil {
		conn.Close()
		return nil, err
	}
	tokenOpt, err := bearerToken(ctx, kc, opts.TokenFile)
	if err != nil {
		conn.Close()
		return nil, err
	}
	gc, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig": [{"round_robin":{}}]}`),
		tokenOpt,
	)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("dial ateapi at %s: %w", endpoint, err)
	}
	conn.grpcConn = gc
	conn.Control = ateapi.NewControlClient(gc)
	return conn, nil
}

// serverTLSConfig verifies serverName's certificate against the live
// servicedns ClusterTrustBundles. The signer rotates its CA, so a
// certificate the bundles read last do not verify reads them again (at most
// once a minute) before the handshake fails.
func serverTLSConfig(ctx context.Context, kc kubernetes.Interface, serverName string) (*tls.Config, error) {
	ts, err := newTrustStore(ctx, kc)
	if err != nil {
		return nil, err
	}
	return ts.tlsConfig(serverName), nil
}

func newTrustStore(ctx context.Context, kc kubernetes.Interface) (*trustStore, error) {
	pool, err := trustPool(ctx, kc)
	if err != nil {
		return nil, err
	}
	return &trustStore{kube: kc, pool: pool, fetched: time.Now()}, nil
}

func (ts *trustStore) tlsConfig(serverName string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		// The standard verification, against a pool that can change, is
		// done by VerifyConnection.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return ts.verify(cs.PeerCertificates, serverName)
		},
	}
}

const trustRefetchEvery = time.Minute

type trustStore struct {
	kube kubernetes.Interface

	mu      sync.Mutex
	pool    *x509.CertPool
	fetched time.Time
}

func (t *trustStore) verify(certs []*x509.Certificate, serverName string) error {
	if len(certs) == 0 {
		return errors.New("tls: server presented no certificate")
	}
	opts := x509.VerifyOptions{DNSName: serverName, Intermediates: x509.NewCertPool()}
	for _, c := range certs[1:] {
		opts.Intermediates.AddCert(c)
	}
	t.mu.Lock()
	opts.Roots = t.pool
	stale := time.Since(t.fetched) >= trustRefetchEvery
	t.mu.Unlock()
	_, err := certs[0].Verify(opts)
	if err == nil || !stale {
		return err
	}
	// One handshake reads the bundles again; the others meanwhile verify
	// against what is there and do not wait on the apiserver.
	t.mu.Lock()
	if time.Since(t.fetched) < trustRefetchEvery {
		opts.Roots = t.pool
		t.mu.Unlock()
		_, err = certs[0].Verify(opts)
		return err
	}
	t.fetched = time.Now()
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, ferr := trustPool(ctx, t.kube)
	if ferr != nil {
		return err
	}
	t.mu.Lock()
	t.pool = pool
	t.mu.Unlock()
	opts.Roots = pool
	_, err = certs[0].Verify(opts)
	return err
}

func trustPool(ctx context.Context, kc kubernetes.Interface) (*x509.CertPool, error) {
	pems, err := listTrustBundles(ctx, kc)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for name, pem := range pems {
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			return nil, fmt.Errorf("ClusterTrustBundle %q contains no valid certificates", name)
		}
	}
	if len(pems) == 0 {
		return nil, fmt.Errorf("no live ClusterTrustBundle found for signer %q", serviceDNSSignerName)
	}
	return pool, nil
}

// listTrustBundles returns the live servicedns bundles by name, from the v1
// API or, when the cluster does not serve it, v1beta1.
func listTrustBundles(ctx context.Context, kc kubernetes.Interface) (map[string]string, error) {
	opts := metav1.ListOptions{LabelSelector: liveBundleSelector}
	out := map[string]string{}
	v1, err := kc.CertificatesV1().ClusterTrustBundles().List(ctx, opts)
	if err == nil {
		for _, b := range v1.Items {
			if b.Spec.SignerName == serviceDNSSignerName {
				out[b.Name] = b.Spec.TrustBundle
			}
		}
		return out, nil
	}
	beta, berr := kc.CertificatesV1beta1().ClusterTrustBundles().List(ctx, opts)
	if berr != nil {
		return nil, fmt.Errorf("list ClusterTrustBundles: %w", err)
	}
	for _, b := range beta.Items {
		if b.Spec.SignerName == serviceDNSSignerName {
			out[b.Name] = b.Spec.TrustBundle
		}
	}
	return out, nil
}

func bearerToken(ctx context.Context, kc kubernetes.Interface, tokenFile string) (grpc.DialOption, error) {
	if tokenFile != "" {
		return grpc.WithPerRPCCredentials(fileToken(tokenFile)), nil
	}
	t := &mintedToken{kube: kc}
	// Mint now, so a missing ServiceAccount or RBAC fails the connect.
	if _, err := t.GetRequestMetadata(ctx); err != nil {
		return nil, err
	}
	return grpc.WithPerRPCCredentials(t), nil
}

// tokenLifetime is what the TokenRequest asks for; the token is minted again
// once half of what was granted has passed, so a session outlives it.
const tokenLifetime = time.Hour

// mintedToken is an ate-client ServiceAccount token, minted again before it
// expires.
type mintedToken struct {
	kube kubernetes.Interface

	mu        sync.Mutex
	token     string
	expires   time.Time
	refreshAt time.Time
}

// mintTimeout bounds a mint. It does not take the deadline of the RPC that
// asked: a short one would fail the mint for every RPC waiting on it.
const mintTimeout = 30 * time.Second

func (t *mintedToken) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	switch {
	case t.token != "" && now.Before(t.refreshAt):
	case t.token != "" && now.Before(t.expires):
		// Still valid: mint the next one in the background, and use this
		// one meanwhile. A failure keeps it, and tries again in a minute.
		t.refreshAt = now.Add(time.Minute)
		go t.refresh()
	default:
		// No valid token: every RPC waits for this one mint.
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
		defer cancel()
		token, exp, err := mintToken(mctx, t.kube)
		if err != nil {
			return nil, err
		}
		t.set(token, exp, time.Now())
	}
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}

func (t *mintedToken) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()
	token, exp, err := mintToken(ctx, t.kube)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.set(token, exp, time.Now())
}

func (t *mintedToken) set(token string, exp, now time.Time) {
	t.token, t.expires = token, exp
	t.refreshAt = now.Add(exp.Sub(now) / 2)
}

func (*mintedToken) RequireTransportSecurity() bool { return true }

func mintToken(ctx context.Context, kc kubernetes.Interface) (string, time.Time, error) {
	exp := int64(tokenLifetime / time.Second)
	requested := time.Now()
	tr, err := kc.CoreV1().ServiceAccounts(SystemNamespace()).CreateToken(ctx, envOr(clientSAEnv, defaultClientSA), &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{Audiences: []string{apiServerName()}, ExpirationSeconds: &exp},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("request ateapi bearer token: %w", err)
	}
	if tr.Status.Token == "" {
		return "", time.Time{}, fmt.Errorf("request ateapi bearer token: token response was empty")
	}
	expires := tr.Status.ExpirationTimestamp.Time
	if expires.IsZero() || !expires.After(requested) {
		expires = requested.Add(tokenLifetime)
	}
	return tr.Status.Token, expires, nil
}

type fileToken string

func (f fileToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(string(f))
	if err != nil {
		return nil, fmt.Errorf("read bearer token file %q: %w", string(f), err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return nil, fmt.Errorf("bearer token file %q is empty", string(f))
	}
	return map[string]string{"authorization": "Bearer " + t}, nil
}

func (fileToken) RequireTransportSecurity() bool { return true }
