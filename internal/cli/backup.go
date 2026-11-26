package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
	"github.com/egoist/saba/internal/client"
)

// backupScope is how a backup command names its app: `saba db backup`
// takes the database as its first argument, `saba backup` uses -a.
type backupScope struct {
	db bool
}

func (s backupScope) cmd() string {
	if s.db {
		return "saba db backup"
	}
	return "saba backup"
}

// use builds a command's usage line from the words after its name.
func (s backupScope) use(verb, rest string) string {
	if s.db {
		rest = strings.TrimSpace("<database> " + rest)
	}
	return strings.TrimSpace(verb + " " + rest)
}

// args validates n to m positional arguments besides the database.
func (s backupScope) args(n, m int) cobra.PositionalArgs {
	if s.db {
		n, m = n+1, m+1
	}
	return cobra.RangeArgs(n, m)
}

// split returns the app a command acts on and its remaining arguments.
func (s backupScope) split(c *CLI, args []string) (string, []string, error) {
	if s.db {
		return args[0], args[1:], nil
	}
	name, err := c.app()
	return name, args, err
}

// ref is how a message names the app in a command line.
func (s backupScope) ref(name string) string {
	if s.db {
		return " " + name
	}
	return " -a " + name
}

// dbBackupCmds returns the `db backup` group and `db restore`.
func (c *CLI) dbBackupCmds() []*cobra.Command {
	backup, restore := c.backupCmds(backupScope{db: true})
	backup.Short = "Create, schedule, download and delete database backups"
	backup.Long = `Backups of Postgres, MySQL, MariaDB and MongoDB are compressed dumps made by
their own tools (pg_dump, mysqldump, mariadb-dump, mongodump) while the
database runs. Other databases (Redis) are backed up as an archive of their
data volume. Backups are stored on the server in /var/lib/saba/backups/<name>/.

Scheduled backups are pruned to the newest --keep; manual ones are kept until
you delete them.

Configure S3-compatible storage once ("saba db backup s3 set") and every
backup is also uploaded there. Listing, downloading and restoring then work
from the bucket too, so the backups of a lost server are available on its
replacement.`
	backup.Example = `  saba db backup create main                    # dump now, keep it on the server
  saba db backup create main -o main.sql.gz     # ...and download it
  saba db backup schedule main @daily --keep 14 # automatic backups (UTC)
  saba db backup schedule main off
  saba db backup list main
  saba db backup download main latest
  saba db restore main --backup latest --yes
  saba db restore main ./main.sql.gz --yes
  saba db backup s3 set --bucket my-backups --region eu-central-1`
	backup.AddCommand(c.backupS3Cmd())
	restore.Use = "restore <database> [file]"
	return []*cobra.Command{backup, restore}
}

// backupCmd is `saba backup`: backups of any app.
func (c *CLI) backupCmd() *cobra.Command {
	backup, restore := c.backupCmds(backupScope{})
	backup.Short = "Back up an app's volumes (or a database), on demand or on a schedule"
	backup.Long = `Back up the data of an app. For an app with named volumes (volumes =
["data:/data"]) a backup is a .tar.gz with one directory per volume, made
next to the volumes by a small helper container, so the app's image needs no
tools. Managed databases are dumped with their own tools, exactly like
"saba db backup". Bind mounts (/host/path:/path) are not included.

Backups run while the app keeps serving. Files written during the backup can
be caught half-way; for data like SQLite that must not be copied while in
use, --stop stops the app for the duration: requests wait in the proxy
meanwhile (up to a minute) and nothing is refused.

Backups are stored on the server in /var/lib/saba/backups/<app>/, mirrored to
S3 when "saba db backup s3 set" is configured, and share retention,
download and delete with database backups. A restore empties the volumes in
the archive and unpacks it, with the app stopped; the archive is checked in
full first, so a damaged file changes nothing.`
	backup.Example = `  saba backup create                          # the app in ./saba.toml
  saba backup create -a uploads --stop        # stop the app while archiving
  saba backup schedule -a uploads @daily --keep 14
  saba backup list -a uploads
  saba backup download -a uploads latest -o .
  saba backup restore -a uploads --backup latest --yes
  saba backup restore -a uploads ./uploads-20270101T030000Z-scheduled.tar.gz --yes`
	backup.AddCommand(restore)
	return backup
}

// backupCmds builds the backup group and the restore command for a scope.
func (c *CLI) backupCmds(s backupScope) (*cobra.Command, *cobra.Command) {
	backup := &cobra.Command{Use: "backup"}

	var (
		out  string
		stop bool
	)
	create := &cobra.Command{
		Use:   s.use("create", ""),
		Short: "Back up now",
		Args:  s.args(0, 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := s.split(c, args)
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			b, err := cl.BackupCreate(cmd.Context(), name, api.BackupRequest{Stop: stop})
			if err != nil {
				return err
			}
			file := ""
			if out != "" {
				if file, err = c.downloadBackup(cmd, cl, name, b.ID, out); err != nil {
					return err
				}
			}
			return c.result(backupResult{Backup: b, SavedTo: file}, func() {
				// With `-o -` the backup itself is on stdout; keep messages off it.
				ui := c.ui
				if out == "-" {
					ui = newUI(c.stderr, c.noColor)
				}
				what := ""
				if len(b.Volumes) > 0 {
					what = ", " + plural(len(b.Volumes), "volume") + ": " + strings.Join(b.Volumes, ", ")
				}
				ui.success("Backup %s of %s created (%s, %s%s)", b.ID, ui.bold(name), humanBytes(b.Size), b.Location, what)
				if b.Warning != "" {
					ui.printf("%s %s\n", ui.yellow("!"), b.Warning)
				}
				if file != "" && file != "-" {
					ui.note("Saved to %s", file)
				} else if file == "" {
					where := "on the server"
					if strings.Contains(b.Location, "s3") {
						where = "on the server and in S3"
					}
					ui.note("Stored %s. Download it with `%s download%s %s`.", where, s.cmd(), s.ref(name), b.ID)
				}
			})
		},
	}
	create.Flags().StringVarP(&out, "output", "o", "", "Also download the backup to this file or directory (- for stdout)")
	create.Flags().BoolVar(&stop, "stop", false, "Stop the app while its volumes are archived (requests wait); not for database dumps")

	list := &cobra.Command{
		Use: s.use("list", ""), Aliases: []string{"ls"}, Short: "List stored backups", Args: s.args(0, 0),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _, err := s.split(c, args)
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			backups, err := cl.Backups(cmd.Context(), name)
			if err != nil {
				return err
			}
			if backups == nil {
				backups = []api.Backup{}
			}
			return c.result(backups, func() {
				if len(backups) == 0 {
					c.ui.printf("%s has no backups. Create one with `%s create%s`.\n", name, s.cmd(), s.ref(name))
					return
				}
				rows := make([][]string, 0, len(backups))
				for _, b := range backups {
					rows = append(rows, []string{b.ID, b.Kind, b.Method, humanBytes(b.Size), b.Location, ago(b.CreatedAt)})
				}
				c.ui.table([]string{"ID", "KIND", "METHOD", "SIZE", "LOCATION", "CREATED"}, rows)
			})
		},
	}

	var dlOut string
	download := &cobra.Command{
		Use:   s.use("download", "[id]"),
		Short: "Download a stored backup (default: the latest)",
		Args:  s.args(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, rest, err := s.split(c, args)
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			id := "latest"
			if len(rest) == 1 {
				id = rest[0]
			}
			dest := dlOut
			if dest == "" {
				dest = "."
			}
			file, err := c.downloadBackup(cmd, cl, name, id, dest)
			if err != nil {
				return err
			}
			return c.result(map[string]string{"app": name, "file": file}, func() {
				if file != "-" {
					c.ui.success("Saved to %s", file)
				}
			})
		},
	}
	download.Flags().StringVarP(&dlOut, "output", "o", "", "File or directory to write to (- for stdout; default: the current directory)")

	var delYes bool
	del := &cobra.Command{
		Use: s.use("delete", "<id>"), Aliases: []string{"rm"}, Short: "Delete a stored backup", Args: s.args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, rest, err := s.split(c, args)
			if err != nil {
				return err
			}
			if err := c.confirm(delYes, "Deleting backup "+rest[0], s.cmd()+" delete"+s.ref(name)+" "+rest[0]); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			if err := cl.BackupDelete(cmd.Context(), name, rest[0]); err != nil {
				return err
			}
			return c.result(map[string]string{"app": name, "deleted": rest[0]}, func() {
				c.ui.success("Backup %s deleted", rest[0])
			})
		},
	}
	del.Flags().BoolVarP(&delYes, "yes", "y", false, "Confirm the deletion")

	var (
		keep      int
		schedStop bool
	)
	schedule := &cobra.Command{
		Use:   s.use("schedule", "[cron|off]"),
		Short: "Show or set the automatic backup schedule",
		Long: `Schedule automatic backups with a five-field cron expression or @hourly,
@daily, @weekly, @monthly (UTC). "off" disables them. Without a schedule
argument, the current policy is shown. The app is not restarted.`,
		Example: fmt.Sprintf("  %[1]s schedule%[2]s @daily\n  %[1]s schedule%[2]s \"0 */6 * * *\" --keep 28\n  %[1]s schedule%[2]s off",
			s.cmd(), map[bool]string{true: " main", false: " -a uploads"}[s.db]),
		Args: s.args(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, rest, err := s.split(c, args)
			if err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			var policy *api.BackupPolicy
			if len(rest) == 0 {
				app, err := cl.App(cmd.Context(), name)
				if err != nil {
					return err
				}
				policy = app.Backup
				if policy == nil {
					policy = &api.BackupPolicy{}
				}
			} else if policy, err = cl.BackupPolicy(cmd.Context(), name, api.BackupPolicy{Schedule: rest[0], Keep: keep, Stop: schedStop}); err != nil {
				return err
			}
			return c.result(policy, func() {
				if policy.Schedule == "" {
					c.ui.printf("Automatic backups of %s are off. Enable them with `%s schedule%s @daily`.\n", name, s.cmd(), s.ref(name))
					return
				}
				stopped := ""
				if policy.Stop {
					stopped = ", stopping it meanwhile"
				}
				c.ui.success("%s is backed up on schedule %q (UTC), keeping the newest %d%s", c.ui.bold(name), policy.Schedule, policy.Keep, stopped)
			})
		},
	}
	schedule.Flags().IntVar(&keep, "keep", 0, "Number of scheduled backups to retain (default 7)")
	schedule.Flags().BoolVar(&schedStop, "stop", false, "Stop the app while its volumes are archived (requests wait)")
	backup.AddCommand(create, list, download, del, schedule)

	var (
		fromBackup string
		yes        bool
	)
	restore := &cobra.Command{
		Use:   s.use("restore", "[file]"),
		Short: "Restore from a stored backup or a local file",
		Long: `Replace an app's data with a backup.

The source is either a backup stored on the server (--backup <id>, or
"latest") or a local file (- reads stdin): a database dump, plain or
gzip-compressed, or a volume archive (.tar or .tar.gz with one top-level
directory per volume).

For a database dump, the database keeps running; for Postgres the restore is
a single transaction, so a failed one changes nothing. For volumes, the
archive is checked in full first, then the app is stopped, the volumes found
in the archive are emptied and unpacked, and the app is started again
(requests wait meanwhile). Volumes of the app that are not in the archive are
left alone. This command requires --yes.`,
		Example: fmt.Sprintf("  %[1]s --backup latest --yes\n  %[1]s ./backup.gz --yes", strings.TrimSuffix(map[bool]string{true: "saba db restore main", false: "saba backup restore -a uploads"}[s.db], " ")),
		Args:    s.args(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, rest, err := s.split(c, args)
			if err != nil {
				return err
			}
			restoreCmd := map[bool]string{true: "saba db restore", false: "saba backup restore"}[s.db] + s.ref(name)
			if (len(rest) == 1) == (fromBackup != "") {
				return api.Errorf(api.CodeInvalid, "give exactly one source: a file or --backup <id>").
					WithHint("Examples: `%s --backup latest --yes` or `%s ./backup.gz --yes`.", restoreCmd, restoreCmd)
			}
			retry := restoreCmd
			if fromBackup != "" {
				retry += " --backup " + fromBackup
			} else {
				retry += " " + rest[0]
			}
			var dump io.Reader
			if len(rest) == 1 {
				if rest[0] == "-" {
					dump = os.Stdin
				} else {
					f, err := os.Open(rest[0])
					if err != nil {
						return api.Errorf(api.CodeInvalid, "cannot read %s: %v", rest[0], err)
					}
					defer f.Close()
					dump = f
				}
			}
			if err := c.confirm(yes, "Restoring "+name+" (it replaces the current data)", retry); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			res, err := cl.Restore(cmd.Context(), name, fromBackup, dump)
			if err != nil {
				return err
			}
			return c.result(res, func() {
				src := ""
				switch {
				case res.Backup != nil:
					src = "backup " + res.Backup.ID
				case rest[0] == "-":
					src = "stdin"
				default:
					src = rest[0]
				}
				if len(res.Volumes) > 0 {
					c.ui.success("%s restored from %s (%s)", c.ui.bold(name), src, strings.Join(res.Volumes, ", "))
				} else {
					c.ui.success("%s restored from %s", c.ui.bold(name), src)
				}
				if len(res.Skipped) > 0 {
					c.ui.printf("%s Not restored, %s has no such volume: %s\n", c.ui.yellow("!"), name, strings.Join(res.Skipped, ", "))
				}
			})
		},
	}
	restore.Flags().StringVar(&fromBackup, "backup", "", "Restore this stored backup (an id, or \"latest\")")
	restore.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm replacing the data")
	return backup, restore
}

type backupResult struct {
	*api.Backup
	// SavedTo is the local path the backup was downloaded to (with -o).
	SavedTo string `json:"saved_to,omitempty"`
}

// downloadBackup writes a stored backup to dest: a file, a directory (the
// server's file name is used) or "-" for stdout. It returns the path written.
func (c *CLI) downloadBackup(cmd *cobra.Command, cl *client.Client, name, id, dest string) (string, error) {
	if dest == "-" {
		_, err := cl.BackupDownload(cmd.Context(), name, id, c.stdout)
		return "-", err
	}
	dir, final := filepath.Dir(dest), ""
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		dir = dest
	} else {
		final = dest
	}
	// Download under a temporary name: an interrupted transfer must not leave
	// behind something that looks like a complete backup.
	tmp, err := os.CreateTemp(dir, ".saba-backup-*")
	if err != nil {
		return "", api.Errorf(api.CodeInvalid, "cannot write to %s: %v", dir, err)
	}
	serverName, err := cl.BackupDownload(cmd.Context(), name, id, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if final == "" {
		if serverName == "" {
			serverName = fmt.Sprintf("%s-%s.gz", name, id)
		}
		final = filepath.Join(dir, name+"-"+serverName)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return final, nil
}

func (c *CLI) backupS3Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "s3",
		Short: "Mirror backups to S3-compatible storage",
		Long: `Send every database backup to a bucket as well: AWS S3, Cloudflare R2,
Backblaze B2, MinIO, RustFS or anything else that speaks the S3 API.

Once set:
  - new backups (manual and scheduled) are uploaded right after they are made,
    and existing ones are uploaded in the background
  - "list", "download" and "restore" also see backups that exist only in the
    bucket, so a replacement server can restore the old server's backups
  - retention (--keep) and "delete" apply to the bucket too
  - "saba doctor" reports failed uploads

Objects are stored as <prefix>/<database>/<backup-id>.<format>.gz.`,
		Example: `  # AWS: endpoint is derived from the region; keys come from the environment
  AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... saba db backup s3 set --bucket my-backups --region eu-central-1

  # Cloudflare R2
  saba db backup s3 set --bucket my-backups --endpoint https://<account>.r2.cloudflarestorage.com \
      --region auto --access-key-id <id> --secret-access-key -      # secret read from stdin

  saba db backup s3 show
  saba db backup s3 remove`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.backupS3Show(cmd) },
	}
	show := &cobra.Command{
		Use: "show", Short: "Show the configured bucket", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.backupS3Show(cmd) },
	}

	var (
		cfg          api.S3Config
		pathStyle    bool
		createBucket bool
	)
	set := &cobra.Command{
		Use:   "set --bucket <name>",
		Short: "Set (and verify) the bucket backups are uploaded to",
		Long: `Save the S3 destination. The server first proves the settings work by
writing, reading, listing and deleting a small object; nothing is saved if
that fails.

The access key and secret default to AWS_ACCESS_KEY_ID and
AWS_SECRET_ACCESS_KEY from this machine's environment. Pass "-" as the secret
to read it from stdin.`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfg.Bucket == "" {
				return api.Errorf(api.CodeInvalid, "--bucket is required").
					WithHint("Example: `saba db backup s3 set --bucket my-backups --region eu-central-1`.")
			}
			if cfg.AccessKeyID == "" {
				cfg.AccessKeyID = os.Getenv("AWS_ACCESS_KEY_ID")
			}
			switch cfg.SecretAccessKey {
			case "":
				cfg.SecretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
			case "-":
				b, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
				if err != nil {
					return err
				}
				cfg.SecretAccessKey = strings.TrimSpace(string(b))
			}
			if cmd.Flags().Changed("path-style") {
				cfg.PathStyle = &pathStyle
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			saved, err := cl.SetBackupStorage(cmd.Context(), cfg, createBucket)
			if err != nil {
				return err
			}
			return c.result(saved, func() {
				c.ui.success("Backups are mirrored to %s", c.ui.bold(s3URL(saved)))
				c.ui.note("Verified: write, read, list and delete all work. Existing backups are being uploaded in the background.")
			})
		},
	}
	fl := set.Flags()
	fl.StringVar(&cfg.Bucket, "bucket", "", "Bucket name")
	fl.StringVar(&cfg.Endpoint, "endpoint", "", "Service URL (default: AWS S3 in --region)")
	fl.StringVar(&cfg.Region, "region", "", "Region (default us-east-1; often \"auto\" for non-AWS services)")
	fl.StringVar(&cfg.Prefix, "prefix", "", "Key prefix inside the bucket, e.g. saba/prod")
	fl.StringVar(&cfg.AccessKeyID, "access-key-id", "", "Access key id (default: $AWS_ACCESS_KEY_ID)")
	fl.StringVar(&cfg.SecretAccessKey, "secret-access-key", "", "Secret access key, or - to read it from stdin (default: $AWS_SECRET_ACCESS_KEY)")
	fl.BoolVar(&pathStyle, "path-style", false, "Force path-style (true) or virtual-hosted (false) addressing; default: automatic")
	fl.BoolVar(&createBucket, "create-bucket", false, "Create the bucket if it does not exist")

	remove := &cobra.Command{
		Use: "remove", Aliases: []string{"rm"}, Short: "Stop uploading backups (objects in the bucket are kept)", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			if err := cl.RemoveBackupStorage(cmd.Context()); err != nil {
				return err
			}
			return c.result(map[string]bool{"removed": true}, func() {
				c.ui.success("Backups are no longer uploaded")
				c.ui.note("Nothing was deleted from the bucket.")
			})
		},
	}
	cmd.AddCommand(show, set, remove)
	return cmd
}

func s3URL(cfg *api.S3Config) string {
	u := "s3://" + cfg.Bucket
	if cfg.Prefix != "" {
		u += "/" + strings.Trim(cfg.Prefix, "/")
	}
	if cfg.Endpoint != "" {
		u += " (" + cfg.Endpoint + ")"
	}
	return u
}

func (c *CLI) backupS3Show(cmd *cobra.Command) error {
	cl, err := c.api()
	if err != nil {
		return err
	}
	cfg, err := cl.BackupStorage(cmd.Context())
	if err != nil {
		return err
	}
	return c.result(cfg, func() {
		if cfg.Bucket == "" {
			c.ui.printf("Backups are stored on the server only. Mirror them to a bucket with `saba db backup s3 set --bucket <name>`.\n")
			return
		}
		field := func(k, v string) { c.ui.printf("%s %s\n", c.ui.dim(pad(k, 12)), v) }
		field("Bucket", cfg.Bucket)
		field("Endpoint", orDash(cfg.Endpoint))
		field("Region", orDash(cfg.Region))
		field("Prefix", orDash(cfg.Prefix))
		field("Access key", cfg.AccessKeyID)
	})
}
