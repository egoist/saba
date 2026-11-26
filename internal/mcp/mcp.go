// Package mcp implements a minimal Model Context Protocol server (stdio
// transport, JSON-RPC 2.0) that exposes saba's commands as tools.
//
// Every tool call runs the saba CLI itself with --json, so MCP clients get
// exactly the behaviour, validation and structured errors of the CLI.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Server struct {
	// Exe is the saba binary to run for tool calls.
	Exe string
	// GlobalArgs are prepended to every invocation (e.g. --context prod).
	GlobalArgs []string
	Version    string
	Guide      string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type schema map[string]any

type tool struct {
	Name        string
	Description string
	Props       schema
	Required    []string
	ReadOnly    bool
	Destructive bool
	// Plain tools return the CLI's human output (compact) instead of JSON.
	Plain bool
	// Args builds the CLI arguments for a call.
	Args func(in map[string]any) ([]string, error)
}

func str(desc string) schema  { return schema{"type": "string", "description": desc} }
func num(desc string) schema  { return schema{"type": "integer", "description": desc} }
func flag(desc string) schema { return schema{"type": "boolean", "description": desc} }
func strs(desc string) schema {
	return schema{"type": "array", "items": schema{"type": "string"}, "description": desc}
}
func strMap(desc string) schema {
	return schema{"type": "object", "additionalProperties": schema{"type": "string"}, "description": desc}
}

const appDesc = "App name"

func getStr(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return s
}

func getInt(in map[string]any, k string) (int, bool) {
	switch v := in[k].(type) {
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	return 0, false
}

func getBool(in map[string]any, k string) bool {
	b, _ := in[k].(bool)
	return b
}

func getStrs(in map[string]any, k string) []string {
	arr, _ := in[k].([]any)
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func getMap(in map[string]any, k string) []string {
	m, _ := in[k].(map[string]any)
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, key := range keys {
		out = append(out, key+"="+fmt.Sprint(m[key]))
	}
	return out
}

func appArgs(base ...string) func(map[string]any) ([]string, error) {
	return func(in map[string]any) ([]string, error) {
		return append(base, "-a", getStr(in, "app")), nil
	}
}

func tools() []tool {
	return []tool{
		{
			Name: "saba_guide", ReadOnly: true,
			Description: "Read the complete saba reference: concepts, commands, saba.toml schema, JSON shapes, error codes. Read this first if you are unsure how something works.",
			Props:       schema{},
		},
		{
			Name: "saba_apps", ReadOnly: true,
			Description: "List all apps and databases on the server with status, live version and URLs.",
			Props:       schema{},
			Args:        func(map[string]any) ([]string, error) { return []string{"apps"}, nil },
		},
		{
			Name: "saba_status", ReadOnly: true,
			Description: "Detailed state of one app: status, URLs, config, containers, recent deployments.",
			Props:       schema{"app": str(appDesc)}, Required: []string{"app"},
			Args: appArgs("status"),
		},
		{
			Name:        "saba_deploy",
			Description: "Deploy an app with zero downtime from a local directory (uploaded and built on the server), a registry image, or a git repository. Creates the app if it does not exist. Returns when the new version is live or has failed; on failure the previous version keeps serving and the error contains the relevant logs.",
			Props: schema{
				"app":             str("App name. Defaults to the name in the directory's saba.toml, or the directory name."),
				"path":            str("Absolute path of the project directory to build and deploy (default: the server's working directory)."),
				"image":           str("Registry image to deploy instead of building source, e.g. nginx:alpine."),
				"git":             str("Git repository URL to clone and build on the server."),
				"ref":             str("Branch, tag or commit for git deployments."),
				"port":            num("Port the app listens on (default: the image's EXPOSE)."),
				"domains":         strs("Domains to route to the app (replaces the list)."),
				"env":             strMap("Environment variables to set."),
				"replicas":        num("Number of containers."),
				"nodes":           strs("Servers to spread replicas across: \"local\", node names from saba_nodes, or \"*\" for all."),
				"publish":         strs("Host ports to expose, e.g. [\"127.0.0.1:5432:5432\"] (replaces the list; [] clears it)."),
				"health_path":     str("HTTP path that must return 2xx/3xx before traffic is switched."),
				"release_command": str("Command run to completion before the new version gets traffic (e.g. migrations)."),
				"message":         str("Note to attach to the deployment."),
				"dry_run":         flag("Validate and return the plan without deploying."),
			},
			Args: func(in map[string]any) ([]string, error) {
				args := []string{"deploy", "--quiet"}
				if p := getStr(in, "path"); p != "" {
					args = append(args, p)
				}
				for k, f := range map[string]string{"app": "-a", "image": "--image", "git": "--git", "ref": "--ref",
					"health_path": "--health-path", "release_command": "--release-command", "message": "--message"} {
					if v := getStr(in, k); v != "" {
						args = append(args, f, v)
					}
				}
				if n, ok := getInt(in, "port"); ok {
					args = append(args, "--port", strconv.Itoa(n))
				}
				if n, ok := getInt(in, "replicas"); ok {
					args = append(args, "--replicas", strconv.Itoa(n))
				}
				for _, d := range getStrs(in, "domains") {
					args = append(args, "--domain", d)
				}
				for _, n := range getStrs(in, "nodes") {
					args = append(args, "--node", n)
				}
				if v, ok := in["publish"].([]any); ok {
					if len(v) == 0 {
						args = append(args, "--publish=")
					}
					for _, p := range getStrs(in, "publish") {
						args = append(args, "--publish", p)
					}
				}
				for _, kv := range getMap(in, "env") {
					args = append(args, "--env", kv)
				}
				if getBool(in, "dry_run") {
					args = append(args, "--dry-run")
				}
				return args, nil
			},
		},
		{
			Name: "saba_logs", ReadOnly: true, Plain: true,
			Description: "Recent runtime logs of an app, or (with deployment) the build and rollout log of a deployment. Use deployment=\"latest\" to see why the last deploy failed.",
			Props: schema{
				"app":        str(appDesc),
				"tail":       num("Number of lines (default 100)."),
				"since":      str("Only logs newer than this, e.g. 10m or 2h."),
				"deployment": str("Show this deployment's log instead: a version like v3, or latest."),
			},
			Required: []string{"app"},
			Args: func(in map[string]any) ([]string, error) {
				args := []string{"logs", "-a", getStr(in, "app")}
				if n, ok := getInt(in, "tail"); ok {
					args = append(args, "--tail", strconv.Itoa(n))
				}
				if s := getStr(in, "since"); s != "" {
					args = append(args, "--since", s)
				}
				if d := getStr(in, "deployment"); d != "" {
					args = append(args, "--deployment", d)
				}
				return args, nil
			},
		},
		{
			Name: "saba_deployments", ReadOnly: true,
			Description: "Deployment history of an app, newest first, including which versions can be rolled back to.",
			Props:       schema{"app": str(appDesc)}, Required: []string{"app"},
			Args: appArgs("deployments"),
		},
		{
			Name:        "saba_cancel",
			Description: "Stop an app's deployment in progress. The new containers are removed and the current version keeps serving. Returns once the cleanup is done; a deployment that already switched traffic finishes anyway (then use saba_rollback).",
			Props:       schema{"app": str(appDesc)}, Required: []string{"app"},
			Args: func(in map[string]any) ([]string, error) {
				return []string{"cancel", getStr(in, "app")}, nil
			},
		},
		{
			Name:        "saba_rollback",
			Description: "Roll an app back to its previous live version (or a specific one) with zero downtime. Restores image, config and environment.",
			Props:       schema{"app": str(appDesc), "to": str("Version to restore, e.g. v12 (default: previous live version).")},
			Required:    []string{"app"},
			Args: func(in map[string]any) ([]string, error) {
				args := []string{"rollback", "-a", getStr(in, "app")}
				if to := getStr(in, "to"); to != "" {
					args = append(args, "--to", to)
				}
				return args, nil
			},
		},
		{
			Name:        "saba_restart",
			Description: "Restart an app with zero downtime, applying saved-but-undeployed config and env changes.",
			Props:       schema{"app": str(appDesc)}, Required: []string{"app"},
			Args: appArgs("restart"),
		},
		{
			Name:        "saba_scale",
			Description: "Set the number of replicas of an app.",
			Props:       schema{"app": str(appDesc), "replicas": num("Desired number of containers (>= 1).")},
			Required:    []string{"app", "replicas"},
			Args: func(in map[string]any) ([]string, error) {
				n, ok := getInt(in, "replicas")
				if !ok {
					return nil, fmt.Errorf("replicas must be a number")
				}
				return []string{"scale", getStr(in, "app"), strconv.Itoa(n)}, nil
			},
		},
		{
			Name:        "saba_stop",
			Description: "Stop an app's containers. Config, data and versions are kept; start it again with saba_start.",
			Props:       schema{"app": str(appDesc)}, Required: []string{"app"},
			Args: appArgs("stop"),
		},
		{
			Name:        "saba_start",
			Description: "Start a stopped app.",
			Props:       schema{"app": str(appDesc)}, Required: []string{"app"},
			Args: appArgs("start"),
		},
		{
			Name: "saba_env_list", ReadOnly: true,
			Description: "List an app's environment variables (raw values and values with ${{app.KEY}} references resolved).",
			Props:       schema{"app": str(appDesc)}, Required: []string{"app"},
			Args: appArgs("env", "list"),
		},
		{
			Name:        "saba_env_set",
			Description: "Set and/or remove environment variables, then roll out the change with zero downtime. Values may reference other apps: ${{db.URL}}.",
			Props: schema{
				"app":       str(appDesc),
				"set":       strMap("Variables to set."),
				"unset":     strs("Variable names to remove."),
				"no_deploy": flag("Save without rolling out; applies on the next deploy or restart."),
			},
			Required: []string{"app"},
			Args: func(in map[string]any) ([]string, error) {
				set, unset := getMap(in, "set"), getStrs(in, "unset")
				if len(set) == 0 && len(unset) == 0 {
					return nil, fmt.Errorf("provide `set` and/or `unset`")
				}
				// Removals are saved first so that one rollout applies both.
				if len(set) > 0 && len(unset) > 0 {
					return nil, fmt.Errorf("use separate calls for `set` and `unset` (pass no_deploy: true on the first to roll out only once)")
				}
				args := []string{"env"}
				if len(set) > 0 {
					args = append(append(args, "set"), set...)
				} else {
					args = append(append(args, "unset"), unset...)
				}
				args = append(args, "-a", getStr(in, "app"))
				if getBool(in, "no_deploy") {
					args = append(args, "--no-deploy")
				}
				return args, nil
			},
		},
		{
			Name:        "saba_domains",
			Description: "List, add or remove the domains routed to an app. HTTPS certificates are issued automatically once DNS points at the server.",
			Props: schema{
				"app":    str(appDesc),
				"add":    strs("Domains to add."),
				"remove": strs("Domains to remove."),
			},
			Required: []string{"app"},
			Args: func(in map[string]any) ([]string, error) {
				add, remove := getStrs(in, "add"), getStrs(in, "remove")
				switch {
				case len(add) > 0 && len(remove) > 0:
					return nil, fmt.Errorf("add and remove domains in separate calls")
				case len(add) > 0:
					return append(append([]string{"domains", "add"}, add...), "-a", getStr(in, "app")), nil
				case len(remove) > 0:
					return append(append([]string{"domains", "remove"}, remove...), "-a", getStr(in, "app")), nil
				}
				return []string{"domains", "list", "-a", getStr(in, "app")}, nil
			},
		},
		{
			Name:        "saba_exec",
			Description: "Run a command inside a running container of an app (or, with one_off, in a fresh container of its image) and return stdout, stderr and the exit code.",
			Props: schema{
				"app":     str(appDesc),
				"command": strs("Command and arguments, e.g. [\"ls\", \"-la\"]."),
				"one_off": flag("Run in a new one-off container instead of a serving replica (for migrations and scripts)."),
			},
			Required: []string{"app", "command"},
			Args: func(in map[string]any) ([]string, error) {
				cmd := getStrs(in, "command")
				if len(cmd) == 0 {
					return nil, fmt.Errorf("command must be a non-empty array of strings")
				}
				verb := "exec"
				if getBool(in, "one_off") {
					verb = "run"
				}
				return append([]string{verb, "-a", getStr(in, "app"), "--"}, cmd...), nil
			},
		},
		{
			Name:        "saba_db_create",
			Description: "Create a managed database (postgres, mysql, mariadb, redis or mongo) with a persistent volume and generated credentials. Returns its internal connection URL.",
			Props: schema{
				"name":    str("Database name (also its hostname for other apps)."),
				"engine":  schema{"type": "string", "enum": []string{"postgres", "mysql", "mariadb", "redis", "mongo"}, "description": "Database engine."},
				"version": str("Image tag of the engine (optional)."),
			},
			Required: []string{"name", "engine"},
			Args: func(in map[string]any) ([]string, error) {
				args := []string{"db", "create", getStr(in, "name"), "--engine", getStr(in, "engine")}
				if v := getStr(in, "version"); v != "" {
					args = append(args, "--version", v)
				}
				return args, nil
			},
		},
		{
			Name:        "saba_db_link",
			Description: "Connect an app to a managed database by setting an env var (DATABASE_URL, REDIS_URL or MONGODB_URL by default) that references the database URL, then roll out the app.",
			Props: schema{
				"database": str("Database name."),
				"app":      str("App to connect."),
				"as":       str("Variable name to set (optional)."),
			},
			Required: []string{"database", "app"},
			Args: func(in map[string]any) ([]string, error) {
				args := []string{"db", "link", getStr(in, "database"), "-a", getStr(in, "app")}
				if as := getStr(in, "as"); as != "" {
					args = append(args, "--as", as)
				}
				return args, nil
			},
		},
		{
			Name:        "saba_backup",
			Description: "Back up an app now: a managed database is dumped with its own tool, any other app has its named volumes archived (.tar.gz). Stored on the server (and in S3 when configured); returns its id, size and method.",
			Props: schema{
				"app":  str("App or database name."),
				"stop": flag("Stop the app while its volumes are archived, for files unsafe to copy in use (SQLite). Requests wait meanwhile. Not for database dumps."),
			},
			Required: []string{"app"},
			Args: func(in map[string]any) ([]string, error) {
				args := []string{"backup", "create", "-a", getStr(in, "app")}
				if getBool(in, "stop") {
					args = append(args, "--stop")
				}
				return args, nil
			},
		},
		{
			Name: "saba_backups", ReadOnly: true,
			Description: "List the stored backups of an app or database, newest first.",
			Props:       schema{"app": str("App or database name.")}, Required: []string{"app"},
			Args: func(in map[string]any) ([]string, error) {
				return []string{"backup", "list", "-a", getStr(in, "app")}, nil
			},
		},
		{
			Name: "saba_restore", Destructive: true,
			Description: "Restore an app or database from one of its stored backups, replacing its current data (for volume backups the app is stopped meanwhile). Only call when the user explicitly asked for a restore.",
			Props: schema{
				"app":     str("App or database name."),
				"backup":  str("Backup id from saba_backups, or \"latest\"."),
				"confirm": flag("Must be true."),
			},
			Required: []string{"app", "backup", "confirm"},
			Args: func(in map[string]any) ([]string, error) {
				if !getBool(in, "confirm") {
					return nil, fmt.Errorf("set confirm to true to replace the data of %q with the backup", getStr(in, "app"))
				}
				return []string{"backup", "restore", "-a", getStr(in, "app"), "--backup", getStr(in, "backup"), "--yes"}, nil
			},
		},
		{
			Name: "saba_nodes", ReadOnly: true,
			Description: "List the servers of the cluster (the primary \"local\" and added nodes) with connection status and replica counts. Use node names in saba_deploy's `nodes`.",
			Props:       schema{},
			Args:        func(map[string]any) ([]string, error) { return []string{"node", "list"}, nil },
		},
		{
			Name: "saba_certs", ReadOnly: true,
			Description: "List TLS certificates (uploaded, wildcard via DNS-01, and on-demand per routed domain) with status, expiry and errors. A pending on-demand certificate is issued on the domain's first HTTPS request.",
			Props:       schema{},
			Args:        func(map[string]any) ([]string, error) { return []string{"certs", "list"}, nil },
		},
		{
			Name: "saba_doctor", ReadOnly: true,
			Description: "Diagnose the server and all apps: Docker, disk, DNS of routed domains, crashed or degraded apps. Failed checks include a suggested fix.",
			Props:       schema{},
			Args:        func(map[string]any) ([]string, error) { return []string{"doctor"}, nil },
		},
		{
			Name: "saba_destroy", Destructive: true,
			Description: "Permanently delete an app or database: containers, images and history. Volumes (data) are kept unless delete_volumes is true. Irreversible — only call when the user explicitly asked for deletion.",
			Props: schema{
				"app":            str("App or database to delete."),
				"delete_volumes": flag("Also delete its volumes (permanent data loss)."),
				"confirm":        flag("Must be true."),
			},
			Required: []string{"app", "confirm"},
			Args: func(in map[string]any) ([]string, error) {
				if !getBool(in, "confirm") {
					return nil, fmt.Errorf("set confirm to true to delete %q; this cannot be undone", getStr(in, "app"))
				}
				args := []string{"destroy", getStr(in, "app"), "--yes"}
				if getBool(in, "delete_volumes") {
					args = append(args, "--volumes")
				}
				return args, nil
			},
		},
	}
}

// Serve handles requests until the input ends.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	var mu sync.Mutex
	write := func(resp response) {
		resp.JSONRPC = "2.0"
		b, _ := json.Marshal(resp)
		mu.Lock()
		defer mu.Unlock()
		_, _ = out.Write(append(b, '\n'))
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			write(response{ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			write(response{ID: req.ID, Result: map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "saba", "version": s.Version},
				"instructions": "saba deploys and operates apps on the user's own servers. " +
					"Typical flow: saba_apps or saba_status to look, saba_deploy to ship, saba_logs to debug. " +
					"Tool errors are JSON with `code`, `message` and a `hint` describing the fix. Call saba_guide for the full reference.",
			}})
		case "ping":
			write(response{ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			write(response{ID: req.ID, Result: map[string]any{"tools": s.list()}})
		case "tools/call":
			// Calls run concurrently so a long deploy does not block status checks.
			wg.Add(1)
			go func(req request) {
				defer wg.Done()
				write(response{ID: req.ID, Result: s.call(ctx, req.Params)})
			}(req)
		default:
			write(response{ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
		}
	}
	return sc.Err()
}

func (s *Server) list() []map[string]any {
	var out []map[string]any
	for _, t := range tools() {
		input := schema{"type": "object", "properties": t.Props}
		if len(t.Required) > 0 {
			input["required"] = t.Required
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": input,
			"annotations": map[string]any{
				"readOnlyHint":    t.ReadOnly,
				"destructiveHint": t.Destructive,
				"openWorldHint":   true,
			},
		})
	}
	return out
}

func textResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func (s *Server) call(ctx context.Context, params json.RawMessage) map[string]any {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return textResult("invalid tool call parameters: "+err.Error(), true)
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	for _, t := range tools() {
		if t.Name != p.Name {
			continue
		}
		if t.Name == "saba_guide" {
			return textResult(s.Guide, false)
		}
		for _, r := range t.Required {
			if v, ok := p.Arguments[r]; !ok || v == "" {
				return textResult(fmt.Sprintf("missing required argument %q", r), true)
			}
		}
		args, err := t.Args(p.Arguments)
		if err != nil {
			return textResult(err.Error(), true)
		}
		return s.run(ctx, args, t.Plain)
	}
	return textResult("unknown tool: "+p.Name, true)
}

// run executes the CLI and returns its JSON output; failures return the
// structured error the CLI printed.
func (s *Server) run(ctx context.Context, args []string, plain bool) map[string]any {
	full := append([]string{}, s.GlobalArgs...)
	if plain {
		full = append(full, "--no-color")
	} else {
		full = append(full, "--json")
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, s.Exe, full...)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// A JSON document on stdout is the result even when the exit code is
		// non-zero (e.g. `exec` of a failing command).
		if out != "" && stderr.Len() == 0 {
			return textResult(out, true)
		}
		if out != "" {
			msg = out + "\n" + msg
		}
		return textResult(msg, true)
	}
	if out == "" && plain {
		out = "(no output)"
	} else if out == "" {
		out = "{}"
	}
	return textResult(out, false)
}
