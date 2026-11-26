package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"
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
	// TLSConfig provides certificates when TLS is on: its GetCertificate
	// (and NextProtos, which must include ACME's "acme-tls/1").
	TLSConfig *tls.Config
	// HTTPChallenge wraps the plain-HTTP handler so ACME HTTP-01 challenges
	// are answered.
	HTTPChallenge func(http.Handler) http.Handler
	// HTTPListener and HTTPSListener are already-bound sockets (systemd socket
	// activation). When nil, the addresses above are bound instead.
	HTTPListener  net.Listener
	HTTPSListener net.Listener
	// Cluster connections share the HTTPS port: TLS handshakes offering
	// ClusterALPN get ClusterTLS and are handed to Cluster, which serves the
	// connection until it ends.
	ClusterALPN string
	ClusterTLS  *tls.Config
	Cluster     func(*tls.Conn)
	// OnDrained runs during shutdown once proxied requests have finished (or
	// the grace period passed), before waiting for remaining connections.
	// Long-lived cluster connections should be closed there.
	OnDrained func()
}

func listen(inherited net.Listener, addr string) (net.Listener, error) {
	if inherited != nil {
		return inherited, nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", addr, err)
	}
	return l, nil
}

// ShutdownGrace is how long in-flight requests may finish when the proxy stops.
const ShutdownGrace = 30 * time.Second

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
		if opts.TLSConfig == nil {
			return nil, fmt.Errorf("TLS is on but no certificate source is configured")
		}
		// Redirects must keep a non-standard HTTPS port.
		if _, port, err := net.SplitHostPort(opts.HTTPSAddr); err == nil && port != "443" {
			p.httpsPort = ":" + port
		}
		tlsConf := opts.TLSConfig.Clone()
		tlsConf.MinVersion = tls.VersionTLS12
		s.https = &http.Server{
			Handler:           hsts(p),
			TLSConfig:         tlsConf,
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       120 * time.Second,
			ErrorLog:          quietLog(p),
		}
		if opts.Cluster != nil && opts.ClusterTLS != nil {
			tlsConf.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				for _, proto := range hello.SupportedProtos {
					if proto == opts.ClusterALPN {
						return opts.ClusterTLS, nil
					}
				}
				return nil, nil
			}
			s.https.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
				opts.ClusterALPN: func(_ *http.Server, c *tls.Conn, _ http.Handler) { opts.Cluster(c) },
			}
			// A custom TLSNextProto turns off Go's automatic HTTP/2; turn it back on.
			if err := http2.ConfigureServer(s.https, nil); err != nil {
				return nil, err
			}
		}
		l, err := listen(opts.HTTPSListener, opts.HTTPSAddr)
		if err != nil {
			return nil, err
		}
		s.tlsL = l
		httpHandler = http.HandlerFunc(p.redirectHTTPS)
		if opts.HTTPChallenge != nil {
			httpHandler = opts.HTTPChallenge(httpHandler)
		}
	}

	s.http = &http.Server{
		Handler:           httpHandler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          quietLog(p),
	}
	l, err := listen(opts.HTTPListener, opts.HTTPAddr)
	if err != nil {
		if s.tlsL != nil {
			s.tlsL.Close()
		}
		return nil, err
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
	// Stop accepting and let in-flight requests finish. Under socket
	// activation new connections queue in the kernel meanwhile and are served
	// by the next daemon process.
	shutCtx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()
	done := make(chan struct{}, 2)
	go func() { _ = s.http.Shutdown(shutCtx); done <- struct{}{} }()
	go func() {
		if s.https != nil {
			_ = s.https.Shutdown(shutCtx)
		}
		done <- struct{}{}
	}()
	// Requests to replicas on other nodes travel over cluster connections, so
	// those may only close once no request is in flight.
	for s.proxy.inFlight() > 0 && shutCtx.Err() == nil {
		time.Sleep(25 * time.Millisecond)
	}
	if s.opts.OnDrained != nil {
		s.opts.OnDrained()
	}
	<-done
	<-done
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
