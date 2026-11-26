// Package daemon wires the proxy, the deploy engine and the control API into
// the long-running `saba daemon` process.
package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/certs"
	"github.com/egoist/saba/internal/cluster"
	"github.com/egoist/saba/internal/cron"
	"github.com/egoist/saba/internal/engine"
	"github.com/egoist/saba/internal/proxy"
	"github.com/egoist/saba/internal/runtime"
	"github.com/egoist/saba/internal/store"
)

const (
	DefaultSocket  = "/run/saba/saba.sock"
	DefaultDataDir = "/var/lib/saba"
)

type Options struct {
	Version   string
	DataDir   string
	Socket    string
	HTTPAddr  string
	HTTPSAddr string
	// TLS is "auto" or "off".
	TLS string
	// ACMEEmail and BaseDomain seed the stored server config when it is empty.
	// BaseDomain "auto" derives <public-ip>.sslip.io.
	ACMEEmail     string
	BaseDomain    string
	ACMEDirectory string
	// ACMECAFile is an extra root certificate to trust for the ACME directory
	// (private or test CAs).
	ACMECAFile string
	// ClusterAddr is where agents connect when TLS is off. With TLS on they
	// use the HTTPS port.
	ClusterAddr string
	Log         *slog.Logger
	// Runtime overrides the container runtime (tests).
	Runtime runtime.Runtime
	// Ready, if set, is closed once the daemon is serving.
	Ready chan<- struct{}
}

type Daemon struct {
	opts    Options
	log     *slog.Logger
	store   *store.Store
	rt      runtime.Runtime
	proxy   *proxy.Proxy
	engine  *engine.Engine
	cluster *cluster.Manager
	certs   *certs.Store
	issuer  *certs.Issuer
	started time.Time

	cronMu      sync.Mutex
	cronRunning map[string]bool
}

// Run starts the daemon and blocks until ctx is cancelled.
func Run(ctx context.Context, opts Options) error {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if opts.TLS == "" {
		opts.TLS = proxy.TLSAuto
	}
	if opts.TLS != proxy.TLSAuto && opts.TLS != proxy.TLSOff {
		return fmt.Errorf("invalid --tls %q (use \"auto\" or \"off\")", opts.TLS)
	}
	log := opts.Log
	for _, dir := range []string{opts.DataDir, filepath.Join(opts.DataDir, "certs"), filepath.Join(opts.DataDir, "logs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cannot create data directory: %w", err)
		}
	}
	// Build contexts left behind by a crash.
	tmp := filepath.Join(opts.DataDir, "tmp")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}

	st, err := store.Open(filepath.Join(opts.DataDir, "state.json"))
	if err != nil {
		return err
	}
	rt := opts.Runtime
	if rt == nil {
		rt = runtime.NewDocker()
	}
	dockerVersion, err := rt.Version(ctx)
	if err != nil {
		return fmt.Errorf("docker is not available: %w", err)
	}

	d := &Daemon{opts: opts, log: log, store: st, rt: rt, started: time.Now(), cronRunning: map[string]bool{}}
	if err := d.seedConfig(ctx); err != nil {
		return err
	}

	d.proxy = proxy.New(log)
	d.engine = engine.New(st, rt, d.proxy, opts.DataDir, log)
	d.engine.TLS = opts.TLS == proxy.TLSAuto
	d.engine.URLSuffix = urlSuffix(opts)
	d.proxy.Dial = d.engine.Dial
	clusterID, err := cluster.LoadIdentity(filepath.Join(opts.DataDir, "cluster"), "primary")
	if err != nil {
		return fmt.Errorf("cannot load the cluster key: %w", err)
	}
	if d.cluster, err = cluster.NewManager(clusterID, filepath.Join(filepath.Dir(opts.Socket), "nodes"), opts.Version, log); err != nil {
		return err
	}
	d.cluster.Authorize = d.authorizeAgent
	d.cluster.OnConnect = func(node string) {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		d.engine.ReconcileNode(rctx, node)
	}
	for name := range st.Snapshot().Nodes {
		d.cluster.Prepare(name)
	}
	d.engine.Nodes = d.cluster
	remote := &remoteStore{}
	d.engine.RemoteBackups = func() engine.RemoteStore { return remote.get(d.store.Snapshot().Server.BackupS3) }

	acmeClient, err := acmeHTTPClient(opts.ACMECAFile)
	if err != nil {
		return err
	}
	if err := d.setupCerts(acmeClient); err != nil {
		return err
	}

	inherited := inheritedListeners()
	httpL := takeListener(&inherited, opts.HTTPAddr)
	var httpsL net.Listener
	if opts.TLS == proxy.TLSAuto {
		httpsL = takeListener(&inherited, opts.HTTPSAddr)
	}
	for _, l := range inherited {
		log.Warn("ignoring a socket passed by systemd that matches no configured address", "addr", l.Addr().String())
		l.Close()
	}
	socketActivated := httpL != nil || httpsL != nil

	srv, err := d.proxy.Listen(proxy.ServerOptions{
		HTTPListener:  httpL,
		HTTPSListener: httpsL,
		HTTPAddr:      opts.HTTPAddr,
		HTTPSAddr:     opts.HTTPSAddr,
		TLS:           opts.TLS,
		CertDir:       filepath.Join(opts.DataDir, "certs"),
		Email:         st.Snapshot().Server.ACMEEmail,
		ACMEDirectory: opts.ACMEDirectory,
		ACMEClient:    acmeClient,
		Certs:         d.certs.Lookup,
		ClusterALPN:   cluster.ALPN,
		ClusterTLS:    d.cluster.TLSConfig(),
		Cluster:       d.cluster.Accept,
		OnDrained:     d.cluster.Close,
	})
	if err != nil {
		return err
	}

	if opts.TLS != proxy.TLSAuto {
		// Without an HTTPS listener, agents get a port of their own.
		if l, err := tls.Listen("tcp", opts.ClusterAddr, d.cluster.TLSConfig()); err != nil {
			log.Error("cannot listen for nodes; nodes will not be able to connect", "addr", opts.ClusterAddr, "err", err)
		} else {
			defer l.Close()
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					go d.cluster.Accept(c.(*tls.Conn))
				}
			}()
		}
	}
	d.engine.Reconcile(ctx)

	apiL, err := listenUnix(opts.Socket)
	if err != nil {
		return err
	}
	apiSrv := &http.Server{Handler: d.routes(), ReadHeaderTimeout: 30 * time.Second}

	log.Info("saba daemon started", "version", opts.Version, "docker", dockerVersion,
		"http", opts.HTTPAddr, "https", opts.HTTPSAddr, "tls", opts.TLS, "socket", opts.Socket,
		"socket_activated", socketActivated,
		"base_domain", st.Snapshot().Server.BaseDomain, "apps", len(st.Snapshot().Apps))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		if err := srv.Serve(ctx); err != nil {
			errc <- err
		}
	}()
	go func() {
		if err := apiSrv.Serve(apiL); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go d.cronLoop(ctx)
	go d.ensureCerts(ctx)
	if opts.Ready != nil {
		close(opts.Ready)
	}

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = apiSrv.Shutdown(shutCtx)
	apiSrv.Close()
	// The proxy drains in-flight requests before its Serve returns.
	<-proxyDone
	// Only now is it safe to drop the node connections those requests used.
	d.cluster.Close()
	// Let a deployment that is about to finish record its outcome. Anything
	// still running after that is failed by Reconcile on the next start.
	if !d.engine.Wait(5 * time.Second) {
		log.Warn("stopping while a deployment is in progress; it will be marked failed on the next start")
	}
	os.Remove(opts.Socket)
	log.Info("saba daemon stopped")
	return err
}

func urlSuffix(opts Options) string {
	addr, std := opts.HTTPAddr, "80"
	if opts.TLS == proxy.TLSAuto {
		addr, std = opts.HTTPSAddr, "443"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == std || port == "0" {
		return ""
	}
	return ":" + port
}

func (d *Daemon) seedConfig(ctx context.Context) error {
	cur := d.store.Snapshot().Server
	base, email := d.opts.BaseDomain, d.opts.ACMEEmail
	if base == "auto" {
		base = ""
		if cur.BaseDomain == "" {
			if ip := PublicIP(ctx); ip != "" {
				base = strings.ReplaceAll(ip, ".", "-") + ".sslip.io"
				d.log.Info("derived base domain from the public IP", "base_domain", base)
			} else {
				d.log.Warn("could not detect a public IP; set one with `saba server config set base_domain <domain>`")
			}
		}
	}
	if (base == "" || cur.BaseDomain != "") && (email == "" || cur.ACMEEmail != "") {
		return nil
	}
	return d.store.Update(func(st *store.State) error {
		if st.Server.BaseDomain == "" && base != "" {
			st.Server.BaseDomain = api.NormalizeDomain(base)
		}
		if st.Server.ACMEEmail == "" && email != "" {
			st.Server.ACMEEmail = email
		}
		return nil
	})
}

// PublicIP returns this machine's public IPv4 address, or "".
func PublicIP(ctx context.Context) string {
	// No packet is sent: connecting a UDP socket only selects the outbound interface.
	if c, err := net.Dial("udp4", "1.1.1.1:53"); err == nil {
		ip := c.LocalAddr().(*net.UDPAddr).IP
		c.Close()
		if isPublic(ip) {
			return ip.String()
		}
	}
	// Behind NAT (most clouds): ask an echo service.
	client := &http.Client{Timeout: 4 * time.Second}
	for _, url := range []string{"https://api.ipify.org", "https://ipv4.icanhazip.com", "https://checkip.amazonaws.com"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil && ip.To4() != nil && isPublic(ip) {
			return ip.String()
		}
	}
	return ""
}

func isPublic(ip net.IP) bool {
	return ip != nil && ip.To4() != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() &&
		!(ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64) // CGNAT 100.64.0.0/10
}

// listenUnix creates the control socket. Access to it is the only
// authorization check, so it is restricted to root and the docker group
// (whose members already control the host's containers).
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create socket directory: %w", err)
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, fmt.Errorf("another saba daemon is already listening on %s", path)
	}
	os.Remove(path)
	if len(path) > 107 {
		return nil, fmt.Errorf("socket path %s is too long (%d characters; the limit for unix sockets is 107)", path, len(path))
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", path, err)
	}
	mode := os.FileMode(0o600)
	if os.Geteuid() == 0 {
		if g, err := user.LookupGroup("docker"); err == nil {
			if gid, err := strconv.Atoi(g.Gid); err == nil && os.Chown(path, 0, gid) == nil {
				mode = 0o660
			}
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// cronLoop fires due cron jobs at the top of every minute (UTC).
func (d *Daemon) cronLoop(ctx context.Context) {
	for {
		now := time.Now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(now)):
		}
		tick := next.UTC()
		go d.ensureCerts(ctx)
		if tick.Minute() == 0 {
			// Hourly: upload any backup that exists only on this server.
			go func() {
				if _, err := d.engine.SyncBackups(ctx); err != nil {
					d.log.Warn("backup sync failed", "err", err)
				}
			}()
		}
		for _, name := range d.engine.DueBackups(tick) {
			go d.runBackup(ctx, name)
		}
		st := d.store.Snapshot()
		for _, a := range st.Apps {
			cur := a.Current()
			if cur == nil || a.Stopped {
				continue
			}
			for _, job := range cur.Config.Cron {
				sched, err := cron.Parse(job.Schedule)
				if err != nil || !sched.Matches(tick) {
					continue
				}
				go d.runCron(ctx, a.Name, job.Name)
			}
		}
	}
}

func (d *Daemon) runBackup(ctx context.Context, name string) {
	bctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	if _, err := d.engine.Backup(bctx, name, "scheduled"); err != nil {
		d.log.Error("scheduled backup failed", "app", name, "err", err)
	}
}

func (d *Daemon) runCron(ctx context.Context, app, job string) {
	key := app + "/" + job
	d.cronMu.Lock()
	if d.cronRunning[key] {
		d.cronMu.Unlock()
		d.log.Warn("cron job still running, skipping this tick", "app", app, "job", job)
		return
	}
	d.cronRunning[key] = true
	d.cronMu.Unlock()
	defer func() {
		d.cronMu.Lock()
		delete(d.cronRunning, key)
		d.cronMu.Unlock()
	}()
	state, err := d.engine.RunCron(ctx, app, job)
	if err != nil {
		d.log.Warn("cron job failed to run", "app", app, "job", job, "err", err)
		return
	}
	d.log.Info("cron job finished", "app", app, "job", job, "status", state.LastStatus, "exit", state.ExitCode, "took", state.Duration)
}
