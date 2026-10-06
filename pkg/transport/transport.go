package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bdpdx/slack-mcp-server/pkg/version"
	"go.uber.org/zap"
)

// maxRedirects bounds how many redirects an authenticated request may follow.
const maxRedirects = 10

// UserAgent returns the honest User-Agent this server sends to Slack.
func UserAgent() string {
	return version.BinaryName + "/" + version.Version
}

// UserAgentTransport wraps another RoundTripper to set the User-Agent header.
type UserAgentTransport struct {
	roundTripper http.RoundTripper
	userAgent    string
	logger       *zap.Logger
}

// NewUserAgentTransport creates a new UserAgentTransport
func NewUserAgentTransport(roundTripper http.RoundTripper, userAgent string, logger *zap.Logger) *UserAgentTransport {
	return &UserAgentTransport{
		roundTripper: roundTripper,
		userAgent:    userAgent,
		logger:       logger,
	}
}

// RoundTrip implements the RoundTripper interface
func (t *UserAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clonedReq := req.Clone(req.Context())
	clonedReq.Header.Set("User-Agent", t.userAgent)

	t.logger.Debug("Making request",
		zap.String("host", clonedReq.URL.Host),
		zap.String("path", clonedReq.URL.Path))

	resp, err := t.roundTripper.RoundTrip(clonedReq)
	if err != nil {
		t.logger.Error("Request failed", zap.Error(err))
	}
	return resp, err
}

// IsSlackHost reports whether host (without port) belongs to Slack.
func IsSlackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	switch host {
	case "slack.com", "slack-gov.com", "files.slack.com", "files-edu.slack.com":
		return true
	}
	for _, suffix := range []string{".slack.com", ".slack-edge.com", ".slack-files.com", ".slack-gov.com"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// checkRedirect refuses redirects that leave https Slack hosts, so the
// Authorization header and request body never reach a third party.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("stopped after too many redirects")
	}
	if req.URL.Scheme != "https" || !IsSlackHost(req.URL.Hostname()) {
		return fmt.Errorf("refusing redirect to non-Slack URL host %q", req.URL.Hostname())
	}
	return nil
}

// ProvideHTTPClient creates the HTTP client used for all Slack API calls.
//
// Supported environment variables:
//   - SLACK_MCP_PROXY: proxy URL for outgoing requests.
//   - SLACK_MCP_SERVER_CA: PEM file of extra CAs appended to the system roots.
func ProvideHTTPClient(logger *zap.Logger) *http.Client {
	var proxy func(*http.Request) (*url.URL, error)
	if proxyURL := os.Getenv("SLACK_MCP_PROXY"); proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			// The raw value (and the parse error, which quotes parts of it)
			// may contain credentials and cannot be redacted reliably, so
			// neither is logged.
			logger.Fatal("Failed to parse SLACK_MCP_PROXY: not a valid URL")
		}
		logger.Debug("Using proxy", zap.String("proxy_url", parsed.Redacted()))
		proxy = http.ProxyURL(parsed)
	}

	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}

	if localCertFile := os.Getenv("SLACK_MCP_SERVER_CA"); localCertFile != "" {
		certs, err := os.ReadFile(localCertFile)
		if err != nil {
			logger.Fatal("Failed to read local certificate file",
				zap.String("cert_file", localCertFile),
				zap.Error(err))
		}
		if ok := rootCAs.AppendCertsFromPEM(certs); !ok {
			logger.Warn("No certs appended, using system certs only")
		}
	}

	transport := &http.Transport{
		Proxy: proxy,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootCAs,
		},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Transport:     NewUserAgentTransport(transport, UserAgent(), logger),
		CheckRedirect: checkRedirect,
		Timeout:       30 * time.Second,
	}
}
