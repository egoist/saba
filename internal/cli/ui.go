package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/egoist/saba/internal/api"
)

// UI renders human output. Colors are used only on terminals.
type UI struct {
	w     io.Writer
	color bool
}

func newUI(w io.Writer, noColor bool) *UI {
	color := !noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	if f, ok := w.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		color = false
	}
	if os.Getenv("FORCE_COLOR") != "" && !noColor {
		color = true
	}
	return &UI{w: w, color: color}
}

func (u *UI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *UI) bold(s string) string   { return u.paint("1", s) }
func (u *UI) dim(s string) string    { return u.paint("2", s) }
func (u *UI) red(s string) string    { return u.paint("31", s) }
func (u *UI) green(s string) string  { return u.paint("32", s) }
func (u *UI) yellow(s string) string { return u.paint("33", s) }
func (u *UI) cyan(s string) string   { return u.paint("36", s) }

func (u *UI) printf(format string, args ...any) { fmt.Fprintf(u.w, format, args...) }

func (u *UI) success(format string, args ...any) {
	fmt.Fprintf(u.w, "%s %s\n", u.green("✓"), fmt.Sprintf(format, args...))
}

func (u *UI) note(format string, args ...any) {
	fmt.Fprintf(u.w, "  %s\n", u.dim(fmt.Sprintf(format, args...)))
}

// status colors a status word.
func (u *UI) status(s string) string {
	switch s {
	case api.AppRunning, api.StatusLive, "succeeded", "healthy", "ok":
		return u.green(s)
	case api.AppDeploying, api.StatusBuilding, api.AppDegraded, "restarting", "starting":
		return u.yellow(s)
	case api.AppCrashed, api.AppFailed, "exited", "dead", "error", "unhealthy":
		return u.red(s)
	default:
		return u.dim(s)
	}
}

// table prints rows in aligned columns. Cells may contain ANSI colors.
func (u *UI) table(header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, cell := range r {
			if i < len(widths) {
				if n := visibleLen(cell); n > widths[i] {
					widths[i] = n
				}
			}
		}
	}
	line := func(cells []string, style func(string) string) {
		var b strings.Builder
		for i, cell := range cells {
			if i >= len(widths) {
				break
			}
			text := cell
			if style != nil {
				text = style(cell)
			}
			b.WriteString(text)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-visibleLen(cell)+2))
			}
		}
		fmt.Fprintln(u.w, strings.TrimRight(b.String(), " "))
	}
	line(header, u.dim)
	for _, r := range rows {
		line(r, nil)
	}
}

// visibleLen is the printed width of s, ignoring ANSI escape sequences.
func visibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			n++
		}
	}
	return n
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func millis(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
