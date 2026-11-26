package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/certs"
	"github.com/egoist/saba/internal/store"
)

// On-demand certificates for subdomains matched only by a wildcard route,
// when no DNS provider can issue the wildcard itself, are limited so that
// made-up names cannot exhaust Let's Encrypt's per-domain limits.
const wildcardOnDemandPerHour = 10

// tlsManager provides every certificate the proxy serves:
//   - uploaded certificates (certs.Store), used first for the names they list,
//   - wildcard certificates, managed by certmagic through ACME DNS-01 with a
//     libdns provider,
//   - everything else on demand: certmagic obtains a certificate for a routed
//     hostname on its first TLS handshake (HTTP-01 or TLS-ALPN-01).
type tlsManager struct {
	d      *Daemon
	store  *certs.Store
	cache  *certmagic.Cache
	logger *zap.Logger

	storage  certmagic.Storage
	onDemand *certmagic.Config

	syncMu      sync.Mutex // one sync at a time
	mu          sync.Mutex
	wild        *certmagic.Config // nil without a DNS provider
	wildCtx     context.Context   // cancelled when wild is replaced, ending its retries
	wildCancel  context.CancelFunc
	providerKey string
	managed     map[string]bool
	failures    map[string]string
	issued      map[string][]time.Time // wildcard route → recent on-demand issuances under it
}

func acmeRoots(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read --acme-ca-file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("--acme-ca-file %s contains no PEM certificates", caFile)
	}
	return pool, nil
}

func newTLSManager(d *Daemon) (*tlsManager, error) {
	store, err := certs.Open(filepath.Join(d.opts.DataDir, "certs", "store"))
	if err != nil {
		return nil, err
	}
	m := &tlsManager{
		d: d, store: store, logger: zap.NewNop(),
		storage:  &certmagic.FileStorage{Path: filepath.Join(d.opts.DataDir, "certs", "acme")},
		managed:  map[string]bool{},
		failures: map[string]string{},
		issued:   map[string][]time.Time{},
	}
	if os.Getenv("SABA_DEBUG") != "" {
		if l, err := zap.NewDevelopment(); err == nil {
			m.logger = l
		}
	}
	if d.opts.TLS != "auto" {
		return m, nil
	}
	m.cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(c certmagic.Certificate) (*certmagic.Config, error) {
			for _, n := range c.Names {
				if strings.HasPrefix(n, "*.") {
					m.mu.Lock()
					w := m.wild
					m.mu.Unlock()
					if w != nil {
						return w, nil
					}
				}
			}
			return m.onDemand, nil
		},
		Logger: m.logger,
	})
	m.onDemand = certmagic.New(m.cache, certmagic.Config{
		Storage:  m.storage,
		Logger:   m.logger,
		OnEvent:  m.onEvent,
		OnDemand: &certmagic.OnDemandConfig{DecisionFunc: m.decide},
	})
	iss, err := m.issuer(m.onDemand, nil)
	if err != nil {
		return nil, err
	}
	m.onDemand.Issuers = []certmagic.Issuer{iss}
	return m, nil
}

// issuer builds an ACME issuer: on-demand (HTTP-01 and TLS-ALPN-01, answered
// by the proxy's own listeners) or, with a DNS provider, DNS-01 only.
func (m *tlsManager) issuer(cfg *certmagic.Config, provider certs.Provider) (*certmagic.ACMEIssuer, error) {
	roots, err := acmeRoots(m.d.opts.ACMECAFile)
	if err != nil {
		return nil, err
	}
	tmpl := certmagic.ACMEIssuer{
		CA:           m.d.opts.ACMEDirectory,
		Email:        m.d.store.Snapshot().Server.ACMEEmail,
		Agreed:       true,
		TrustedRoots: roots,
		Logger:       m.logger,
	}
	if tmpl.CA == "" {
		tmpl.CA = certmagic.LetsEncryptProductionCA
	}
	// Challenges arrive on the proxy's ports; tell the solver which ones so it
	// recognizes them as served instead of trying to bind them itself.
	host, httpPort := splitPort(m.d.opts.HTTPAddr, 80)
	_, httpsPort := splitPort(m.d.opts.HTTPSAddr, 443)
	tmpl.ListenHost, tmpl.AltHTTPPort, tmpl.AltTLSALPNPort = host, httpPort, httpsPort
	if provider != nil {
		dns := &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{DNSProvider: provider, Logger: m.logger}}
		if r := m.d.opts.DNSResolvers; len(r) > 0 {
			dns.Resolvers = r
		}
		tmpl.DNS01Solver = dns
		tmpl.DisableHTTPChallenge, tmpl.DisableTLSALPNChallenge = true, true
	}
	return certmagic.NewACMEIssuer(cfg, tmpl), nil
}

func splitPort(addr string, def int) (string, int) {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", def
	}
	n, err := strconv.Atoi(p)
	if err != nil || n == 0 {
		return host, def
	}
	return host, n
}

// TLSConfig is the proxy's certificate source.
func (m *tlsManager) TLSConfig() *tls.Config {
	conf := m.onDemand.TLSConfig()
	conf.NextProtos = append([]string{"h2", "http/1.1"}, conf.NextProtos...)
	onDemand := conf.GetCertificate
	conf.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		challenge := len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == "acme-tls/1"
		if !challenge && m.d.proxy.HasHost(hello.ServerName) {
			if c := m.store.Lookup(hello.ServerName); c != nil {
				return c, nil
			}
		}
		return onDemand(hello)
	}
	return conf
}

// HTTPChallenge answers ACME HTTP-01 challenges ahead of h.
func (m *tlsManager) HTTPChallenge(h http.Handler) http.Handler {
	for _, iss := range m.onDemand.Issuers {
		if a, ok := iss.(*certmagic.ACMEIssuer); ok {
			return a.HTTPChallengeHandler(h)
		}
	}
	return h
}

// decide is the on-demand policy: certificates only for routed hostnames.
func (m *tlsManager) decide(ctx context.Context, name string) error {
	exact, wildcard, ok := m.d.proxy.MatchHost(name)
	switch {
	case !ok:
		return fmt.Errorf("%s is not routed to any app", name)
	case name == "localhost" || strings.HasSuffix(name, ".localhost") || net.ParseIP(name) != nil:
		return fmt.Errorf("no public certificate can be issued for %s", name)
	case exact:
		return nil
	}
	// Only a wildcard route matches. A cached wildcard certificate would have
	// been served without asking, so there is none (no DNS provider, or it is
	// not issued yet): fall back to a certificate of its own, within limits.
	m.mu.Lock()
	defer m.mu.Unlock()
	var recent []time.Time
	for _, t := range m.issued[wildcard] {
		if time.Since(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= wildcardOnDemandPerHour {
		return fmt.Errorf("too many new subdomains of %s this hour; configure a DNS provider for a wildcard certificate", wildcard)
	}
	m.issued[wildcard] = append(recent, time.Now())
	return nil
}

func (m *tlsManager) onEvent(_ context.Context, event string, data map[string]any) error {
	name, _ := data["identifier"].(string)
	switch event {
	case "cert_obtained":
		m.mu.Lock()
		delete(m.failures, name)
		m.mu.Unlock()
		m.d.log.Info("certificate obtained", "name", name)
	case "cert_failed":
		msg := fmt.Sprint(data["error"])
		// Drop the order and CA URLs certmagic appends; they only add noise.
		if i := strings.Index(msg, " (order="); i > 0 {
			msg = msg[:i]
		}
		msg = strings.TrimPrefix(msg, "["+name+"] ")
		m.mu.Lock()
		m.failures[name] = msg
		m.mu.Unlock()
		m.d.log.Warn("certificate could not be obtained", "name", name, "err", msg)
	}
	return nil // a non-nil error would abort what certmagic is doing
}

// wantedWildcards lists the wildcard names saba should hold a certificate
// for: wildcard domains routed to apps and the base domain (so new apps get
// HTTPS without any issuance at all).
func wantedWildcards(routed []string, base string) []string {
	var names []string
	for _, h := range routed {
		if strings.HasPrefix(h, "*.") {
			names = append(names, h)
		}
	}
	magic := strings.HasSuffix(base, "sslip.io") || strings.HasSuffix(base, "nip.io") || base == "localhost"
	if base != "" && !magic && !slices.Contains(names, "*."+base) {
		names = append(names, "*."+base)
	}
	sort.Strings(names)
	return names
}

// sync makes certmagic manage exactly the wanted wildcard certificates with
// the configured provider. Cheap when nothing changed.
func (m *tlsManager) sync(ctx context.Context) {
	if m.cache == nil {
		return
	}
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	st := m.d.store.Snapshot()
	key, _ := json.Marshal(st.Server.DNS)
	m.mu.Lock()
	if string(key) != m.providerKey {
		if len(m.managed) > 0 {
			var subjects []certmagic.SubjectIssuer
			for n := range m.managed {
				subjects = append(subjects, certmagic.SubjectIssuer{Subject: n})
			}
			m.cache.RemoveManaged(subjects)
		}
		if m.wildCancel != nil {
			m.wildCancel()
		}
		m.managed, m.wild, m.providerKey = map[string]bool{}, nil, string(key)
		if dns := st.Server.DNS; dns != nil {
			provider, err := certs.NewProvider(dns.Provider, dns.Settings)
			if err != nil {
				m.d.log.Error("the DNS provider is misconfigured", "err", err)
			} else {
				wild := certmagic.New(m.cache, certmagic.Config{Storage: m.storage, Logger: m.logger, OnEvent: m.onEvent})
				iss, err := m.issuer(wild, provider)
				if err == nil {
					wild.Issuers = []certmagic.Issuer{iss}
					m.wild = wild
					m.wildCtx, m.wildCancel = context.WithCancel(context.Background())
				}
			}
		}
	}
	wild, wildCtx := m.wild, m.wildCtx
	var add []string
	want := map[string]bool{}
	if wild != nil {
		for _, n := range wantedWildcards(m.d.proxy.Hosts(), st.Server.BaseDomain) {
			want[n] = true
			if !m.managed[n] {
				add = append(add, n)
				m.managed[n] = true
			}
		}
	}
	var drop []certmagic.SubjectIssuer
	for n := range m.managed {
		if !want[n] {
			drop = append(drop, certmagic.SubjectIssuer{Subject: n})
			delete(m.managed, n)
		}
	}
	m.mu.Unlock()
	if len(drop) > 0 {
		m.cache.RemoveManaged(drop)
	}
	if len(add) > 0 {
		// Obtains (or loads from storage) in the background and keeps them renewed.
		if err := wild.ManageAsync(wildCtx, add); err != nil {
			m.d.log.Error("cannot manage wildcard certificates", "names", add, "err", err)
		}
	}
}

// list describes the certificates of every routed hostname, wildcard and
// uploaded certificate.
func (m *tlsManager) list(ctx context.Context) []api.Certificate {
	var out []api.Certificate
	covered := func(host string) bool {
		for _, c := range out {
			for _, n := range c.Names {
				if n == host || (strings.HasPrefix(n, "*.") && strings.HasSuffix(host, n[1:]) && !strings.Contains(strings.TrimSuffix(host, n[1:]), ".")) {
					return c.Status == "active"
				}
			}
		}
		return false
	}
	out = append(out, m.store.List()...)
	if m.cache == nil {
		return out
	}
	routed := m.d.proxy.Hosts()
	m.mu.Lock()
	failures := make(map[string]string, len(m.failures))
	for k, v := range m.failures {
		failures[k] = v
	}
	provider := m.wild != nil
	m.mu.Unlock()

	describe := func(name, source string) api.Certificate {
		c := api.Certificate{Names: []string{name}, Source: source, Status: "pending"}
		find := func() bool {
			for _, cert := range m.cache.AllMatchingCertificates(name) {
				if cert.Leaf == nil || !containsName(cert.Names, name) {
					continue
				}
				c.Status, c.Issuer = "active", cert.Leaf.Issuer.CommonName
				c.NotBefore, c.NotAfter = cert.Leaf.NotBefore.UTC(), cert.Leaf.NotAfter.UTC()
				if time.Now().After(c.NotAfter) {
					c.Status = "expired"
				}
				return true
			}
			return false
		}
		if find() {
			return c
		}
		// On-demand certificates are loaded from storage on their first
		// handshake after a restart; load it now to report it.
		if source == "on-demand" {
			if _, err := m.onDemand.CacheManagedCertificate(ctx, name); err == nil && find() {
				return c
			}
		}
		if msg, ok := failures[name]; ok {
			c.Status, c.Error = "failed", msg
		}
		return c
	}
	for _, name := range wantedWildcards(routed, m.d.store.Snapshot().Server.BaseDomain) {
		c := describe(name, "wildcard")
		if !provider {
			c.Status = "skipped"
			c.Error = "no DNS provider is configured; each subdomain gets its own certificate on first request (rate limited)"
		}
		out = append(out, c)
	}
	for _, h := range routed {
		if strings.HasPrefix(h, "*.") || covered(h) {
			continue
		}
		out = append(out, describe(h, "on-demand"))
	}
	return out
}

func containsName(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

func (d *Daemon) handleCerts(w http.ResponseWriter, r *http.Request) error {
	out := d.tlsm.list(r.Context())
	if out == nil {
		out = []api.Certificate{}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (d *Daemon) handleCertAdd(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Cert string `json:"cert"`
		Key  string `json:"key"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	c, err := d.tlsm.store.Put(certs.SourceCustom, []byte(req.Cert), []byte(req.Key))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, c)
	return nil
}

func (d *Daemon) handleCertRemove(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if err := d.tlsm.store.Remove(id); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": id})
	return nil
}

// handleDNSGet shows the DNS provider, without credentials.
func (d *Daemon) handleDNSGet(w http.ResponseWriter, r *http.Request) error {
	out := redact(d.store.Snapshot().Server).DNS
	if out == nil {
		out = &api.DNSConfig{}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (d *Daemon) handleDNSProviders(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, certs.Providers())
	return nil
}

// handleDNSSet configures the DNS provider used for wildcard certificates.
func (d *Daemon) handleDNSSet(w http.ResponseWriter, r *http.Request) error {
	var req api.DNSConfig
	if err := readJSON(r, &req); err != nil {
		return err
	}
	req.Provider = strings.TrimSpace(req.Provider)
	// Changing other settings must not require typing the secrets again.
	if cur := d.store.Snapshot().Server.DNS; cur != nil && cur.Provider == req.Provider {
		merged := map[string]string{}
		for k, v := range cur.Settings {
			merged[k] = v
		}
		for k, v := range req.Settings {
			merged[k] = v
		}
		req.Settings = merged
	}
	if _, err := certs.NewProvider(req.Provider, req.Settings); err != nil {
		return err
	}
	if req.Provider == "hook" {
		path := req.Settings["path"]
		if info, err := os.Stat(path); err != nil || info.IsDir() || !filepath.IsAbs(path) || info.Mode()&0o111 == 0 {
			return api.Errorf(api.CodeInvalid, "the hook path must be an absolute path to an executable on the server")
		}
	}
	if err := d.store.Update(func(st *store.State) error {
		st.Server.DNS = &req
		return nil
	}); err != nil {
		return err
	}
	go d.tlsm.sync(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, redact(d.store.Snapshot().Server).DNS)
	return nil
}

func (d *Daemon) handleDNSRemove(w http.ResponseWriter, r *http.Request) error {
	if err := d.store.Update(func(st *store.State) error {
		st.Server.DNS = nil
		return nil
	}); err != nil {
		return err
	}
	go d.tlsm.sync(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
	return nil
}

// redact hides secrets in a server config before it leaves the daemon.
func redact(cfg api.ServerConfig) api.ServerConfig {
	if cfg.DNS != nil {
		dns := *cfg.DNS
		dns.Settings = certs.Redact(dns.Settings)
		cfg.DNS = &dns
	}
	if cfg.BackupS3 != nil {
		c := *cfg.BackupS3
		if c.SecretAccessKey != "" {
			c.SecretAccessKey = "********"
		}
		cfg.BackupS3 = &c
	}
	return cfg
}
