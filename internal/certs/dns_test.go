package certs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
)

func TestNewProvider(t *testing.T) {
	p, err := NewProvider("cloudflare", map[string]string{"api_token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if cf := p.(*cloudflare.Provider); cf.APIToken != "tok" {
		t.Fatalf("api_token not set: %+v", cf)
	}
	for name, settings := range map[string]map[string]string{
		"unknown provider":  nil,
		"unknown setting":   {"api_tokn": "x"},
		"hook without path": {},
	} {
		provider := map[string]string{"unknown provider": "nope", "unknown setting": "cloudflare", "hook without path": "hook"}[name]
		if _, err := NewProvider(provider, settings); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := NewProvider("cloudflare", map[string]string{"api_tokn": "x"}); err == nil || !strings.Contains(err.(interface{ Error() string }).Error(), "api_tokn") {
		t.Fatalf("unknown settings must be named: %v", err)
	}
}

func TestProvidersAreDescribed(t *testing.T) {
	all := Providers()
	var names []string
	for _, p := range all {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, " "); got != "cloudflare hetzner hook namesilo porkbun" {
		t.Fatalf("providers: %s", got)
	}
	secrets := map[string]bool{}
	for _, p := range all {
		if p.Title == "" || len(p.Settings) == 0 {
			t.Errorf("%s has no title or settings", p.Name)
		}
		for _, s := range p.Settings {
			secrets[p.Name+"."+s.Name] = s.Secret
		}
	}
	for setting, want := range map[string]bool{
		"cloudflare.api_token": true, "cloudflare.zone_token": true, "hetzner.auth_api_token": true,
		"namesilo.api_token": true, "porkbun.api_key": true, "porkbun.api_secret_key": true, "hook.path": false,
	} {
		if secrets[setting] != want {
			t.Errorf("%s secret = %v, want %v", setting, secrets[setting], want)
		}
	}
	red := Redact(map[string]string{"api_token": "abc", "region": "eu"})
	if red["api_token"] != "********" || red["region"] != "eu" {
		t.Fatalf("redaction: %v", red)
	}
}

func TestHookProvider(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "hook.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$1 $2 $3\" >> "+log+"\n[ \"$3\" = fail ] && { echo nope >&2; exit 1; }\nexit 0\n"), 0o755)
	p, err := NewProvider("hook", map[string]string{"path": script})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rec := []libdns.Record{libdns.TXT{Name: "_acme-challenge.apps", Text: "token-value"}}
	if _, err := p.AppendRecords(ctx, "example.com.", rec); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeleteRecords(ctx, "example.com.", rec); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	want := "present _acme-challenge.apps.example.com token-value\ncleanup _acme-challenge.apps.example.com token-value\n"
	if string(b) != want {
		t.Fatalf("hook calls:\n%s\nwant:\n%s", b, want)
	}
	_, err = p.AppendRecords(ctx, "example.com.", []libdns.Record{libdns.TXT{Name: "x", Text: "fail"}})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("a failing hook must report its output: %v", err)
	}
}
