package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// TLS modes.
const (
	TLSAuto = "auto" // Let's Encrypt certificates, issued on demand for routed hosts
	TLSOff  = "off"  // plain HTTP only (local use, or behind another TLS terminator)
)

type ServerOptions struct {
	HTTPAddr  string
	HTTPSAddr string
	TLS       string
	// CertDir caches issued certificates.
	CertDir string
	// Email is the ACME account contact (optional).
	Email string
	// ACMEDirectory overrides the ACME endpoint (e.g. Let's Encrypt staging).
	ACMEDirectory string
}

// Server owns the proxy's listeners.
type Server struct {
	proxy *Proxy
	opts  ServerOptions
	http  *http.Server
	https *http.Server
	httpL net.Listener
	tlsL  net.Listener
}

// Listen binds the configured addresses. Binding is separate from serving so
// that address conflicts fail fast at startup.
func (p *Proxy) Listen(opts ServerOptions) (*Server, error) {
	s := &Server{proxy: p, opts: opts}
	var httpHandler http.Handler = p

	if opts.TLS == TLSAuto {
		mgr := &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Cache:  autocert.DirCache(opts.CertDir),
			Email:  opts.Email,
			HostPolicy: func(_ context.Context, host string) error {
				if p.HasHost(host) {
					return nil
				}
				return fmt.Errorf("saba: host %q is not routed to any app", host)
			},
		}
		if opts.ACMEDirectory != "" {
			mgr.Client = &acme.Client{DirectoryURL: opts.ACMEDirectory}
		}
		// Redirects must keep a non-standard HTTPS port.
		if _, port, err := net.SplitHostPort(opts.HTTPSAddr); err == nil && port != "443" {
			p.httpsPort = ":" + port
		}
		tlsConf := mgr.TLSConfig()
		tlsConf.MinVersion = tls.VersionTLS12
		s.https = &http.Server{
			Handler:           hsts(p),
			TLSConfig:         tlsConf,
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       120 * time.Second,
			ErrorLog:          quietLog(p),
		}
		l, err := net.Listen("tcp", opts.HTTPSAddr)
		if err != nil {
			return nil, fmt.Errorf("cannot listen on %s: %w", opts.HTTPSAddr, err)
		}
		s.tlsL = l
		httpHandler = mgr.HTTPHandler(http.HandlerFunc(p.redirectHTTPS))
	}

	s.http = &http.Server{
		Handler:           httpHandler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          quietLog(p),
	}
	l, err := net.Listen("tcp", opts.HTTPAddr)
	if err != nil {
		if s.tlsL != nil {
			s.tlsL.Close()
		}
		return nil, fmt.Errorf("cannot listen on %s: %w", opts.HTTPAddr, err)
	}
	s.httpL = l
	return s, nil
}

// HTTPAddr returns the bound HTTP address (useful when listening on port 0).
func (s *Server) HTTPAddr() string { return s.httpL.Addr().String() }

// Serve blocks until ctx is cancelled or a listener fails.
func (s *Server) Serve(ctx context.Context) error {
	errc := make(chan error, 2)
	go func() { errc <- s.http.Serve(s.httpL) }()
	if s.https != nil {
		go func() { errc <- s.https.ServeTLS(s.tlsL, "", "") }()
	}
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = s.http.Shutdown(shutCtx)
	if s.https != nil {
		_ = s.https.Shutdown(shutCtx)
	}
	return nil
}

func (p *Proxy) redirectHTTPS(w http.ResponseWriter, req *http.Request) {
	if !p.HasHost(req.Host) {
		errorPage(w, req, http.StatusNotFound, "No app is configured for this domain",
			fmt.Sprintf("Nothing is routed to %s on this server.", hostOnly(req.Host)))
		return
	}
	target := "https://" + hostOnly(req.Host) + p.httpsPort + req.URL.RequestURI()
	http.Redirect(w, req, target, http.StatusPermanentRedirect)
}

func hsts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		next.ServeHTTP(w, r)
	})
}
