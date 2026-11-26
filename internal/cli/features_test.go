package cli

import (
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/saba/internal/api"
)

func TestDatabaseBackupsThroughTheCLI(t *testing.T) {
	startDaemon(t)
	if code, _, errOut := saba("db", "create", "main", "--engine", "postgres", "--json"); code != 0 {
		t.Fatalf("db create: %s", errOut)
	}

	code, out, errOut := saba("db", "backup", "create", "main", "--json")
	var b backupResult
	mustJSON(t, out, &b)
	if code != 0 || b.Backup == nil || b.Kind != "manual" || b.Engine != "postgres" {
		t.Fatalf("backup create: %d %s %s", code, out, errOut)
	}

	// Create and download in one step, into a directory.
	dir := t.TempDir()
	code, out, _ = saba("db", "backup", "create", "main", "-o", dir, "--json")
	b = backupResult{}
	mustJSON(t, out, &b)
	if code != 0 || filepath.Dir(b.SavedTo) != dir || !strings.HasSuffix(b.SavedTo, ".sql.gz") {
		t.Fatalf("backup download: %d %s", code, out)
	}
	f, err := os.Open(b.SavedTo)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("downloaded backup is not gzip: %v", err)
	}
	io.Copy(io.Discard, gz)
	f.Close()
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("download left temporary files behind: %v", entries)
	}

	code, out, _ = saba("db", "backup", "list", "main", "--json")
	var list []api.Backup
	mustJSON(t, out, &list)
	if code != 0 || len(list) != 2 || list[0].ID <= list[1].ID {
		t.Fatalf("backup list should be newest first: %s", out)
	}

	// Restores replace data, so they need --yes and exactly one source.
	if code, _, errOut = saba("db", "restore", "main", "--backup", "latest", "--json"); code != api.ExitUsage || errorCode(t, errOut) != api.CodeConfirmation {
		t.Fatalf("restore without --yes: %d %s", code, errOut)
	}
	if code, _, _ = saba("db", "restore", "main", "--yes", "--json"); code != api.ExitUsage {
		t.Fatalf("restore without a source: %d", code)
	}
	if code, out, errOut = saba("db", "restore", "main", "--backup", "latest", "--yes", "--json"); code != 0 || !strings.Contains(out, list[0].ID) {
		t.Fatalf("restore from a stored backup: %d %s %s", code, out, errOut)
	}
	if code, _, errOut = saba("db", "restore", "main", b.SavedTo, "--yes", "--json"); code != 0 {
		t.Fatalf("restore from a file: %s", errOut)
	}

	code, out, _ = saba("db", "backup", "schedule", "main", "@daily", "--keep", "3", "--json")
	var policy api.BackupPolicy
	mustJSON(t, out, &policy)
	if code != 0 || policy.Schedule != "@daily" || policy.Keep != 3 {
		t.Fatalf("schedule: %s", out)
	}
	if _, out, _ = saba("status", "main", "--json"); !strings.Contains(out, `"schedule": "@daily"`) {
		t.Fatalf("the policy should show up in the database's status: %s", out)
	}
	if code, _, errOut = saba("db", "backup", "schedule", "main", "sometimes", "--json"); code != api.ExitUsage {
		t.Fatalf("invalid schedule: %d %s", code, errOut)
	}
	saba("db", "backup", "schedule", "main", "off")
	if _, out, _ = saba("db", "backup", "schedule", "main", "--json"); !strings.Contains(out, `"schedule": ""`) {
		t.Fatalf("schedule should be off: %s", out)
	}

	if code, _, _ = saba("db", "backup", "delete", "main", list[0].ID, "--json"); code != api.ExitUsage {
		t.Fatalf("delete without --yes: %d", code)
	}
	if code, _, errOut = saba("db", "backup", "delete", "main", list[0].ID, "--yes", "--json"); code != 0 {
		t.Fatalf("delete: %s", errOut)
	}
	if code, _, _ = saba("db", "backup", "download", "main", list[0].ID, "-o", dir, "--json"); code != api.ExitNotFound {
		t.Fatalf("downloading a deleted backup: %d", code)
	}

	// Apps are not databases.
	saba("deploy", "-a", "web", "--image", "ok:1", "--json")
	if code, _, errOut = saba("db", "backup", "create", "web", "--json"); code != api.ExitUsage || errorCode(t, errOut) != api.CodeInvalid {
		t.Fatalf("backing up an app: %d %s", code, errOut)
	}
}

func TestNodesCertsAndSecretsThroughTheCLI(t *testing.T) {
	startDaemon(t)

	// A fresh server is a cluster of one.
	code, out, _ := saba("node", "list", "--json")
	var nodes []api.Node
	mustJSON(t, out, &nodes)
	if code != 0 || len(nodes) != 1 || nodes[0].Name != api.LocalNode || nodes[0].Status != "connected" {
		t.Fatalf("node list: %s", out)
	}
	if code, out, _ = saba("node", "key", "--json"); code != 0 || !strings.Contains(out, "ssh-ed25519 ") {
		t.Fatalf("node key: %d %s", code, out)
	}
	if code, _, errOut := saba("deploy", "-a", "web", "--image", "ok:1", "--node", "ghost", "--json"); code != api.ExitNotFound || !strings.Contains(errOut, "saba node add") {
		t.Fatalf("deploying to an unknown node: %d %s", code, errOut)
	}
	if code, _, _ = saba("node", "add", "local", "root@example.test", "--no-authorize", "--json"); code != api.ExitUsage {
		t.Fatalf("the name local is reserved, exit %d", code)
	}
	if code, _, _ = saba("node", "remove", "ghost", "--yes", "--json"); code != api.ExitNotFound {
		t.Fatalf("removing an unknown node: %d", code)
	}
	// "local" alone is the default placement and always valid.
	if code, _, errOut := saba("deploy", "-a", "web", "--image", "ok:1", "--node", "local", "--json"); code != 0 {
		t.Fatalf("deploy --node local: %s", errOut)
	}

	// Custom certificates.
	dir := t.TempDir()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(240 * time.Hour),
		DNSNames: []string{"*.example.com", "example.com"},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)

	if code, out, _ = saba("certs", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty certs list: %d %s", code, out)
	}
	code, out, errOut := saba("certs", "add", "--cert", certFile, "--key", keyFile, "--json")
	var cert api.Certificate
	mustJSON(t, out, &cert)
	if code != 0 || cert.ID != "custom-wildcard.example.com" || cert.Status != "active" || len(cert.Names) != 2 {
		t.Fatalf("certs add: %d %s %s", code, out, errOut)
	}
	if code, _, _ = saba("certs", "add", "--cert", certFile, "--key", certFile, "--json"); code != api.ExitUsage {
		t.Fatalf("a certificate file is not a key, exit %d", code)
	}
	if _, out, _ = saba("certs", "list", "--json"); strings.Contains(out, "PRIVATE KEY") || !strings.Contains(out, "custom-wildcard.example.com") {
		t.Fatalf("certs list: %s", out)
	}
	if code, _, _ = saba("certs", "remove", cert.ID, "--json"); code != api.ExitUsage {
		t.Fatalf("remove without --yes: %d", code)
	}
	if code, _, _ = saba("certs", "remove", cert.ID, "--yes", "--json"); code != 0 {
		t.Fatal("certs remove failed")
	}

	// The DNS token is write-only: it never comes back in full.
	const token = "cf-secret-token-value-9876"
	if code, out, errOut = saba("server", "config", "set", "cloudflare_api_token", token, "--json"); code != 0 || strings.Contains(out, token) || !strings.Contains(out, "9876") {
		t.Fatalf("setting the token: %d %s %s", code, out, errOut)
	}
	for _, args := range [][]string{{"server", "info", "--json"}, {"server", "config", "--json"}, {"server", "config"}, {"certs", "--json"}, {"doctor", "--json"}} {
		if _, out, errOut = saba(args...); strings.Contains(out+errOut, token) {
			t.Fatalf("%v leaks the Cloudflare token", args)
		}
	}
	if code, _, _ = saba("server", "config", "set", "dns_hook", "relative/path", "--json"); code != api.ExitUsage {
		t.Fatalf("a relative dns_hook must be rejected, exit %d", code)
	}
}

// fakeS3 is a minimal in-memory S3 endpoint: enough of the API for saba's
// backup mirroring, without signature checks.
func fakeS3(t *testing.T) (endpoint string, objects func() []string) {
	t.Helper()
	var mu sync.Mutex
	store := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-key/") {
			http.Error(w, "<Error><Code>AccessDenied</Code><Message>bad credentials</Message></Error>", http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		switch {
		case r.URL.Path == "/bucket" && r.URL.Query().Get("list-type") == "2":
			var b strings.Builder
			b.WriteString("<ListBucketResult><IsTruncated>false</IsTruncated>")
			for k, v := range store {
				if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
					fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-01-01T00:00:00Z</LastModified></Contents>", k, len(v))
				}
			}
			b.WriteString("</ListBucketResult>")
			io.WriteString(w, b.String())
		case r.Method == http.MethodPut:
			store[key], _ = io.ReadAll(r.Body)
		case r.Method == http.MethodGet:
			v, ok := store[key]
			if !ok {
				http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
				return
			}
			w.Write(v)
		case r.Method == http.MethodDelete:
			delete(store, key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		var keys []string
		for k := range store {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
}

func TestBackupsToS3ThroughTheCLI(t *testing.T) {
	startDaemon(t)
	endpoint, objects := fakeS3(t)
	saba("db", "create", "main", "--engine", "postgres", "--json")

	if code, out, _ := saba("db", "backup", "s3", "show", "--json"); code != 0 || !strings.Contains(out, `"bucket": ""`) {
		t.Fatalf("show before configuring: %d %s", code, out)
	}
	// Settings that do not work are rejected and not saved.
	code, _, errOut := saba("db", "backup", "s3", "set", "--bucket", "bucket", "--endpoint", endpoint, "--access-key-id", "wrong", "--secret-access-key", "x", "--json")
	if code != api.ExitUsage || !strings.Contains(errOut, "AccessDenied") {
		t.Fatalf("bad credentials: %d %s", code, errOut)
	}
	if code, _, _ = saba("db", "backup", "s3", "set", "--endpoint", endpoint, "--json"); code != api.ExitUsage {
		t.Fatalf("a bucket is required, exit %d", code)
	}

	// Credentials default to the AWS environment variables.
	const secret = "s3-secret-value-123456"
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	code, out, errOut := saba("db", "backup", "s3", "set", "--bucket", "bucket", "--endpoint", endpoint, "--prefix", "prod/", "--json")
	if code != 0 || strings.Contains(out, secret) || !strings.Contains(out, `"access_key_id": "test-key"`) {
		t.Fatalf("s3 set: %d %s %s", code, out, errOut)
	}
	if got := objects(); len(got) != 0 {
		t.Fatalf("the access check left objects behind: %v", got)
	}

	code, out, _ = saba("db", "backup", "create", "main", "--json")
	var b backupResult
	mustJSON(t, out, &b)
	if code != 0 || b.Location != "local+s3" || b.Warning != "" {
		t.Fatalf("backup with S3 configured: %s", out)
	}
	if got := objects(); len(got) != 1 || got[0] != "prod/main/"+b.Backup.File {
		t.Fatalf("objects in the bucket: %v", got)
	}
	// Changing a setting does not require the secret again.
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if code, _, errOut = saba("db", "backup", "s3", "set", "--bucket", "bucket", "--endpoint", endpoint, "--prefix", "prod", "--region", "auto", "--json"); code != 0 {
		t.Fatalf("updating without the secret: %s", errOut)
	}
	for _, args := range [][]string{{"db", "backup", "s3", "show", "--json"}, {"db", "backup", "s3", "show"}, {"server", "info", "--json"}, {"doctor", "--json"}} {
		if _, out, errOut = saba(args...); strings.Contains(out+errOut, secret) {
			t.Fatalf("%v leaks the S3 secret", args)
		}
	}
	if _, out, _ = saba("doctor", "--json"); !strings.Contains(out, "s3://bucket/prod") {
		t.Fatalf("doctor should report the mirror: %s", out)
	}

	// Deleting a backup removes it from the bucket as well.
	if code, _, errOut = saba("db", "backup", "delete", "main", b.ID, "--yes", "--json"); code != 0 || len(objects()) != 0 {
		t.Fatalf("delete: %s, bucket %v", errOut, objects())
	}
	if code, _, _ = saba("db", "backup", "s3", "remove", "--json"); code != 0 {
		t.Fatal("s3 remove failed")
	}
	_, out, _ = saba("db", "backup", "create", "main", "--json")
	if !strings.Contains(out, `"location": "local"`) {
		t.Fatalf("after removing S3 backups should be local: %s", out)
	}
}

func TestDirectNodeValidation(t *testing.T) {
	startDaemon(t)
	for name, args := range map[string][]string{
		"no address":     {"node", "add", "w1", "root@198.51.100.7", "--direct", "--no-authorize"},
		"public address": {"node", "add", "w1", "root@198.51.100.7", "--direct", "--address", "198.51.100.7", "--no-authorize"},
		"not an address": {"node", "add", "w1", "root@198.51.100.7", "--direct", "--address", "node.internal", "--no-authorize"},
	} {
		code, _, errOut := saba(append(args, "--json")...)
		if code != api.ExitUsage || errorCode(t, errOut) != api.CodeInvalid {
			t.Errorf("%s: exit %d %s", name, code, errOut)
		}
	}
}
