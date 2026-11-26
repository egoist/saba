package daemon

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/proxy"
)

func TestOnDemandDecision(t *testing.T) {
	p := proxy.New(nil)
	p.SetApp("web", 1, []string{"www.example.com", "web.localhost"}, nil)
	p.SetApp("tenants", 1, []string{"*.tenants.example.com"}, nil)
	m := &tlsManager{d: &Daemon{proxy: p}, issued: map[string][]time.Time{}}
	ctx := context.Background()

	for _, name := range []string{"www.example.com", "WWW.example.com"} {
		if err := m.decide(ctx, name); err != nil {
			t.Errorf("%s is routed: %v", name, err)
		}
	}
	for _, name := range []string{"other.example.com", "a.b.tenants.example.com", "web.localhost", "203.0.113.10"} {
		if err := m.decide(ctx, name); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	// Subdomains matched only by a wildcard route are limited per hour.
	for i := range wildcardOnDemandPerHour {
		if err := m.decide(ctx, fmt.Sprintf("t%d.tenants.example.com", i)); err != nil {
			t.Fatalf("subdomain %d: %v", i, err)
		}
	}
	if err := m.decide(ctx, "one-too-many.tenants.example.com"); err == nil {
		t.Fatal("the hourly limit for a wildcard route must apply")
	}
	if err := m.decide(ctx, "www.example.com"); err != nil {
		t.Fatalf("exact routes are not limited: %v", err)
	}
}

func TestWantedWildcards(t *testing.T) {
	routed := []string{"api.example.com", "*.tenants.example.com", "*.apps.example.com"}
	got := wantedWildcards(routed, "apps.example.com")
	if want := []string{"*.apps.example.com", "*.tenants.example.com"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, base := range []string{"203-0-113-10.sslip.io", "localhost", ""} {
		if got := wantedWildcards(nil, base); len(got) != 0 {
			t.Errorf("base %q needs no wildcard, got %v", base, got)
		}
	}
}

func TestRedactDNS(t *testing.T) {
	cfg := api.ServerConfig{
		DNS:      &api.DNSConfig{Provider: "porkbun", Settings: map[string]string{"api_key": "s3cr3t", "other": "eu-west-1"}},
		BackupS3: &api.S3Config{Bucket: "b", SecretAccessKey: "k3y"},
	}
	out := redact(cfg)
	if out.DNS.Settings["api_key"] != "********" || out.DNS.Settings["other"] != "eu-west-1" || out.BackupS3.SecretAccessKey != "********" {
		t.Fatalf("not redacted: %+v %+v", out.DNS, out.BackupS3)
	}
	if cfg.DNS.Settings["api_key"] != "s3cr3t" {
		t.Fatal("redact must not modify the stored config")
	}
}
