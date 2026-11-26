package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestDueForRenewal(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	cases := []struct {
		lifetime, left time.Duration
		want           bool
	}{
		{90 * day, 60 * day, false},     // a fresh 90-day certificate
		{90 * day, 29 * day, true},      // renew 30 days before expiry
		{6 * day, 5 * day, false},       // a short-lived certificate is not renewed immediately...
		{6 * day, 47 * time.Hour, true}, // ...but once a third of its life is left
		{90 * day, -time.Hour, true},
	}
	for _, c := range cases {
		notAfter := now.Add(c.left)
		if got := dueForRenewal(notAfter.Add(-c.lifetime), notAfter, now); got != c.want {
			t.Errorf("lifetime %v, %v left: due = %v, want %v", c.lifetime, c.left, got, c.want)
		}
	}
}

// A stand-in for the Cloudflare API that checks what saba sends.
func TestCloudflareProvider(t *testing.T) {
	var (
		mu      sync.Mutex
		records = map[string]map[string]any{}
		calls   []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		reply := func(result any) {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
		}
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 9109, "message": "Invalid access token"}}})
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" {
				reply([]map[string]string{{"id": "zone1", "name": "example.com"}})
			} else {
				reply([]any{})
			}
		case r.Method == "POST" && r.URL.Path == "/zones/zone1/dns_records":
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
			records["rec1"] = body
			reply(map[string]string{"id": "rec1"})
		case r.Method == "DELETE" && r.URL.Path == "/zones/zone1/dns_records/rec1":
			delete(records, "rec1")
			reply(map[string]string{"id": "rec1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	cf := &Cloudflare{Token: "good-token", BaseURL: srv.URL}
	cleanup, err := cf.Present(ctx, "_acme-challenge.apps.example.com", "challenge-value")
	if err != nil {
		t.Fatal(err)
	}
	rec := records["rec1"]
	if rec == nil || rec["type"] != "TXT" || rec["name"] != "_acme-challenge.apps.example.com" || rec["content"] != "challenge-value" {
		t.Fatalf("record created: %v", rec)
	}
	// The zone is found by walking up from the record name.
	if calls[0] != "GET /zones?name=apps.example.com" || calls[1] != "GET /zones?name=example.com" {
		t.Fatalf("zone lookup calls: %v", calls)
	}
	cleanup(ctx)
	if len(records) != 0 {
		t.Fatal("cleanup did not delete the record")
	}

	if _, err := cf.Present(ctx, "_acme-challenge.other.org", "v"); err == nil || !strings.Contains(err.Error(), "no Cloudflare zone") {
		t.Fatalf("a domain outside the account: %v", err)
	}
	bad := &Cloudflare{Token: "bad", BaseURL: srv.URL}
	if _, err := bad.Present(ctx, "_acme-challenge.example.com", "v"); err == nil || !strings.Contains(err.Error(), "check the API token") {
		t.Fatalf("a rejected token should explain itself: %v", err)
	}
}

func TestEnsureSkipsHealthyCertificatesAndBacksOff(t *testing.T) {
	s, _ := Open(t.TempDir())
	now := time.Now()
	c, k := selfSigned(t, "ok", now.Add(-time.Hour), now.Add(80*24*time.Hour), "*.fine.test")
	if _, err := s.Put(SourceDNS, c, k); err != nil {
		t.Fatal(err)
	}
	// An unreachable CA: any attempt to issue fails fast.
	i := &Issuer{Store: s, Dir: t.TempDir(), DirectoryURL: "http://127.0.0.1:1/dir", Log: discardLogger()}
	i.Ensure(context.Background(), []string{"*.fine.test", "*.broken.test"}, Hook{Path: "/bin/true"})
	f := i.Failures()
	if _, failed := f["*.fine.test"]; failed {
		t.Fatal("a certificate with plenty of validity left must not be re-issued")
	}
	first, failed := f["*.broken.test"]
	if !failed {
		t.Fatal("the failure should be recorded")
	}
	i.DirectoryURL = "http://127.0.0.1:2/dir"
	i.Ensure(context.Background(), []string{"*.broken.test"}, Hook{Path: "/bin/true"})
	if i.Failures()["*.broken.test"] != first {
		t.Fatal("a recent failure must not be retried immediately")
	}
	i.Forget()
	i.Ensure(context.Background(), []string{"*.broken.test"}, Hook{Path: "/bin/true"})
	if i.Failures()["*.broken.test"] == first {
		t.Fatal("after Forget the issuance should be retried")
	}
}
