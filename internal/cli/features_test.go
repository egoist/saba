package cli

import (
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"github.com/egoist/saba/internal/cluster"
	"io"
	"log/slog"
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
	if code, _, errOut := saba("deploy", "-a", "web", "--image", "ok:1", "--node", "ghost", "--json"); code != api.ExitNotFound || !strings.Contains(errOut, "saba node add") {
		t.Fatalf("deploying to an unknown node: %d %s", code, errOut)
	}
	if code, _, _ = saba("node", "add", "local", "--primary-address", "127.0.0.1:1", "--json"); code != api.ExitUsage {
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
// fakeS3PutsFail makes the fake S3 endpoint reject uploads.
var fakeS3PutsFail bool

func fakeS3(t *testing.T) (endpoint string, objects func() []string) {
	t.Helper()
	var mu sync.Mutex
	store := map[string][]byte{}
	fakeS3PutsFail = false
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
			if fakeS3PutsFail {
				http.Error(w, "<Error><Code>InternalError</Code><Message>disk full</Message></Error>", http.StatusInternalServerError)
				return
			}
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

	// A backup whose upload failed keeps doctor failing until it is in the
	// bucket, even once S3 answers again.
	fakeS3PutsFail = true
	_, out, _ = saba("db", "backup", "create", "main", "--json")
	fakeS3PutsFail = false
	if !strings.Contains(out, `"location": "local"`) || !strings.Contains(out, "upload to S3 failed") {
		t.Fatalf("backup during an upload outage: %s", out)
	}
	if code, out, _ = saba("doctor", "--json"); code == 0 || !strings.Contains(out, "exist only on this server") {
		t.Fatalf("doctor must report the local-only backup: %d %s", code, out)
	}
	saba("db", "backup", "s3", "set", "--bucket", "bucket", "--endpoint", endpoint, "--prefix", "prod", "--json")
	for i := 0; i < 50 && len(objects()) < 2; i++ {
		time.Sleep(20 * time.Millisecond) // the catch-up upload runs in the background
	}
	if code, out, _ = saba("doctor", "--json"); code != 0 {
		t.Fatalf("doctor after the catch-up: %d %s", code, out)
	}

	// Deleting a backup removes it from the bucket as well.
	if code, _, errOut = saba("db", "backup", "delete", "main", b.ID, "--yes", "--json"); code != 0 || len(objects()) != 1 {
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

// The full join flow, with the real daemon and a real agent in-process.
func TestNodeJoinThroughTheCLI(t *testing.T) {
	clusterAddr := startDaemon(t)

	// Without an SSH destination, node add prints how to install the agent.
	code, out, errOut := saba("node", "add", "w1", "--primary-address", clusterAddr, "--address", "10.0.0.2", "--json")
	var res nodeAddResult
	mustJSON(t, out, &res)
	if code != 0 || res.Join == "" || len(res.Instructions) != 2 || !strings.Contains(res.Instructions[1], "saba agent install --join saba-join-") {
		t.Fatalf("node add: %d %s %s", code, out, errOut)
	}
	j, err := cluster.ParseJoin(res.Join)
	if err != nil || j.Primary != clusterAddr || j.Node != "w1" {
		t.Fatalf("join code: %+v %v", j, err)
	}
	nodeStatus := func(name string) api.Node {
		t.Helper()
		_, out, _ := saba("node", "list", "--json")
		var nodes []api.Node
		mustJSON(t, out, &nodes)
		for _, n := range nodes {
			if n.Name == name {
				if n.TokenHash != "" {
					t.Fatal("node list must not expose the token hash")
				}
				return n
			}
		}
		return api.Node{}
	}
	if n := nodeStatus("w1"); n.Status != "pending" || n.Address != "10.0.0.2" {
		t.Fatalf("before joining: %+v", n)
	}
	if code, out, _ = saba("doctor", "--json"); code == 0 || !strings.Contains(out, "Waiting for the agent to join") {
		t.Fatalf("doctor should report the pending node: %s", out)
	}

	// The agent joins with the code and shows up connected, with stats.
	runAgent := func(code string) (context.CancelFunc, chan error) {
		dir, _ := os.MkdirTemp("", "ag")
		t.Cleanup(func() { os.RemoveAll(dir) })
		jj, _ := cluster.ParseJoin(code)
		cfg := filepath.Join(dir, "agent.json")
		cluster.ConfigFromJoin(jj).Save(cfg)
		ctx, cancel := context.WithCancel(context.Background())
		a := &cluster.Agent{ConfigPath: cfg, DataDir: filepath.Join(dir, "data"), Version: "test",
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HeartbeatInterval: 50 * time.Millisecond,
			DockerVersion: func(context.Context) string { return "27.1.0" }}
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		t.Cleanup(cancel)
		return cancel, done
	}
	stop, _ := runAgent(res.Join)
	waitStatus := func(name, want string) api.Node {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if n := nodeStatus(name); n.Status == want && (want != "connected" || n.Stats != nil) {
				return n
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("%s never became %s: %+v", name, want, nodeStatus(name))
		return api.Node{}
	}
	n := waitStatus("w1", "connected")
	if n.Version != "test" || n.Docker != "27.1.0" || n.Stats.CPUs == 0 || n.KeyFingerprint == "" || n.JoinedAt.IsZero() {
		t.Fatalf("connected node: %+v", n)
	}
	if code, out, _ = saba("doctor", "--json"); !strings.Contains(out, "node:w1") || !strings.Contains(out, "Connected") {
		t.Fatalf("doctor: %d %s", code, out)
	}

	// Adding a joined node again only updates it; it does not hand out a code.
	code, out, _ = saba("node", "add", "w1", "--address", "10.0.0.3", "--json")
	res = nodeAddResult{}
	mustJSON(t, out, &res)
	if code != 0 || res.Join != "" || res.Node.Address != "10.0.0.3" {
		t.Fatalf("re-adding a joined node: %s", out)
	}

	// The agent reconnects by itself, with its pinned key.
	stop()
	waitStatus("w1", "unreachable")
	runAgent(j.String()) // the same config again; the spent token is ignored, the key is new
	time.Sleep(400 * time.Millisecond)
	if n := nodeStatus("w1"); n.Status == "connected" {
		t.Fatal("an agent with a different key must not take over a joined node")
	}

	// --rejoin issues a new code for a reinstalled machine.
	code, out, _ = saba("node", "add", "w1", "--rejoin", "--primary-address", clusterAddr, "--json")
	res = nodeAddResult{}
	mustJSON(t, out, &res)
	if code != 0 || res.Join == "" {
		t.Fatalf("rejoin: %s", out)
	}
	runAgent(res.Join)
	waitStatus("w1", "connected")

	// Removing the node refuses it from then on.
	if code, _, errOut = saba("node", "remove", "w1", "--yes", "--json"); code != 0 {
		t.Fatalf("remove: %s", errOut)
	}
	if n := nodeStatus("w1"); n.Name != "" {
		t.Fatalf("removed node still listed: %+v", n)
	}
}

// Settings can be given to an app before its first deploy: env set, domains
// add and db link create it without starting it.
func TestSettingsBeforeTheFirstDeploy(t *testing.T) {
	startDaemon(t)
	saba("db", "create", "main", "--engine", "postgres", "--json")

	code, out, errOut := saba("env", "set", "-a", "api", "SECRET=s", "--json")
	var res api.DeployResult
	mustJSON(t, out, &res)
	if code != 0 || res.Deployed || !res.Saved || !strings.Contains(strings.Join(res.Plan, " "), "without starting it") {
		t.Fatalf("env set on a new app: %d %s %s", code, out, errOut)
	}
	if code, _, errOut = saba("db", "link", "main", "-a", "api", "--json"); code != 0 {
		t.Fatalf("db link on a new app: %s", errOut)
	}
	if code, _, errOut = saba("domains", "add", "-a", "api", "api.example.com", "--json"); code != 0 {
		t.Fatalf("domains add on a new app: %s", errOut)
	}
	_, out, _ = saba("status", "api", "--json")
	var app api.App
	mustJSON(t, out, &app)
	if app.Status != api.AppCreated || len(app.Containers) != 0 || app.Config.Domains[0] != "api.example.com" {
		t.Fatalf("the app must exist without running: %s", out)
	}

	// Removing things from an app that does not exist is still an error.
	if code, _, _ = saba("env", "unset", "-a", "ghost", "X", "--json"); code != api.ExitNotFound {
		t.Fatalf("env unset on a missing app: %d", code)
	}
	// A reference to something that does not exist is rejected, and creates nothing.
	if code, _, _ = saba("env", "set", "-a", "typo", "DB=${{nope.URL}}", "--json"); code != api.ExitUsage {
		t.Fatalf("bad reference: %d", code)
	}
	if code, _, _ = saba("status", "typo", "--json"); code != api.ExitNotFound {
		t.Fatal("a rejected change must not create the app")
	}

	// The first deploy starts it with everything saved so far.
	if code, _, errOut = saba("deploy", "-a", "api", "--image", "ok:1", "--json"); code != 0 {
		t.Fatalf("first deploy: %s", errOut)
	}
	_, out, _ = saba("env", "list", "-a", "api", "--json")
	var env api.EnvResponse
	mustJSON(t, out, &env)
	if env.Env["SECRET"] != "s" || !strings.HasPrefix(env.Resolved["DATABASE_URL"], "postgres://postgres:") {
		t.Fatalf("env after the first deploy: %+v", env)
	}
	if _, out, _ = saba("status", "api", "--json"); !strings.Contains(out, "https://api.example.com") && !strings.Contains(out, "http://api.example.com") {
		t.Fatalf("domain after the first deploy: %s", out)
	}
}

func TestCancelPublishAndListFlags(t *testing.T) {
	startDaemon(t)
	saba("deploy", "-a", "web", "--image", "ok:1", "--domain", "web.example.com", "--json")

	// Nothing to cancel.
	if code, _, errOut := saba("cancel", "web", "--json"); code != api.ExitConflict {
		t.Fatalf("cancel with nothing running: %d %s", code, errOut)
	}
	// A deployment that would wait for its health check for a long time.
	if code, _, errOut := saba("deploy", "-a", "web", "--image", "silent:1", "--health-path", "/", "-d", "--json"); code != 0 {
		t.Fatalf("detached deploy: %s", errOut)
	}
	saba("deploy", "-a", "web", "--image", "silent:1", "--json") // must be rejected meanwhile
	code, out, errOut := saba("cancel", "web", "--json")
	var c cancelResult
	mustJSON(t, out, &c)
	if code != 0 || !c.Canceled || c.Version != 2 || c.Status != "failed" {
		t.Fatalf("cancel: %d %s %s", code, out, errOut)
	}
	_, out, _ = saba("deployments", "web", "--json")
	var deps []api.Deployment
	mustJSON(t, out, &deps)
	if deps[0].Version != 2 || deps[0].Status != api.StatusFailed || !strings.Contains(deps[0].Error.Message, "canceled") || deps[1].Status != api.StatusLive {
		t.Fatalf("after cancel: %s", out)
	}
	if _, out, _ = saba("status", "web", "--json"); !strings.Contains(out, `"status": "running"`) {
		t.Fatalf("the previous version must keep running: %s", out)
	}

	// --publish sets the list; a single empty value clears a list flag.
	code, out, errOut = saba("deploy", "-a", "web", "--image", "ok:1", "--publish", "127.0.0.1:19999:8080", "--json")
	var res api.DeployResult
	mustJSON(t, out, &res)
	if code != 0 || len(res.Deployment.Config.Publish) != 1 || res.Deployment.Config.EffectiveStrategy() != api.StrategyRecreate {
		t.Fatalf("--publish: %d %s %s", code, out, errOut)
	}
	code, out, errOut = saba("deploy", "-a", "web", "--image", "ok:1", "--publish=", "--domain=", "--json")
	res = api.DeployResult{}
	mustJSON(t, out, &res)
	if code != 0 || len(res.Deployment.Config.Publish) != 0 || len(res.Deployment.Config.Domains) != 0 || res.URLs[0] != "http://web.apps.test" {
		t.Fatalf("clearing lists: %d %s %s", code, out, errOut)
	}

	// Databases: publish, show, clear; never built from source.
	saba("db", "create", "main", "--engine", "postgres", "--json")
	if code, _, errOut = saba("db", "publish", "main", "127.0.0.1:5432:5432", "--json"); code != 0 {
		t.Fatalf("db publish: %s", errOut)
	}
	if _, out, _ = saba("db", "publish", "main", "--json"); !strings.Contains(out, "127.0.0.1:5432:5432") {
		t.Fatalf("db publish (show): %s", out)
	}
	saba("db", "publish", "main", "--clear", "--json")
	if _, out, _ = saba("db", "publish", "main", "--json"); !strings.Contains(out, `"publish": []`) {
		t.Fatalf("db publish --clear: %s", out)
	}
	if code, _, _ = saba("db", "publish", "web", "1.2.3.4:80:80", "--json"); code != api.ExitUsage {
		t.Fatalf("db publish on an app: %d", code)
	}
	os.WriteFile("index.html", []byte("hi"), 0o644)
	if code, _, errOut = saba("deploy", "-a", "main", "--json"); code != api.ExitUsage || !strings.Contains(errOut, "is a database") {
		t.Fatalf("building a database from source must be refused: %d %s", code, errOut)
	}
}

func TestDurationsAsBareNumbersInSabaToml(t *testing.T) {
	startDaemon(t)
	os.WriteFile("index.html", []byte("hi"), 0o644)
	os.WriteFile("saba.toml", []byte("name = \"site\"\ndrain_timeout = 45\nstop_timeout = 5\n\n[health]\ntimeout = 90\n"), 0o644)
	code, out, errOut := saba("deploy", "--json")
	if code != 0 {
		t.Fatalf("deploy: %s", errOut)
	}
	var res api.DeployResult
	mustJSON(t, out, &res)
	cfg := res.Deployment.Config
	if cfg.DrainTimeout != "45" || cfg.StopTimeout != "5" || cfg.Health.Timeout != "90" {
		t.Fatalf("durations: %+v %+v", cfg, cfg.Health)
	}
}
