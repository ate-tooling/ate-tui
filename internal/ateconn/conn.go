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
	"fmt"
	"os"
	"strings"

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

	grpcConn    *grpc.ClientConn
	stopForward func()
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

	conn := &Conn{Kube: kc, Ate: ac, Host: config.Host, Context: opts.Context}
	if conn.Context == "" {
		conn.Context = raw.CurrentContext
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		port, stop, err := servicePortForward(ctx, config, kc, SystemNamespace(), envOr(apiServiceEnv, defaultAPIService), 443)
		if err != nil {
			return nil, err
		}
		conn.stopForward = stop
		endpoint = fmt.Sprintf("127.0.0.1:%d", port)
	}
	conn.Endpoint = endpoint

	// Verify the server before attaching the bearer token: the token must
	// never go over an unauthenticated channel.
	tlsCfg, err := serverTLSConfig(ctx, kc)
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

func serverTLSConfig(ctx context.Context, kc kubernetes.Interface) (*tls.Config, error) {
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
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: apiServerName()}, nil
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
	exp := int64(3600)
	tr, err := kc.CoreV1().ServiceAccounts(SystemNamespace()).CreateToken(ctx, envOr(clientSAEnv, defaultClientSA), &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{Audiences: []string{apiServerName()}, ExpirationSeconds: &exp},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("request ateapi bearer token: %w", err)
	}
	if tr.Status.Token == "" {
		return nil, fmt.Errorf("request ateapi bearer token: token response was empty")
	}
	return grpc.WithPerRPCCredentials(staticToken(tr.Status.Token)), nil
}

type staticToken string

func (t staticToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}

func (staticToken) RequireTransportSecurity() bool { return true }

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
