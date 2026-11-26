package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestToolDefinitionsAreConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range tools() {
		if seen[tl.Name] || !strings.HasPrefix(tl.Name, "saba_") || tl.Description == "" {
			t.Errorf("bad tool definition: %q", tl.Name)
		}
		seen[tl.Name] = true
		for _, r := range tl.Required {
			if _, ok := tl.Props[r]; !ok {
				t.Errorf("%s: required argument %q is not declared", tl.Name, r)
			}
		}
		if tl.Args == nil && tl.Name != "saba_guide" {
			t.Errorf("%s has no implementation", tl.Name)
		}
	}
}

func TestProtocol(t *testing.T) {
	// `echo` stands in for the saba binary: the tool result is the argv it would run.
	s := &Server{Exe: "echo", GlobalArgs: []string{"--context", "prod"}, Version: "test", Guide: "THE GUIDE"}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"saba_deploy","arguments":{"app":"web","image":"nginx","port":80,"env":{"B":"2","A":"x y"},"domains":["a.com"]}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"saba_destroy","arguments":{"app":"web","confirm":false}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"saba_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"saba_guide"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`,
		`not json`,
	}, "\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	type reply struct {
		ID     json.RawMessage
		Result struct {
			ProtocolVersion string
			Tools           []map[string]any
			Content         []struct{ Text string }
			IsError         bool
		}
		Error *rpcError
	}
	replies := map[string]reply{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r reply
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("invalid reply %q: %v", line, err)
		}
		replies[string(r.ID)] = r
	}
	if len(replies) != 8 {
		t.Fatalf("expected 8 replies (none for the notification), got %d:\n%s", len(replies), out.String())
	}
	if replies["1"].Result.ProtocolVersion != "2025-03-26" {
		t.Errorf("initialize should echo the client's protocol version: %+v", replies["1"])
	}
	if len(replies["2"].Result.Tools) != len(tools()) {
		t.Errorf("tools/list returned %d tools", len(replies["2"].Result.Tools))
	}
	deploy := replies["3"].Result
	want := "--context prod --json deploy --quiet"
	if deploy.IsError || !strings.HasPrefix(deploy.Content[0].Text, want) {
		t.Errorf("deploy argv = %q", deploy.Content[0].Text)
	}
	for _, part := range []string{"-a web", "--image nginx", "--port 80", "--domain a.com", "--env A=x y --env B=2"} {
		if !strings.Contains(deploy.Content[0].Text, part) {
			t.Errorf("deploy argv lacks %q: %q", part, deploy.Content[0].Text)
		}
	}
	if r := replies["4"].Result; !r.IsError || !strings.Contains(r.Content[0].Text, "confirm") {
		t.Errorf("destroy without confirmation must be refused: %+v", r)
	}
	if r := replies["5"].Result; !r.IsError || !strings.Contains(r.Content[0].Text, `"app"`) {
		t.Errorf("missing required argument should be reported: %+v", r)
	}
	if r := replies["6"].Result; r.IsError || r.Content[0].Text != "THE GUIDE" {
		t.Errorf("guide: %+v", r)
	}
	if replies["7"].Error == nil || replies["7"].Error.Code != -32601 {
		t.Errorf("unknown method should be a JSON-RPC error: %+v", replies["7"])
	}
	if replies["null"].Error == nil || replies["null"].Error.Code != -32700 {
		t.Errorf("malformed input should be a parse error: %+v", replies["null"])
	}
}
