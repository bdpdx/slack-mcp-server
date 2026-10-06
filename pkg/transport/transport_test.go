package transport

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bdpdx/slack-mcp-server/pkg/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestUnitIsSlackHost(t *testing.T) {
	for _, h := range []string{"slack.com", "acme.slack.com", "files.slack.com", "a.slack-edge.com", "x.slack-files.com", "slack-gov.com", "acme.slack-gov.com", "ACME.SLACK.COM"} {
		assert.True(t, IsSlackHost(h), h)
	}
	for _, h := range []string{"", "evil.com", "slack.com.evil.com", "evilslack.com", "slack-edge.com.evil", "127.0.0.1"} {
		assert.False(t, IsSlackHost(h), h)
	}
}

func TestUnitCheckRedirect(t *testing.T) {
	mk := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return &http.Request{URL: u}
	}
	assert.NoError(t, checkRedirect(mk("https://files.slack.com/x"), nil))
	assert.Error(t, checkRedirect(mk("https://evil.com/x"), nil))
	assert.Error(t, checkRedirect(mk("http://files.slack.com/x"), nil))

	via := make([]*http.Request, maxRedirects)
	assert.Error(t, checkRedirect(mk("https://files.slack.com/x"), via))
}

func TestUnitProvideHTTPClient(t *testing.T) {
	t.Setenv("SLACK_MCP_PROXY", "")
	t.Setenv("SLACK_MCP_SERVER_CA", "")

	client := ProvideHTTPClient(zap.NewNop())
	require.NotNil(t, client.CheckRedirect, "redirects must be restricted")

	uat, ok := client.Transport.(*UserAgentTransport)
	require.True(t, ok)
	assert.Equal(t, version.BinaryName+"/"+version.Version, uat.userAgent)
	assert.NotContains(t, uat.userAgent, "Mozilla")

	base, ok := uat.roundTripper.(*http.Transport)
	require.True(t, ok, "must use the standard library transport")
	assert.Equal(t, uint16(tls.VersionTLS12), base.TLSClientConfig.MinVersion)
	assert.False(t, base.TLSClientConfig.InsecureSkipVerify)
}

func TestUnitUserAgentHeaderAndNoCookies(t *testing.T) {
	var gotUA, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotCookie = r.Header.Get("Cookie")
	}))
	defer srv.Close()

	client := &http.Client{Transport: NewUserAgentTransport(http.DefaultTransport, UserAgent(), zap.NewNop())}
	resp, err := client.Get(srv.URL)
	require.NoError(t, err)
	resp.Body.Close()

	assert.True(t, strings.HasPrefix(gotUA, version.BinaryName+"/"), gotUA)
	assert.Empty(t, gotCookie)
}

func TestUnitRedirectToForeignHostRefused(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("foreign host must not be contacted (Authorization=%q)", r.Header.Get("Authorization"))
	}))
	defer foreign.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/steal", http.StatusFound)
	}))
	defer origin.Close()

	client := &http.Client{CheckRedirect: checkRedirect}
	req, err := http.NewRequest(http.MethodGet, origin.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer xoxp-secret")
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-Slack")
}
