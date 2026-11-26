// Package certs manages certificates that saba's on-demand issuance cannot
// produce: wildcard certificates obtained through the ACME DNS-01 challenge,
// and certificates supplied by the user.
package certs

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
)

// Sources of a stored certificate.
const (
	SourceCustom = "custom" // uploaded with `saba certs add`
	SourceDNS    = "dns"    // issued through the DNS-01 challenge
)

type item struct {
	info api.Certificate
	cert *tls.Certificate
}

// Store keeps certificates on disk (one PEM file each: key, then chain) and
// answers TLS handshakes from memory.
type Store struct {
	dir   string
	mu    sync.RWMutex
	items map[string]*item
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, items: map[string]*item{}}
	files, _ := filepath.Glob(filepath.Join(dir, "*.pem"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(f), ".pem")
		source, _, _ := strings.Cut(id, "-")
		if it, err := parse(source, b, b); err == nil {
			it.info.ID = id
			s.items[id] = it
		}
	}
	return s, nil
}

func parse(source string, certPEM, keyPEM []byte) (*item, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalid, "the certificate and key are not a valid pair: %v", err).
			WithHint("Pass the full chain (leaf certificate first) with --cert and its private key with --key, both PEM encoded.")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, api.Errorf(api.CodeInvalid, "cannot parse the certificate: %v", err)
	}
	pair.Leaf = leaf
	var names []string
	for _, n := range leaf.DNSNames {
		names = append(names, strings.ToLower(n))
	}
	if len(names) == 0 {
		return nil, api.Errorf(api.CodeInvalid, "the certificate lists no DNS names (subject alternative names)")
	}
	return &item{cert: &pair, info: api.Certificate{
		Names: names, Source: source, Issuer: leaf.Issuer.CommonName,
		NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(),
	}}, nil
}

func idFor(source, name string) string {
	return source + "-" + strings.ReplaceAll(name, "*", "wildcard")
}

// Put validates and stores a certificate, replacing one with the same
// primary name and source.
func (s *Store) Put(source string, certPEM, keyPEM []byte) (api.Certificate, error) {
	it, err := parse(source, certPEM, keyPEM)
	if err != nil {
		return api.Certificate{}, err
	}
	it.info.ID = idFor(source, it.info.Names[0])
	var out []byte
	for rest := keyPEM; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		} else if strings.Contains(b.Type, "PRIVATE KEY") {
			out = append(out, pem.EncodeToMemory(b)...)
		}
	}
	for rest := certPEM; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		} else if b.Type == "CERTIFICATE" {
			out = append(out, pem.EncodeToMemory(b)...)
		}
	}
	path := filepath.Join(s.dir, it.info.ID+".pem")
	if err := os.WriteFile(path+".tmp", out, 0o600); err != nil {
		return api.Certificate{}, err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return api.Certificate{}, err
	}
	s.mu.Lock()
	s.items[it.info.ID] = it
	s.mu.Unlock()
	return s.describe(it), nil
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return api.Errorf(api.CodeNotFound, "no certificate with id %q", id).WithHint("List certificates with `saba certs`.")
	}
	delete(s.items, id)
	return os.Remove(filepath.Join(s.dir, id+".pem"))
}

func (s *Store) describe(it *item) api.Certificate {
	c := it.info
	c.Status = "active"
	if time.Now().After(c.NotAfter) {
		c.Status = "expired"
	}
	return c
}

// List returns all stored certificates, sorted by id.
func (s *Store) List() []api.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]api.Certificate, 0, len(s.items))
	for _, it := range s.items {
		out = append(out, s.describe(it))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// matches reports whether a certificate name (possibly a wildcard) covers host.
func matches(pattern, host string) bool {
	if pattern == host {
		return true
	}
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		if i := strings.IndexByte(host, '.'); i > 0 && host[i+1:] == rest {
			return true
		}
	}
	return false
}

// Lookup returns the best unexpired certificate for a TLS server name, or nil.
// Exact names win over wildcards; among equals the one valid longest wins.
func (s *Store) Lookup(serverName string) *tls.Certificate {
	host := strings.ToLower(strings.TrimSuffix(serverName, "."))
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *item
	bestExact := false
	for _, it := range s.items {
		if now.After(it.info.NotAfter) || now.Before(it.info.NotBefore) {
			continue
		}
		for _, n := range it.info.Names {
			if !matches(n, host) {
				continue
			}
			exact := n == host
			if best == nil || (exact && !bestExact) || (exact == bestExact && it.info.NotAfter.After(best.info.NotAfter)) {
				best, bestExact = it, exact
			}
		}
	}
	if best == nil {
		return nil
	}
	return best.cert
}

// Covering returns the stored certificate from source that lists exactly name.
func (s *Store) Covering(source, name string) (api.Certificate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[idFor(source, name)]
	if !ok {
		return api.Certificate{}, false
	}
	return s.describe(it), true
}
