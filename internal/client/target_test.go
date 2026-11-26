package client

import (
	"strings"
	"testing"
)

// The hint must match the actual cause; these are the messages ssh and
// `saba dial-stdio` print on the server side.
func TestUnreachableHints(t *testing.T) {
	remote := Target{Name: "prod", SSH: "deploy@example.com"}
	cases := map[string]string{
		"saba: permission denied on /run/saba/saba.sock: this user must be root or a member of the docker group":                                     "docker group",
		"saba: the daemon is not running (cannot connect to /run/saba/saba.sock: dial unix /run/saba/saba.sock: connect: no such file or directory)": "systemctl status saba",
		"bash: line 1: exec: /usr/local/bin/saba: not found\nsh: saba: command not found":                                                            "not installed",
		"deploy@example.com: Permission denied (publickey).":                                                                                         "SSH authentication failed",
		"Host key verification failed.": "host key",
	}
	for output, want := range cases {
		if hint := unreachable(remote, output).Hint; !strings.Contains(hint, want) {
			t.Errorf("for %q the hint is %q; want it to mention %q", output, hint, want)
		}
	}
}
