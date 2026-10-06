package provider

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnitValidateFileURL(t *testing.T) {
	allowed := []string{
		"https://files.slack.com/files-pri/T1-F1/download/a.txt",
		"https://files-edu.slack.com/files-pri/T1-F1/a.txt",
		"https://acme.slack.com/files-pri/T1-F1/a.txt",
		"https://a.slack-edge.com/x.png",
		"https://files.slack-files.com/x",
		"https://files.slack-gov.com/files-pri/x",
		"https://FILES.SLACK.COM/x",
	}
	for _, u := range allowed {
		_, err := validateFileURL(u)
		assert.NoError(t, err, u)
	}

	refused := []string{
		"",
		"http://files.slack.com/x",
		"ftp://files.slack.com/x",
		"https://evil.com/x",
		"https://slack.com.evil.com/x",
		"https://evilslack.com/x",
		"https://files.slack.com.evil.com/x",
		"https://files.slack.com@evil.com/x",
		"https://user:pass@files.slack.com/x",
		"https://127.0.0.1/x",
		"file:///etc/passwd",
		"//files.slack.com/x",
		"https://%zz",
	}
	for _, u := range refused {
		_, err := validateFileURL(u)
		assert.Error(t, err, u)
	}
}

// newSlackFilesTestClient returns an HTTP client that sends every request to
// srv while still validating the server's TLS certificate, so URLs on Slack
// hosts can be exercised against a local test server.
func newSlackFilesTestClient(srv *httptest.Server) *http.Client {
	base := srv.Client().Transport.(*http.Transport).Clone()
	base.TLSClientConfig = base.TLSClientConfig.Clone()
	base.TLSClientConfig.ServerName = "example.com" // httptest cert SAN
	addr := srv.Listener.Addr().String()
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	base.DialTLSContext = nil
	return &http.Client{Transport: base}
}

func TestUnitDownloadFile(t *testing.T) {
	const limit = 1024

	var gotAuth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/small":
			w.Write([]byte("hello"))
		case "/exact":
			w.Write(bytes.Repeat([]byte("a"), limit))
		case "/big-declared":
			w.Header().Set("Content-Length", "999999")
			w.Write(bytes.Repeat([]byte("a"), limit+1))
		case "/big-chunked":
			// No Content-Length: flush in pieces so the size is only
			// discovered while reading.
			f := w.(http.Flusher)
			for i := 0; i < 8; i++ {
				w.Write(bytes.Repeat([]byte("b"), limit/2))
				f.Flush()
			}
		case "/missing":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := newSlackFilesTestClient(srv)
	ctx := context.Background()

	t.Run("downloads small file with bearer token", func(t *testing.T) {
		var buf bytes.Buffer
		err := downloadFile(ctx, client, "xoxp-test", "https://files.slack.com/small", &buf, limit)
		require.NoError(t, err)
		assert.Equal(t, "hello", buf.String())
		assert.Equal(t, "Bearer xoxp-test", gotAuth)
	})

	t.Run("allows a body exactly at the cap", func(t *testing.T) {
		var buf bytes.Buffer
		err := downloadFile(ctx, client, "t", "https://files.slack.com/exact", &buf, limit)
		require.NoError(t, err)
		assert.Equal(t, limit, buf.Len())
	})

	t.Run("refuses declared oversize body", func(t *testing.T) {
		var buf bytes.Buffer
		err := downloadFile(ctx, client, "t", "https://files.slack.com/big-declared", &buf, limit)
		assert.True(t, errors.Is(err, ErrFileTooLarge), "got %v", err)
	})

	t.Run("stops reading undeclared oversize body", func(t *testing.T) {
		var buf bytes.Buffer
		err := downloadFile(ctx, client, "t", "https://files.slack.com/big-chunked", &buf, limit)
		assert.True(t, errors.Is(err, ErrFileTooLarge), "got %v", err)
		assert.LessOrEqual(t, buf.Len(), limit+1, "must not buffer past the cap")
	})

	t.Run("reports HTTP errors", func(t *testing.T) {
		var buf bytes.Buffer
		err := downloadFile(ctx, client, "t", "https://files.slack.com/missing", &buf, limit)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
	})

	t.Run("never contacts non-Slack hosts", func(t *testing.T) {
		gotAuth = ""
		var buf bytes.Buffer
		err := downloadFile(ctx, client, "secret", "https://evil.example/small", &buf, limit)
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "non-Slack host"), err.Error())
		assert.Empty(t, gotAuth, "token must not be sent")
	})
}

func TestUnitMaxFileDownloadBytes(t *testing.T) {
	assert.Equal(t, 5*1024*1024, MaxFileDownloadBytes)
}
