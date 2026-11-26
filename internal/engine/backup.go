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
	if existing, _ := e.Backups(name); len(existing) > 0 && !existing[0].CreatedAt.Before(at) {
		at = existing[0].CreatedAt.Add(time.Second)
	}
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
	if kind == "scheduled" {
		keep := defaultBackupKeep
		if p := a.Database.Backup; p != nil && p.Keep > 0 {
			keep = p.Keep
		}
		e.pruneBackups(name, keep)
	}
	b, err := e.backup(name, id)
	if err != nil {
		return nil, err
	}
	e.Log.Info("backup created", "app", name, "id", id, "bytes", b.Size)
	return b, nil
}

// pruneBackups removes the oldest scheduled backups beyond keep. Manual
// backups are never removed automatically.
func (e *Engine) pruneBackups(name string, keep int) {
	all, err := e.Backups(name)
	if err != nil {
		return
	}
	n := 0
	for _, b := range all { // newest first
		if b.Kind != "scheduled" {
			continue
		}
		if n++; n > keep {
			if path, err := e.backupPath(name, b.ID); err == nil {
				os.Remove(path)
			}
		}
	}
}

// Backups lists a database's stored backups, newest first.
func (e *Engine) Backups(name string) ([]api.Backup, error) {
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
	out := []api.Backup{}
	for _, en := range entries {
		id, _, ok := strings.Cut(en.Name(), ".")
		m := backupIDRe.FindStringSubmatch(id)
		if !ok || m == nil || !strings.HasSuffix(en.Name(), ".gz") {
			continue
		}
		info, err := en.Info()
		if err != nil {
			continue
		}
		created, _ := time.Parse("20060102T150405Z", m[1])
		out = append(out, api.Backup{ID: id, App: name, Kind: m[2], Engine: engine, Size: info.Size(), CreatedAt: created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (e *Engine) backup(name, id string) (*api.Backup, error) {
	all, err := e.Backups(name)
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

func (e *Engine) backupPath(name, id string) (string, error) {
	b, err := e.backup(name, id)
	if err != nil {
		return "", err
	}
	matches, _ := filepath.Glob(filepath.Join(e.backupDir(name), b.ID+".*.gz"))
	if len(matches) == 0 {
		return "", api.Errorf(api.CodeNotFound, "backup %q is missing on disk", id)
	}
	return matches[0], nil
}

// OpenBackup opens a stored backup (gzip-compressed) for download.
func (e *Engine) OpenBackup(name, id string) (*os.File, *api.Backup, error) {
	b, err := e.backup(name, id)
	if err != nil {
		return nil, nil, err
	}
	path, err := e.backupPath(name, b.ID)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, b, nil
}

// DeleteBackup removes a stored backup.
func (e *Engine) DeleteBackup(name, id string) error {
	if id == "latest" {
		return api.Errorf(api.CodeInvalid, "name the backup to delete by its id")
	}
	path, err := e.backupPath(name, id)
	if err != nil {
		return err
	}
	return os.Remove(path)
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

// RestoreBackup restores a backup stored on the server.
func (e *Engine) RestoreBackup(ctx context.Context, name, id string) (*api.Backup, error) {
	f, b, err := e.OpenBackup(name, id)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return b, e.Restore(ctx, name, f)
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
