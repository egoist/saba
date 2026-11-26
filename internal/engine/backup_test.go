package engine

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	f, _, err := h.engine.OpenBackup("main", "latest")
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
	if err := h.engine.DeleteBackup("main", b.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.engine.Backups("main"); len(list) != 0 {
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
	list, _ := h.engine.Backups("main")
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
