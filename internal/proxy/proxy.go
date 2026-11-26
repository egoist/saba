// Package proxy is saba's built-in reverse proxy. It terminates HTTP(S),
// routes requests by hostname to the live containers of an app and lets the
// deploy engine swap an app's upstreams atomically, drain the old ones and
// briefly hold requests while a single-instance app is being replaced.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream is one container endpoint.
type Upstream struct {
	Addr      string
	downUntil atomic.Int64 // unix nanos; passive health marking
}

func (u *Upstream) down() bool { return time.Now().UnixNano() < u.downUntil.Load() }

// Pool is an immutable set of upstreams serving one version of an app.
type Pool struct {
	Version   int
	upstreams []*Upstream
	next      atomic.Uint32
	inflight  atomic.Int64
	// Each pool has its own connection pool so that retiring a version can
	// close exactly its keep-alive connections. Servers that shut down
	// gracefully otherwise wait for those idle connections before exiting.
	transport *http.Transport
}

func newPool(version int, addrs []string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *Pool {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	p := &Pool{Version: version, transport: &http.Transport{
		DialContext:           dial,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}}
	for _, a := range addrs {
		p.upstreams = append(p.upstreams, &Upstream{Addr: a})
	}
	return p
}

// Close releases the pool's idle upstream connections. Call it once the pool
// has been replaced and drained.
func (p *Pool) Close() { p.transport.CloseIdleConnections() }

// InFlight returns the number of requests currently being served by the pool.
func (p *Pool) InFlight() int64 { return p.inflight.Load() }

// Drain waits until the pool has no in-flight requests or ctx ends. It
// returns the number of requests still in flight.
func (p *Pool) Drain(ctx context.Context) int64 {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		if n := p.inflight.Load(); n == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return p.inflight.Load()
		case <-t.C:
		}
	}
}

type route struct {
	name     string
	hosts    []string
	pool     atomic.Pointer[Pool]
	gate     atomic.Pointer[chan struct{}] // non-nil while paused
	requests atomic.Uint64
	errors   atomic.Uint64
}

// Stats is a snapshot of an app's traffic counters.
type Stats struct {
	Requests uint64
	Errors   uint64
	InFlight int64
}

type Proxy struct {
	mu        sync.RWMutex
	apps      map[string]*route
	hosts     map[string]*route
	rp        *httputil.ReverseProxy
	log       *slog.Logger
	httpsPort string // ":8443" when HTTPS is not on 443
	// PauseTimeout bounds how long a request is held while an app is paused.
	PauseTimeout time.Duration
	// Dial, if set, connects to upstream addresses (replicas on other nodes
	// are reached through their agent). Failures must be *net.OpError dial
	// errors for failover to work.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// OnHostsChanged, if set, is called (in its own goroutine) after the set
	// of routed hostnames changed.
	OnHostsChanged func()
}

type ctxKey struct{}

func New(log *slog.Logger) *Proxy {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p := &Proxy{
		apps:         map[string]*route{},
		hosts:        map[string]*route{},
		log:          log,
		PauseTimeout: 60 * time.Second,
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "upstream.invalid" // replaced per attempt by lbTransport
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport:     lbTransport{},
		FlushInterval: -1,
		ErrorHandler:  p.onError,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	return p
}

// SetApp atomically replaces an app's hostnames and upstreams. It returns the
// previous pool (nil if there was none) so the caller can drain it.
func (p *Proxy) SetApp(name string, version int, hosts, addrs []string) *Pool {
	pool := newPool(version, addrs, p.Dial)
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.apps[name]
	if r == nil {
		r = &route{name: name}
		p.apps[name] = r
	}
	old := strings.Join(r.hosts, " ")
	defer func() {
		if strings.Join(r.hosts, " ") != old {
			p.hostsChanged()
		}
	}()
	for _, h := range r.hosts {
		if p.hosts[h] == r {
			delete(p.hosts, h)
		}
	}
	r.hosts = r.hosts[:0:0]
	for _, h := range hosts {
		h = strings.ToLower(h)
		r.hosts = append(r.hosts, h)
		p.hosts[h] = r
	}
	return r.pool.Swap(pool)
}

// RemoveApp drops an app's routes.
func (p *Proxy) RemoveApp(name string) *Pool {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.apps[name]
	if r == nil {
		return nil
	}
	if len(r.hosts) > 0 {
		p.hostsChanged()
	}
	for _, h := range r.hosts {
		if p.hosts[h] == r {
			delete(p.hosts, h)
		}
	}
	delete(p.apps, name)
	p.resumeLocked(r)
	return r.pool.Swap(nil)
}

// Pause makes requests for the app wait (up to PauseTimeout) until Resume.
func (p *Proxy) Pause(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.apps[name]
	if r == nil || r.gate.Load() != nil {
		return
	}
	ch := make(chan struct{})
	r.gate.Store(&ch)
}

// Resume releases requests held by Pause.
func (p *Proxy) Resume(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.apps[name]; r != nil {
		p.resumeLocked(r)
	}
}

func (p *Proxy) resumeLocked(r *route) {
	if ch := r.gate.Swap(nil); ch != nil {
		close(*ch)
	}
}

// inFlight counts requests being served across all apps.
func (p *Proxy) inFlight() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var n int64
	for _, r := range p.apps {
		if pool := r.pool.Load(); pool != nil {
			n += pool.InFlight()
		}
	}
	return n
}

// Pool returns the app's current pool, or nil.
func (p *Proxy) Pool(name string) *Pool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if r := p.apps[name]; r != nil {
		return r.pool.Load()
	}
	return nil
}

// Stats returns traffic counters for an app.
func (p *Proxy) Stats(name string) (Stats, bool) {
	p.mu.RLock()
	r := p.apps[name]
	p.mu.RUnlock()
	if r == nil {
		return Stats{}, false
	}
	s := Stats{Requests: r.requests.Load(), Errors: r.errors.Load()}
	if pool := r.pool.Load(); pool != nil {
		s.InFlight = pool.InFlight()
	}
	return s, true
}

func (p *Proxy) hostsChanged() {
	if f := p.OnHostsChanged; f != nil {
		go f()
	}
}

// Hosts lists the routed hostnames, sorted.
func (p *Proxy) Hosts() []string {
	p.mu.RLock()
	out := make([]string, 0, len(p.hosts))
	for h := range p.hosts {
		out = append(out, h)
	}
	p.mu.RUnlock()
	sort.Strings(out)
	return out
}

// HasHost reports whether a hostname is routed.
func (p *Proxy) HasHost(host string) bool { return p.lookup(host) != nil }

// MatchHost says how a hostname is routed: exactly, through a wildcard route
// (which it then returns, e.g. "*.example.com"), or not at all.
func (p *Proxy) MatchHost(host string) (exact bool, wildcard string, ok bool) {
	host = strings.TrimSuffix(strings.ToLower(hostOnly(host)), ".")
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.hosts[host] != nil {
		return true, "", true
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		if w := "*" + host[i:]; p.hosts[w] != nil {
			return false, w, true
		}
	}
	return false, "", false
}

func (p *Proxy) lookup(host string) *route {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	p.mu.RLock()
	defer p.mu.RUnlock()
	if r := p.hosts[host]; r != nil {
		return r
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		return p.hosts["*"+host[i:]]
	}
	return nil
}

// acquire pins the route's current pool for one request. The re-check closes
// the window in which a deploy could swap the pool, see zero in-flight
// requests and stop the containers this request is about to use.
func (r *route) acquire() *Pool {
	for {
		pool := r.pool.Load()
		if pool == nil {
			return nil
		}
		pool.inflight.Add(1)
		if r.pool.Load() == pool {
			return pool
		}
		pool.inflight.Add(-1)
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r := p.lookup(req.Host)
	if r == nil {
		errorPage(w, req, http.StatusNotFound, "No app is configured for this domain",
			fmt.Sprintf("Nothing is routed to %s on this server.", hostOnly(req.Host)))
		return
	}
	r.requests.Add(1)
	if gate := r.gate.Load(); gate != nil {
		t := time.NewTimer(p.PauseTimeout)
		select {
		case <-*gate:
			t.Stop()
		case <-req.Context().Done():
			t.Stop()
			return
		case <-t.C:
			r.errors.Add(1)
			errorPage(w, req, http.StatusServiceUnavailable, "The app is restarting",
				"A new version is being deployed. Try again in a few seconds.")
			return
		}
	}
	pool := r.acquire()
	if pool == nil || len(pool.upstreams) == 0 {
		if pool != nil {
			pool.inflight.Add(-1)
		}
		r.errors.Add(1)
		w.Header().Set("Retry-After", "5")
		errorPage(w, req, http.StatusServiceUnavailable, "The app is not running",
			"This app has no running instances right now.")
		return
	}
	defer pool.inflight.Add(-1)
	ctx := context.WithValue(req.Context(), ctxKey{}, &reqState{pool: pool, route: r})
	p.rp.ServeHTTP(w, req.WithContext(ctx))
}

type reqState struct {
	pool  *Pool
	route *route
}

func (p *Proxy) onError(w http.ResponseWriter, req *http.Request, err error) {
	if errors.Is(err, context.Canceled) && req.Context().Err() != nil {
		return // client went away
	}
	if st, ok := req.Context().Value(ctxKey{}).(*reqState); ok {
		st.route.errors.Add(1)
		p.log.Warn("proxy error", "app", st.route.name, "host", req.Host, "path", req.URL.Path, "err", err)
	}
	errorPage(w, req, http.StatusBadGateway, "The app did not respond",
		"The app is not accepting connections or closed the connection unexpectedly.")
}

// lbTransport picks an upstream per request (round robin), skipping upstreams
// that recently refused connections and retrying dial failures on the next one.
type lbTransport struct{}

func (lbTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	st, ok := req.Context().Value(ctxKey{}).(*reqState)
	if !ok || len(st.pool.upstreams) == 0 {
		return nil, errors.New("proxy: no upstream available")
	}
	ups := st.pool.upstreams
	n := len(ups)
	start := int(st.pool.next.Add(1))
	// A failed dial never consumes the body, but the transport closes it;
	// shield it so the request can be retried on another upstream.
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = noClose{req.Body}
	}
	// Healthy upstreams first, in round-robin order; recently failed ones last.
	order := make([]*Upstream, 0, n)
	var down []*Upstream
	for i := 0; i < n; i++ {
		if u := ups[(start+i)%n]; u.down() {
			down = append(down, u)
		} else {
			order = append(order, u)
		}
	}
	order = append(order, down...)
	var lastErr error
	for _, u := range order {
		req.URL.Host = u.Addr
		resp, err := st.pool.transport.RoundTrip(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isDialError(err) || req.Context().Err() != nil {
			return nil, err
		}
		u.downUntil.Store(time.Now().Add(2 * time.Second).UnixNano())
	}
	return nil, lastErr
}

func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

type noClose struct{ io.Reader }

func (noClose) Close() error { return nil }

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func errorPage(w http.ResponseWriter, req *http.Request, status int, title, detail string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Saba-Error", http.StatusText(status))
	if strings.Contains(req.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, errorHTML, status, html.EscapeString(title), status, html.EscapeString(title), html.EscapeString(detail))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "%d %s\n%s\n", status, title, detail)
}

const errorHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%d %s</title>
<style>
:root{color-scheme:light dark}
body{margin:0;min-height:100vh;display:grid;place-items:center;font:16px/1.5 ui-sans-serif,system-ui,sans-serif;background:Canvas;color:CanvasText}
main{max-width:28rem;padding:2rem}
b{display:block;font:600 3rem/1 ui-monospace,monospace;opacity:.35;margin-bottom:1rem}
h1{font-size:1.25rem;margin:0 0 .5rem}
p{margin:0;opacity:.7}
</style></head>
<body><main><b>%d</b><h1>%s</h1><p>%s</p></main></body></html>
`

func quietLog(p *Proxy) *log.Logger { return slog.NewLogLogger(p.log.Handler(), slog.LevelDebug) }
