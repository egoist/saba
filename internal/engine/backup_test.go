package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/saba/internal/api"
)

func newDatabase(t *testing.T, h *harness, name, engine string) string {
	t.Helper()
	req := image("ok:1")
	req.Kind = api.KindDatabase
	req.Database = &api.DatabaseInfo{Engine: engine, Version: "1"}
	req.Config = map[string]any{"health": map[string]any{"command": "true"}, "strategy": "recreate"}
	h.mustDeploy(name, req)
	return containerName(name, 1, 1)
}

func gunzip(t *testing.T, r io.Reader) string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(gz)
	return string(b)
}

func TestBackupAndRestore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	container := newDatabase(t, h, "main", "postgres")
	h.rt.SetData(container, "rows: 1,2,3")

	b, err := h.engine.Backup(ctx, "main", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != "manual" || b.Engine != "postgres" || b.Size == 0 || time.Since(b.CreatedAt) > time.Minute {
		t.Fatalf("backup metadata: %+v", b)
	}
	f, _, err := h.engine.OpenBackup(ctx, "main", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if got := gunzip(t, f); got != "rows: 1,2,3" {
		t.Fatalf("stored backup contains %q", got)
	}
	f.Close()

	// Restore a stored backup over changed data.
	h.rt.SetData(container, "rows: damaged")
	if _, err := h.engine.RestoreBackup(ctx, "main", b.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.rt.Data(container); got != "rows: 1,2,3" {
		t.Fatalf("after restore the database holds %q", got)
	}

	// Uploaded dumps may be plain or gzip-compressed.
	if err := h.engine.Restore(ctx, "main", strings.NewReader("plain dump")); err != nil || h.rt.Data(container) != "plain dump" {
		t.Fatalf("plain restore: %v, data %q", err, h.rt.Data(container))
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("gzipped dump"))
	gz.Close()
	if err := h.engine.Restore(ctx, "main", &buf); err != nil || h.rt.Data(container) != "gzipped dump" {
		t.Fatalf("gzip restore: %v, data %q", err, h.rt.Data(container))
	}

	// A dump the database rejects is reported with the client's output.
	err = h.engine.Restore(ctx, "main", strings.NewReader("CORRUPT"))
	var ae *api.Error
	if codeOf(err) != api.CodeCommandFailed || !asError(err, &ae) || !strings.Contains(ae.Details["output"].(string), "syntax error") {
		t.Fatalf("failed restore: %v", err)
	}
	if err := h.engine.Restore(ctx, "main", strings.NewReader("")); codeOf(err) != api.CodeInvalid {
		t.Fatalf("an empty dump should be rejected, got %v", err)
	}
	if _, err := h.engine.RestoreBackup(ctx, "main", "20000101T000000Z-manual"); codeOf(err) != api.CodeNotFound {
		t.Fatalf("unknown backup id: %v", err)
	}

	// No partial files are ever left behind.
	entries, _ := os.ReadDir(h.engine.backupDir("main"))
	if len(entries) != 1 || strings.Contains(entries[0].Name(), "partial") {
		t.Fatalf("backup directory: %v", entries)
	}
	if err := h.engine.DeleteBackup(ctx, "main", b.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.engine.Backups(ctx, "main"); len(list) != 0 {
		t.Fatalf("backup not deleted: %v", list)
	}
}

func asError(err error, target **api.Error) bool {
	e, ok := err.(*api.Error)
	*target = e
	return ok
}

func TestBackupRequiresASupportedRunningDatabase(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mustDeploy("web", image("ok:1"))
	newDatabase(t, h, "cache", "redis")
	newDatabase(t, h, "main", "postgres")

	if _, err := h.engine.Backup(ctx, "web", "manual"); codeOf(err) != api.CodeInvalid {
		t.Fatalf("backing up an app: %v", err)
	}
	if _, err := h.engine.Backup(ctx, "cache", "manual"); codeOf(err) != api.CodeInvalid || !strings.Contains(err.Error(), "redis") {
		t.Fatalf("unsupported engine: %v", err)
	}
	if _, err := h.engine.Backup(ctx, "ghost", "manual"); codeOf(err) != api.CodeNotFound {
		t.Fatalf("unknown database: %v", err)
	}
	if err := h.engine.Stop(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Backup(ctx, "main", "manual"); codeOf(err) != api.CodeConflict {
		t.Fatalf("stopped database: %v", err)
	}
}

func TestBackupScheduleAndRetention(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	newDatabase(t, h, "main", "postgres")
	newDatabase(t, h, "other", "postgres")

	if _, err := h.engine.SetBackupPolicy("main", api.BackupPolicy{Schedule: "every day"}); codeOf(err) != api.CodeInvalid {
		t.Fatalf("invalid schedule: %v", err)
	}
	if _, err := h.engine.SetBackupPolicy("main", api.BackupPolicy{Schedule: "@daily", Keep: 1000}); codeOf(err) != api.CodeInvalid {
		t.Fatalf("invalid keep: %v", err)
	}
	p, err := h.engine.SetBackupPolicy("main", api.BackupPolicy{Schedule: "30 2 * * *", Keep: 2})
	if err != nil || p.Keep != 2 {
		t.Fatalf("set policy: %+v %v", p, err)
	}
	at := func(s string) time.Time { tm, _ := time.Parse("2006-01-02 15:04", s); return tm }
	if due := h.engine.DueBackups(at("2026-01-01 02:30")); len(due) != 1 || due[0] != "main" {
		t.Fatalf("due at 02:30: %v", due)
	}
	if due := h.engine.DueBackups(at("2026-01-01 02:31")); len(due) != 0 {
		t.Fatalf("due at 02:31: %v", due)
	}
	// Setting a policy must not restart the database.
	if a, _ := h.engine.Store.App("main"); a.LastVersion != 1 {
		t.Fatalf("policy change created version %d", a.LastVersion)
	}

	// Scheduled backups are pruned to the newest `keep`; manual ones are never pruned.
	if _, err := h.engine.Backup(ctx, "main", "manual"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 4; i++ {
		b, err := h.engine.Backup(ctx, "main", "scheduled")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	list, _ := h.engine.Backups(ctx, "main")
	var kept []string
	manual := 0
	for _, b := range list {
		if b.Kind == "manual" {
			manual++
		} else {
			kept = append(kept, b.ID)
		}
	}
	if manual != 1 || len(kept) != 2 || kept[0] != ids[3] || kept[1] != ids[2] {
		t.Fatalf("retention kept %v (manual: %d), want the two newest of %v", kept, manual, ids)
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID <= list[i].ID {
			t.Fatalf("backups are not listed newest first: %v", list)
		}
	}

	if p, err := h.engine.SetBackupPolicy("main", api.BackupPolicy{Schedule: "off"}); err != nil || p != nil {
		t.Fatalf("disabling: %+v %v", p, err)
	}
	if due := h.engine.DueBackups(at("2026-01-01 02:30")); len(due) != 0 {
		t.Fatalf("disabled schedule still due: %v", due)
	}

	// Destroy keeps backups unless data deletion was requested.
	if _, err := h.engine.Destroy(ctx, "main", false); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.engine.DataDir, "backups", "main")); len(entries) != 3 {
		t.Fatalf("destroy without --volumes must keep backups, found %d", len(entries))
	}
	if _, err := h.engine.Backup(ctx, "other", "manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Destroy(ctx, "other", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.engine.DataDir, "backups", "other")); !os.IsNotExist(err) {
		t.Fatal("destroy --volumes must delete backups")
	}
}

// memStore is an in-memory RemoteStore.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	down    bool
}

var errStoreDown = errors.New("connection refused")

func (m *memStore) Put(_ context.Context, key string, f *os.File, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errStoreDown
	}
	b, err := io.ReadAll(io.NewSectionReader(f, 0, size))
	m.objects[key] = b
	return err
}

func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if m.down || !ok {
		return nil, 0, errStoreDown
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return errStoreDown
	}
	delete(m.objects, key)
	return nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]RemoteObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, errStoreDown
	}
	var out []RemoteObject
	for k, v := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, RemoteObject{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (m *memStore) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBackupsAreMirroredToRemoteStorage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	container := newDatabase(t, h, "main", "postgres")
	h.rt.SetData(container, "before storage")

	// A backup made before storage is configured is local only...
	early, err := h.engine.Backup(ctx, "main", "manual")
	if err != nil || early.Location != "local" {
		t.Fatalf("early backup: %+v %v", early, err)
	}
	remote := &memStore{objects: map[string][]byte{}}
	h.engine.RemoteBackups = func() RemoteStore { return remote }
	// ...until a sync uploads it.
	if n, err := h.engine.SyncBackups(ctx); err != nil || n != 1 {
		t.Fatalf("sync uploaded %d: %v", n, err)
	}

	h.rt.SetData(container, "precious data")
	b, err := h.engine.Backup(ctx, "main", "manual")
	if err != nil || b.Location != "local+s3" || b.Warning != "" {
		t.Fatalf("mirrored backup: %+v %v", b, err)
	}
	if keys := remote.keys(); len(keys) != 2 || keys[1] != "main/"+b.File {
		t.Fatalf("objects in the bucket: %v", keys)
	}

	// Disaster: this server's copies are gone. The bucket's are listed and restorable.
	if err := os.RemoveAll(h.engine.backupDir("main")); err != nil {
		t.Fatal(err)
	}
	h.rt.SetData(container, "")
	list, _ := h.engine.Backups(ctx, "main")
	if len(list) != 2 || list[0].ID != b.ID || list[0].Location != "s3" || list[0].Size == 0 {
		t.Fatalf("listing from the bucket: %+v", list)
	}
	if _, err := h.engine.RestoreBackup(ctx, "main", "latest"); err != nil {
		t.Fatal(err)
	}
	if got := h.rt.Data(container); got != "precious data" {
		t.Fatalf("restored from the bucket: %q", got)
	}

	// An outage: the backup still succeeds locally, with a warning, and the
	// failure is visible to `saba doctor`.
	remote.down = true
	local, err := h.engine.Backup(ctx, "main", "manual")
	if err != nil || local.Location != "local" || !strings.Contains(local.Warning, "upload to S3 failed") {
		t.Fatalf("backup during an outage: %+v %v", local, err)
	}
	if h.engine.RemoteBackupError() == "" {
		t.Fatal("the failed upload was not recorded")
	}
	remote.down = false
	if n, err := h.engine.SyncBackups(ctx); err != nil || n != 1 || h.engine.RemoteBackupError() != "" {
		t.Fatalf("sync after the outage uploaded %d: %v (%q)", n, err, h.engine.RemoteBackupError())
	}

	// Retention and deletion apply to the bucket as well.
	if _, err := h.engine.SetBackupPolicy("main", api.BackupPolicy{Schedule: "@daily", Keep: 1}); err != nil {
		t.Fatal(err)
	}
	var newest string
	for i := 0; i < 3; i++ {
		s, err := h.engine.Backup(ctx, "main", "scheduled")
		if err != nil {
			t.Fatal(err)
		}
		newest = s.File
	}
	scheduled := 0
	for _, k := range remote.keys() {
		if strings.Contains(k, "-scheduled") {
			scheduled++
			if k != "main/"+newest {
				t.Fatalf("pruning kept the wrong scheduled backup in the bucket: %s", k)
			}
		}
	}
	if scheduled != 1 || len(remote.keys()) != 4 {
		t.Fatalf("after pruning the bucket holds %v", remote.keys())
	}
	if err := h.engine.DeleteBackup(ctx, "main", b.ID); err != nil {
		t.Fatal(err)
	}
	for _, k := range remote.keys() {
		if strings.Contains(k, b.ID) {
			t.Fatalf("deleted backup is still in the bucket: %s", k)
		}
	}

	// Destroying a database, even with its data, never touches the bucket.
	before := len(remote.keys())
	if _, err := h.engine.Destroy(ctx, "main", true); err != nil {
		t.Fatal(err)
	}
	if len(remote.keys()) != before {
		t.Fatal("destroy removed off-site backups")
	}
}
