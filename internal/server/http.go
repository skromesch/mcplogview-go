package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skromesch/mcplogview-go/internal/config"
	"github.com/skromesch/mcplogview-go/internal/serverlog"
)

// RunHTTP serves Streamable HTTP (or HTTPS with a dev certificate) until ctx ends.
func RunHTTP(ctx context.Context, srv *mcp.Server, cfg config.Config, logger serverlog.Logger, token func() string) error {
	tc := cfg.Transport
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	if token == nil {
		token = func() string { return "" }
	}
	if !tc.IsLoopback() && token() == "" && !tc.AllowNoAuth {
		err := fmt.Errorf("refusing to listen on %s without an auth token: set one with `mcplogview transport --new-token` (or $%s); on a trusted network you can set transport.allowNoAuth", tc.Host, config.EnvToken)
		logger.Error("http server not started", err)
		return err
	}

	mux := http.NewServeMux()
	mux.Handle(tc.Path, withRequestLog(withCORS(withAuth(handler, token, logger), corsPolicy(tc, token)), logger, cfg.Logging.Debug))

	addr := net.JoinHostPort(tc.Host, strconv.Itoa(tc.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, errAddrInUse) {
			fmt.Fprintf(os.Stderr, "Port already in use: %s. Stop the other process or change transport.port in config.\n", addr)
			logger.Error(fmt.Sprintf("network listen failed code=EADDRINUSE host=%s port=%d", tc.Host, tc.Port), err)
		} else {
			logger.Error("http server listen error", err)
		}
		return err
	}

	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	scheme := "http"
	if tc.Mode == config.TransportHTTPSDev {
		cert, err := devCertificate(tc, logger)
		if err != nil {
			_ = ln.Close()
			return err
		}
		httpSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		ln = tls.NewListener(ln, httpSrv.TLSConfig)
		scheme = "https"
	}

	auth := "auth: bearer token required"
	if token() == "" {
		auth = "auth: NONE"
	}
	msg := fmt.Sprintf("MCP Streamable HTTP listening on %s://%s%s (%s)", scheme, addr, tc.Path, auth)
	fmt.Fprintln(os.Stderr, msg)
	logger.Info(msg)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// validBearer reports whether header is "Bearer <want>". The scheme is matched
// case-insensitively (RFC 7235) and is mandatory; the token comparison is
// constant-time.
func validBearer(header, want string) bool {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(want)) == 1
}

// withAuth requires "Authorization: Bearer <token>" when a token is configured.
// The token is read per request, so changing it in the config takes effect
// through the config hot reload. Preflight (OPTIONS) requests stay open.
func withAuth(next http.Handler, token func() string, logger serverlog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := token()
		if want == "" || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if !validBearer(r.Header.Get("Authorization"), want) {
			logger.Info(fmt.Sprintf("http request rejected: missing or wrong token method=%s url=%s remote=%s", r.Method, r.URL.Path, r.RemoteAddr))
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcplogview"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// corsPolicy decides which browser origins may talk to the endpoint.
//
//   - With a token configured, any origin is fine: the token is a bearer
//     header, not an ambient cookie, so a foreign web page cannot use it.
//   - Without a token only local origins (localhost, 127.0.0.1, [::1]) and the
//     explicitly allowed ones pass, so a random website cannot read your logs
//     through your browser.
func corsPolicy(tc config.Transport, token func() string) func(origin string) bool {
	return func(origin string) bool {
		if token() != "" {
			return true
		}
		for _, o := range tc.AllowedOrigins {
			if o == "*" || strings.EqualFold(o, origin) {
				return true
			}
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
}

// withCORS adds CORS headers to every response of an allowed browser origin
// (not just to preflights, or browsers hide the response from the page) and
// exposes the headers MCP clients need to read.
func withCORS(next http.Handler, allowed func(origin string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		ok := origin != "" && allowed(origin)
		if ok {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id, Mcp-Protocol-Version, WWW-Authenticate")
		}
		if r.Method != http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if origin != "" && !ok {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		headers := r.Header.Get("Access-Control-Request-Headers")
		if headers == "" {
			headers = "content-type,accept,authorization,mcp-session-id,mcp-protocol-version"
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		h.Set("Access-Control-Allow-Headers", headers)
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush keeps SSE streaming working through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func withRequestLog(next http.Handler, logger serverlog.Logger, debug bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info(fmt.Sprintf("http request method=%s url=%s", r.Method, r.URL.String()))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if debug {
			logger.Info(fmt.Sprintf("http response status=%d", rec.status))
		}
		if rec.status >= 500 {
			logger.Error(fmt.Sprintf("http response >=500 method=%s url=%s status=%d", r.Method, r.URL.String(), rec.status), nil)
		}
	})
}

// devCertificate loads httpsKey/httpsCert, or generates a self-signed cert.
func devCertificate(tc config.Transport, logger serverlog.Logger) (tls.Certificate, error) {
	if tc.HTTPSKey != "" && tc.HTTPSCert != "" {
		cert, err := tls.LoadX509KeyPair(tc.HTTPSCert, tc.HTTPSKey)
		if err == nil {
			return cert, nil
		}
		logger.Error("failed to load https key/cert from files, generating self-signed", err)
	}
	return selfSigned()
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
