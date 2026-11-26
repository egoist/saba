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

// dbBackupCmds returns the `db backup` group and `db restore`.
func (c *CLI) dbBackupCmds() []*cobra.Command {
	backup := &cobra.Command{
		Use:   "backup",
		Short: "Create, schedule, download and delete database backups",
		Long: `Backups are compressed dumps (pg_dump, mysqldump, mariadb-dump, mongodump)
stored on the server in /var/lib/saba/backups/<database>/.

Scheduled backups are pruned to the newest --keep; manual ones are kept until
you delete them.

Configure S3-compatible storage once ("saba db backup s3 set") and every
backup is also uploaded there. Listing, downloading and restoring then work
from the bucket too, so the backups of a lost server are available on its
replacement.`,
		Example: `  saba db backup create main                    # dump now, keep it on the server
  saba db backup create main -o main.sql.gz     # ...and download it
  saba db backup schedule main @daily --keep 14 # automatic backups (UTC)
  saba db backup schedule main off
  saba db backup list main
  saba db backup download main latest
  saba db restore main --backup latest --yes
  saba db restore main ./main.sql.gz --yes
  saba db backup s3 set --bucket my-backups --region eu-central-1`,
	}

	var out string
	create := &cobra.Command{
		Use:   "create <database>",
		Short: "Back up a database now",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			b, err := cl.BackupCreate(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			file := ""
			if out != "" {
				if file, err = c.downloadBackup(cmd, cl, args[0], b.ID, out); err != nil {
					return err
				}
			}
			return c.result(backupResult{Backup: b, SavedTo: file}, func() {
				// With `-o -` the dump itself is on stdout; keep messages off it.
				ui := c.ui
				if out == "-" {
					ui = newUI(c.stderr, c.noColor)
				}
				ui.success("Backup %s of %s created (%s, %s)", b.ID, ui.bold(args[0]), humanBytes(b.Size), b.Location)
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
					ui.note("Stored %s. Download it with `saba db backup download %s %s`.", where, args[0], b.ID)
				}
			})
		},
	}
	create.Flags().StringVarP(&out, "output", "o", "", "Also download the backup to this file or directory (- for stdout)")

	list := &cobra.Command{
		Use: "list <database>", Aliases: []string{"ls"}, Short: "List stored backups", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			backups, err := cl.Backups(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if backups == nil {
				backups = []api.Backup{}
			}
			return c.result(backups, func() {
				if len(backups) == 0 {
					c.ui.printf("%s has no backups. Create one with `saba db backup create %s`.\n", args[0], args[0])
					return
				}
				rows := make([][]string, 0, len(backups))
				for _, b := range backups {
					rows = append(rows, []string{b.ID, b.Kind, humanBytes(b.Size), b.Location, ago(b.CreatedAt)})
				}
				c.ui.table([]string{"ID", "KIND", "SIZE", "LOCATION", "CREATED"}, rows)
			})
		},
	}

	var dlOut string
	download := &cobra.Command{
		Use:   "download <database> [id]",
		Short: "Download a stored backup (default: the latest)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			id := "latest"
			if len(args) == 2 {
				id = args[1]
			}
			dest := dlOut
			if dest == "" {
				dest = "."
			}
			file, err := c.downloadBackup(cmd, cl, args[0], id, dest)
			if err != nil {
				return err
			}
			return c.result(map[string]string{"database": args[0], "file": file}, func() {
				if file != "-" {
					c.ui.success("Saved to %s", file)
				}
			})
		},
	}
	download.Flags().StringVarP(&dlOut, "output", "o", "", "File or directory to write to (- for stdout; default: the current directory)")

	var delYes bool
	del := &cobra.Command{
		Use: "delete <database> <id>", Aliases: []string{"rm"}, Short: "Delete a stored backup", Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.confirm(delYes, "Deleting backup "+args[1], "saba db backup delete "+args[0]+" "+args[1]); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			if err := cl.BackupDelete(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			return c.result(map[string]string{"database": args[0], "deleted": args[1]}, func() {
				c.ui.success("Backup %s deleted", args[1])
			})
		},
	}
	del.Flags().BoolVarP(&delYes, "yes", "y", false, "Confirm the deletion")

	var keep int
	schedule := &cobra.Command{
		Use:   "schedule <database> [cron|off]",
		Short: "Show or set the automatic backup schedule",
		Long: `Schedule automatic backups with a five-field cron expression or @hourly,
@daily, @weekly, @monthly (UTC). "off" disables them. Without a schedule
argument, the current policy is shown. The database is not restarted.`,
		Example: "  saba db backup schedule main @daily\n  saba db backup schedule main \"0 */6 * * *\" --keep 28\n  saba db backup schedule main off",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			var policy *api.BackupPolicy
			if len(args) == 1 {
				app, err := cl.App(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if app.Database == nil {
					return api.Errorf(api.CodeInvalid, "%q is not a managed database", args[0])
				}
				policy = app.Database.Backup
				if policy == nil {
					policy = &api.BackupPolicy{}
				}
			} else if policy, err = cl.BackupPolicy(cmd.Context(), args[0], api.BackupPolicy{Schedule: args[1], Keep: keep}); err != nil {
				return err
			}
			return c.result(policy, func() {
				if policy.Schedule == "" {
					c.ui.printf("Automatic backups of %s are off. Enable them with `saba db backup schedule %s @daily`.\n", args[0], args[0])
					return
				}
				c.ui.success("%s is backed up on schedule %q (UTC), keeping the newest %d", c.ui.bold(args[0]), policy.Schedule, policy.Keep)
			})
		},
	}
	schedule.Flags().IntVar(&keep, "keep", 0, "Number of scheduled backups to retain (default 7)")
	backup.AddCommand(create, list, download, del, schedule, c.backupS3Cmd())

	var (
		fromBackup string
		yes        bool
	)
	restore := &cobra.Command{
		Use:   "restore <database> [file]",
		Short: "Restore a database from a stored backup or a dump file",
		Long: `Load a backup into a running database, replacing the data it contains.

The source is either a backup stored on the server (--backup <id>, or
"latest") or a local dump file, plain or gzip-compressed (- reads stdin).

For Postgres the restore runs in a single transaction: if it fails, the
database is left unchanged. This command requires --yes.`,
		Example: "  saba db restore main --backup latest --yes\n  saba db restore main ./main.sql.gz --yes\n  pg_dump ... | saba db restore main - --yes",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if (len(args) == 2) == (fromBackup != "") {
				return api.Errorf(api.CodeInvalid, "give exactly one source: a dump file or --backup <id>").
					WithHint("Examples: `saba db restore %s --backup latest --yes` or `saba db restore %s ./dump.sql.gz --yes`.", name, name)
			}
			retry := "saba db restore " + name
			if fromBackup != "" {
				retry += " --backup " + fromBackup
			} else {
				retry += " " + args[1]
			}
			var dump io.Reader
			if len(args) == 2 {
				if args[1] == "-" {
					dump = os.Stdin
				} else {
					f, err := os.Open(args[1])
					if err != nil {
						return api.Errorf(api.CodeInvalid, "cannot read %s: %v", args[1], err)
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
				if res.Backup != nil {
					c.ui.success("%s restored from backup %s", c.ui.bold(name), res.Backup.ID)
				} else {
					src := args[1]
					if src == "-" {
						src = "stdin"
					}
					c.ui.success("%s restored from %s", c.ui.bold(name), src)
				}
			})
		},
	}
	restore.Flags().StringVar(&fromBackup, "backup", "", "Restore this stored backup (an id, or \"latest\")")
	restore.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm replacing the database's data")
	return []*cobra.Command{backup, restore}
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
