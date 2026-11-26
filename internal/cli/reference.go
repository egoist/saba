package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Reference renders the complete command reference as Markdown. It is the
// source of docs/cli-reference.md; a test fails when that file is stale.
func Reference() string {
	c := &CLI{}
	root := c.rootCmd()
	root.InitDefaultHelpCmd()

	var cmds []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			cmds = append(cmds, sub)
			walk(sub)
		}
	}
	walk(root)

	var b strings.Builder
	b.WriteString("<!-- Generated from the command definitions by `make docs`. Do not edit by hand. -->\n\n")
	b.WriteString("# CLI reference\n\n")
	b.WriteString("Every command saba has, with all of its flags. For a guided tour, start with the\n")
	b.WriteString("[getting started guide](getting-started.md); for a compact overview, run `saba guide`.\n\n")

	b.WriteString("## Global flags\n\n")
	b.WriteString("These work with every command.\n\n")
	flagTable(&b, root.PersistentFlags())

	b.WriteString("## Commands\n\n")
	for _, cmd := range cmds {
		depth := strings.Count(cmd.CommandPath(), " ") - 1
		fmt.Fprintf(&b, "%s- [`%s`](#%s) — %s\n", strings.Repeat("  ", depth), cmd.CommandPath(), anchor(cmd.CommandPath()), inline(cmd.Short))
	}
	b.WriteString("\n")

	for _, cmd := range cmds {
		fmt.Fprintf(&b, "## %s\n\n", cmd.CommandPath())
		fmt.Fprintf(&b, "%s\n\n", inline(cmd.Short))
		if cmd.Runnable() {
			fmt.Fprintf(&b, "```sh\n%s\n```\n\n", cmd.UseLine())
		}
		if long := strings.TrimSpace(cmd.Long); long != "" && long != cmd.Short {
			fmt.Fprintf(&b, "```text\n%s\n```\n\n", long)
		}
		if len(cmd.Aliases) > 0 {
			fmt.Fprintf(&b, "Aliases: `%s`\n\n", strings.Join(cmd.Aliases, "`, `"))
		}
		if ex := strings.TrimRight(cmd.Example, " \n"); ex != "" {
			fmt.Fprintf(&b, "**Examples**\n\n```sh\n%s\n```\n\n", dedent(ex))
		}
		if hasVisibleFlags(cmd.NonInheritedFlags()) {
			b.WriteString("**Flags**\n\n")
			flagTable(&b, cmd.NonInheritedFlags())
		}
		var subs []string
		for _, sub := range cmd.Commands() {
			if !sub.Hidden && sub.Name() != "help" {
				subs = append(subs, fmt.Sprintf("[`%s`](#%s)", sub.Name(), anchor(sub.CommandPath())))
			}
		}
		if len(subs) > 0 {
			fmt.Fprintf(&b, "Subcommands: %s\n\n", strings.Join(subs, ", "))
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func hasVisibleFlags(fs *pflag.FlagSet) bool {
	visible := false
	fs.VisitAll(func(f *pflag.Flag) {
		if !f.Hidden && f.Name != "help" {
			visible = true
		}
	})
	return visible
}

func flagTable(b *strings.Builder, fs *pflag.FlagSet) {
	b.WriteString("| Flag | Default | Description |\n|---|---|---|\n")
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		name := "--" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", " + name
		}
		if t := f.Value.Type(); t != "bool" {
			name += " " + strings.TrimSuffix(t, "Array")
		}
		def := ""
		switch f.DefValue {
		case "", "false", "0", "[]":
		default:
			def = "`" + cell(f.DefValue) + "`"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s |\n", cell(name), def, cell(inline(f.Usage)))
	})
	b.WriteString("\n")
}

// anchor reproduces GitHub's heading anchors for command paths.
func anchor(s string) string { return strings.ReplaceAll(strings.ToLower(s), " ", "-") }

// inline makes free text safe in Markdown prose: <placeholders> would
// otherwise be read as HTML tags.
func inline(s string) string {
	return strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(s)
}

func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// dedent removes the indentation shared by all non-empty lines.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	common := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " "))
		if common < 0 || n < common {
			common = n
		}
	}
	for i, l := range lines {
		if len(l) >= common && common > 0 {
			lines[i] = l[common:]
		}
	}
	return strings.Join(lines, "\n")
}
