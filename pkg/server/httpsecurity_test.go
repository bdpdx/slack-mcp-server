package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestUnitIsLoopbackHost(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST", "127.0.0.1", "127.1.2.3", "::1", "[::1]"} {
		assert.True(t, IsLoopbackHost(h), h)
	}
	for _, h := range []string{"0.0.0.0", "::", "", "192.168.1.10", "example.com"} {
		assert.False(t, IsLoopbackHost(h), h)
	}
}

func TestUnitNewHTTPSecurityRequiresKey(t *testing.T) {
	_, err := NewHTTPSecurity("127.0.0.1", "13080", envMap(nil), nil)
	require.ErrorContains(t, err, "SLACK_MCP_API_KEY")

	_, err = NewHTTPSecurity("127.0.0.1", "13080", envMap(map[string]string{EnvAllowUnauthenticated: "false"}), nil)
	require.Error(t, err)

	_, err = NewHTTPSecurity("127.0.0.1", "13080", envMap(map[string]string{EnvAllowUnauthenticated: "true"}), nil)
	require.NoError(t, err)

	_, err = NewHTTPSecurity("localhost", "13080", envMap(map[string]string{EnvAllowUnauthenticated: "yes"}), nil)
	require.NoError(t, err)

	for _, host := range []string{"0.0.0.0", "::", "192.168.1.10"} {
		_, err = NewHTTPSecurity(host, "13080", envMap(map[string]string{EnvAllowUnauthenticated: "true"}), nil)
		require.ErrorContains(t, err, "non-loopback", host)
	}

	_, err = NewHTTPSecurity("0.0.0.0", "13080", envMap(map[string]string{"SLACK_MCP_API_KEY": "k"}), nil)
	require.NoError(t, err)

	_, err = NewHTTPSecurity("127.0.0.1", "13080", envMap(map[string]string{"SLACK_MCP_SSE_API_KEY": "k"}), nil)
	require.NoError(t, err, "deprecated key variable still counts")

	_, err = NewHTTPSecurity("127.0.0.1", "13080", envMap(map[string]string{"SLACK_MCP_API_KEY": "k", EnvAllowedOrigins: "*"}), nil)
	require.Error(t, err)
}

type wrapCase struct {
	name   string
	method string
	host   string
	header map[string]string
	want   int
}

func TestUnitHTTPSecurityWrap(t *testing.T) {
	sec, err := NewHTTPSecurity("127.0.0.1", "13080", envMap(map[string]string{
		"SLACK_MCP_API_KEY": "secret",
		EnvAllowedOrigins:   "https://app.example.com/",
		EnvAllowedHosts:     "mcp.internal, proxy.example.com:443",
	}), zap.NewNop())
	require.NoError(t, err)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
	})
	h := sec.Wrap(next)

	auth := map[string]string{"Authorization": "Bearer secret", "Content-Type": "application/json"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range auth {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	cases := []wrapCase{
		{"authorized POST", http.MethodPost, "127.0.0.1:13080", auth, http.StatusOK},
		{"localhost host", http.MethodPost, "localhost:13080", auth, http.StatusOK},
		{"ipv6 loopback host", http.MethodPost, "[::1]:13080", auth, http.StatusOK},
		{"extra allowed host gets bind port", http.MethodPost, "mcp.internal:13080", auth, http.StatusOK},
		{"extra allowed host with port", http.MethodPost, "proxy.example.com:443", auth, http.StatusOK},
		{"rebinding host rejected", http.MethodPost, "evil.example.com:13080", auth, http.StatusForbidden},
		{"host without port rejected", http.MethodPost, "127.0.0.1", auth, http.StatusForbidden},
		{"wrong port rejected", http.MethodPost, "127.0.0.1:80", auth, http.StatusForbidden},
		{"missing auth on SSE GET", http.MethodGet, "127.0.0.1:13080", nil, http.StatusUnauthorized},
		{"authorized SSE GET", http.MethodGet, "127.0.0.1:13080", map[string]string{"Authorization": "Bearer secret"}, http.StatusOK},
		{"raw key accepted", http.MethodGet, "127.0.0.1:13080", map[string]string{"Authorization": "secret"}, http.StatusOK},
		{"wrong key", http.MethodPost, "127.0.0.1:13080", with(map[string]string{"Authorization": "Bearer nope"}), http.StatusUnauthorized},
		{"foreign origin rejected", http.MethodPost, "127.0.0.1:13080", with(map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden},
		{"allowed origin", http.MethodPost, "127.0.0.1:13080", with(map[string]string{"Origin": "https://APP.example.com"}), http.StatusOK},
		{"POST without JSON content type", http.MethodPost, "127.0.0.1:13080", map[string]string{"Authorization": "Bearer secret", "Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"POST JSON with charset", http.MethodPost, "127.0.0.1:13080", with(map[string]string{"Content-Type": "application/json; charset=utf-8"}), http.StatusOK},
		{"preflight from foreign origin", http.MethodOptions, "127.0.0.1:13080", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"preflight from allowed origin", http.MethodOptions, "127.0.0.1:13080", map[string]string{"Origin": "https://app.example.com"}, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://"+tc.host+"/mcp", strings.NewReader("{}"))
			req.Host = tc.host
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}

	t.Run("wildcard CORS is stripped", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:13080/mcp", strings.NewReader("{}"))
		for k, v := range auth {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("CORS echoes only the allowed origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:13080/mcp", strings.NewReader("{}"))
		for k, v := range with(map[string]string{"Origin": "https://app.example.com"}) {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	})
}

func TestUnitHTTPSecurityWithRealSSEServer(t *testing.T) {
	sec, err := NewHTTPSecurity("127.0.0.1", "0", envMap(map[string]string{"SLACK_MCP_API_KEY": "secret"}), zap.NewNop())
	require.NoError(t, err)

	mcpSrv := &MCPServer{server: server.NewMCPServer("test", "1.0.0"), logger: zap.NewNop()}
	sse := mcpSrv.ServeSSE("127.0.0.1", "0")
	ts := httptest.NewServer(sec.Wrap(sse))
	defer ts.Close()

	// httptest binds a random port; send the Host the policy expects.
	do := func(authz string) *http.Response {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/sse", nil)
		require.NoError(t, err)
		req.Host = "127.0.0.1:0"
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		return resp
	}

	resp := do("")
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "SSE session endpoint requires auth")

	resp = do("Bearer secret")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"), "mcp-go's wildcard CORS header must be stripped")

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "event: endpoint\n", line)
}
