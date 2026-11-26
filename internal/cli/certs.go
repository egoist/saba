package cli

import (
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
)

func (c *CLI) certsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "certs",
		Aliases: []string{"cert", "certificates"},
		Short:   "TLS certificates: status, wildcards through DNS, your own",
		Long: `Every routed domain gets a Let's Encrypt certificate automatically, on its
first HTTPS request, and saba renews it. ` + "`saba certs`" + ` shows them all.

Wildcard certificates. Let's Encrypt only issues them through the DNS-01
challenge, which requires access to the domain's DNS. Give saba that access
once and it obtains and renews a wildcard certificate for the server's base
domain and for every wildcard domain routed to an app:

  saba certs dns providers                              # supported DNS hosts and their settings
  echo "$CF_TOKEN" | saba certs dns set cloudflare api_token=-

Your own certificates. A certificate from another CA (or a Cloudflare origin
certificate) is used for the names it lists instead of an issued one:

  saba certs add --cert fullchain.pem --key privkey.pem`,
		Example: "  saba certs\n  saba certs dns set hetzner auth_api_token=-\n  saba certs add --cert fullchain.pem --key privkey.pem\n  saba certs remove custom-example.com --yes",
		Args:    maxArgs(0),
		RunE:    func(cmd *cobra.Command, args []string) error { return c.certsList(cmd) },
	}
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Short: "List stored and pending certificates", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error { return c.certsList(cmd) },
	}

	var certFile, keyFile string
	add := &cobra.Command{
		Use:   "add --cert <file> --key <file>",
		Short: "Upload a certificate to use for the names it lists",
		Args:  maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if certFile == "" || keyFile == "" {
				return api.Errorf(api.CodeInvalid, "both --cert and --key are required").
					WithHint("Example: `saba certs add --cert fullchain.pem --key privkey.pem`.")
			}
			certPEM, err := os.ReadFile(certFile)
			if err != nil {
				return api.Errorf(api.CodeInvalid, "cannot read %s: %v", certFile, err)
			}
			keyPEM, err := os.ReadFile(keyFile)
			if err != nil {
				return api.Errorf(api.CodeInvalid, "cannot read %s: %v", keyFile, err)
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			cert, err := cl.CertAdd(cmd.Context(), string(certPEM), string(keyPEM))
			if err != nil {
				return err
			}
			return c.result(cert, func() {
				c.ui.success("Certificate %s stored for %s (expires %s)", c.ui.bold(cert.ID), strings.Join(cert.Names, ", "), cert.NotAfter.Format("2006-01-02"))
				if cert.Status == "expired" {
					c.ui.printf("%s It has already expired and will not be used.\n", c.ui.red("!"))
				}
			})
		},
	}
	add.Flags().StringVar(&certFile, "cert", "", "PEM file with the certificate chain, leaf first")
	add.Flags().StringVar(&keyFile, "key", "", "PEM file with the private key")

	var yes bool
	remove := &cobra.Command{
		Use: "remove <id>", Aliases: []string{"rm"}, Short: "Delete a stored certificate", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.confirm(yes, "Removing certificate "+args[0], "saba certs remove "+args[0]); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			if err := cl.CertRemove(cmd.Context(), args[0]); err != nil {
				return err
			}
			return c.result(map[string]string{"removed": args[0]}, func() { c.ui.success("Certificate %s removed", args[0]) })
		},
	}
	remove.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm the removal")
	cmd.AddCommand(list, add, remove, c.certsDNSCmd())
	return cmd
}

func (c *CLI) certsDNSCmd() *cobra.Command {
	show := func(cmd *cobra.Command, cfg *api.DNSConfig) error {
		return c.result(cfg, func() {
			if cfg.Provider == "" {
				c.ui.printf("No DNS provider. Wildcard domains get a certificate per subdomain instead, on first use.\n")
				c.ui.note("Configure one with `saba certs dns set <provider> key=value...`; `saba certs dns providers` lists them.")
				return
			}
			c.ui.printf("provider = %s\n", cfg.Provider)
			keys := make([]string, 0, len(cfg.Settings))
			for k := range cfg.Settings {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				c.ui.printf("%s = %s\n", k, cfg.Settings[k])
			}
		})
	}
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Show the DNS provider used for wildcard certificates",
		Long: `saba obtains wildcard certificates through the ACME DNS-01 challenge, by
creating a TXT record at your DNS host. This configures which host and the
credentials for it. The record is created only during issuance and removed
right after.

Credentials are stored on the server and never shown again; a value of "-"
reads it from stdin to keep it out of shell history.`,
		Example: `  saba certs dns
  saba certs dns providers
  echo "$CF_TOKEN" | saba certs dns set cloudflare api_token=-
  saba certs dns set porkbun api_key=pk1_... api_secret_key=-
  saba certs dns set hook path=/usr/local/bin/dns-hook
  saba certs dns remove --yes`,
		Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			cfg, err := cl.DNS(cmd.Context())
			if err != nil {
				return err
			}
			return show(cmd, cfg)
		},
	}
	set := &cobra.Command{
		Use:   "set <provider> [key=value...]",
		Short: "Configure the DNS provider (settings are checked before saving)",
		Long: `Configure the DNS provider. Settings not given keep their current value when
the provider stays the same, so a token can be rotated alone.

The hook provider runs an executable on the server for any DNS host without
built-in support, as "<path> present|cleanup <record-name> <value>"; it must
create or remove that TXT record and exit 0.`,
		Args: minArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := api.DNSConfig{Provider: args[0], Settings: map[string]string{}}
			stdinUsed := false
			for _, kv := range args[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return api.Errorf(api.CodeInvalid, "%q is not key=value", kv).
						WithHint("Example: `saba certs dns set cloudflare api_token=-`.")
				}
				if v == "-" {
					if stdinUsed {
						return api.Errorf(api.CodeInvalid, "only one setting can be read from stdin")
					}
					stdinUsed = true
					b, err := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
					if err != nil {
						return err
					}
					v = strings.TrimSpace(string(b))
				}
				cfg.Settings[k] = v
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			out, err := cl.DNSSet(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			return c.result(out, func() {
				c.ui.success("DNS provider set to %s", out.Provider)
				c.ui.note("Wildcard certificates are being obtained; follow them with `saba certs`.")
			})
		},
	}
	providers := &cobra.Command{
		Use: "providers", Short: "List supported DNS hosts and their settings", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := c.api()
			if err != nil {
				return err
			}
			list, err := cl.DNSProviders(cmd.Context())
			if err != nil {
				return err
			}
			return c.result(list, func() {
				rows := make([][]string, 0, len(list))
				for _, p := range list {
					var settings []string
					for _, s := range p.Settings {
						name := s.Name
						if s.Secret {
							name += c.ui.dim("*")
						}
						settings = append(settings, name)
					}
					rows = append(rows, []string{p.Name, p.Title, strings.Join(settings, " ")})
				}
				c.ui.table([]string{"PROVIDER", "DNS HOST", "SETTINGS (* = secret)"}, rows)
			})
		},
	}
	var yes bool
	remove := &cobra.Command{
		Use: "remove", Aliases: []string{"rm"}, Short: "Forget the DNS provider and its credentials", Args: maxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.confirm(yes, "Removing the DNS provider (wildcard certificates will no longer renew)", "saba certs dns remove"); err != nil {
				return err
			}
			cl, err := c.api()
			if err != nil {
				return err
			}
			if err := cl.DNSRemove(cmd.Context()); err != nil {
				return err
			}
			return c.result(map[string]bool{"removed": true}, func() { c.ui.success("DNS provider removed") })
		},
	}
	remove.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm the removal")
	cmd.AddCommand(set, providers, remove)
	return cmd
}

func (c *CLI) certsList(cmd *cobra.Command) error {
	cl, err := c.api()
	if err != nil {
		return err
	}
	certs, err := cl.Certs(cmd.Context())
	if err != nil {
		return err
	}
	if certs == nil {
		certs = []api.Certificate{}
	}
	return c.result(certs, func() {
		if len(certs) == 0 {
			c.ui.printf("No certificates yet. Each routed domain gets one on its first HTTPS request.\n")
			c.ui.note("See `saba certs --help` for wildcard certificates and uploading your own.")
			return
		}
		rows := make([][]string, 0, len(certs))
		for _, ct := range certs {
			expires := "-"
			if !ct.NotAfter.IsZero() {
				days := int(time.Until(ct.NotAfter).Hours() / 24)
				expires = ct.NotAfter.Format("2006-01-02") + c.ui.dim(" ("+plural(days, "day")+")")
			}
			status := ct.Status
			switch ct.Status {
			case "active":
				status = c.ui.green(status)
			case "failed", "expired":
				status = c.ui.red(status)
			default:
				status = c.ui.yellow(status)
			}
			rows = append(rows, []string{orDash(ct.ID), strings.Join(ct.Names, ", "), ct.Source, status, expires, orDash(ct.Issuer)})
		}
		c.ui.table([]string{"ID", "NAMES", "SOURCE", "STATUS", "EXPIRES", "ISSUER"}, rows)
		for _, ct := range certs {
			if ct.Error != "" {
				c.ui.printf("%s %s: %s\n", c.ui.yellow("!"), strings.Join(ct.Names, ", "), ct.Error)
			}
		}
	})
}
