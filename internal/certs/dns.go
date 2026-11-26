package certs

import (
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"sort"
	"strings"

	"github.com/libdns/cloudflare"
	"github.com/libdns/hetzner"
	"github.com/libdns/libdns"
	"github.com/libdns/namesilo"
	"github.com/libdns/porkbun"

	"github.com/egoist/saba/internal/api"
)

// Provider creates and removes DNS records for the ACME DNS-01 challenge.
type Provider interface {
	libdns.RecordAppender
	libdns.RecordDeleter
}

type providerInfo struct {
	title string
	new   func() Provider
}

// providers are the DNS hosts saba can manage records at, by name.
var providers = map[string]providerInfo{
	"cloudflare": {"Cloudflare (token with Zone:Read and DNS:Edit)", func() Provider { return &cloudflare.Provider{} }},
	"hetzner":    {"Hetzner DNS", func() Provider { return &hetzner.Provider{} }},
	"hook":       {"Your own script, called as `<path> present|cleanup <record-name> <value>`", func() Provider { return &HookProvider{} }},
	"namesilo":   {"NameSilo", func() Provider { return &namesilo.Provider{} }},
	"porkbun":    {"Porkbun", func() Provider { return &porkbun.Provider{} }},
}

// Providers lists the supported DNS hosts and their settings.
func Providers() []api.DNSProvider {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]api.DNSProvider, 0, len(names))
	for _, n := range names {
		info := providers[n]
		d := api.DNSProvider{Name: n, Title: info.title}
		for _, f := range settingFields(info.new()) {
			d.Settings = append(d.Settings, api.DNSProviderSetting{Name: f.name, Secret: isSecret(f.name)})
		}
		out = append(out, d)
	}
	return out
}

type settingField struct {
	name  string
	index int
}

// settingFields lists the settable fields of a provider struct: exported
// strings with a JSON name.
func settingFields(p Provider) []settingField {
	t := reflect.TypeOf(p).Elem()
	var out []settingField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || tag == "" || tag == "-" || f.Type.Kind() != reflect.String {
			continue
		}
		out = append(out, settingField{name: tag, index: i})
	}
	return out
}

// isSecret reports whether a setting holds a credential.
func isSecret(name string) bool {
	return strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "password") ||
		name == "key" || strings.HasSuffix(name, "_key")
}

// NewProvider builds a provider from its name and string settings.
func NewProvider(name string, settings map[string]string) (Provider, error) {
	info, ok := providers[name]
	if !ok {
		return nil, api.Errorf(api.CodeInvalid, "unknown DNS provider %q", name).
			WithHint("Supported: %s. Run `saba certs dns providers` for their settings.", strings.Join(ProviderNames(), ", "))
	}
	p := info.new()
	fields := map[string]settingField{}
	var valid []string
	for _, f := range settingFields(p) {
		fields[f.name] = f
		valid = append(valid, f.name)
	}
	v := reflect.ValueOf(p).Elem()
	for key, raw := range settings {
		f, ok := fields[key]
		if !ok {
			return nil, api.Errorf(api.CodeInvalid, "%s has no setting %q", name, key).
				WithHint("Settings of %s: %s.", name, strings.Join(valid, ", "))
		}
		v.Field(f.index).SetString(raw)
	}
	if h, ok := p.(*HookProvider); ok && h.Path == "" {
		return nil, api.Errorf(api.CodeInvalid, "the hook provider needs path=<absolute path of an executable on the server>")
	}
	return p, nil
}

func ProviderNames() []string {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Redact hides credential values for display.
func Redact(settings map[string]string) map[string]string {
	out := make(map[string]string, len(settings))
	for k, v := range settings {
		if isSecret(k) && v != "" {
			v = "********"
		}
		out[k] = v
	}
	return out
}

// HookProvider runs an executable to manage records, for DNS hosts without
// built-in support. It is called as
//
//	<path> present <record-name> <value>
//	<path> cleanup <record-name> <value>
//
// with the fully qualified record name, without a trailing dot.
type HookProvider struct {
	Path string `json:"path"`
}

func (h *HookProvider) run(ctx context.Context, action, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	for _, r := range recs {
		rr := r.RR()
		name := strings.TrimSuffix(libdns.AbsoluteName(rr.Name, zone), ".")
		out, err := exec.CommandContext(ctx, h.Path, action, name, rr.Data).CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			return nil, fmt.Errorf("dns hook %s %s %s failed: %s", h.Path, action, name, msg)
		}
	}
	return recs, nil
}

func (h *HookProvider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return h.run(ctx, "present", zone, recs)
}

func (h *HookProvider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	return h.run(ctx, "cleanup", zone, recs)
}
