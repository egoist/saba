package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/egoist/saba/internal/api"
)

// selfSigned returns a certificate and key in PEM form.
func selfSigned(t *testing.T, cn string, notBefore, notAfter time.Time, names ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore, NotAfter: notAfter,
		DNSNames: names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func issuerOf(t *testing.T, s *Store, host string) string {
	t.Helper()
	c := s.Lookup(host)
	if c == nil {
		return ""
	}
	return c.Leaf.Issuer.CommonName
}

func TestStoreLookup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	put := func(source, cn string, from, to time.Time, names ...string) api.Certificate {
		c, k := selfSigned(t, cn, from, to, names...)
		info, err := s.Put(source, c, k)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	wild := put(SourceDNS, "wild", now.Add(-time.Hour), now.Add(60*24*time.Hour), "*.example.com")
	put(SourceCustom, "exact", now.Add(-time.Hour), now.Add(24*time.Hour), "api.example.com", "example.com")
	put(SourceCustom, "expired", now.Add(-48*time.Hour), now.Add(-time.Hour), "old.example.com")

	if wild.ID != "dns-wildcard.example.com" || wild.Status != "active" {
		t.Fatalf("wildcard info: %+v", wild)
	}
	cases := map[string]string{
		"web.example.com":     "wild",  // covered by the wildcard
		"API.example.com.":    "exact", // an exact name beats a wildcard; names are case-insensitive
		"example.com":         "exact", // a wildcard does not cover the apex
		"a.b.example.com":     "",      // nor more than one label
		"old.example.com":     "wild",  // the expired exact certificate is skipped
		"example.org":         "",
		"wildcard.example.co": "",
	}
	for host, want := range cases {
		if got := issuerOf(t, s, host); got != want {
			t.Errorf("Lookup(%q) served by %q, want %q", host, got, want)
		}
	}

	// A renewed certificate replaces the old one under the same id.
	put(SourceDNS, "wild-renewed", now.Add(-time.Minute), now.Add(90*24*time.Hour), "*.example.com")
	if got := issuerOf(t, s, "web.example.com"); got != "wild-renewed" {
		t.Fatalf("after renewal served by %q", got)
	}
	if n := len(s.List()); n != 3 {
		t.Fatalf("store holds %d certificates, want 3", n)
	}

	// Certificates survive a restart; removal is permanent.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := issuerOf(t, reopened, "api.example.com"); got != "exact" {
		t.Fatalf("after reopening served by %q", got)
	}
	var statuses []string
	for _, c := range reopened.List() {
		statuses = append(statuses, c.Source+":"+c.Status)
	}
	if strings.Join(statuses, ",") != "custom:active,custom:expired,dns:active" {
		t.Fatalf("listing after reopen: %v", statuses)
	}
	if err := reopened.Remove("custom-api.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := issuerOf(t, reopened, "api.example.com"); got != "wild-renewed" {
		t.Fatalf("after removing the exact certificate served by %q", got)
	}
	if err := reopened.Remove("custom-api.example.com"); err == nil {
		t.Fatal("removing twice should fail")
	}
}

func TestStoreRejectsBadInput(t *testing.T) {
	s, _ := Open(t.TempDir())
	now := time.Now()
	certA, _ := selfSigned(t, "a", now, now.Add(time.Hour), "a.test")
	_, keyB := selfSigned(t, "b", now, now.Add(time.Hour), "b.test")
	noNames, keyN := selfSigned(t, "n", now, now.Add(time.Hour))
	for name, pair := range map[string][2][]byte{
		"mismatched key": {certA, keyB},
		"not pem":        {[]byte("hello"), []byte("world")},
		"no dns names":   {noNames, keyN},
	} {
		if _, err := s.Put(SourceCustom, pair[0], pair[1]); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if len(s.List()) != 0 {
		t.Fatal("rejected certificates must not be stored")
	}
}
