package cli

import (
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/egoist/saba/internal/api"
)

func (c *CLI) certsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "certs",
		Aliases: []string{"cert", "certificates"},
		Short:   "Wildcard and custom TLS certificates",
		Long: `Every routed domain gets a Let's Encrypt certificate automatically, on its
first request. This command is for the two cases that needs more:

Wildcard certificates. Let's Encrypt only issues them through the DNS-01
challenge, which requires access to the domain's DNS. Give saba that access
once and it obtains and renews a wildcard certificate for the server's base
domain and for every wildcard domain routed to an app:

  saba server config set cloudflare_api_token -     # token with Zone:Read + DNS:Edit, read from stdin
  saba server config set dns_hook /usr/local/bin/my-dns-hook    # any other DNS host

  The hook is called as "<hook> present|cleanup <record-name> <value>" and
  must create or remove that TXT record.

Your own certificates. A certificate from another CA (or a Cloudflare origin
certificate) is used for the names it lists instead of an issued one:

  saba certs add --cert fullchain.pem --key privkey.pem`,
		Example: "  saba certs\n  saba certs add --cert fullchain.pem --key privkey.pem\n  saba certs remove custom-example.com --yes",
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
	cmd.AddCommand(list, add, remove)
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
			c.ui.printf("No wildcard or custom certificates. Regular domains get theirs automatically on first use.\n")
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
