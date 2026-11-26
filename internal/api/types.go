// Package api defines the wire types shared by the saba daemon and its clients.
package api

import (
	"fmt"
	"time"
)

// Deployment statuses.
const (
	StatusBuilding   = "building"
	StatusDeploying  = "deploying"
	StatusLive       = "live"
	StatusSuperseded = "superseded"
	StatusFailed     = "failed"
)

// App statuses (derived, never persisted).
const (
	AppCreated   = "created"
	AppDeploying = "deploying"
	AppRunning   = "running"
	AppDegraded  = "degraded"
	AppCrashed   = "crashed"
	AppStopped   = "stopped"
	AppFailed    = "failed"
)

// App kinds.
const (
	KindApp      = "app"
	KindDatabase = "database"
)

// Deploy strategies.
const (
	StrategyRolling  = "rolling"
	StrategyRecreate = "recreate"
)

// AppConfig is the declarative configuration of an app. The same keys are
// used in saba.toml, in the JSON API and in `--json` output.
type AppConfig struct {
	// Image is a registry image to run. Empty when the app is built from source.
	Image string `json:"image,omitempty"`
	// Build controls how source is turned into an image.
	Build *BuildConfig `json:"build,omitempty"`
	// Port the app listens on inside the container. 0 means "worker" (no HTTP).
	Port int `json:"port,omitempty"`
	// Domains routed to this app. Empty means the auto-assigned domain.
	Domains []string `json:"domains,omitempty"`
	// Internal apps are reachable from other apps only, never from the internet.
	Internal bool `json:"internal,omitempty"`
	// Worker apps do not serve HTTP: no port detection, routing or HTTP health check.
	Worker bool `json:"worker,omitempty"`
	// Nodes lists the servers replicas are spread across, e.g. ["local",
	// "worker1"], or ["*"] for all of them. Default: the primary ("local").
	Nodes []string `json:"nodes,omitempty"`
	// Replicas is the number of containers to run (default 1).
	Replicas int `json:"replicas,omitempty"`
	// Command overrides the image's default command.
	Command string `json:"command,omitempty"`
	// ReleaseCommand runs to completion before a new version receives traffic.
	ReleaseCommand string `json:"release_command,omitempty"`
	// Strategy is "rolling" (default, zero downtime) or "recreate".
	Strategy string `json:"strategy,omitempty"`
	// Health configures the readiness check gating traffic switch.
	Health *HealthConfig `json:"health,omitempty"`
	// Resources limits container resources.
	Resources *Resources `json:"resources,omitempty"`
	// Volumes are "name:/container/path" or "/host/path:/container/path".
	Volumes []string `json:"volumes,omitempty"`
	// Publish exposes raw TCP/UDP ports on the host: "5432:5432".
	Publish []string `json:"publish,omitempty"`
	// Cron jobs run commands in the app's image on a schedule.
	Cron []CronJob `json:"cron,omitempty"`
	// DrainTimeout is how long in-flight requests may finish on the old version (default 30s).
	DrainTimeout string `json:"drain_timeout,omitempty"`
	// StopTimeout is the grace period between SIGTERM and SIGKILL (default 10s).
	StopTimeout string `json:"stop_timeout,omitempty"`
}

type BuildConfig struct {
	Dockerfile string            `json:"dockerfile,omitempty"`
	Target     string            `json:"target,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
}

type HealthConfig struct {
	// Path is requested over HTTP; 2xx/3xx means healthy.
	Path string `json:"path,omitempty"`
	// Command runs inside the container; exit code 0 means healthy.
	Command string `json:"command,omitempty"`
	// Timeout is the total time a new version has to become healthy (default 60s).
	Timeout string `json:"timeout,omitempty"`
	// Interval between attempts (default 500ms).
	Interval string `json:"interval,omitempty"`
}

type Resources struct {
	Memory string `json:"memory,omitempty"`
	CPUs   string `json:"cpus,omitempty"`
}

type CronJob struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
}

// App is the public view of an app.
type App struct {
	Name           string                `json:"name"`
	Kind           string                `json:"kind"`
	Config         AppConfig             `json:"config"`
	CurrentVersion int                   `json:"current_version"`
	LastVersion    int                   `json:"last_version"`
	Stopped        bool                  `json:"stopped,omitempty"`
	Database       *DatabaseInfo         `json:"database,omitempty"`
	Deployments    []*Deployment         `json:"deployments,omitempty"`
	Cron           map[string]*CronState `json:"cron_state,omitempty"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`

	// Derived fields, filled in on read.
	Status         string            `json:"status,omitempty"`
	URLs           []string          `json:"urls,omitempty"`
	Containers     []ContainerStatus `json:"containers,omitempty"`
	Traffic        *Traffic          `json:"traffic,omitempty"`
	PendingChanges bool              `json:"pending_changes,omitempty"`
}

type DatabaseInfo struct {
	Engine  string `json:"engine"`
	Version string `json:"version"`
	// EnvName is the conventional variable used when linking (DATABASE_URL, REDIS_URL...).
	EnvName string `json:"env_name"`
	// Backup is the automatic backup policy, if one is set.
	Backup *BackupPolicy `json:"backup,omitempty"`
}

// BackupPolicy schedules automatic backups of a database.
type BackupPolicy struct {
	// Schedule is a cron expression (UTC). Empty disables automatic backups.
	Schedule string `json:"schedule"`
	// Keep is how many scheduled backups are retained (default 7).
	Keep int `json:"keep"`
}

// Backup is a stored database dump.
type Backup struct {
	ID  string `json:"id"`
	App string `json:"app"`
	// File is the backup's file name (<id>.<format>.gz).
	File string `json:"file"`
	// Location says where copies exist: "local", "s3" or "local+s3".
	Location string `json:"location"`
	// Warning is set when the backup exists but something went wrong, e.g.
	// the upload to S3 failed.
	Warning string `json:"warning,omitempty"`
	// Kind is "manual" or "scheduled". Only scheduled backups are pruned.
	Kind      string    `json:"kind"`
	Engine    string    `json:"engine"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

type CronState struct {
	LastRun    time.Time `json:"last_run"`
	LastStatus string    `json:"last_status"`
	ExitCode   int       `json:"exit_code"`
	Duration   string    `json:"duration,omitempty"`
	Output     string    `json:"output,omitempty"`
}

type Traffic struct {
	Requests uint64 `json:"requests"`
	Errors   uint64 `json:"errors"`
	InFlight int64  `json:"in_flight"`
}

type Source struct {
	// Type is image, upload, git, current or rollback.
	Type string `json:"type"`
	// Image for type=image.
	Image string `json:"image,omitempty"`
	// Git URL and Ref for type=git.
	Git string `json:"git,omitempty"`
	Ref string `json:"ref,omitempty"`
	// Commit is informational (set by the client when deploying from a git checkout).
	Commit string `json:"commit,omitempty"`
	// Version to roll back to for type=rollback (0 = previous successful).
	Version int `json:"version,omitempty"`
	// Builder that produced the image (dockerfile, node, go, ...).
	Builder string `json:"builder,omitempty"`
}

type Replica struct {
	// Node is the server the replica runs on; empty means the primary.
	Node        string `json:"node,omitempty"`
	Index       int    `json:"index"`
	Container   string `json:"container"`
	ContainerID string `json:"container_id,omitempty"`
	HostPort    int    `json:"host_port,omitempty"`
}

type Deployment struct {
	Version int    `json:"version"`
	Status  string `json:"status"`
	Trigger string `json:"trigger"`
	Message string `json:"message,omitempty"`
	Source  Source `json:"source"`
	// Image is the pinned local image; empty once pruned (no longer restorable).
	Image string `json:"image,omitempty"`
	// Port is the effective HTTP port (configured, or detected from the image).
	Port       int        `json:"port,omitempty"`
	Config     AppConfig  `json:"config"`
	Replicas   []Replica  `json:"replicas,omitempty"`
	Error      *Error     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
}

func (d *Deployment) Ref() string { return fmt.Sprintf("v%d", d.Version) }

func (d *Deployment) Terminal() bool {
	return d.Status == StatusLive || d.Status == StatusSuperseded || d.Status == StatusFailed
}

type ContainerStatus struct {
	Name      string    `json:"name"`
	Node      string    `json:"node"`
	Replica   int       `json:"replica"`
	Version   int       `json:"version"`
	State     string    `json:"state"`
	Health    string    `json:"health,omitempty"`
	Restarts  int       `json:"restarts"`
	ExitCode  int       `json:"exit_code,omitempty"`
	OOMKilled bool      `json:"oom_killed,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	HostPort  int       `json:"host_port,omitempty"`
}

// DeployRequest is the single mutation that creates a new version of an app.
// Restart, scale, rollback, env and config changes are all expressed with it.
type DeployRequest struct {
	Source Source `json:"source"`
	// Config is a JSON merge patch (RFC 7386) applied to the app's config.
	Config map[string]any `json:"config,omitempty"`
	// EnvSet / EnvUnset modify the app's environment.
	EnvSet   map[string]string `json:"env_set,omitempty"`
	EnvUnset []string          `json:"env_unset,omitempty"`
	// AddDomains / RemoveDomains modify the domain list atomically.
	AddDomains    []string `json:"add_domains,omitempty"`
	RemoveDomains []string `json:"remove_domains,omitempty"`
	// NoDeploy saves config/env changes without creating a new version.
	NoDeploy bool `json:"no_deploy,omitempty"`
	// IfLive turns the request into a no-deploy one when the app has no
	// running version (never deployed, or stopped), so that config-only
	// changes never start an app as a side effect.
	IfLive bool `json:"if_live,omitempty"`
	// DryRun validates and returns the plan without changing anything.
	DryRun bool `json:"dry_run,omitempty"`
	// Trigger and Message annotate the deployment.
	Trigger string `json:"trigger,omitempty"`
	Message string `json:"message,omitempty"`
	// Kind and Database are set when creating a managed database.
	Kind     string            `json:"kind,omitempty"`
	Database *DatabaseInfo     `json:"database,omitempty"`
	Outputs  map[string]string `json:"outputs,omitempty"`
	// CreateOnly fails if the app already exists.
	CreateOnly bool `json:"create_only,omitempty"`
}

// Event is one line of a streaming (NDJSON) response.
type Event struct {
	// Type is phase, log, result or error.
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	// Phase is one of: prepare, build, release, start, health, switch, drain, cleanup.
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	// Stream names the origin of a log line (build, release, container, stdout, stderr).
	Stream  string `json:"stream,omitempty"`
	Replica int    `json:"replica,omitempty"`
	// Data is base64-encoded raw output (exec only).
	Data []byte `json:"data,omitempty"`
	// ExitCode is set on the final event of exec/run streams.
	ExitCode *int `json:"exit_code,omitempty"`
	// Result is set on the final event of a successful deploy stream.
	Result *DeployResult `json:"result,omitempty"`
	Error  *Error        `json:"error,omitempty"`
}

type DeployResult struct {
	App        string      `json:"app"`
	Deployment *Deployment `json:"deployment,omitempty"`
	URLs       []string    `json:"urls,omitempty"`
	// Deployed is true when a new version went live.
	Deployed bool `json:"deployed"`
	// Saved is true when config/env changes were stored without deploying.
	Saved bool `json:"saved,omitempty"`
	// DryRun is true when nothing was changed.
	DryRun bool `json:"dry_run,omitempty"`
	// Plan describes what would happen (dry run) or what was saved (no_deploy).
	Plan []string `json:"plan,omitempty"`
}

type EnvResponse struct {
	App string            `json:"app"`
	Env map[string]string `json:"env"`
	// Resolved has ${{app.VAR}} references expanded.
	Resolved map[string]string `json:"resolved,omitempty"`
}

type ExecRequest struct {
	Command []string `json:"command"`
	Replica int      `json:"replica,omitempty"`
	// OneOff runs the command in a fresh container instead of a running replica.
	OneOff bool `json:"one_off,omitempty"`
}

type ServerConfig struct {
	// BaseDomain is used for auto-assigned app domains: <app>.<base_domain>.
	BaseDomain string `json:"base_domain,omitempty"`
	// ACMEEmail is the contact address for Let's Encrypt.
	ACMEEmail string `json:"acme_email,omitempty"`
	// KeepVersions is how many old images are kept per app for rollbacks (default 5).
	KeepVersions int `json:"keep_versions,omitempty"`
	// Address is the primary's address on the network shared with other
	// nodes. Containers elsewhere use it to reach apps that publish ports here.
	Address string `json:"address,omitempty"`
	// CloudflareAPIToken enables wildcard certificates through the DNS-01
	// challenge for zones hosted at Cloudflare. It is never returned in full.
	CloudflareAPIToken string `json:"cloudflare_api_token,omitempty"`
	// DNSHook is an executable that creates and removes DNS-01 challenge
	// records, for other DNS hosts.
	DNSHook string `json:"dns_hook,omitempty"`
	// BackupS3 is the off-site destination database backups are mirrored to.
	BackupS3 *S3Config `json:"backup_s3,omitempty"`
}

// S3Config addresses a bucket on S3-compatible object storage.
type S3Config struct {
	// Endpoint is the service URL; empty means AWS S3 in Region.
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket"`
	// Prefix is prepended to every object key.
	Prefix      string `json:"prefix,omitempty"`
	AccessKeyID string `json:"access_key_id"`
	// SecretAccessKey is write-only: it is never returned in full.
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	// PathStyle selects https://endpoint/bucket/key addressing. Unset means
	// automatic: virtual-hosted for AWS, path-style everywhere else.
	PathStyle *bool `json:"path_style,omitempty"`
}

// Certificate is a stored (custom or DNS-issued) TLS certificate, or a
// wildcard certificate saba is trying to obtain.
type Certificate struct {
	ID string `json:"id,omitempty"`
	// Names are the DNS names the certificate covers.
	Names []string `json:"names"`
	// Source is "custom" (uploaded) or "dns" (issued through DNS-01).
	Source    string    `json:"source"`
	Issuer    string    `json:"issuer,omitempty"`
	NotBefore time.Time `json:"not_before,omitempty"`
	NotAfter  time.Time `json:"not_after,omitempty"`
	// Status is active, expired, pending or failed.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// LocalNode is the name of the server the daemon itself runs on.
const LocalNode = "local"

// Node is an additional server that runs replicas. A saba agent on the node
// connects out to the primary and carries Docker control and replica traffic.
type Node struct {
	Name string `json:"name"`
	// SSH is the destination the agent was installed through, if any.
	SSH string `json:"ssh,omitempty"`
	// Address is the node's address on the network shared with the other
	// nodes (optional); see ServerConfig.Address.
	Address string `json:"address,omitempty"`
	// KeyFingerprint pins the agent's key, recorded when it first joined.
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	// TokenHash and TokenExpires describe the pending one-time join token.
	// Never returned by the API.
	TokenHash    string    `json:"token_hash,omitempty"`
	TokenExpires time.Time `json:"token_expires,omitzero"`
	JoinedAt     time.Time `json:"joined_at,omitzero"`
	CreatedAt    time.Time `json:"created_at"`

	// Derived on read.
	Status   string     `json:"status,omitempty"` // connected | unreachable | pending
	Error    string     `json:"error,omitempty"`
	Docker   string     `json:"docker_version,omitempty"`
	Version  string     `json:"version,omitempty"` // saba version of the agent
	Hostname string     `json:"hostname,omitempty"`
	Platform string     `json:"platform,omitempty"` // os/arch
	LastSeen time.Time  `json:"last_seen,omitzero"`
	Stats    *HostStats `json:"stats,omitempty"`
	Replicas int        `json:"replicas"`
}

// NodeAddRequest registers a node (or updates it, or issues a new join code).
type NodeAddRequest struct {
	Name           string `json:"name"`
	SSH            string `json:"ssh,omitempty"`
	Address        string `json:"address,omitempty"`
	PrimaryAddress string `json:"primary_address,omitempty"`
	// Rejoin issues a new join code and forgets the node's pinned key, for a
	// reinstalled machine.
	Rejoin bool `json:"rejoin,omitempty"`
}

type NodeAddResponse struct {
	Node Node `json:"node"`
	// Join is the one-time join code, when the node has to (re)join.
	Join        string    `json:"join,omitempty"`
	JoinExpires time.Time `json:"join_expires,omitzero"`
}

// HostStats is a snapshot of a server's resources.
type HostStats struct {
	CPUs      int     `json:"cpus"`
	Load1     float64 `json:"load1"`
	MemTotal  uint64  `json:"mem_total"`
	MemUsed   uint64  `json:"mem_used"`
	DiskTotal uint64  `json:"disk_total"`
	DiskUsed  uint64  `json:"disk_used"`
}

type ServerInfo struct {
	Version    string       `json:"version"`
	APIVersion string       `json:"api_version"`
	Hostname   string       `json:"hostname"`
	Docker     string       `json:"docker_version"`
	TLS        string       `json:"tls"`
	HTTPAddr   string       `json:"http_addr"`
	HTTPSAddr  string       `json:"https_addr,omitempty"`
	DataDir    string       `json:"data_dir"`
	Config     ServerConfig `json:"config"`
	Apps       int          `json:"apps"`
	Uptime     string       `json:"uptime"`
}

type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type DoctorReport struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

type Stats struct {
	App       string `json:"app"`
	Node      string `json:"node"`
	Container string `json:"container"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
	MemoryPct string `json:"memory_percent"`
	NetIO     string `json:"net_io"`
	BlockIO   string `json:"block_io"`
	PIDs      string `json:"pids"`
}

type PruneResult struct {
	ContainersRemoved []string `json:"containers_removed"`
	ImagesRemoved     []string `json:"images_removed"`
	Output            string   `json:"output,omitempty"`
}

type LogsQuery struct {
	Tail    int
	Since   string
	Follow  bool
	Replica int
}
