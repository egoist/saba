package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/store"
)

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				writeError(w, err)
			}
		})
	}
	h("GET /v1/info", d.handleInfo)
	h("GET /v1/config", d.handleGetConfig)
	h("PATCH /v1/config", d.handlePatchConfig)
	h("GET /v1/doctor", d.handleDoctor)
	h("GET /v1/stats", d.handleStats)
	h("POST /v1/prune", d.handlePrune)

	h("GET /v1/backup-storage", d.handleStorageGet)
	h("PUT /v1/backup-storage", d.handleStorageSet)
	h("DELETE /v1/backup-storage", d.handleStorageRemove)

	h("GET /v1/certs", d.handleCerts)
	h("POST /v1/certs", d.handleCertAdd)
	h("DELETE /v1/certs/{id}", d.handleCertRemove)

	h("GET /v1/nodes", d.handleNodes)
	h("POST /v1/nodes", d.handleNodeAdd)
	h("GET /v1/nodes/key", d.handleNodeKey)
	h("DELETE /v1/nodes/{node}", d.handleNodeRemove)

	h("GET /v1/apps", d.handleApps)
	h("GET /v1/apps/{app}", d.handleApp)
	h("DELETE /v1/apps/{app}", d.handleDestroy)
	h("POST /v1/apps/{app}/deploy", d.handleDeploy)
	h("POST /v1/apps/{app}/cancel", d.handleCancel)
	h("POST /v1/apps/{app}/stop", d.handleStop)
	h("POST /v1/apps/{app}/start", d.handleStart)
	h("GET /v1/apps/{app}/env", d.handleEnv)
	h("GET /v1/apps/{app}/outputs", d.handleOutputs)
	h("GET /v1/apps/{app}/deployments", d.handleDeployments)
	h("GET /v1/apps/{app}/deployments/{version}/events", d.handleDeployEvents)
	h("GET /v1/apps/{app}/logs", d.handleLogs)
	h("GET /v1/apps/{app}/container", d.handleContainer)
	h("POST /v1/apps/{app}/exec", d.handleExec)
	h("POST /v1/apps/{app}/cron/{job}/run", d.handleCronRun)
	h("GET /v1/apps/{app}/backups", d.handleBackups)
	h("POST /v1/apps/{app}/backups", d.handleBackupCreate)
	h("GET /v1/apps/{app}/backups/{id}", d.handleBackupDownload)
	h("DELETE /v1/apps/{app}/backups/{id}", d.handleBackupDelete)
	h("PUT /v1/apps/{app}/backup-policy", d.handleBackupPolicy)
	h("POST /v1/apps/{app}/restore", d.handleRestore)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, api.Errorf(api.CodeNotFound, "unknown API endpoint %s %s", r.Method, r.URL.Path).
			WithHint("The saba CLI and daemon versions may differ. Upgrade the older one."))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	e := api.AsError(err)
	writeJSON(w, e.HTTPStatus(), api.ErrorResponse{Error: e})
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return api.Errorf(api.CodeInvalid, "cannot read request body: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return api.Errorf(api.CodeInvalid, "invalid JSON body: %v", err)
	}
	return nil
}

// stream writes NDJSON events, flushing after each one.
type stream struct {
	mu  sync.Mutex
	w   http.ResponseWriter
	enc *json.Encoder
	rc  *http.ResponseController
}

func newStream(w http.ResponseWriter) *stream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &stream{w: w, enc: json.NewEncoder(w), rc: http.NewResponseController(w)}
	_ = s.rc.Flush()
	return s
}

func (s *stream) send(ev api.Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(ev); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (d *Daemon) handleInfo(w http.ResponseWriter, r *http.Request) error {
	st := d.store.Snapshot()
	host, _ := os.Hostname()
	docker, _ := d.rt.Version(r.Context())
	info := api.ServerInfo{
		Version:    d.opts.Version,
		APIVersion: "v1",
		Hostname:   host,
		Docker:     docker,
		TLS:        d.opts.TLS,
		HTTPAddr:   d.opts.HTTPAddr,
		DataDir:    d.opts.DataDir,
		Config:     redact(st.Server),
		Apps:       len(st.Apps),
		Uptime:     time.Since(d.started).Round(time.Second).String(),
	}
	if d.opts.TLS == "auto" {
		info.HTTPSAddr = d.opts.HTTPSAddr
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func (d *Daemon) handleGetConfig(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, redact(d.store.Snapshot().Server))
	return nil
}

func (d *Daemon) handlePatchConfig(w http.ResponseWriter, r *http.Request) error {
	var patch map[string]any
	if err := readJSON(r, &patch); err != nil {
		return err
	}
	var out api.ServerConfig
	err := d.store.Update(func(st *store.State) error {
		for k, v := range patch {
			switch k {
			case "base_domain":
				s, _ := v.(string)
				s = api.NormalizeDomain(s)
				st.Server.BaseDomain = s
			case "acme_email":
				s, _ := v.(string)
				st.Server.ACMEEmail = strings.TrimSpace(s)
			case "cloudflare_api_token":
				s, _ := v.(string)
				st.Server.CloudflareAPIToken = strings.TrimSpace(s)
			case "dns_hook":
				s, _ := v.(string)
				s = strings.TrimSpace(s)
				if s != "" {
					if info, err := os.Stat(s); err != nil || info.IsDir() || !filepath.IsAbs(s) || info.Mode()&0o111 == 0 {
						return api.Errorf(api.CodeInvalid, "dns_hook must be the absolute path of an executable on the server")
					}
				}
				st.Server.DNSHook = s
			case "address":
				s, _ := v.(string)
				st.Server.Address = strings.TrimSpace(s)
			case "keep_versions":
				n, ok := v.(float64)
				if !ok || n < 1 || n > 100 {
					return api.Errorf(api.CodeInvalid, "keep_versions must be a number between 1 and 100")
				}
				st.Server.KeepVersions = int(n)
			default:
				return api.Errorf(api.CodeInvalid, "unknown server config key %q", k).
					WithHint("Valid keys: base_domain, acme_email, keep_versions, address, cloudflare_api_token, dns_hook.")
			}
		}
		out = st.Server
		return nil
	})
	if err != nil {
		return err
	}
	d.engine.RefreshRoutes()
	// New DNS settings deserve an immediate attempt.
	d.issuer.Forget()
	go d.ensureCerts(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, redact(out))
	return nil
}

func (d *Daemon) handleApps(w http.ResponseWriter, r *http.Request) error {
	apps, err := d.engine.Apps(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, apps)
	return nil
}

func (d *Daemon) handleApp(w http.ResponseWriter, r *http.Request) error {
	app, err := d.engine.App(r.Context(), r.PathValue("app"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, app)
	return nil
}

func (d *Daemon) handleDestroy(w http.ResponseWriter, r *http.Request) error {
	// Destruction must finish even if the client disconnects midway.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	name := r.PathValue("app")
	kept, err := d.engine.Destroy(ctx, name, r.URL.Query().Get("volumes") == "true")
	if err != nil {
		return err
	}
	if kept == nil {
		kept = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": name, "destroyed": true, "volumes_kept": kept})
	return nil
}

func (d *Daemon) handleDeploy(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("app")
	var (
		req        api.DeployRequest
		contextDir string
	)
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				os.RemoveAll(contextDir)
				return api.Errorf(api.CodeInvalid, "invalid upload: %v", err)
			}
			switch part.FormName() {
			case "request":
				if err := json.NewDecoder(io.LimitReader(part, 8<<20)).Decode(&req); err != nil {
					os.RemoveAll(contextDir)
					return api.Errorf(api.CodeInvalid, "invalid deploy request: %v", err)
				}
			case "context":
				dir, err := os.MkdirTemp(filepath.Join(d.opts.DataDir, "tmp"), "ctx-")
				if err != nil {
					return err
				}
				contextDir = dir
				if err := extractTarGz(part, dir); err != nil {
					os.RemoveAll(dir)
					return api.Errorf(api.CodeInvalid, "cannot unpack the uploaded source: %v", err)
				}
			}
		}
	} else if err := readJSON(r, &req); err != nil {
		return err
	}

	run, result, err := d.engine.Deploy(name, req, contextDir)
	if err != nil {
		return err
	}
	s := newStream(w)
	if result != nil {
		return ignore(s.send(api.Event{Type: "result", Result: result}))
	}
	if r.URL.Query().Get("detach") == "true" {
		deps, _ := d.engine.Deployments(name)
		res := &api.DeployResult{App: name, Plan: []string{fmt.Sprintf("v%d started in the background", run.Version)}}
		for _, dep := range deps {
			if dep.Version == run.Version {
				res.Deployment = dep
			}
		}
		return ignore(s.send(api.Event{Type: "result", Message: "started", Result: res}))
	}
	_ = s.send(api.Event{Type: "phase", Phase: "queued", Message: fmt.Sprintf("Deployment v%d created", run.Version)})
	// The deployment runs to completion regardless of what happens to this stream.
	_ = run.Events.Stream(r.Context(), true, s.send)
	return nil
}

func ignore(error) error { return nil }

func (d *Daemon) handleCancel(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("app")
	if err := d.engine.Cancel(name); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": name, "canceled": true})
	return nil
}

func (d *Daemon) handleStop(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	name := r.PathValue("app")
	if err := d.engine.Stop(ctx, name); err != nil {
		return err
	}
	return d.respondApp(w, ctx, name)
}

func (d *Daemon) handleStart(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	name := r.PathValue("app")
	if err := d.engine.Start(ctx, name); err != nil {
		return err
	}
	return d.respondApp(w, ctx, name)
}

func (d *Daemon) respondApp(w http.ResponseWriter, ctx context.Context, name string) error {
	app, err := d.engine.App(ctx, name)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, app)
	return nil
}

func (d *Daemon) handleEnv(w http.ResponseWriter, r *http.Request) error {
	env, err := d.engine.Env(r.PathValue("app"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, env)
	return nil
}

func (d *Daemon) handleOutputs(w http.ResponseWriter, r *http.Request) error {
	out, err := d.engine.Outputs(r.PathValue("app"))
	if err != nil {
		return err
	}
	if out == nil {
		out = map[string]string{}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (d *Daemon) handleDeployments(w http.ResponseWriter, r *http.Request) error {
	deps, err := d.engine.Deployments(r.PathValue("app"))
	if err != nil {
		return err
	}
	if deps == nil {
		deps = []*api.Deployment{}
	}
	writeJSON(w, http.StatusOK, deps)
	return nil
}

func parseVersion(s string) (int, error) {
	if s == "" || s == "latest" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(s), "v"))
	if err != nil || n <= 0 {
		return 0, api.Errorf(api.CodeInvalid, "invalid version %q", s).WithHint("Use a version like \"v3\" or \"latest\".")
	}
	return n, nil
}

func (d *Daemon) handleDeployEvents(w http.ResponseWriter, r *http.Request) error {
	version, err := parseVersion(r.PathValue("version"))
	if err != nil {
		return err
	}
	var s *stream
	err = d.engine.DeployEvents(r.Context(), r.PathValue("app"), version, r.URL.Query().Get("follow") == "true", func(ev api.Event) error {
		if s == nil {
			s = newStream(w)
		}
		return s.send(ev)
	})
	if s == nil {
		if err != nil {
			return err
		}
		newStream(w)
	}
	return nil
}

func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	lq := api.LogsQuery{Since: q.Get("since"), Follow: q.Get("follow") == "true"}
	lq.Tail, _ = strconv.Atoi(q.Get("tail"))
	lq.Replica, _ = strconv.Atoi(q.Get("replica"))
	name := r.PathValue("app")
	// Resolve errors before committing to a streaming response.
	if _, err := d.engine.Container(name, lq.Replica); err != nil {
		return err
	}
	s := newStream(w)
	if err := d.engine.Logs(r.Context(), name, lq, func(ev api.Event) { _ = s.send(ev) }); err != nil {
		_ = s.send(api.Event{Type: "error", Error: api.AsError(err)})
	}
	return nil
}

func (d *Daemon) handleContainer(w http.ResponseWriter, r *http.Request) error {
	replica, _ := strconv.Atoi(r.URL.Query().Get("replica"))
	rep, err := d.engine.Replica(r.PathValue("app"), replica)
	if err != nil {
		return err
	}
	out := map[string]string{"container": rep.Container}
	if rep.Node != "" {
		// How to reach the node's Docker engine from the primary.
		out["node"] = rep.Node
		out["docker_host"] = d.cluster.DockerHost(rep.Node)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// eventWriter turns raw output into base64 "output" events.
type eventWriter struct {
	s      *stream
	stream string
}

func (w eventWriter) Write(p []byte) (int, error) {
	if err := w.s.send(api.Event{Type: "output", Stream: w.stream, Data: append([]byte(nil), p...)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (d *Daemon) handleExec(w http.ResponseWriter, r *http.Request) error {
	var req api.ExecRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	name := r.PathValue("app")
	if len(req.Command) == 0 {
		return api.Errorf(api.CodeInvalid, "no command given")
	}
	if !req.OneOff {
		if _, err := d.engine.Container(name, req.Replica); err != nil {
			return err
		}
	} else if _, err := d.engine.Container(name, 0); err != nil {
		return err
	}
	s := newStream(w)
	code, err := d.engine.Exec(r.Context(), name, req, nil, eventWriter{s, "stdout"}, eventWriter{s, "stderr"})
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			return nil
		}
		_ = s.send(api.Event{Type: "error", Error: api.AsError(err)})
		return nil
	}
	_ = s.send(api.Event{Type: "exit", ExitCode: &code})
	return nil
}

func (d *Daemon) handleCronRun(w http.ResponseWriter, r *http.Request) error {
	state, err := d.engine.RunCron(r.Context(), r.PathValue("app"), r.PathValue("job"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, state)
	return nil
}

func (d *Daemon) handleStats(w http.ResponseWriter, r *http.Request) error {
	stats, err := d.engine.Stats(r.Context(), r.URL.Query().Get("app"))
	if err != nil {
		return err
	}
	if stats == nil {
		stats = []api.Stats{}
	}
	writeJSON(w, http.StatusOK, stats)
	return nil
}

func (d *Daemon) handlePrune(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	out, err := d.rt.Prune(ctx)
	if err != nil {
		return api.Errorf(api.CodeRuntime, "prune failed: %v", err)
	}
	writeJSON(w, http.StatusOK, api.PruneResult{Output: out})
	return nil
}

func (d *Daemon) handleBackups(w http.ResponseWriter, r *http.Request) error {
	backups, err := d.engine.Backups(r.Context(), r.PathValue("app"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, backups)
	return nil
}

func (d *Daemon) handleBackupCreate(w http.ResponseWriter, r *http.Request) error {
	// A dump runs to completion even if the client goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Hour)
	defer cancel()
	b, err := d.engine.Backup(ctx, r.PathValue("app"), "manual")
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, b)
	return nil
}

func (d *Daemon) handleBackupDownload(w http.ResponseWriter, r *http.Request) error {
	f, b, err := d.engine.OpenBackup(r.Context(), r.PathValue("app"), r.PathValue("id"))
	if err != nil {
		return err
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.FormatInt(b.Size, 10))
	w.Header().Set("X-Saba-Backup-Id", b.ID)
	w.Header().Set("X-Saba-Backup-File", b.File)
	_, _ = io.Copy(w, f)
	return nil
}

func (d *Daemon) handleBackupDelete(w http.ResponseWriter, r *http.Request) error {
	if err := d.engine.DeleteBackup(r.Context(), r.PathValue("app"), r.PathValue("id")); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": r.PathValue("app"), "deleted": r.PathValue("id")})
	return nil
}

func (d *Daemon) handleBackupPolicy(w http.ResponseWriter, r *http.Request) error {
	var p api.BackupPolicy
	if err := readJSON(r, &p); err != nil {
		return err
	}
	out, err := d.engine.SetBackupPolicy(r.PathValue("app"), p)
	if err != nil {
		return err
	}
	if out == nil {
		out = &api.BackupPolicy{}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// handleRestore restores a stored backup (?backup=<id>) or the dump sent as
// the request body.
func (d *Daemon) handleRestore(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("app")
	// Abandoning a restore halfway would be worse than finishing it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Hour)
	defer cancel()
	if id := r.URL.Query().Get("backup"); id != "" {
		b, err := d.engine.RestoreBackup(ctx, name, id)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"app": name, "restored": true, "backup": b})
		return nil
	}
	if err := d.engine.Restore(ctx, name, r.Body); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": name, "restored": true})
	return nil
}
