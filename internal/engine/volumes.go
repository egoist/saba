package engine

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/runtime"
)

// backupImage runs tar next to an app's volumes for backups and restores, so
// the app's own image needs no tools of its own (distroless and scratch
// images work too).
const backupImage = "busybox:1.37"

// manifestName is the first entry of a volume archive. It is informational;
// restores go by the top-level directories, one per volume.
const manifestName = "saba-backup.json"

// restoreScript empties the volumes named as arguments, then extracts the
// archive on stdin into /v, where each volume is mounted at /v/<name>.
const restoreScript = `for d in "$@"; do find "/v/$d" -mindepth 1 -delete || exit 1; done; exec tar -C /v -xpf -`

var volumeDirRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// appVolume is a named volume of an app.
type appVolume struct {
	Name   string // as in the app config: "data"
	Docker string // the Docker volume: "saba-<app>-data"
}

// appVolumes lists an app's named volumes, and counts its bind mounts.
func appVolumes(app string, cfg *api.AppConfig) (vols []appVolume, binds int) {
	seen := map[string]bool{}
	for _, spec := range cfg.Volumes {
		v, err := api.ParseVolume(spec)
		switch {
		case err != nil:
		case v.Bind:
			binds++
		case !seen[v.Source]:
			seen[v.Source] = true
			vols = append(vols, appVolume{Name: v.Source, Docker: volumeName(app, v.Source)})
		}
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	return vols, binds
}

type volumeManifest struct {
	App       string    `json:"app"`
	Volumes   []string  `json:"volumes"`
	CreatedAt time.Time `json:"created_at"`
}

// archiveVolumes writes a tar archive of the app's volumes to w: the
// manifest, then one top-level directory per volume.
func (e *Engine) archiveVolumes(ctx context.Context, t *backupTarget, w io.Writer, stop bool) ([]string, error) {
	name := t.app.Name
	names := make([]string, len(t.volumes))
	for i, v := range t.volumes {
		names[i] = v.Name
	}
	tw := tar.NewWriter(w)
	m, _ := json.MarshalIndent(volumeManifest{App: name, Volumes: names, CreatedAt: time.Now().UTC()}, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: manifestName, Mode: 0o600, Size: int64(len(m)), ModTime: time.Now()}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(m); err != nil {
		return nil, err
	}
	archive := func() error {
		opts := runtime.RunOptions{
			Name:    "saba-" + name + "-backup-" + randomID(),
			Image:   backupImage,
			Network: "none",
			Command: append([]string{"tar", "-C", "/v", "-cf", "-"}, names...),
		}
		for _, v := range t.volumes {
			opts.Volumes = append(opts.Volumes, v.Docker+":/v/"+v.Name+":ro")
		}
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			var stderr tail
			code, err := e.rt(t.node).RunOnce(ctx, opts, pw, &stderr)
			switch {
			case err != nil:
				err = api.Errorf(api.CodeRuntime, "cannot archive the volumes of %q: %v", name, err).
					WithHint("saba runs tar in a %s container next to the volumes; the server must be able to pull it.", backupImage)
			case code != 0:
				err = api.Errorf(api.CodeCommandFailed, "archiving the volumes of %q failed: tar exited with code %d", name, code).
					WithDetail("output", stderr.String()).
					WithHint("Files that change while they are read can make tar fail; retry, or stop the app meanwhile with --stop.")
			}
			pw.CloseWithError(err)
			done <- err
		}()
		cerr := copyTar(tw, tar.NewReader(pr), nil)
		if cerr == nil {
			// tar pads its output past the end-of-archive marker.
			_, cerr = io.Copy(io.Discard, pr)
		}
		pr.CloseWithError(io.ErrClosedPipe) // unblocks the writer if copying stopped early
		if err := <-done; err != nil {
			return err
		}
		if cerr != nil {
			return api.Errorf(api.CodeRuntime, "cannot archive the volumes of %q: %v", name, cerr)
		}
		return nil
	}
	var err error
	if stop {
		err = e.whileStopped(ctx, t, "backup", archive)
	} else {
		err = archive()
	}
	if err != nil {
		return nil, err
	}
	return names, tw.Close()
}

// copyTar copies the entries of tr that keep accepts (all when nil) to tw.
func copyTar(tw *tar.Writer, tr *tar.Reader, keep func(*tar.Header) bool) error {
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if keep != nil && !keep(h) {
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// cleanEntry normalizes an archive entry name, rejecting absolute paths and
// paths that leave the archive's root.
func cleanEntry(name string) (string, bool) {
	if path.IsAbs(name) {
		return "", false
	}
	c := path.Clean(name)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

func topDir(name string) string {
	top, _, _ := strings.Cut(name, "/")
	return top
}

func invalidArchive(format string, args ...any) error {
	return api.Errorf(api.CodeInvalid, "not a usable volume archive: "+format, args...).
		WithHint("Volume backups are .tar.gz files with one top-level directory per volume, as `saba backup create` makes them.")
}

// scanVolumeArchive reads a whole volume archive (plain or gzip-compressed
// tar) and returns its volumes, so a damaged or hostile file is refused
// before any volume is emptied.
func scanVolumeArchive(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	in, closeIn, err := decompressed(f)
	if err != nil {
		return nil, err
	}
	defer closeIn()
	tr := tar.NewReader(in)
	var vols []string
	seen := map[string]bool{}
	links := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, invalidArchive("%v", err)
		}
		name, ok := cleanEntry(h.Name)
		if !ok {
			return nil, invalidArchive("unsafe path %q", h.Name)
		}
		if name == "." || name == manifestName {
			continue
		}
		top := topDir(name)
		if !volumeDirRe.MatchString(top) {
			return nil, invalidArchive("%q is not a volume name", top)
		}
		// Nothing may be written through a symlink from the archive itself.
		for p := path.Dir(name); p != "."; p = path.Dir(p) {
			if links[p] {
				return nil, invalidArchive("%q is inside the symlink %q", h.Name, p)
			}
		}
		switch h.Typeflag {
		case tar.TypeSymlink:
			links[name] = true
		case tar.TypeLink:
			if l, ok := cleanEntry(h.Linkname); !ok || topDir(l) != top {
				return nil, invalidArchive("hard link %q points outside its volume", h.Name)
			}
		}
		if !seen[top] {
			seen[top] = true
			vols = append(vols, top)
		}
	}
	// Read to the end, so gzip verifies its checksum.
	if _, err := io.Copy(io.Discard, in); err != nil {
		return nil, invalidArchive("%v", err)
	}
	if len(vols) == 0 {
		return nil, invalidArchive("it contains no volumes")
	}
	sort.Strings(vols)
	return vols, nil
}

// restoreVolumes replaces the contents of the app's volumes found in the
// archive. The app is stopped meanwhile; requests wait for it.
func (e *Engine) restoreVolumes(ctx context.Context, t *backupTarget, file string) (*api.RestoreResult, error) {
	name := t.app.Name
	lock := e.backupLock(name)
	if !lock.TryLock() {
		return nil, api.Errorf(api.CodeConflict, "a backup or restore of %q is already running", name).
			WithHint("Wait for it to finish, then retry.")
	}
	defer lock.Unlock()

	found, err := scanVolumeArchive(file)
	if err != nil {
		return nil, err
	}
	have := map[string]appVolume{}
	var haveNames []string
	for _, v := range t.volumes {
		have[v.Name] = v
		haveNames = append(haveNames, v.Name)
	}
	var restore, skipped []string
	selected := map[string]bool{}
	for _, v := range found {
		if _, ok := have[v]; ok {
			restore = append(restore, v)
			selected[v] = true
		} else {
			skipped = append(skipped, v)
		}
	}
	if len(restore) == 0 {
		return nil, api.Errorf(api.CodeInvalid, "the archive holds no volume of %q (it has %s)", name, strings.Join(found, ", ")).
			WithDetail("archive_volumes", found).
			WithHint("%q has the volumes %s. Volumes are matched by name.", name, strings.Join(haveNames, ", "))
	}
	extract := func() error {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		in, closeIn, err := decompressed(f)
		if err != nil {
			return err
		}
		defer closeIn()
		pr, pw := io.Pipe()
		go func() {
			tw := tar.NewWriter(pw)
			err := copyTar(tw, tar.NewReader(in), func(h *tar.Header) bool {
				n, ok := cleanEntry(h.Name)
				if !ok || n == "." || !selected[topDir(n)] {
					return false
				}
				if h.Typeflag == tar.TypeDir {
					n += "/"
				}
				h.Name = n
				return true
			})
			if err == nil {
				err = tw.Close()
			}
			pw.CloseWithError(err)
		}()
		opts := runtime.RunOptions{
			Name:    "saba-" + name + "-restore-" + randomID(),
			Image:   backupImage,
			Network: "none",
			Command: append([]string{"sh", "-c", restoreScript, "sh"}, restore...),
			Stdin:   pr,
		}
		for _, v := range restore {
			opts.Volumes = append(opts.Volumes, have[v].Docker+":/v/"+v)
		}
		var stderr tail
		code, err := e.rt(t.node).RunOnce(ctx, opts, discard{}, &stderr)
		pr.CloseWithError(io.ErrClosedPipe)
		if err != nil {
			return api.Errorf(api.CodeRuntime, "cannot restore the volumes of %q: %v", name, err)
		}
		if code != 0 {
			return api.Errorf(api.CodeCommandFailed, "restoring the volumes of %q failed: exit code %d; they may be partly restored", name, code).
				WithDetail("output", stderr.String()).
				WithHint("Fix the cause in details.output and run the restore again; it empties the volumes first.")
		}
		return nil
	}
	if err := e.whileStopped(ctx, t, "restore", extract); err != nil {
		return nil, err
	}
	e.Log.Info("volumes restored", "app", name, "volumes", restore)
	return &api.RestoreResult{App: name, Restored: true, Volumes: restore, Skipped: skipped}, nil
}

// whileStopped runs fn with the app's replicas stopped: requests are held
// by the proxy, in-flight ones finish first, and the replicas are started
// and checked again afterwards, whatever fn returned.
func (e *Engine) whileStopped(ctx context.Context, t *backupTarget, what string, fn func() error) error {
	name, cur := t.app.Name, t.cur
	if t.app.Stopped || len(cur.Replicas) == 0 {
		return fn()
	}
	run := &Run{App: name, Events: newEventLog("")}
	if err := e.lock(name, run); err != nil {
		return err
	}
	defer e.unlock(name, run)
	// A deployment may have finished since t was taken.
	_, a, cur, err := e.current(name)
	if err != nil {
		return err
	}
	if a.Stopped || len(cur.Replicas) == 0 {
		return fn()
	}
	e.Proxy.Pause(name)
	defer e.Proxy.Resume(name)
	if pool := e.Proxy.Pool(name); pool != nil {
		dctx, cancel := context.WithTimeout(ctx, dur(cur.Config.DrainTimeout, defaultDrainTimeout))
		pool.Drain(dctx)
		cancel()
	}
	// Bringing the app back must not depend on the caller still waiting.
	sctx, cancel := bg(ctx, 10*time.Minute)
	defer cancel()
	for _, r := range cur.Replicas {
		if err = e.rt(r.Node).Stop(ctx, r.Container, dur(cur.Config.StopTimeout, defaultStopTimeout)); err != nil {
			err = api.Errorf(api.CodeRuntime, "cannot stop %s for the %s: %v", r.Container, what, err)
			break
		}
	}
	if err == nil {
		err = fn()
	}
	var startErr error
	for _, r := range cur.Replicas {
		if serr := e.rt(r.Node).Start(sctx, r.Container); serr != nil && startErr == nil {
			startErr = serr
		}
	}
	if startErr == nil {
		startErr = e.waitHealthy(sctx, run, a, cur, cur.Replicas)
	}
	if startErr != nil {
		e.Log.Error("app did not come back after its "+what, "app", name, "err", startErr)
		if err == nil {
			err = api.Errorf(api.CodeRuntime, "the %s finished, but %q did not come back healthy: %v", what, name, startErr).
				WithHint("Inspect it with `saba logs -a %s`; `saba restart -a %s` recreates its containers.", name, name)
		}
	}
	return err
}
