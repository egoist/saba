package engine

import (
	"bufio"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/cron"
	"github.com/egoist/saba/internal/store"
)

const defaultBackupKeep = 7

// backupSpec describes how to dump and restore one database engine. The
// commands run inside the database container and read credentials from its
// own environment, so the daemon never handles them.
type backupSpec struct {
	ext     string
	dump    string // writes the dump to stdout
	restore string // reads a dump from stdin
}

var backupSpecs = map[string]backupSpec{
	"postgres": {
		ext:  "sql",
		dump: `pg_dump -U "${POSTGRES_USER:-postgres}" -d "$POSTGRES_DB" --clean --if-exists --no-owner --no-acl`,
		// One transaction: a restore that fails midway changes nothing.
		restore: `psql -U "${POSTGRES_USER:-postgres}" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -q -1`,
	},
	"mysql": {
		ext:     "sql",
		dump:    `mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --single-transaction --routines --triggers --no-tablespaces --databases "$MYSQL_DATABASE"`,
		restore: `mysql -uroot -p"$MYSQL_ROOT_PASSWORD"`,
	},
	"mariadb": {
		ext:     "sql",
		dump:    `mariadb-dump -uroot -p"$MARIADB_ROOT_PASSWORD" --single-transaction --routines --triggers --databases "$MARIADB_DATABASE"`,
		restore: `mariadb -uroot -p"$MARIADB_ROOT_PASSWORD"`,
	},
	"mongo": {
		ext:     "archive",
		dump:    `mongodump --quiet --archive -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin`,
		restore: `mongorestore --quiet --archive --drop -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin`,
	},
}

var backupIDRe = regexp.MustCompile(`^(\d{8}T\d{6}Z)-(manual|scheduled)$`)

func (e *Engine) backupDir(app string) string { return filepath.Join(e.DataDir, "backups", app) }

// database returns a running managed database, its container and dump spec.
func (e *Engine) database(name string) (*store.AppRecord, *api.Replica, backupSpec, error) {
	_, a, cur, err := e.current(name)
	if err != nil {
		return nil, nil, backupSpec{}, err
	}
	if a.Kind != api.KindDatabase || a.Database == nil {
		return nil, nil, backupSpec{}, api.Errorf(api.CodeInvalid, "%q is not a managed database", name).
			WithHint("List databases with `saba db list`.")
	}
	spec, ok := backupSpecs[a.Database.Engine]
	if !ok {
		return nil, nil, backupSpec{}, api.Errorf(api.CodeInvalid, "backups are not supported for %s databases", a.Database.Engine).
			WithHint("Supported engines: mariadb, mongo, mysql, postgres. For others, snapshot the volume saba-%s-data.", name)
	}
	if a.Stopped || len(cur.Replicas) == 0 {
		return nil, nil, backupSpec{}, api.Errorf(api.CodeConflict, "%q is not running", name).
			WithHint("Start it first: `saba start %s`.", name)
	}
	return a, &cur.Replicas[0], spec, nil
}

func (e *Engine) backupLock(app string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.backupLocks == nil {
		e.backupLocks = map[string]*sync.Mutex{}
	}
	l := e.backupLocks[app]
	if l == nil {
		l = &sync.Mutex{}
		e.backupLocks[app] = l
	}
	return l
}

// Backup dumps a database into a compressed file on the server.
func (e *Engine) Backup(ctx context.Context, name, kind string) (*api.Backup, error) {
	a, rep, spec, err := e.database(name)
	if err != nil {
		return nil, err
	}
	lock := e.backupLock(name)
	if !lock.TryLock() {
		return nil, api.Errorf(api.CodeConflict, "a backup or restore of %q is already running", name).
			WithHint("Wait for it to finish, then retry.")
	}
	defer lock.Unlock()

	dir := e.backupDir(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// IDs are UTC timestamps, so they sort chronologically. A new backup is
	// always newer than every existing one, even within the same second.
	at := time.Now().UTC().Truncate(time.Second)
	if existing, _ := e.Backups(ctx, name); len(existing) > 0 && !existing[0].CreatedAt.Before(at) {
		at = existing[0].CreatedAt.Add(time.Second)
	}
	// The listing can miss remote backups while the storage is unreachable;
	// the last id handed out by this process is a second guard.
	e.mu.Lock()
	if e.lastBackup == nil {
		e.lastBackup = map[string]time.Time{}
	}
	if last := e.lastBackup[name]; !last.Before(at) {
		at = last.Add(time.Second)
	}
	e.lastBackup[name] = at
	e.mu.Unlock()
	id := at.Format("20060102T150405Z") + "-" + kind
	final := filepath.Join(dir, id+"."+spec.ext+".gz")
	// Written under a temporary name so a failed dump never looks like a backup.
	tmp := final + ".partial"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*api.Backup, error) {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	gz := gzip.NewWriter(f)
	var stderr tail
	code, err := e.rt(rep.Node).Exec(ctx, rep.Container, []string{"sh", "-c", spec.dump}, nil, gz, &stderr)
	if err != nil {
		return fail(api.Errorf(api.CodeRuntime, "backup of %q failed: %v", name, err))
	}
	if code != 0 {
		return fail(api.Errorf(api.CodeCommandFailed, "backup of %q failed: the dump command exited with code %d", name, code).
			WithDetail("output", stderr.String()).
			WithHint("Check that the database is healthy: `saba status %s` and `saba logs %s`.", name, name))
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	b, err := e.backup(ctx, name, id)
	if err != nil {
		return nil, err
	}
	e.Log.Info("backup created", "app", name, "id", id, "bytes", b.Size)
	if remote := e.remote(); remote != nil {
		uploadErr := e.upload(ctx, remote, name, b.File)
		if uploadErr != nil {
			// The backup itself succeeded; say clearly that it is local only.
			b.Warning = "stored locally, but the upload to S3 failed: " + uploadErr.Error()
			e.Log.Error("backup upload failed", "app", name, "id", id, "err", uploadErr)
		} else {
			b.Location = "local+s3"
		}
		e.noteRemote(uploadErr)
		// Once uploads work again, catch up on what an outage left behind.
		e.mu.Lock()
		catchUp := uploadErr == nil && e.needSync
		e.needSync = uploadErr != nil || (e.needSync && !catchUp)
		e.mu.Unlock()
		if catchUp {
			go func() {
				sctx, cancel := bg(ctx, time.Hour)
				defer cancel()
				if _, err := e.SyncBackups(sctx); err != nil {
					e.mu.Lock()
					e.needSync = true
					e.mu.Unlock()
				}
			}()
		}
	}
	if kind == "scheduled" {
		keep := defaultBackupKeep
		if p := a.Database.Backup; p != nil && p.Keep > 0 {
			keep = p.Keep
		}
		e.pruneBackups(ctx, name, keep)
	}
	return b, nil
}

// RemoteStore is off-site storage that backups are mirrored to.
type RemoteStore interface {
	Put(ctx context.Context, key string, f *os.File, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]RemoteObject, error)
}

type RemoteObject struct {
	Key  string
	Size int64
}

func (e *Engine) remote() RemoteStore {
	if e.RemoteBackups == nil {
		return nil
	}
	return e.RemoteBackups()
}

// noteRemote remembers the outcome of the last remote operation for `saba doctor`.
func (e *Engine) noteRemote(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.remoteErr = ""
	if err != nil {
		e.remoteErr = err.Error()
	}
}

// RemoteBackupError returns the error of the last remote backup operation, if it failed.
func (e *Engine) RemoteBackupError() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.remoteErr
}

func (e *Engine) upload(ctx context.Context, remote RemoteStore, name, file string) error {
	f, err := os.Open(filepath.Join(e.backupDir(name), file))
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return remote.Put(ctx, name+"/"+file, f, info.Size())
}

// SyncBackups uploads backups that exist only locally, e.g. the ones made
// before remote storage was configured. It returns how many were uploaded.
func (e *Engine) SyncBackups(ctx context.Context) (int, error) {
	remote := e.remote()
	if remote == nil {
		return 0, nil
	}
	n := 0
	for _, name := range e.Store.Snapshot().AppNames() {
		list, err := e.Backups(ctx, name)
		if err != nil {
			continue
		}
		for _, b := range list {
			if b.Location != "local" {
				continue
			}
			if err := e.upload(ctx, remote, name, b.File); err != nil {
				e.noteRemote(err)
				return n, err
			}
			n++
		}
	}
	e.noteRemote(nil)
	return n, nil
}

// pruneBackups removes the oldest scheduled backups beyond keep, locally and
// remotely. Manual backups are never removed automatically.
func (e *Engine) pruneBackups(ctx context.Context, name string, keep int) {
	all, err := e.Backups(ctx, name)
	if err != nil {
		return
	}
	n := 0
	for _, b := range all { // newest first
		if b.Kind != "scheduled" {
			continue
		}
		if n++; n > keep {
			if err := e.removeBackup(ctx, name, b); err != nil {
				e.Log.Warn("cannot prune backup", "app", name, "id", b.ID, "err", err)
			}
		}
	}
}

func parseBackupFile(name, file string) (api.Backup, bool) {
	id, _, ok := strings.Cut(file, ".")
	m := backupIDRe.FindStringSubmatch(id)
	if !ok || m == nil || !strings.HasSuffix(file, ".gz") {
		return api.Backup{}, false
	}
	created, _ := time.Parse("20060102T150405Z", m[1])
	return api.Backup{ID: id, App: name, File: file, Kind: m[2], CreatedAt: created}, true
}

// Backups lists a database's backups, newest first: the ones on this server
// merged with the ones in remote storage. That makes backups of a lost
// server visible on its replacement as soon as the storage is configured.
func (e *Engine) Backups(ctx context.Context, name string) ([]api.Backup, error) {
	a, err := e.Store.App(name)
	if err != nil {
		return nil, err
	}
	engine := ""
	if a.Database != nil {
		engine = a.Database.Engine
	}
	entries, err := os.ReadDir(e.backupDir(name))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	byID := map[string]*api.Backup{}
	for _, en := range entries {
		b, ok := parseBackupFile(name, en.Name())
		info, ierr := en.Info()
		if !ok || ierr != nil {
			continue
		}
		b.Engine, b.Size, b.Location = engine, info.Size(), "local"
		byID[b.ID] = &b
	}
	if remote := e.remote(); remote != nil {
		objs, err := remote.List(ctx, name+"/")
		e.noteRemote(err)
		if err != nil {
			e.Log.Warn("cannot list remote backups", "app", name, "err", err)
		}
		for _, o := range objs {
			b, ok := parseBackupFile(name, strings.TrimPrefix(o.Key, name+"/"))
			if !ok {
				continue
			}
			if local := byID[b.ID]; local != nil {
				local.Location = "local+s3"
				continue
			}
			b.Engine, b.Size, b.Location = engine, o.Size, "s3"
			byID[b.ID] = &b
		}
	}
	out := make([]api.Backup, 0, len(byID))
	for _, b := range byID {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (e *Engine) backup(ctx context.Context, name, id string) (*api.Backup, error) {
	all, err := e.Backups(ctx, name)
	if err != nil {
		return nil, err
	}
	if id == "latest" && len(all) > 0 {
		return &all[0], nil
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, api.Errorf(api.CodeNotFound, "%q has no backup %q", name, id).
		WithHint("List backups with `saba db backup list %s`.", name)
}

// OpenBackup opens a backup (gzip-compressed) for reading: the local copy if
// there is one, otherwise a stream from remote storage.
func (e *Engine) OpenBackup(ctx context.Context, name, id string) (io.ReadCloser, *api.Backup, error) {
	b, err := e.backup(ctx, name, id)
	if err != nil {
		return nil, nil, err
	}
	if strings.HasPrefix(b.Location, "local") {
		f, err := os.Open(filepath.Join(e.backupDir(name), b.File))
		if err == nil {
			return f, b, nil
		}
	}
	remote := e.remote()
	if remote == nil {
		return nil, nil, api.Errorf(api.CodeNotFound, "backup %q is missing on disk", id)
	}
	body, size, err := remote.Get(ctx, name+"/"+b.File)
	if err != nil {
		return nil, nil, api.Errorf(api.CodeRuntime, "cannot fetch backup %s from S3: %v", b.ID, err)
	}
	if size > 0 {
		b.Size = size
	}
	return body, b, nil
}

func (e *Engine) removeBackup(ctx context.Context, name string, b api.Backup) error {
	if strings.Contains(b.Location, "s3") {
		remote := e.remote()
		if remote == nil {
			return api.Errorf(api.CodeRuntime, "backup %s is in S3, but no S3 storage is configured", b.ID)
		}
		if err := remote.Delete(ctx, name+"/"+b.File); err != nil {
			return api.Errorf(api.CodeRuntime, "cannot delete backup %s from S3: %v", b.ID, err)
		}
	}
	if strings.HasPrefix(b.Location, "local") {
		if err := os.Remove(filepath.Join(e.backupDir(name), b.File)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// DeleteBackup removes a backup everywhere it is stored.
func (e *Engine) DeleteBackup(ctx context.Context, name, id string) error {
	if id == "latest" {
		return api.Errorf(api.CodeInvalid, "name the backup to delete by its id")
	}
	b, err := e.backup(ctx, name, id)
	if err != nil {
		return err
	}
	return e.removeBackup(ctx, name, *b)
}

// Restore loads a dump into a database. The input may be gzip-compressed.
func (e *Engine) Restore(ctx context.Context, name string, r io.Reader) error {
	a, rep, spec, err := e.database(name)
	if err != nil {
		return err
	}
	lock := e.backupLock(name)
	if !lock.TryLock() {
		return api.Errorf(api.CodeConflict, "a backup or restore of %q is already running", name).
			WithHint("Wait for it to finish, then retry.")
	}
	defer lock.Unlock()

	br := bufio.NewReaderSize(r, 64*1024)
	var in io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return api.Errorf(api.CodeInvalid, "the backup is not a valid gzip file: %v", err)
		}
		defer gz.Close()
		in = gz
	} else if len(magic) == 0 {
		return api.Errorf(api.CodeInvalid, "the backup is empty")
	}
	var stderr tail
	code, err := e.rt(rep.Node).Exec(ctx, rep.Container, []string{"sh", "-c", spec.restore}, in, discard{}, &stderr)
	if err != nil {
		return api.Errorf(api.CodeRuntime, "restore of %q failed: %v", name, err)
	}
	if code != 0 {
		return api.Errorf(api.CodeCommandFailed, "restore of %q failed: the restore command exited with code %d", name, code).
			WithDetail("output", stderr.String()).
			WithHint("The database client's output is in details.output. Make sure the file is a %s dump.", a.Database.Engine)
	}
	e.Log.Info("backup restored", "app", name)
	return nil
}

// RestoreBackup restores a stored backup, from this server or from remote storage.
func (e *Engine) RestoreBackup(ctx context.Context, name, id string) (*api.Backup, error) {
	r, b, err := e.OpenBackup(ctx, name, id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return b, e.Restore(ctx, name, r)
}

// SetBackupPolicy enables, changes or (with an empty schedule) disables
// automatic backups. It does not restart the database.
func (e *Engine) SetBackupPolicy(name string, p api.BackupPolicy) (*api.BackupPolicy, error) {
	if _, _, _, err := e.database(name); err != nil {
		return nil, err
	}
	p.Schedule = strings.TrimSpace(p.Schedule)
	if p.Schedule == "off" || p.Schedule == "none" {
		p.Schedule = ""
	}
	if p.Schedule != "" {
		if _, err := cron.Parse(p.Schedule); err != nil {
			return nil, api.Errorf(api.CodeInvalid, "invalid backup schedule: %v", err).
				WithHint("Examples: \"@daily\", \"0 3 * * *\" (03:00 UTC every day), \"0 */6 * * *\", or \"off\".")
		}
	}
	if p.Keep == 0 {
		p.Keep = defaultBackupKeep
	}
	if p.Keep < 1 || p.Keep > 365 {
		return nil, api.Errorf(api.CodeInvalid, "keep must be between 1 and 365")
	}
	var out *api.BackupPolicy
	err := e.Store.UpdateApp(name, func(a *store.AppRecord) error {
		if p.Schedule == "" {
			a.Database.Backup = nil
			return nil
		}
		a.Database.Backup = &p
		out = &p
		return nil
	})
	return out, err
}

// DueBackups returns the databases whose backup schedule matches t.
func (e *Engine) DueBackups(t time.Time) []string {
	var due []string
	st := e.Store.Snapshot()
	for _, name := range st.AppNames() {
		a := st.Apps[name]
		if a.Database == nil || a.Database.Backup == nil || a.Stopped || a.Current() == nil {
			continue
		}
		if s, err := cron.Parse(a.Database.Backup.Schedule); err == nil && s.Matches(t) {
			due = append(due, name)
		}
	}
	return due
}
