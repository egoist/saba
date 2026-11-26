package certs

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

const (
	renewBefore  = 30 * 24 * time.Hour
	retryAfter   = time.Hour
	issueTimeout = 5 * time.Minute
)

// dueForRenewal reports whether a certificate should be replaced: 30 days
// before it expires, or once a third of its lifetime is left for
// short-lived certificates.
func dueForRenewal(notBefore, notAfter, now time.Time) bool {
	window := renewBefore
	if third := notAfter.Sub(notBefore) / 3; third < window {
		window = third
	}
	return notAfter.Sub(now) <= window
}

// Provider publishes ACME DNS-01 challenge records.
type Provider interface {
	// Present creates a TXT record and returns a function that removes it.
	Present(ctx context.Context, fqdn, value string) (cleanup func(context.Context), err error)
}

// Issuer obtains and renews wildcard certificates through DNS-01.
type Issuer struct {
	Store        *Store
	Dir          string // holds the ACME account key
	DirectoryURL string
	Email        string
	HTTPClient   *http.Client
	Log          *slog.Logger
	// PropagationTimeout bounds the wait for the TXT record to reach the
	// zone's name servers before the CA is asked to validate.
	PropagationTimeout time.Duration

	mu       sync.Mutex
	running  bool
	failures map[string]failure
}

type failure struct {
	at  time.Time
	msg string
}

// Failures returns the last issuance error per name.
func (i *Issuer) Failures() map[string]string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := map[string]string{}
	for n, f := range i.failures {
		out[n] = f.msg
	}
	return out
}

// Forget clears recorded failures so the next Ensure retries immediately
// (after a configuration change, for example).
func (i *Issuer) Forget() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failures = nil
}

// Ensure issues or renews a certificate for every name that needs one.
// Names that are fine are skipped; failures are retried at most hourly.
func (i *Issuer) Ensure(ctx context.Context, names []string, p Provider) {
	i.mu.Lock()
	if i.running {
		i.mu.Unlock()
		return
	}
	i.running = true
	if i.failures == nil {
		i.failures = map[string]failure{}
	}
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		i.running = false
		i.mu.Unlock()
	}()

	for _, name := range names {
		if c, ok := i.Store.Covering(SourceDNS, name); ok && !dueForRenewal(c.NotBefore, c.NotAfter, time.Now()) {
			continue
		}
		i.mu.Lock()
		f, failed := i.failures[name]
		i.mu.Unlock()
		if failed && time.Since(f.at) < retryAfter {
			continue
		}
		ictx, cancel := context.WithTimeout(ctx, issueTimeout)
		err := i.issue(ictx, name, p)
		cancel()
		i.mu.Lock()
		if err != nil {
			i.failures[name] = failure{time.Now(), err.Error()}
			i.Log.Error("wildcard certificate issuance failed", "name", name, "err", err)
		} else {
			delete(i.failures, name)
			i.Log.Info("wildcard certificate issued", "name", name)
		}
		i.mu.Unlock()
	}
}

func (i *Issuer) client(ctx context.Context) (*acme.Client, error) {
	path := filepath.Join(i.Dir, "dns01_account.key")
	var key *ecdsa.PrivateKey
	if b, err := os.ReadFile(path); err == nil {
		if block, _ := pem.Decode(b); block != nil {
			key, _ = x509.ParseECPrivateKey(block.Bytes)
		}
	}
	if key == nil {
		var err error
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, err
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
	}
	dir := i.DirectoryURL
	if dir == "" {
		dir = acme.LetsEncryptURL
	}
	c := &acme.Client{Key: key, DirectoryURL: dir, HTTPClient: i.HTTPClient, UserAgent: "saba"}
	acct := &acme.Account{}
	if i.Email != "" {
		acct.Contact = []string{"mailto:" + i.Email}
	}
	if _, err := c.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("ACME account registration failed: %w", err)
	}
	return c, nil
}

func (i *Issuer) issue(ctx context.Context, name string, p Provider) error {
	if p == nil {
		return errors.New("no DNS provider is configured")
	}
	c, err := i.client(ctx)
	if err != nil {
		return err
	}
	order, err := c.AuthorizeOrder(ctx, acme.DomainIDs(name))
	if err != nil {
		return fmt.Errorf("creating the order failed: %w", err)
	}
	for _, zurl := range order.AuthzURLs {
		z, err := c.GetAuthorization(ctx, zurl)
		if err != nil {
			return err
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var chal *acme.Challenge
		for _, ch := range z.Challenges {
			if ch.Type == "dns-01" {
				chal = ch
			}
		}
		if chal == nil {
			return fmt.Errorf("the CA offers no DNS-01 challenge for %s", z.Identifier.Value)
		}
		value, err := c.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return err
		}
		fqdn := "_acme-challenge." + strings.TrimPrefix(z.Identifier.Value, "*.")
		cleanup, err := p.Present(ctx, fqdn, value)
		if err != nil {
			return fmt.Errorf("creating the DNS record %s failed: %w", fqdn, err)
		}
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			cleanup(cctx)
		}()
		i.waitForRecord(ctx, fqdn, value)
		if _, err := c.Accept(ctx, chal); err != nil {
			return fmt.Errorf("the CA rejected the challenge: %w", err)
		}
		if _, err := c.WaitAuthorization(ctx, z.URI); err != nil {
			return fmt.Errorf("DNS validation of %s failed: %w", z.Identifier.Value, err)
		}
	}
	orderURL := order.URI
	if order, err = c.WaitOrder(ctx, orderURL); err != nil {
		return fmt.Errorf("the order did not become ready: %w", err)
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{name}}, crypto.Signer(certKey))
	if err != nil {
		return err
	}
	chain, _, err := c.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		// A CA may finalize asynchronously and answer without repeating the
		// order's URL, which the ACME library needs to poll it. The order is
		// already known here, so wait for it and fetch the certificate.
		if done, werr := c.WaitOrder(ctx, orderURL); werr == nil && done.CertURL != "" {
			chain, err = c.FetchCert(ctx, done.CertURL, true)
		}
	}
	if err != nil {
		return fmt.Errorf("the CA did not issue the certificate: %w", err)
	}
	var certPEM []byte
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	_, err = i.Store.Put(SourceDNS, certPEM, keyPEM)
	return err
}

// waitForRecord waits until the zone's authoritative name servers serve the
// TXT record, so the CA does not validate against a stale answer. If the
// name servers cannot be determined it just pauses briefly.
func (i *Issuer) waitForRecord(ctx context.Context, fqdn, value string) {
	servers := authoritative(ctx, fqdn)
	if len(servers) == 0 {
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
		return
	}
	timeout := i.PropagationTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		seen := 0
		for _, ns := range servers {
			r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, network, net.JoinHostPort(ns, "53"))
			}}
			qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			txts, _ := r.LookupTXT(qctx, fqdn)
			cancel()
			for _, t := range txts {
				if t == value {
					seen++
					break
				}
			}
		}
		if seen == len(servers) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
	i.Log.Warn("the DNS record is not visible on all name servers yet; asking the CA to validate anyway", "record", fqdn)
}

// authoritative returns the name servers of the zone containing fqdn.
func authoritative(ctx context.Context, fqdn string) []string {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 1; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		nss, err := net.DefaultResolver.LookupNS(qctx, zone)
		cancel()
		if err != nil || len(nss) == 0 {
			continue
		}
		var out []string
		for _, ns := range nss {
			if len(out) < 4 {
				out = append(out, strings.TrimSuffix(ns.Host, "."))
			}
		}
		return out
	}
	return nil
}
