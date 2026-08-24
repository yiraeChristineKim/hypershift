package fromhub

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	apiGroup      = "hcp.ocm.io"
	apiVersion    = "v1alpha1"
	apiPathPrefix = "/apis/" + apiGroup + "/" + apiVersion
)

// buildHTTPClient returns an authenticated HTTP client and the base host URL.
//
// Two modes:
//
//	Normal (production): authenticates via hub kubeconfig; requests go through
//	the hub kube-apiserver which forwards to the HCP proxy extension API.
//
//	Direct (--proxy-url, local testing): bypasses the hub kube-apiserver and
//	connects straight to the HCP proxy. TLS verification is skipped because the
//	proxy uses a self-signed certificate in local environments.
func buildHTTPClient(opts *Options) (*http.Client, string, error) {
	if opts.ProxyURL != "" {
		return buildDirectClient(opts)
	}
	return buildHubClient(opts)
}

// buildHubClient connects via the hub kube-apiserver (production path).
func buildHubClient(opts *Options) (*http.Client, string, error) {
	cfg, err := loadConfig(opts)
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	rt, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("build transport: %w", err)
	}
	return &http.Client{Transport: rt}, cfg.Host, nil
}

// buildDirectClient connects straight to the HCP proxy (local testing path).
// TLS verification is disabled because the proxy uses a self-signed cert.
// If opts.ImpersonateUser or opts.ImpersonateGroups are set, the corresponding
// X-Remote-User / X-Remote-Group headers are injected into every request.
func buildDirectClient(opts *Options) (*http.Client, string, error) {
	host := strings.TrimRight(opts.ProxyURL, "/")
	var rt http.RoundTripper = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	if opts.ImpersonateUser != "" || len(opts.ImpersonateGroups) > 0 {
		rt = &impersonatingTransport{
			wrapped: rt,
			user:    opts.ImpersonateUser,
			groups:  opts.ImpersonateGroups,
		}
	}
	return &http.Client{Transport: rt}, host, nil
}

// impersonatingTransport injects X-Remote-User and X-Remote-Group headers.
// Used with --proxy-url to simulate the headers normally set by the hub kube-apiserver.
type impersonatingTransport struct {
	wrapped http.RoundTripper
	user    string
	groups  []string
}

func (t *impersonatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if t.user != "" {
		req.Header.Set("X-Remote-User", t.user)
	}
	for _, g := range t.groups {
		req.Header.Add("X-Remote-Group", g)
	}
	return t.wrapped.RoundTrip(req)
}

// loadConfig loads the REST config honoring --hub-kubeconfig and --context.
func loadConfig(opts *Options) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		loadingRules.ExplicitPath = opts.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if opts.Context != "" {
		overrides.CurrentContext = opts.Context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		overrides,
	).ClientConfig()
}

// hostedClusterURL returns the URL for a collection or single HostedCluster.
//
//	hostingCluster is appended as ?hostingCluster=<name>.
func hostedClusterURL(host, ns string, name string, hostingCluster string, extraQuery ...string) string {
	base := host + apiPathPrefix + "/namespaces/" + ns + "/hostedclusters"
	if name != "" {
		base += "/" + name
	}
	base += "?hostingCluster=" + hostingCluster
	for _, q := range extraQuery {
		base += "&" + q
	}
	return base
}

// newRequest is a convenience wrapper around http.NewRequestWithContext using
// context.Background(). Callers that need a specific context should use
// req.WithContext(ctx) on the result.
func newRequest(method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequestWithContext(context.Background(), method, url, body)
}

// skipVerifyTransport wraps a RoundTripper and skips TLS verification.
// Used for the extension API server's self-signed cert.
type skipVerifyTransport struct {
	wrapped http.RoundTripper
}

func (t *skipVerifyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.wrapped != nil {
		return t.wrapped.RoundTrip(req)
	}
	return (&http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}).RoundTrip(req)
}
