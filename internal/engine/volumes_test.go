package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/saba/internal/api"
)

// volumeApp deploys an app with two named volumes and a bind mount.
func volumeApp(t *testing.T, h *harness) {
	t.Helper()
	req := image("ok:1")
	req.Config = map[string]any{"volumes": []any{"data:/data", "uploads:/srv/uploads", "/etc/app:/config"}}
	h.mustDeploy("files", req)
	h.rt.SetVolumeFile("saba-files-data", "app.db", "rows v1")
	h.rt.SetVolumeFile("saba-files-data", "sub/notes.txt", "notes")
	h.rt.SetVolumeFile("saba-files-uploads", "cat.png", "meow")
}

// archive builds a gzip-compressed tar from name → content ("" for a directory
// when the name ends in /, "->target" for a symlink).
func archive(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e[0], Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(e[1]))}
		switch {
		case strings.HasSuffix(e[0], "/"):
			h.Typeflag, h.Size, h.Mode = tar.TypeDir, 0, 0o755
		case strings.HasPrefix(e[1], "->"):
			h.Typeflag, h.Size, h.Linkname = tar.TypeSymlink, 0, strings.TrimPrefix(e[1], "->")
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			io.WriteString(tw, e[1])
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestVolumeBackupAndRestore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	volumeApp(t, h)

	b, err := h.engine.Backup(ctx, "files", "manual", api.BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Method != api.BackupVolumes || !slices.Equal(b.Volumes, []string{"data", "uploads"}) || !strings.HasSuffix(b.File, ".tar.gz") || b.Engine != "" {
		t.Fatalf("backup: %+v", b)
	}
	// The archive: the manifest, then one directory per volume.
	r, _, err := h.engine.OpenBackup(ctx, "files", b.ID)
	if err != nil {
		t.Fatal(err)
	}
	gz, _ := gzip.NewReader(r)
	tr := tar.NewReader(gz)
	var names []string
	for {
		hd, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hd.Name)
	}
	r.Close()
	want := []string{manifestName, "data/", "data/app.db", "data/sub/notes.txt", "uploads/", "uploads/cat.png"}
	if !slices.Equal(names, want) {
		t.Fatalf("archive entries: %v, want %v", names, want)
	}
	if list, _ := h.engine.Backups(ctx, "files"); len(list) != 1 || list[0].Method != api.BackupVolumes {
		t.Fatalf("listing: %+v", list)
	}

	// Change the data, then restore: the volume is exactly as backed up.
	h.rt.SetVolumeFile("saba-files-data", "app.db", "rows v2")
	h.rt.SetVolumeFile("saba-files-data", "later.txt", "written after the backup")
	res, err := h.engine.RestoreBackup(ctx, "files", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Volumes, []string{"data", "uploads"}) || res.Backup == nil || res.Backup.ID != b.ID {
		t.Fatalf("restore result: %+v", res)
	}
	got := h.rt.VolumeFiles("saba-files-data")
	if len(got) != 2 || got["app.db"] != "rows v1" || got["sub/notes.txt"] != "notes" {
		t.Fatalf("data after restore: %v", got)
	}
	// The app is running and serving again.
	if code, _ := h.get("files.test", "/"); code != 200 {
		t.Fatalf("app after restore: %d", code)
	}
	if a, _ := h.engine.App(ctx, "files"); a.Status != api.AppRunning {
		t.Fatalf("status after restore: %s", a.Status)
	}
}

func TestVolumeBackupWithStopHoldsRequests(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	volumeApp(t, h)

	var bad atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if code, _ := h.get("files.test", "/"); code != 200 {
					bad.Add(1)
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := h.engine.Backup(ctx, "files", "manual", api.BackupRequest{Stop: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.RestoreBackup(ctx, "files", "latest"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d requests failed while the app was stopped for a backup and a restore", bad.Load())
	}
}

func TestVolumeRestoreChecksTheArchiveFirst(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	volumeApp(t, h)
	before := h.rt.VolumeFiles("saba-files-data")

	full := archive(t, [2]string{"data/", ""}, [2]string{"data/app.db", "restored"})
	for name, body := range map[string][]byte{
		"truncated":         full[:len(full)-12],
		"not a tar":         []byte("SELECT 1;"),
		"empty":             {},
		"path traversal":    archive(t, [2]string{"data/../../etc/passwd", "x"}),
		"absolute path":     archive(t, [2]string{"/data/app.db", "x"}),
		"through a symlink": archive(t, [2]string{"data/link", "->/v/uploads"}, [2]string{"data/link/cat.png", "x"}),
		"no volumes":        archive(t, [2]string{manifestName, "{}"}),
		"bad volume name":   archive(t, [2]string{"Data/app.db", "x"}),
	} {
		if _, err := h.engine.Restore(ctx, "files", bytes.NewReader(body)); codeOf(err) != api.CodeInvalid {
			t.Errorf("%s: want an invalid-input error, got %v", name, err)
		}
	}
	if got := h.rt.VolumeFiles("saba-files-data"); len(got) != len(before) || got["app.db"] != before["app.db"] {
		t.Fatalf("a refused archive changed the volume: %v", got)
	}

	// Only volumes in the archive are replaced; unknown ones are reported.
	res, err := h.engine.Restore(ctx, "files", bytes.NewReader(archive(t,
		[2]string{"./", ""}, [2]string{"./data/", ""}, [2]string{"./data/app.db", "restored"}, [2]string{"old/x", "y"})))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Volumes, []string{"data"}) || !slices.Equal(res.Skipped, []string{"old"}) {
		t.Fatalf("result: %+v", res)
	}
	if got := h.rt.VolumeFiles("saba-files-data"); len(got) != 1 || got["app.db"] != "restored" {
		t.Fatalf("data: %v", got)
	}
	if got := h.rt.VolumeFiles("saba-files-uploads"); got["cat.png"] != "meow" {
		t.Fatalf("a volume missing from the archive was touched: %v", got)
	}
	if _, err := h.engine.Restore(ctx, "files", bytes.NewReader(archive(t, [2]string{"other/x", "y"}))); codeOf(err) != api.CodeInvalid || !strings.Contains(err.Error(), "no volume of") {
		t.Fatalf("an archive of another app's volumes: %v", err)
	}
	// Uploads are spooled under the data directory and cleaned up.
	if entries, _ := os.ReadDir(filepath.Join(h.engine.DataDir, "tmp")); len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestVolumeBackupPolicy(t *testing.T) {
	h := newHarness(t)
	volumeApp(t, h)
	newDatabase(t, h, "main", "postgres")

	p, err := h.engine.SetBackupPolicy("files", api.BackupPolicy{Schedule: "0 3 * * *", Stop: true})
	if err != nil || p.Keep != defaultBackupKeep || !p.Stop {
		t.Fatalf("policy: %+v %v", p, err)
	}
	at := time.Date(2027, 1, 1, 3, 0, 0, 0, time.UTC)
	if due := h.engine.DueBackups(at); !slices.Contains(due, "files") {
		t.Fatalf("due: %v", due)
	}
	if _, err := h.engine.SetBackupPolicy("main", api.BackupPolicy{Schedule: "@daily", Stop: true}); codeOf(err) != api.CodeInvalid {
		t.Fatalf("--stop for a dump: %v", err)
	}
	h.mustDeploy("plain", image("ok:1"))
	if _, err := h.engine.SetBackupPolicy("plain", api.BackupPolicy{Schedule: "@daily"}); codeOf(err) != api.CodeInvalid {
		t.Fatalf("a schedule for an app without volumes: %v", err)
	}
}
