package certs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// Cloudflare publishes challenge records through the Cloudflare API. The
// token needs the Zone:Read and DNS:Edit permissions for the zone.
type Cloudflare struct {
	Token string
	// BaseURL overrides the API endpoint (tests).
	BaseURL string
	HTTP    *http.Client
}

type cfResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (c *Cloudflare) call(ctx context.Context, method, path string, body any, result any) error {
	base := c.BaseURL
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare API unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out cfResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("cloudflare API returned %s", resp.Status)
	}
	if !out.Success {
		msg := resp.Status
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg += " (check the API token: it needs Zone:Read and DNS:Edit for this zone)"
		}
		return fmt.Errorf("cloudflare: %s", msg)
	}
	if result != nil {
		return json.Unmarshal(out.Result, result)
	}
	return nil
}

// zone finds the Cloudflare zone that contains fqdn by trying each parent domain.
func (c *Cloudflare) zone(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 1; i < len(labels)-1; i++ {
		name := strings.Join(labels[i:], ".")
		var zones []struct {
			ID string `json:"id"`
		}
		if err := c.call(ctx, "GET", "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
			return "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("no Cloudflare zone for %s is visible to this API token", strings.TrimPrefix(fqdn, "_acme-challenge."))
}

func (c *Cloudflare) Present(ctx context.Context, fqdn, value string) (func(context.Context), error) {
	zone, err := c.zone(ctx, fqdn)
	if err != nil {
		return nil, err
	}
	var rec struct {
		ID string `json:"id"`
	}
	body := map[string]any{"type": "TXT", "name": strings.TrimSuffix(fqdn, "."), "content": value, "ttl": 60}
	if err := c.call(ctx, "POST", "/zones/"+zone+"/dns_records", body, &rec); err != nil {
		return nil, err
	}
	return func(ctx context.Context) {
		_ = c.call(ctx, "DELETE", "/zones/"+zone+"/dns_records/"+rec.ID, nil, nil)
	}, nil
}

// Hook delegates to an executable on the server, for DNS hosts saba has no
// built-in support for. It is called as:
//
//	<hook> present <fqdn> <value>     create the TXT record
//	<hook> cleanup <fqdn> <value>     remove it again
type Hook struct{ Path string }

func (h Hook) run(ctx context.Context, action, fqdn, value string) error {
	cmd := exec.CommandContext(ctx, h.Path, action, fqdn, value)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("dns hook %s %s failed: %s", h.Path, action, msg)
	}
	return nil
}

func (h Hook) Present(ctx context.Context, fqdn, value string) (func(context.Context), error) {
	if h.Path == "" {
		return nil, errors.New("no dns hook configured")
	}
	if err := h.run(ctx, "present", fqdn, value); err != nil {
		return nil, err
	}
	return func(ctx context.Context) { _ = h.run(ctx, "cleanup", fqdn, value) }, nil
}
