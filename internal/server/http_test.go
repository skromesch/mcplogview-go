package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startHTTP runs RunHTTP on a free loopback port and waits until it accepts connections.
func startHTTP(t *testing.T, token func() string) (base string, rt *Runtime) {
	t.Helper()
	return startHTTPWith(t, token, nil)
}

func startHTTPWith(t *testing.T, token func() string, mutate func(*config.Config)) (base string, rt *Runtime) {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "x.log")
	_ = os.WriteFile(logFile, []byte("hello\n"), 0o644)

	cfg := config.Defaults()
	cfg.Logging.Enabled = false
	cfg.Transport.Mode = config.TransportHTTP
	cfg.Transport.Port = freePort(t)
	cfg.Sources = []config.Source{{Type: "local", ID: "app", Label: "App", Path: logFile}}
	if mutate != nil {
		mutate(&cfg)
	}
	rt = NewRuntime(cfg, serverlog.Noop())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- RunHTTP(ctx, New(rt), cfg, serverlog.Noop(), token) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("server did not shut down")
		}
	})

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Transport.Port)
	waitFor(t, "http server to listen", func() bool {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	return "http://" + addr + cfg.Transport.Path, rt
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func post(t *testing.T, url, authHeader string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHTTPRequiresBearerToken(t *testing.T) {
	var token atomic.Value
	token.Store("s3cret-token-0123456789")
	base, _ := startHTTP(t, func() string { return token.Load().(string) })

	if r := post(t, base, ""); r.StatusCode != 401 || !strings.Contains(r.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("no token: %d %q", r.StatusCode, r.Header.Get("WWW-Authenticate"))
	}
	if r := post(t, base, "Bearer wrong"); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	if r := post(t, base, "s3cret-token-0123456789"); r.StatusCode != 401 {
		t.Fatalf("token without the Bearer scheme must be rejected: %d", r.StatusCode)
	}
	if r := post(t, base, "Basic s3cret-token-0123456789"); r.StatusCode != 401 {
		t.Fatalf("other auth schemes must be rejected: %d", r.StatusCode)
	}
	if r := post(t, base, "Bearer s3cret-token-0123456789"); r.StatusCode != 200 {
		t.Fatalf("right token: %d", r.StatusCode)
	}
	if r := post(t, base, "bearer s3cret-token-0123456789"); r.StatusCode != 200 {
		t.Fatalf("scheme is case-insensitive: %d", r.StatusCode)
	}

	// CORS preflight carries no credentials and must not need them
	req, _ := http.NewRequest(http.MethodOptions, base, nil)
	req.Header.Set("Origin", "http://example.test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("preflight: %v %v", resp, err)
	}
	resp.Body.Close()

	// rotating the token takes effect on the next request, no restart
	token.Store("brand-new-token-0123456789")
	if r := post(t, base, "Bearer s3cret-token-0123456789"); r.StatusCode != 401 {
		t.Fatalf("old token must stop working after rotation: %d", r.StatusCode)
	}
	if r := post(t, base, "Bearer brand-new-token-0123456789"); r.StatusCode != 200 {
		t.Fatalf("new token: %d", r.StatusCode)
	}
}

func TestHTTPWithoutTokenOnLoopbackStaysOpen(t *testing.T) {
	base, _ := startHTTP(t, func() string { return "" })
	if r := post(t, base, ""); r.StatusCode != 200 {
		t.Fatalf("loopback without a token should work as before: %d", r.StatusCode)
	}
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPClientOverAuthenticatedHTTP(t *testing.T) {
	const tok = "client-token-0123456789"
	base, _ := startHTTP(t, func() string { return tok })

	ctx := context.Background()
	// without the token the handshake fails
	bad, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base, MaxRetries: -1}, nil)
	if err == nil {
		bad.Close()
		t.Fatal("connecting without a token must fail")
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base, HTTPClient: &http.Client{Transport: bearerRT{tok}}}, nil)
	if err != nil {
		t.Fatalf("connect with token: %v", err)
	}
	defer cs.Close()
	text, isErr := call(t, cs, "read_log_chunk", map[string]any{"sourceId": "app"})
	if isErr || !strings.Contains(text, `"lines":["hello"]`) {
		t.Fatalf("%q %v", text, isErr)
	}
}

func TestHTTPRefusesNetworkWithoutToken(t *testing.T) {
	cfg := config.Defaults()
	cfg.Logging.Enabled = false
	cfg.Transport.Mode = config.TransportHTTP
	cfg.Transport.Host = "0.0.0.0"
	cfg.Transport.Port = freePort(t)
	rt := NewRuntime(cfg, serverlog.Noop())

	err := RunHTTP(context.Background(), New(rt), cfg, serverlog.Noop(), func() string { return "" })
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("must refuse to listen on the network without a token, got %v", err)
	}
	// the escape hatch is explicit
	cfg.Transport.AllowNoAuth = true
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- RunHTTP(ctx, New(rt), cfg, serverlog.Noop(), func() string { return "" }) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("allowNoAuth should start: %v", err)
	}
}

func postWithOrigin(t *testing.T, url, origin, authHeader string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func preflight(t *testing.T, url, origin string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodOptions, url, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type,authorization,mcp-protocol-version")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// A browser-based client (MCP Inspector) can only read a response, and the
// session id in it, if the *actual* response carries the CORS headers.
func TestCORSHeadersOnRealResponsesWithToken(t *testing.T) {
	const tok = "cors-token-0123456789"
	base, _ := startHTTP(t, func() string { return tok })
	origin := "http://localhost:6274"

	ok := postWithOrigin(t, base, origin, "Bearer "+tok)
	if ok.StatusCode != 200 || ok.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("success response needs CORS headers: %d %v", ok.StatusCode, ok.Header)
	}
	if exp := ok.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(exp, "Mcp-Session-Id") {
		t.Fatalf("Mcp-Session-Id must be exposed to the page, got %q", exp)
	}
	if ok.Header.Get("Mcp-Session-Id") == "" {
		t.Fatal("session id missing")
	}

	// the 401 must be readable too, otherwise the page only sees an opaque network error
	denied := postWithOrigin(t, base, origin, "")
	if denied.StatusCode != 401 || denied.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("401 needs CORS headers: %d %v", denied.StatusCode, denied.Header)
	}

	pf := preflight(t, base, origin)
	if pf.StatusCode != 204 || pf.Header.Get("Access-Control-Allow-Origin") != origin ||
		!strings.Contains(strings.ToLower(pf.Header.Get("Access-Control-Allow-Headers")), "authorization") {
		t.Fatalf("preflight: %d %v", pf.StatusCode, pf.Header)
	}
}

// Without a token a foreign web page must not be able to read the logs through
// the user's browser.
func TestCORSWithoutTokenOnlyAllowsLocalOrigins(t *testing.T) {
	base, _ := startHTTP(t, func() string { return "" })

	for _, origin := range []string{"http://localhost:6274", "http://127.0.0.1:6274", "http://[::1]:6274"} {
		if r := postWithOrigin(t, base, origin, ""); r.Header.Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("local origin %q should be allowed: %v", origin, r.Header)
		}
	}
	if r := postWithOrigin(t, base, "https://evil.example", ""); r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("a foreign origin must get no CORS permission: %v", r.Header)
	}
	if r := preflight(t, base, "https://evil.example"); r.StatusCode != 403 || r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight from a foreign origin must be refused: %d %v", r.StatusCode, r.Header)
	}
	// non-browser clients send no Origin at all and are unaffected
	if r := postWithOrigin(t, base, "", ""); r.StatusCode != 200 || r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("no Origin: %d %v", r.StatusCode, r.Header)
	}
}

func TestCORSAllowedOriginsConfig(t *testing.T) {
	base, _ := startHTTPWith(t, func() string { return "" }, func(c *config.Config) {
		c.Transport.AllowedOrigins = []string{"https://inspector.example"}
	})
	if r := postWithOrigin(t, base, "https://inspector.example", ""); r.Header.Get("Access-Control-Allow-Origin") != "https://inspector.example" {
		t.Fatalf("explicitly allowed origin: %v", r.Header)
	}
	if r := postWithOrigin(t, base, "https://other.example", ""); r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("other origins stay blocked: %v", r.Header)
	}
}
