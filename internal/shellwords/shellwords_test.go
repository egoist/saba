package shellwords

import (
	"reflect"
	"testing"
)

func TestCommand(t *testing.T) {
	cases := map[string][]string{
		"":                                 nil,
		"node server.js":                   {"node", "server.js"},
		`redis-server --requirepass "a b"`: {"redis-server", "--requirepass", "a b"},
		`echo 'it''s'`:                     {"echo", "its"},
		`run a\ b`:                         {"run", "a b"},
		"npm run migrate && npm start":     {"sh", "-c", "npm run migrate && npm start"},
		"echo $HOME":                       {"sh", "-c", "echo $HOME"},
	}
	for in, want := range cases {
		got, err := Command(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Command(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Split(`echo "unterminated`); err == nil {
		t.Error("unterminated quote should fail")
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	args := []string{"docker", "exec", "-it", "c", "sh", "-c", `echo "hi" 'there' $x; ls`, ""}
	got, err := Split(Join(args))
	if err != nil || !reflect.DeepEqual(got, args) {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}
