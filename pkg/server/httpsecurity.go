package server

import (
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bdpdx/slack-mcp-server/pkg/server/auth"
	"github.com/bdpdx/slack-mcp-server/pkg/toolconfig"
	"go.uber.org/zap"
)

// Environment variables that configure the sse/http transports.
const (
	EnvAllowUnauthenticated = "SLACK_MCP_ALLOW_UNAUTHENTICATED"
	EnvAllowedOrigins       = "SLACK_MCP_ALLOWED_ORIGINS"
	EnvAllowedHosts         = "SLACK_MCP_ALLOWED_HOSTS"
)

// HTTPSecurity guards the sse and http transports: it requires the API key
// on every request, rejects Host headers other than the bind address or
// loopback names (DNS rebinding), rejects cross-origin browser requests
// unless the origin is allow-listed, and requires JSON bodies on POST.
type HTTPSecurity struct {
	apiKey         string
	allowedHosts   map[string]bool
	allowedOrigins map[string]bool
	logger         *zap.Logger
}

// IsLoopbackHost reports whether host names the loopback interface.
func IsLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isWildcardHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

func normalizeOrigin(origin string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(origin)), "/")
}

// NewHTTPSecurity validates the transport configuration for a server bound
// to host:port. Without an API key it refuses to start unless
// SLACK_MCP_ALLOW_UNAUTHENTICATED is on and host is a loopback address; a
// non-loopback bind always needs a key.
func NewHTTPSecurity(host, port string, getenv func(string) string, logger *zap.Logger) (*HTTPSecurity, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	key, _ := auth.APIKey(getenv)
	if key == "" {
		if !IsLoopbackHost(host) {
			return nil, fmt.Errorf("refusing to listen on non-loopback address %q without an API key; set SLACK_MCP_API_KEY", host)
		}
		allow, ok := toolconfig.ParseBool(getenv(EnvAllowUnauthenticated))
		if !ok {
			return nil, fmt.Errorf("%s must be a boolean (true/false)", EnvAllowUnauthenticated)
		}
		if !allow {
			return nil, fmt.Errorf("refusing to start the HTTP transport without an API key; set SLACK_MCP_API_KEY, or set %s=true to run unauthenticated on a loopback address", EnvAllowUnauthenticated)
		}
		logger.Warn("HTTP transport is running without authentication on a loopback address",
			zap.String("context", "console"),
			zap.String("host", host),
		)
	}

	sec := &HTTPSecurity{
		apiKey:         key,
		allowedHosts:   make(map[string]bool),
		allowedOrigins: make(map[string]bool),
		logger:         logger,
	}

	names := []string{"localhost", "127.0.0.1", "::1"}
	if !isWildcardHost(host) {
		names = append(names, strings.Trim(host, "[]"))
	}
	for _, name := range names {
		sec.allowedHosts[strings.ToLower(net.JoinHostPort(name, port))] = true
	}
	for _, entry := range strings.Split(getenv(EnvAllowedHosts), ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(entry); err == nil {
			sec.allowedHosts[entry] = true
		} else {
			sec.allowedHosts[net.JoinHostPort(strings.Trim(entry, "[]"), port)] = true
		}
	}

	for _, origin := range strings.Split(getenv(EnvAllowedOrigins), ",") {
		if origin = normalizeOrigin(origin); origin != "" {
			if origin == "*" || origin == "null" {
				return nil, fmt.Errorf("%s must list explicit origins, not %q", EnvAllowedOrigins, origin)
			}
			sec.allowedOrigins[origin] = true
		}
	}

	return sec, nil
}

// Wrap returns next guarded by the security checks.
func (s *HTTPSecurity) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHosts[strings.ToLower(r.Host)] {
			s.logger.Warn("Rejected request with unexpected Host header", zap.String("context", "http"), zap.String("host", r.Host))
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}

		origin := r.Header.Get("Origin")
		allowedOrigin := ""
		if origin != "" {
			if !s.allowedOrigins[normalizeOrigin(origin)] {
				s.logger.Warn("Rejected cross-origin request", zap.String("context", "http"), zap.String("origin", origin))
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			allowedOrigin = origin
		}

		if r.Method == http.MethodOptions {
			if allowedOrigin == "" {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", allowedOrigin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if s.apiKey != "" && !auth.CheckAuthorization(r.Header.Get("Authorization"), s.apiKey) {
			s.logger.Warn("Rejected unauthenticated request", zap.String("context", "http"), zap.String("path", r.URL.Path))
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if r.Method == http.MethodPost {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}

		next.ServeHTTP(&corsResponseWriter{ResponseWriter: w, origin: allowedOrigin}, r)
	})
}

// corsResponseWriter rewrites any Access-Control-Allow-Origin header set by
// the wrapped handler (mcp-go's SSE handler sets "*") to the allow-listed
// request origin, or removes it.
type corsResponseWriter struct {
	http.ResponseWriter
	origin      string
	wroteHeader bool
}

func (c *corsResponseWriter) fixHeaders() {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	h := c.ResponseWriter.Header()
	if h.Get("Access-Control-Allow-Origin") == "" {
		return
	}
	if c.origin == "" {
		h.Del("Access-Control-Allow-Origin")
		return
	}
	h.Set("Access-Control-Allow-Origin", c.origin)
	h.Add("Vary", "Origin")
}

func (c *corsResponseWriter) WriteHeader(code int) {
	c.fixHeaders()
	c.ResponseWriter.WriteHeader(code)
}

func (c *corsResponseWriter) Write(b []byte) (int, error) {
	c.fixHeaders()
	return c.ResponseWriter.Write(b)
}

// Flush lets streaming handlers (SSE) flush through the wrapper.
func (c *corsResponseWriter) Flush() {
	c.fixHeaders()
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap supports http.ResponseController.
func (c *corsResponseWriter) Unwrap() http.ResponseWriter {
	return c.ResponseWriter
}

// newHTTPServer builds an http.Server with header and idle timeouts. There
// is no write timeout because SSE responses stream indefinitely.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

// ListenAndServeSSE serves the SSE transport on host:port behind sec.
func (s *MCPServer) ListenAndServeSSE(host, port string, sec *HTTPSecurity) error {
	if sec == nil {
		return errors.New("HTTP security configuration is required")
	}
	addr := net.JoinHostPort(host, port)
	sse := s.ServeSSE(host, port)
	return newHTTPServer(addr, sec.Wrap(sse)).ListenAndServe()
}

// ListenAndServeHTTP serves the streamable HTTP transport at /mcp on
// host:port behind sec.
func (s *MCPServer) ListenAndServeHTTP(host, port string, sec *HTTPSecurity) error {
	if sec == nil {
		return errors.New("HTTP security configuration is required")
	}
	addr := net.JoinHostPort(host, port)
	mux := http.NewServeMux()
	mux.Handle(httpEndpointPath, s.ServeHTTP(addr))
	return newHTTPServer(addr, sec.Wrap(mux)).ListenAndServe()
}
