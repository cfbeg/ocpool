package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	api "github.com/acksell/opencode-go-sdk/sdk"
)

const clientToolPrefix = "ocpool_"

func nativeToolPermissions(provider string, plan *toolPlan) api.PermissionRuleset {
	if provider == "opencode" {
		// The real OpenCode free-tier path rejects requests with its native tool
		// definitions filtered out. Keep the CLI's standard definitions and ask
		// before every execution; the gateway rejects them and hands off only
		// explicitly declared client tools.
		return api.PermissionRuleset{&api.PermissionRule{Permission: "*", Pattern: "*", Action: api.PermissionActionAsk}}
	}
	return clientToolPermissions(plan)
}

// OpenCode filters model-facing tool definitions with the session permissions.
// Keep every internal/unlisted tool hidden, and retain only this request's MCP
// aliases as permission requests so the API caller performs their execution.
func clientToolPermissions(plan *toolPlan) api.PermissionRuleset {
	rules := api.PermissionRuleset{&api.PermissionRule{Permission: "*", Pattern: "*", Action: api.PermissionActionDeny}}
	if plan == nil {
		return rules
	}
	for _, function := range plan.definitions {
		if plan.selected != "" && plan.selected != function.Name {
			continue
		}
		rules = append(rules, &api.PermissionRule{Permission: clientToolPrefix + clientToolAlias(function.Name), Pattern: "*", Action: api.PermissionActionAsk})
	}
	return rules
}

func clientToolAlias(name string) string {
	if len(name) <= 64-len(clientToolPrefix) {
		return name
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	return name[:36] + "_" + hash[:20]
}

// This MCP server publishes schemas only. Native permission requests are handed
// back as function calls; the API client executes the tools on its own machine.
type toolBridge struct {
	plan  *toolPlan
	token string
}

func (b *toolBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&rpc) != nil {
		http.Error(w, "invalid JSON-RPC", 400)
		return
	}
	if len(rpc.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	body := map[string]any{"jsonrpc": "2.0", "id": rpc.ID}
	switch rpc.Method {
	case "initialize":
		body["result"] = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "ocpool-client-tools", "version": "1"}}
	case "ping":
		body["result"] = map[string]any{}
	case "tools/list":
		list := []any{}
		for _, f := range b.plan.definitions {
			if b.plan.selected != "" && b.plan.selected != f.Name {
				continue
			}
			list = append(list, map[string]any{"name": clientToolAlias(f.Name), "description": f.Description, "inputSchema": f.Parameters})
		}
		body["result"] = map[string]any{"tools": list}
	case "tools/call":
		body["error"] = map[string]any{"code": -32601, "message": "client tools must be returned to the API caller; gateway execution is disabled"}
	default:
		body["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	writeJSON(w, 200, body)
}

func (r *Runtime) nativePost(ctx context.Context, path string, body any, result any) error {
	raw, e := json.Marshal(body)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", r.URL+path+"?directory="+url.QueryEscape(r.Dir), bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", r.authorization)
	req.Header.Set("Content-Type", "application/json")
	response, e := http.DefaultClient.Do(req)
	if e != nil {
		return e
	}
	defer response.Body.Close()
	data, e := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if e != nil {
		return e
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("OpenCode %s: HTTP %d: %s", path, response.StatusCode, clip(string(data), 600))
	}
	if result != nil {
		return json.Unmarshal(data, result)
	}
	return nil
}

func (p *toolPlan) connect(ctx context.Context, r *Runtime) (func() error, error) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	bridge := &toolBridge{plan: p, token: secret()}
	server := &http.Server{Handler: bridge, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() { _ = server.Serve(listener) }()
	closeBridge := func() error {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e := r.nativePost(cleanup, "/mcp/ocpool/disconnect", map[string]any{}, nil)
		closeErr := server.Close()
		if e != nil {
			return e
		}
		return closeErr
	}
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var statuses map[string]struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	e = r.nativePost(startup, "/mcp", map[string]any{"name": "ocpool", "config": map[string]any{
		"type": "remote", "url": "http://" + listener.Addr().String(), "oauth": false, "headers": map[string]string{"Authorization": "Bearer " + bridge.token}, "timeout": 10000,
	}}, &statuses)
	if e == nil && statuses["ocpool"].Status != "connected" {
		e = errors.New("OpenCode did not connect client tools: " + statuses["ocpool"].Error)
	}
	if e != nil {
		closeBridge()
		return nil, e
	}
	return closeBridge, nil
}

func (p *toolPlan) toolName(native string) (string, bool) {
	if p == nil || !strings.HasPrefix(native, clientToolPrefix) {
		return "", false
	}
	for _, f := range p.definitions {
		if p.selected != "" && p.selected != f.Name {
			continue
		}
		if clientToolPrefix+clientToolAlias(f.Name) == native {
			return f.Name, true
		}
	}
	return "", false
}
func (p *toolPlan) capture(parts []*api.Part, deferred map[string]bool) ([]ToolCall, error) {
	calls := []ToolCall{}
	for _, part := range parts {
		t := part.Tool
		if t == nil {
			continue
		}
		name, ok := p.toolName(t.Tool)
		if !ok {
			continue
		}
		if t.State == nil || t.State.Error == nil {
			return nil, errors.New("client tool handoff did not stop native execution")
		}
		if !deferred[t.CallID] && t.State.Error.Error != "The user rejected permission to use this specific tool call." {
			return nil, errors.New("native client tool failed before handoff: " + t.State.Error.Error)
		}
		args, e := json.Marshal(t.State.Error.Input)
		if e != nil {
			return nil, e
		}
		call := ToolCall{ID: t.CallID, Type: "function"}
		call.Function.Name = name
		call.Function.Arguments = string(args)
		calls = append(calls, call)
	}
	for id := range deferred {
		found := false
		for _, c := range calls {
			if c.ID == id {
				found = true
			}
		}
		if !found {
			return nil, errors.New("incomplete native client tool handoff")
		}
	}
	if (!p.parallel && len(calls) > 1) || len(calls) > 16 {
		return nil, errors.New("upstream did not honor parallel_tool_calls")
	}
	if (p.choice == "required" || p.selected != "") && len(calls) == 0 {
		return nil, errors.New("upstream returned text instead of the requested tool call (native tool selection is auto)")
	}
	return calls, nil
}
