package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/engine"
	"github.com/egoist/saba/internal/s3"
	"github.com/egoist/saba/internal/store"
)

// s3Store adapts the S3 client to the engine's remote backup interface.
type s3Store struct{ c *s3.Client }

func (s s3Store) Put(ctx context.Context, key string, f *os.File, size int64) error {
	return s.c.Put(ctx, key, f, size)
}
func (s s3Store) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	return s.c.Get(ctx, key)
}
func (s s3Store) Delete(ctx context.Context, key string) error { return s.c.Delete(ctx, key) }
func (s s3Store) List(ctx context.Context, prefix string) ([]engine.RemoteObject, error) {
	objs, err := s.c.List(ctx, prefix)
	out := make([]engine.RemoteObject, 0, len(objs))
	for _, o := range objs {
		out = append(out, engine.RemoteObject{Key: o.Key, Size: o.Size})
	}
	return out, err
}

func s3Client(c *api.S3Config) (*s3.Client, error) {
	return s3.New(s3.Config{
		Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, Prefix: c.Prefix,
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, PathStyle: c.PathStyle,
	})
}

// remoteStore builds the store for the configured bucket, reusing the client
// until the configuration changes.
type remoteStore struct {
	mu    sync.Mutex
	key   string
	store engine.RemoteStore
}

func (r *remoteStore) get(cfg *api.S3Config) engine.RemoteStore {
	if cfg == nil {
		return nil
	}
	b, _ := json.Marshal(cfg)
	r.mu.Lock()
	defer r.mu.Unlock()
	if string(b) != r.key {
		r.key, r.store = string(b), nil
		if c, err := s3Client(cfg); err == nil {
			r.store = s3Store{c}
		}
	}
	return r.store
}

func (d *Daemon) handleStorageGet(w http.ResponseWriter, r *http.Request) error {
	cfg := redact(d.store.Snapshot().Server).BackupS3
	if cfg == nil {
		cfg = &api.S3Config{}
	}
	writeJSON(w, http.StatusOK, cfg)
	return nil
}

func (d *Daemon) handleStorageSet(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		api.S3Config
		CreateBucket bool `json:"create_bucket"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	cfg := req.S3Config
	cfg.Bucket, cfg.Endpoint, cfg.Region = strings.TrimSpace(cfg.Bucket), strings.TrimSpace(cfg.Endpoint), strings.TrimSpace(cfg.Region)
	// Updating other settings must not require typing the secret again.
	if cur := d.store.Snapshot().Server.BackupS3; cfg.SecretAccessKey == "" && cur != nil && cur.AccessKeyID == cfg.AccessKeyID {
		cfg.SecretAccessKey = cur.SecretAccessKey
	}
	client, err := s3Client(&cfg)
	if err != nil {
		return api.Errorf(api.CodeInvalid, "invalid S3 settings: %v", err).
			WithHint("Required: --bucket, --access-key-id and --secret-access-key. See `saba db backup s3 set --help`.")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if req.CreateBucket {
		if err := client.CreateBucket(ctx); err != nil {
			return api.Errorf(api.CodeInvalid, "cannot create the bucket: %v", err)
		}
	}
	// Prove the settings work before saving them: a backup destination that
	// silently fails is worse than none.
	if err := client.Check(ctx); err != nil {
		e := api.Errorf(api.CodeInvalid, "the S3 settings do not work: %v", err)
		switch {
		case strings.Contains(err.Error(), "NoSuchBucket"):
			e.WithHint("The bucket does not exist. Create it, or re-run with --create-bucket.")
		case strings.Contains(err.Error(), "cannot reach"):
			e.WithHint("Check --endpoint; it must be reachable from the server.")
		default:
			e.WithHint("Check the access key, the secret and the bucket's permissions (read, write, list, delete). For non-AWS services --region is often \"auto\" or \"us-east-1\".")
		}
		return e
	}
	if err := d.store.Update(func(st *store.State) error {
		st.Server.BackupS3 = &cfg
		return nil
	}); err != nil {
		return err
	}
	// Mirror what already exists, in the background.
	go func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Hour)
		defer cancel()
		if n, err := d.engine.SyncBackups(sctx); err != nil {
			d.log.Error("uploading existing backups failed", "err", err)
		} else if n > 0 {
			d.log.Info("uploaded existing backups", "count", n)
		}
	}()
	writeJSON(w, http.StatusOK, redact(d.store.Snapshot().Server).BackupS3)
	return nil
}

func (d *Daemon) handleStorageRemove(w http.ResponseWriter, r *http.Request) error {
	if err := d.store.Update(func(st *store.State) error {
		st.Server.BackupS3 = nil
		return nil
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
	return nil
}
