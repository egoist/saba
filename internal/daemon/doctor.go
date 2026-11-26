package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/engine"
)

// handleDoctor runs server-side diagnostics. Every failed check carries a
// hint describing the fix, so the report is directly actionable.
func (d *Daemon) handleDoctor(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rep := api.DoctorReport{OK: true}
	add := func(name string, ok bool, msg, hint string) {
		c := api.Check{Name: name, OK: ok, Message: msg}
		if !ok {
			c.Hint = hint
			rep.OK = false
		}
		rep.Checks = append(rep.Checks, c)
	}

	if v, err := d.rt.Version(ctx); err != nil {
		add("docker", false, "Docker is not reachable: "+err.Error(), "Start Docker on the server: `systemctl start docker`.")
	} else {
		add("docker", true, "Docker "+v+" is running", "")
	}

	st := d.store.Snapshot()
	if st.Server.BaseDomain == "" {
		add("base_domain", false, "No base domain is set, so apps without custom domains get no URL",
			"Set one: `saba server config set base_domain <ip-with-dashes>.sslip.io` or a domain with a wildcard DNS record.")
	} else {
		add("base_domain", true, "Apps get <name>."+st.Server.BaseDomain+" by default", "")
	}

	if d.opts.TLS == "auto" {
		add("tls", true, "Automatic HTTPS (Let's Encrypt) is enabled", "")
	} else {
		add("tls", true, "TLS is off; apps are served over plain HTTP on "+d.opts.HTTPAddr, "")
	}

	for _, path := range []string{d.opts.DataDir} {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(path, &fs); err == nil && fs.Blocks > 0 {
			used := 100 - int(fs.Bavail*100/fs.Blocks)
			free := float64(fs.Bavail*uint64(fs.Bsize)) / (1 << 30)
			add("disk", used < 90, fmt.Sprintf("Disk is %d%% full (%.1f GiB free)", used, free),
				"Free space with `saba server prune`, or remove unused apps and volumes.")
		}
	}

	if s3cfg := st.Server.BackupS3; s3cfg != nil {
		localOnly := 0
		for _, name := range st.AppNames() {
			if st.Apps[name].Kind != api.KindDatabase {
				continue
			}
			list, _ := d.engine.Backups(ctx, name)
			for _, b := range list {
				if b.Location == "local" {
					localOnly++
				}
			}
		}
		switch msg := d.engine.RemoteBackupError(); {
		case msg != "":
			add("backup-storage", false, "The last S3 operation failed: "+msg,
				"Backups made since are stored on this server only. Check the settings with `saba db backup s3 show`; setting them again uploads what is missing.")
		case localOnly > 0:
			add("backup-storage", false, fmt.Sprintf("%d backup(s) exist only on this server and are not in S3 yet", localOnly),
				"They are uploaded automatically within the hour. To upload them now, run `saba db backup s3 set` again with the same settings.")
		default:
			add("backup-storage", true, "Backups are mirrored to s3://"+s3cfg.Bucket+"/"+s3cfg.Prefix, "")
		}
	}

	status := d.cluster.Status()
	for _, name := range sortedKeys(st.Nodes) {
		n := d.nodeView(*st.Nodes[name], status, nil)
		switch n.Status {
		case "connected":
			msg := "Connected (Docker " + n.Docker + ", saba " + n.Version + ")"
			if n.Version != d.opts.Version {
				add("node:"+name, false, msg+": the agent runs another version than the primary",
					"Agents upgrade themselves to the primary's release; for development builds, install the same build on the node.")
				continue
			}
			add("node:"+name, true, msg, "")
		case "pending":
			add("node:"+name, false, "Waiting for the agent to join",
				fmt.Sprintf("Run the join command from `saba node add %s` on the node, or get a new one with `saba node add %s --rejoin`.", name, name))
		default:
			add("node:"+name, false, "Unreachable: "+n.Error,
				fmt.Sprintf("Its replicas are out of rotation until it is back. On the node, check `systemctl status saba-agent` and `journalctl -u saba-agent`; the agent reconnects by itself."))
		}
	}
	for _, c := range d.tlsm.list(ctx) {
		left := time.Until(c.NotAfter)
		switch {
		case c.Status == "failed" && c.Source == "wildcard":
			add("cert:"+c.Names[0], false, "Wildcard certificate could not be issued: "+c.Error,
				"Check the provider settings with `saba certs dns`; saba retries with backoff, and immediately after `saba certs dns set`.")
		case c.Status == "failed":
			add("cert:"+c.Names[0], false, "Certificate could not be issued: "+c.Error,
				"Point the domain's DNS (A/AAAA) at this server and keep ports 80 and 443 reachable; saba retries on the next HTTPS request.")
		case c.Source != "custom":
		case left <= 0:
			add("cert:"+c.ID, false, "Custom certificate has expired and is no longer used",
				"Upload a renewed one with `saba certs add`, or remove it with `saba certs remove "+c.ID+" --yes`.")
		case left < 14*24*time.Hour:
			add("cert:"+c.ID, false, fmt.Sprintf("Custom certificate expires in %d days", int(left.Hours()/24)),
				"Upload a renewed one with `saba certs add --cert ... --key ...`.")
		}
	}

	local := localIPs()
	apps, _ := d.engine.Apps(ctx)
	for _, a := range apps {
		switch a.Status {
		case api.AppCrashed:
			add("app:"+a.Name, false, "No container of the live version is running",
				fmt.Sprintf("Inspect with `saba logs -a %s`, then `saba restart -a %s` or `saba rollback -a %s`.", a.Name, a.Name, a.Name))
		case api.AppDegraded:
			add("app:"+a.Name, false, "Some replicas are down or restarting",
				fmt.Sprintf("Inspect with `saba status -a %s` and `saba logs -a %s`.", a.Name, a.Name))
		case api.AppFailed:
			add("app:"+a.Name, false, "The app has never been deployed successfully",
				fmt.Sprintf("See why with `saba logs -a %s --deployment latest`.", a.Name))
		default:
			add("app:"+a.Name, true, "Status: "+a.Status, "")
		}
		if d.opts.TLS != "auto" || a.Status == api.AppCreated {
			continue
		}
		rec := st.Apps[a.Name]
		if rec == nil {
			continue
		}
		for _, host := range engine.Hosts(st.Server, a.Name, a.Kind, hostConfig(a)) {
			if strings.HasPrefix(host, "*.") {
				continue
			}
			ips, err := net.DefaultResolver.LookupHost(ctx, host)
			switch {
			case err != nil:
				add("dns:"+host, false, "The domain does not resolve",
					"Create an A record for "+host+" pointing at this server; HTTPS certificates cannot be issued until it resolves.")
			case len(local) > 0 && !overlap(ips, local):
				// Not necessarily wrong (NAT, CDN), so this stays informational.
				add("dns:"+host, true, fmt.Sprintf("Resolves to %s, which is not an address of this machine (fine behind NAT or a CDN)", strings.Join(ips, ", ")), "")
			default:
				add("dns:"+host, true, "Resolves to this server", "")
			}
		}
	}
	writeJSON(w, http.StatusOK, rep)
	return nil
}

func hostConfig(a *api.App) *api.AppConfig {
	cfg := a.Config
	for _, d := range a.Deployments {
		if d.Version == a.CurrentVersion {
			cfg = d.Config
			cfg.Port = d.Port
		}
	}
	return &cfg
}

func localIPs() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

func overlap(ips []string, set map[string]bool) bool {
	for _, ip := range ips {
		if set[ip] {
			return true
		}
	}
	return false
}
