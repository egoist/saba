package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/certs"
	"github.com/egoist/saba/internal/engine"
	"github.com/egoist/saba/internal/store"
)

// acmeHTTPClient returns the HTTP client for talking to the CA, trusting an
// extra root when the directory is a private or test CA.
func acmeHTTPClient(caFile string) (*http.Client, error) {
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
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, nil
}

func (d *Daemon) setupCerts(client *http.Client) error {
	var err error
	if d.certs, err = certs.Open(filepath.Join(d.opts.DataDir, "certs", "store")); err != nil {
		return err
	}
	d.issuer = &certs.Issuer{
		Store:        d.certs,
		Dir:          filepath.Join(d.opts.DataDir, "certs"),
		DirectoryURL: d.opts.ACMEDirectory,
		Email:        d.store.Snapshot().Server.ACMEEmail,
		HTTPClient:   client,
		Log:          d.log,
	}
	return nil
}

func dnsProvider(cfg api.ServerConfig) certs.Provider {
	switch {
	case cfg.CloudflareAPIToken != "":
		return &certs.Cloudflare{Token: cfg.CloudflareAPIToken}
	case cfg.DNSHook != "":
		return certs.Hook{Path: cfg.DNSHook}
	}
	return nil
}

// wantedWildcards lists the wildcard names saba should hold a certificate
// for: wildcard domains routed to apps and, with a DNS provider configured,
// the base domain (so new apps get HTTPS without any issuance at all).
func wantedWildcards(st *store.State) []string {
	seen := map[string]bool{}
	for _, a := range st.Apps {
		cur := a.Current()
		if cur == nil {
			continue
		}
		cfg := cur.Config
		cfg.Port = cur.Port
		for _, h := range engine.Hosts(st.Server, a.Name, a.Kind, &cfg) {
			if strings.HasPrefix(h, "*.") {
				seen[h] = true
			}
		}
	}
	base := st.Server.BaseDomain
	magic := strings.HasSuffix(base, "sslip.io") || strings.HasSuffix(base, "nip.io") || base == "localhost"
	if base != "" && !magic && dnsProvider(st.Server) != nil {
		seen["*."+base] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ensureCerts issues or renews wildcard certificates. It is cheap when there
// is nothing to do and never runs concurrently with itself.
func (d *Daemon) ensureCerts(ctx context.Context) {
	if d.opts.TLS != "auto" {
		return
	}
	st := d.store.Snapshot()
	p := dnsProvider(st.Server)
	names := wantedWildcards(st)
	if p == nil || len(names) == 0 {
		return
	}
	d.issuer.Ensure(ctx, names, p)
}

func (d *Daemon) handleCerts(w http.ResponseWriter, r *http.Request) error {
	st := d.store.Snapshot()
	out := d.certs.List()
	failures := d.issuer.Failures()
	for _, name := range wantedWildcards(st) {
		if _, ok := d.certs.Covering(certs.SourceDNS, name); ok {
			continue
		}
		c := api.Certificate{Names: []string{name}, Source: certs.SourceDNS, Status: "pending"}
		switch {
		case d.opts.TLS != "auto":
			c.Status, c.Error = "skipped", "TLS is off on this server"
		case dnsProvider(st.Server) == nil:
			c.Status = "skipped"
			c.Error = "no DNS provider is configured; hosts under this name get individual certificates on first use"
		case failures[name] != "":
			c.Status, c.Error = "failed", failures[name]
		}
		out = append(out, c)
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
	c, err := d.certs.Put(certs.SourceCustom, []byte(req.Cert), []byte(req.Key))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, c)
	return nil
}

func (d *Daemon) handleCertRemove(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if err := d.certs.Remove(id); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": id})
	return nil
}

// redact hides secrets in a server config before it leaves the daemon.
func redact(cfg api.ServerConfig) api.ServerConfig {
	if t := cfg.CloudflareAPIToken; t != "" {
		tail := ""
		if len(t) > 8 {
			tail = t[len(t)-4:]
		}
		cfg.CloudflareAPIToken = "********" + tail
	}
	return cfg
}
