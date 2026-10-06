package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/acksell/opencode-go-sdk/sdk"
	"github.com/acksell/opencode-go-sdk/sdk/client"
	"github.com/acksell/opencode-go-sdk/sdk/option"
)

// Capture the actual SDK HTTP requests, rather than testing an independently
// assembled policy. The MCP handshake and native permission reply are exercised
// here, while model output remains a deterministic local fixture.
type toolScopeFixture struct {
	app             *App
	runtime         *Runtime
	mu              sync.Mutex
	permissions     []*api.PermissionRule
	prompts         []map[string]any
	mcpNames        []string
	replies         []string
	toolName        string
	model           string
	recoverNative   bool
	repeatNative    bool
	internalKind    string
	questionRejects []string
	prefix          string
	maxOutput       int
	parameterCaps   []int
	replyWait       map[string]chan struct{}
}

func newToolScopeFixture(t *testing.T, toolName string) *toolScopeFixture {
	t.Helper()
	f := &toolScopeFixture{toolName: toolName, model: "scope-fixture/mock", replyWait: map[string]chan struct{}{}}
	s := testStore(t)
	addTestAccount(t, s, "tool-scope-account")
	if err := s.SetModels("tool-scope-account", []string{f.model, "opencode/test-free"}); err != nil {
		t.Fatal(err)
	}
	native := httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(native.Close)
	p := newPool(s, t.TempDir(), "")
	t.Cleanup(p.close)
	f.runtime = &Runtime{ID: "tool-scope-account", Dir: t.TempDir(), URL: native.URL, ParamsDir: t.TempDir(), cancel: func() {}, subs: map[string]*subscription{}, Client: client.NewClient(option.WithBaseURL(native.URL), option.WithMaxAttempts(1))}
	p.startFn = func(context.Context, string) (*Runtime, error) { return f.runtime, nil }
	f.app = &App{store: s, pool: p}
	return f
}

func (f *toolScopeFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if r.Method == "POST" || r.Method == "PATCH" {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && r.ContentLength != 0 {
			http.Error(w, err.Error(), 400)
			return
		}
	}
	f.mu.Lock()
	if raw, ok := body["permission"]; ok {
		_ = json.Unmarshal(raw, &f.permissions)
	}
	f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/session":
		writeJSON(w, 200, map[string]string{"id": "ses_scope"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/fork"):
		writeJSON(w, 200, map[string]string{"id": "ses_scope"})
	case r.Method == "PATCH" && r.URL.Path == "/session/ses_scope":
		writeJSON(w, 200, map[string]string{"id": "ses_scope"})
	case r.Method == "POST" && r.URL.Path == "/mcp":
		var config struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		}
		_ = json.Unmarshal(body["config"], &config)
		request, _ := http.NewRequestWithContext(r.Context(), "POST", config.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		for name, value := range config.Headers {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer response.Body.Close()
		var result struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err = json.NewDecoder(response.Body).Decode(&result); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		f.mu.Lock()
		for _, tool := range result.Result.Tools {
			f.mcpNames = append(f.mcpNames, tool.Name)
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ocpool": map[string]string{"status": "connected"}})
	case r.Method == "POST" && ((strings.HasPrefix(r.URL.Path, "/permission/") && strings.HasSuffix(r.URL.Path, "/reply")) || (strings.HasPrefix(r.URL.Path, "/question/") && strings.HasSuffix(r.URL.Path, "/reject"))):
		var reply string
		_ = json.Unmarshal(body["reply"], &reply)
		f.mu.Lock()
		question := strings.HasPrefix(r.URL.Path, "/question/")
		if question {
			reply = "reject"
			f.questionRejects = append(f.questionRejects, r.URL.Path)
		}
		f.replies = append(f.replies, reply)
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/permission/"), "/reply")
		if question {
			id = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/question/"), "/reject")
		}
		replied := f.replyWait[id]
		f.mu.Unlock()
		writeJSON(w, 200, true)
		if replied != nil {
			close(replied)
		}
	case r.Method == "POST" && r.URL.Path == "/session/ses_scope/message":
		params, err := os.ReadFile(filepath.Join(f.runtime.ParamsDir, "ses_scope.json"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var options GenerationOptions
		if err = json.Unmarshal(params, &options); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if err = atomicWrite(filepath.Join(f.runtime.ParamsDir, "ses_scope.applied.json"), params); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		prompt := map[string]any{}
		for key, value := range body {
			var decoded any
			_ = json.Unmarshal(value, &decoded)
			prompt[key] = decoded
		}
		f.mu.Lock()
		f.prompts = append(f.prompts, prompt)
		if options.MaxOutputTokens != nil {
			f.parameterCaps = append(f.parameterCaps, *options.MaxOutputTokens)
		}
		attempt := len(f.prompts)
		f.mu.Unlock()
		internal := f.recoverNative && (f.repeatNative || attempt == 1)
		if f.toolName == "" && !internal {
			syntheticAnswer(w)
			return
		}
		nativeName, callID := clientToolPrefix+clientToolAlias(f.toolName), "call_scope"
		args := map[string]string{"path": "client-only/fixture.txt"}
		if internal {
			nativeName, callID, args = "bash", "call_internal", map[string]string{"command": "printf SYNTHETIC_INTERNAL_TOOL"}
			if f.internalKind == "question" {
				nativeName, args = "question", map[string]string{"question": "Synthetic native question"}
			}
		}
		e := event{Type: "permission.asked"}
		if internal && f.internalKind == "question" {
			e.Type = "question.asked"
		}
		e.Properties.SessionID = "ses_scope"
		e.Properties.ID = fmt.Sprintf("per_scope_%d", attempt)
		e.Properties.Permission = nativeName
		e.Properties.Tool.CallID = callID
		replied := make(chan struct{})
		f.mu.Lock()
		f.replyWait[e.Properties.ID] = replied
		f.mu.Unlock()
		f.runtime.mu.Lock()
		if internal && f.prefix != "" {
			role := event{Type: "message.updated"}
			role.Properties.Info.ID, role.Properties.Info.Role = "msg_scope", "assistant"
			f.runtime.subs["ses_scope"].ch <- role
			text := event{Type: "message.part.updated"}
			text.Properties.Part.ID, text.Properties.Part.MessageID = "part_prefix", "msg_scope"
			text.Properties.Part.Type, text.Properties.Part.Text = "text", f.prefix
			f.runtime.subs["ses_scope"].ch <- text
		}
		f.runtime.subs["ses_scope"].ch <- e
		f.runtime.mu.Unlock()
		select {
		case <-replied:
		case <-r.Context().Done():
			return
		}
		parts := []any{map[string]any{"id": "part_scope", "type": "tool", "tool": nativeName, "callID": callID, "state": map[string]any{"status": "error", "input": args, "error": "The user rejected permission to use this specific tool call.", "time": map[string]int{"start": 1, "end": 2}}}}
		if internal && f.prefix != "" {
			parts = append([]any{map[string]any{"id": "part_prefix", "type": "text", "text": f.prefix}}, parts...)
		}
		writeJSON(w, 200, map[string]any{
			"info":  map[string]any{"id": "msg_scope", "role": "assistant", "finish": "tool-calls", "tokens": map[string]int{"input": 10, "output": 3, "reasoning": 0}},
			"parts": parts,
		})
	default:
		writeJSON(w, 200, true)
	}
}

const toolScopeTools = `[{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},{"type":"function","name":"write","parameters":{"type":"object","properties":{}}}]`

func TestNativeToolScopeFromAPIAndClientHandoff(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, test := range []struct {
			name, tools, choice, call string
			allowed                   []string
		}{
			{name: "no_tools"},
			{name: "tool_choice_none", tools: toolScopeTools, choice: `"none"`},
			{name: "client_auto", tools: toolScopeTools, choice: `"auto"`, call: "read", allowed: []string{"read", "write"}},
			{name: "client_named", tools: toolScopeTools, choice: `{"type":"function","name":"read"}`, call: "read", allowed: []string{"read"}},
		} {
			apiName := map[bool]string{false: "chat", true: "responses"}[responses]
			t.Run(apiName+"/"+test.name, func(t *testing.T) {
				f := newToolScopeFixture(t, test.call)
				result := f.call(t, responses, test.tools, test.choice)
				f.assertPolicy(t, test.allowed)
				if test.call == "" {
					if !strings.Contains(fmt.Sprint(result), "SYNTHETIC_OK") {
						t.Fatal("ordinary conversation did not return text", result)
					}
					return
				}
				if fmt.Sprint(f.replies) != "[reject]" {
					t.Fatal("native client tool execution was not stopped", f.replies)
				}
				if responses {
					output := result["output"].([]any)
					call := output[0].(map[string]any)
					if call["type"] != "function_call" || call["call_id"] != "call_scope" || call["name"] != "read" || call["arguments"] != `{"path":"client-only/fixture.txt"}` {
						t.Fatal("native call did not reach Responses client unchanged", output)
					}
				} else {
					choice := result["choices"].([]any)[0].(map[string]any)
					calls := choice["message"].(map[string]any)["tool_calls"].([]any)
					call := calls[0].(map[string]any)
					function := call["function"].(map[string]any)
					if choice["finish_reason"] != "tool_calls" || call["id"] != "call_scope" || function["name"] != "read" || function["arguments"] != `{"path":"client-only/fixture.txt"}` {
						t.Fatal("native call did not reach Chat client unchanged", choice)
					}
				}
			})
		}
	}
}

func TestForkedNativeSessionReplacesPreviousToolScope(t *testing.T) {
	f := newToolScopeFixture(t, "")
	f.permissions = []*api.PermissionRule{{Permission: "*", Pattern: "*", Action: "allow"}, {Permission: "ocpool_previous_tool", Pattern: "*", Action: "ask"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := f.app.generate(ctx, f.model, []Message{{Role: "user", Content: json.RawMessage(`"reply with text"`)}}, "", "ses_previous", false, GenerationOptions{}, nil)
	if err != nil || result.Text != "SYNTHETIC_OK" {
		t.Fatal(result, err)
	}
	f.assertPolicy(t, nil)
	if f.policyAction("ocpool_previous_tool") != "deny" {
		t.Fatal("fork inherited previous client tool access")
	}
}

func TestOpenCodeInternalToolRecoveryAndClientHandoff(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, client := range []bool{false, true} {
			name := map[bool]string{false: "chat", true: "responses"}[responses] + "/" + map[bool]string{false: "text", true: "client_tool"}[client]
			t.Run(name, func(t *testing.T) {
				toolName, tools := "", ""
				if client {
					toolName, tools = "read", toolScopeTools
				}
				f := newToolScopeFixture(t, toolName)
				f.model, f.recoverNative, f.prefix = "opencode/test-free", true, "VISIBLE_PREFIX "
				result := f.call(t, responses, tools, "")
				f.mu.Lock()
				defer f.mu.Unlock()
				if len(f.prompts) != 2 {
					t.Fatalf("corrective prompt count %d, want 2", len(f.prompts))
				}
				for _, tool := range []string{"bash", "read", "task", "future_native_tool", "ocpool_read"} {
					if f.policyAction(tool) != "ask" {
						t.Errorf("OpenCode %s policy = %s, want ask", tool, f.policyAction(tool))
					}
				}
				for _, prompt := range f.prompts {
					if nativeTools, ok := prompt["tools"].(map[string]any); ok {
						for tool, enabled := range nativeTools {
							if enabled == true {
								t.Errorf("native tool %s was auto-approved", tool)
							}
						}
					}
				}
				wantReplies := "[reject]"
				if client {
					wantReplies = "[reject reject]"
				}
				if fmt.Sprint(f.replies) != wantReplies {
					t.Fatalf("permission replies = %v, want %s", f.replies, wantReplies)
				}
				usage := result["usage"].(map[string]any)
				inputKey, outputKey := "prompt_tokens", "completion_tokens"
				if responses {
					inputKey, outputKey = "input_tokens", "output_tokens"
				}
				if usage[inputKey] != float64(20) || usage[outputKey] != float64(6) {
					t.Errorf("corrective usage did not include both upstream phases: %v", usage)
				}
				if responses {
					output := result["output"].([]any)
					var text strings.Builder
					var call map[string]any
					for _, item := range output {
						part := item.(map[string]any)
						if part["type"] == "function_call" {
							call = part
						}
						if part["type"] == "message" {
							for _, content := range part["content"].([]any) {
								text.WriteString(content.(map[string]any)["text"].(string))
							}
						}
					}
					if client {
						if call == nil || call["call_id"] != "call_scope" || call["name"] != "read" || call["arguments"] != `{"path":"client-only/fixture.txt"}` {
							t.Fatal("recovery lost Responses client handoff", output)
						}
					} else if text.String() != "VISIBLE_PREFIX SYNTHETIC_OK" {
						t.Fatal("recovery lost or duplicated visible text", text.String())
					}
				} else {
					choice := result["choices"].([]any)[0].(map[string]any)
					message := choice["message"].(map[string]any)
					if client {
						calls := message["tool_calls"].([]any)
						call := calls[0].(map[string]any)
						function := call["function"].(map[string]any)
						if len(calls) != 1 || call["id"] != "call_scope" || function["name"] != "read" || function["arguments"] != `{"path":"client-only/fixture.txt"}` {
							t.Fatal("recovery lost Chat client handoff", calls)
						}
					} else if message["content"] != "VISIBLE_PREFIX SYNTHETIC_OK" {
						t.Fatal("recovery lost or duplicated visible text", message)
					}
				}
			})
		}
	}
}

func TestOpenCodeForkReplacesLegacyAutoApproval(t *testing.T) {
	f := newToolScopeFixture(t, "")
	f.model = "opencode/test-free"
	f.permissions = []*api.PermissionRule{{Permission: "*", Pattern: "*", Action: "allow"}, {Permission: "ocpool_previous_tool", Pattern: "*", Action: "allow"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := f.app.generate(ctx, f.model, []Message{{Role: "user", Content: json.RawMessage(`"reply with text"`)}}, "", "ses_previous", false, GenerationOptions{}, nil)
	if err != nil || result.Text != "SYNTHETIC_OK" {
		t.Fatal(result, err)
	}
	if f.policyAction("bash") != "ask" || f.policyAction("ocpool_previous_tool") != "ask" {
		t.Fatal("fork retained legacy automatic execution grants", f.permissions)
	}
}

func TestOpenCodeInternalToolRecoveryIsBounded(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			f := newToolScopeFixture(t, "")
			f.model, f.recoverNative, f.repeatNative = "opencode/test-free", true, true
			handler := f.app.chat
			body := `{"model":"opencode/test-free","messages":[{"role":"user","content":"reply with text"}]}`
			if responses {
				handler = f.app.responses
				body = `{"model":"opencode/test-free","input":"reply with text","store":false}`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(ctx))
			if w.Code != 502 {
				t.Fatalf("unrecoverable native tools returned HTTP %d: %s", w.Code, w.Body.String())
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.prompts) != 3 || fmt.Sprint(f.replies) != "[reject reject reject]" {
				t.Fatalf("correction limit did not stop after 2 retries: prompts=%d replies=%v", len(f.prompts), f.replies)
			}
		})
	}
}

func TestOpenCodeNativeQuestionIsRejectedAndRecovers(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			f := newToolScopeFixture(t, "")
			f.model, f.recoverNative, f.internalKind = "opencode/test-free", true, "question"
			result := f.call(t, responses, "", "")
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.prompts) != 2 || len(f.questionRejects) != 1 || fmt.Sprint(f.replies) != "[reject]" {
				t.Fatalf("question waited instead of recovering: prompts=%d questionRejects=%v replies=%v", len(f.prompts), f.questionRejects, f.replies)
			}
			if !strings.Contains(fmt.Sprint(result), "SYNTHETIC_OK") {
				t.Fatal("native question recovery lost text response", result)
			}
		})
	}
}

func TestOpenCodeCorrectionSharesOutputTokenBudget(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, budget := range []int{6, 3} {
			name := map[bool]string{false: "chat", true: "responses"}[responses] + fmt.Sprintf("/budget_%d", budget)
			t.Run(name, func(t *testing.T) {
				f := newToolScopeFixture(t, "")
				f.model, f.recoverNative, f.prefix, f.maxOutput = "opencode/test-free", true, "VISIBLE_PREFIX ", budget
				result := f.call(t, responses, "", "")
				f.mu.Lock()
				defer f.mu.Unlock()
				wantCaps, wantPrompts, wantText := "[6 3]", 2, "VISIBLE_PREFIX SYNTHETIC_OK"
				if budget == 3 {
					wantCaps, wantPrompts, wantText = "[3]", 1, "VISIBLE_PREFIX "
				}
				if fmt.Sprint(f.parameterCaps) != wantCaps || len(f.prompts) != wantPrompts || fmt.Sprint(f.replies) != "[reject]" {
					t.Fatalf("token budget was reset across correction: caps=%v prompts=%d replies=%v", f.parameterCaps, len(f.prompts), f.replies)
				}
				usage := result["usage"].(map[string]any)
				outputKey := "completion_tokens"
				if responses {
					outputKey = "output_tokens"
				}
				if usage[outputKey] != float64(budget) {
					t.Errorf("usage did not preserve shared budget: %v", usage)
				}
				if responses {
					output := result["output"].([]any)
					text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
					if text != wantText {
						t.Error("budget terminal response changed prefix", text)
					}
					if budget == 3 && (result["status"] != "incomplete" || result["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens") {
						t.Error("Responses did not report budget exhaustion", result)
					}
				} else {
					choice := result["choices"].([]any)[0].(map[string]any)
					if choice["message"].(map[string]any)["content"] != wantText {
						t.Error("budget terminal response changed prefix", choice)
					}
					if budget == 3 && choice["finish_reason"] != "length" {
						t.Error("Chat did not report budget exhaustion", choice)
					}
				}
			})
		}
	}
}

func (f *toolScopeFixture) call(t *testing.T, responses bool, tools, choice string) map[string]any {
	t.Helper()
	body := map[string]any{"model": f.model}
	handler := f.app.chat
	if responses {
		body["input"] = "Read client-only/fixture.txt if client tools are available, otherwise reply with text."
		body["store"] = false
		handler = f.app.responses
	} else {
		body["messages"] = []map[string]string{{"role": "user", "content": "Read client-only/fixture.txt if client tools are available, otherwise reply with text."}}
	}
	if tools != "" {
		body["tools"] = json.RawMessage(tools)
	}
	if f.maxOutput > 0 {
		key := "max_tokens"
		if responses {
			key = "max_output_tokens"
		}
		body[key] = f.maxOutput
	}
	if choice != "" {
		body["tool_choice"] = json.RawMessage(choice)
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("POST", "/", strings.NewReader(string(raw))).WithContext(ctx))
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *toolScopeFixture) policyAction(name string) string {
	action := "ask"
	for _, rule := range f.permissions {
		if rule.Pattern == "*" && (rule.Permission == "*" || rule.Permission == name) {
			action = string(rule.Action)
		}
	}
	return action
}

func (f *toolScopeFixture) assertPolicy(t *testing.T, allowed []string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) != 1 {
		t.Fatalf("native prompt count: %d", len(f.prompts))
	}
	for _, name := range []string{"bash", "read", "write", "edit", "apply_patch", "task", "webfetch", "todowrite", "question", "future_unlisted_internal_tool", "ocpool_undeclared"} {
		if got := f.policyAction(name); got != "deny" {
			t.Errorf("%s native policy = %q, want deny", name, got)
		}
	}
	for _, name := range allowed {
		if got := f.policyAction(clientToolPrefix + clientToolAlias(name)); got != "ask" {
			t.Errorf("client tool %s native policy = %q, want ask for client handoff", name, got)
		}
	}
	if tools, ok := f.prompts[0]["tools"].(map[string]any); ok {
		for name, enabled := range tools {
			if enabled == true {
				t.Errorf("SDK prompt auto-approved %s; client tools require permission handoff", name)
			}
		}
	}
	if fmt.Sprint(f.mcpNames) != fmt.Sprint(allowed) {
		t.Errorf("MCP exposed %v, want only %v", f.mcpNames, allowed)
	}
}

// Run with OCPOOL_NATIVE_TEST=1. This uses the stock OpenCode CLI and its
// embedded SDK transport, but all inference requests terminate at this local
// synthetic provider. It verifies the model's actual tool definitions, including
// native tools unknown to this Go application.
func TestNativeProviderToolScopeWithMockInference(t *testing.T) {
	if os.Getenv("OCPOOL_NATIVE_TEST") != "1" {
		t.Skip("native OpenCode integration test")
	}
	type wireRequest struct {
		session string
		names   []string
	}
	var mu sync.Mutex
	wire := []wireRequest{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages json.RawMessage `json:"messages"`
			Tools    []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		names := []string{}
		clientTool := ""
		for _, tool := range body.Tools {
			names = append(names, tool.Function.Name)
			if tool.Function.Name == "ocpool_read" {
				clientTool = tool.Function.Name
			}
		}
		if bytes.Contains(body.Messages, []byte("TOOL_SCOPE_REQUEST")) {
			mu.Lock()
			wire = append(wire, wireRequest{session: r.Header.Get("x-opencode-session-id"), names: names})
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta any, finish any) {
			data, _ := json.Marshal(map[string]any{"id": "chatcmpl_scope", "object": "chat.completion.chunk", "created": 1, "model": "mock", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
		if clientTool != "" {
			chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_native_scope", "type": "function", "function": map[string]string{"name": clientTool, "arguments": `{"path":"client-only/fixture.txt"}`}}}}, nil)
			chunk(map[string]any{}, "tool_calls")
		} else {
			chunk(map[string]string{"role": "assistant", "content": "NATIVE_SCOPE_OK"}, nil)
			chunk(map[string]any{}, "stop")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()
	const model = "scope-fixture/mock"
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	a, nativeProfile := nativeToolScopeApp(t, model, mock.URL)
	var previousSession string
	for _, test := range []struct {
		name, tools, choice string
		want                []string
		fork                bool
	}{
		{name: "normal"},
		{name: "none", tools: toolScopeTools, choice: `"none"`},
		{name: "auto", tools: toolScopeTools, choice: `"auto"`, want: []string{"ocpool_read", "ocpool_write"}},
		{name: "named", tools: toolScopeTools, choice: `{"type":"function","name":"read"}`, want: []string{"ocpool_read"}},
		{name: "fork_without_tools", fork: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := parseTools(json.RawMessage(test.tools), json.RawMessage(test.choice), nil)
			if err != nil {
				t.Fatal(err)
			}
			session := ""
			if test.fork {
				session = previousSession
			}
			result, err := a.generate(ctx, model, []Message{{Role: "user", Content: json.RawMessage(`"TOOL_SCOPE_REQUEST: read client-only/fixture.txt using the client tool if available, otherwise reply with text."`)}}, "", session, true, GenerationOptions{Tools: plan}, nil)
			if err != nil {
				logs := filepath.Join(*nativeProfile, "data", "opencode", "log")
				files, _ := os.ReadDir(logs)
				for _, file := range files {
					if file.IsDir() {
						continue
					}
					data, _ := os.ReadFile(filepath.Join(logs, file.Name()))
					if len(data) > 5000 {
						data = data[len(data)-5000:]
					}
					t.Logf("isolated native log %s: %s", file.Name(), data)
				}
				t.Fatal(err)
			}
			previousSession = result.SessionID
			if len(test.want) == 0 {
				if result.Text != "NATIVE_SCOPE_OK" || len(result.ToolCalls) != 0 {
					t.Fatal("normal chat did not finish with text", result)
				}
			} else {
				if result.FinishReason != "tool_calls" || len(result.ToolCalls) != 1 || result.ToolCalls[0].ID != "call_native_scope" || result.ToolCalls[0].Function.Name != "read" || result.ToolCalls[0].Function.Arguments != `{"path":"client-only/fixture.txt"}` {
					t.Fatal("native permission handoff did not preserve client call", result)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			matched := false
			for _, request := range wire {
				if request.session != result.SessionID {
					continue
				}
				if fmt.Sprint(request.names) == fmt.Sprint(test.want) {
					matched = true
					continue
				}
				// Native title jobs share the session ID but never expose tools.
				if len(request.names) != 0 || len(test.want) == 0 {
					t.Errorf("actual provider tool definitions = %v, want %v", request.names, test.want)
				}
			}
			if !matched {
				t.Errorf("no actual provider request carried expected scope %v", test.want)
			}
		})
	}
}

func TestNativeOpenCodeInternalToolCorrectionWithMockInference(t *testing.T) {
	if os.Getenv("OCPOOL_NATIVE_TEST") != "1" {
		t.Skip("native OpenCode integration test")
	}
	type nativeMessage struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	type nativeWire struct {
		session  string
		names    []string
		messages []nativeMessage
	}
	var mu sync.Mutex
	wire := []nativeWire{}
	counts := map[string]int{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []nativeMessage `json:"messages"`
			Tools    []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		session := r.Header.Get("x-opencode-session-id")
		names := []string{}
		client := false
		for _, tool := range body.Tools {
			names = append(names, tool.Function.Name)
			client = client || tool.Function.Name == "ocpool_read"
		}
		phase := 0
		if len(names) > 0 {
			mu.Lock()
			wire = append(wire, nativeWire{session: session, names: names, messages: body.Messages})
			counts[session]++
			phase = counts[session]
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta any, finish any) {
			data, _ := json.Marshal(map[string]any{"id": "chatcmpl_open_scope", "object": "chat.completion.chunk", "created": 1, "model": "mock", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
		toolCall := func(name, id, arguments string) {
			chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]string{"name": name, "arguments": arguments}}}}, nil)
			chunk(map[string]any{}, "tool_calls")
		}
		if phase == 1 {
			chunk(map[string]string{"role": "assistant", "content": "NATIVE_PREFIX "}, nil)
			toolCall("bash", "call_open_internal", `{"command":"printf SYNTHETIC_NEVER_EXECUTE","description":"Synthetic fixture that must be rejected before execution"}`)
		} else if phase == 2 && client {
			toolCall("ocpool_read", "call_open_client", `{"path":"client-only/fixture.txt"}`)
		} else {
			chunk(map[string]string{"role": "assistant", "content": "NATIVE_CORRECTED"}, nil)
			chunk(map[string]any{}, "stop")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer mock.Close()
	const model = "opencode/mock"
	a, nativeProfile := nativeToolScopeApp(t, model, mock.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, withClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "client_tool"}[withClient], func(t *testing.T) {
			var plan *toolPlan
			if withClient {
				var err error
				plan, err = parseTools(json.RawMessage(toolScopeTools), nil, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			var streamed strings.Builder
			result, err := a.generate(ctx, model, []Message{{Role: "user", Content: json.RawMessage(`"Use the client's read tool for client-only/fixture.txt when provided; otherwise write a response."`)}}, "", "", true, GenerationOptions{Tools: plan}, func(delta string) error { streamed.WriteString(delta); return nil })
			if err != nil {
				files, _ := os.ReadDir(filepath.Join(*nativeProfile, "data", "opencode", "log"))
				for _, file := range files {
					if file.IsDir() {
						continue
					}
					data, _ := os.ReadFile(filepath.Join(*nativeProfile, "data", "opencode", "log", file.Name()))
					if len(data) > 5000 {
						data = data[len(data)-5000:]
					}
					t.Logf("isolated native log: %s", data)
				}
				t.Fatal(err)
			}
			if withClient {
				if len(result.ToolCalls) != 1 || result.ToolCalls[0].ID != "call_open_client" || result.ToolCalls[0].Function.Name != "read" || result.ToolCalls[0].Function.Arguments != `{"path":"client-only/fixture.txt"}` {
					t.Fatal("corrective native client handoff changed ID or input", result)
				}
				if result.Text != "NATIVE_PREFIX " || streamed.String() != result.Text {
					t.Fatal("native client correction lost streamed prefix", result.Text, streamed.String())
				}
			} else if result.Text != "NATIVE_PREFIX NATIVE_CORRECTED" || streamed.String() != result.Text {
				t.Fatal("native text correction lost or duplicated prefix", result.Text, streamed.String())
			}
			mu.Lock()
			defer mu.Unlock()
			requests := []nativeWire{}
			for _, request := range wire {
				if request.session == result.SessionID {
					requests = append(requests, request)
				}
			}
			if len(requests) != 2 {
				t.Fatalf("native corrective provider phases=%d, want 2", len(requests))
			}
			for _, request := range requests {
				present := map[string]bool{}
				for _, name := range request.names {
					present[name] = true
				}
				if !present["bash"] || !present["read"] || !present["task"] {
					t.Error("OpenCode native tool definitions were filtered", request.names)
				}
				if present["ocpool_read"] != withClient {
					t.Error("client tool exposure did not match request", request.names)
				}
			}
			callReplayed, rejectionReplayed, reminderReplayed := false, false, false
			for _, message := range requests[1].messages {
				for _, call := range message.ToolCalls {
					if call.ID == "call_open_internal" && call.Function.Name == "bash" {
						callReplayed = true
					}
				}
				if message.Role == "tool" && message.ToolCallID == "call_open_internal" && bytes.Contains(message.Content, []byte("rejected permission")) {
					rejectionReplayed = true
				}
				if message.Role == "user" && bytes.Contains(message.Content, []byte("preceding OpenCode internal tool request was rejected")) {
					reminderReplayed = true
				}
			}
			if !callReplayed || !rejectionReplayed || !reminderReplayed {
				t.Fatalf("native conversation replay lost correction history: call=%v rejection=%v reminder=%v", callReplayed, rejectionReplayed, reminderReplayed)
			}
			t.Logf("native %s: 2 provider phases, tool definitions retained, rejected bash/result/corrective user history replayed", map[bool]string{false: "text", true: "client_tool"}[withClient])
		})
	}
}

func nativeToolScopeApp(t *testing.T, model, upstream string) (*App, *string) {
	t.Helper()
	s := testStore(t)
	addTestAccount(t, s, "native-tool-scope")
	settings := s.Settings()
	settings.DefaultModel = model
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModels("native-tool-scope", []string{model}); err != nil {
		t.Fatal(err)
	}
	p := newPool(s, t.TempDir(), defaultExecutable())
	t.Cleanup(p.close)
	var nativeProfile string
	p.startFn = func(ctx context.Context, id string) (*Runtime, error) {
		r, err := p.start(ctx, id)
		if err != nil {
			return nil, err
		}
		nativeProfile = filepath.Dir(r.ParamsDir)
		config := map[string]any{"model": model, "small_model": model, "provider": map[string]any{strings.SplitN(model, "/", 2)[0]: map[string]any{
			"npm": "@ai-sdk/openai-compatible", "options": map[string]string{"baseURL": upstream + "/v1", "apiKey": "synthetic-local-key"},
			"models": map[string]any{"mock": map[string]any{"name": "Local tool scope fixture", "tool_call": true, "limit": map[string]int{"context": 32768, "output": 256}}},
		}}}
		// Project configuration is intentionally disabled by the real pool. The
		// native config API writes workspace/config.json, so install the fixture
		// provider in the isolated global config and ask the API to reload it.
		configPath := filepath.Join(nativeProfile, "config", "opencode.json")
		baseRaw, err := os.ReadFile(configPath)
		if err != nil {
			r.close()
			return nil, err
		}
		base := map[string]any{}
		if err = json.Unmarshal(baseRaw, &base); err != nil {
			r.close()
			return nil, err
		}
		for name, value := range config {
			base[name] = value
		}
		raw, _ := json.Marshal(base)
		if err = atomicWrite(configPath, raw); err != nil {
			r.close()
			return nil, err
		}
		request, _ := http.NewRequestWithContext(ctx, "PATCH", r.URL+"/config?directory="+url.QueryEscape(r.Dir), strings.NewReader("{}"))
		request.Header.Set("Authorization", r.authorization)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			r.close()
			return nil, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if err != nil || response.StatusCode != 200 {
			r.close()
			return nil, fmt.Errorf("local provider configuration: HTTP %d: %s: %v", response.StatusCode, data, err)
		}
		return r, nil
	}
	return &App{store: s, pool: p}, &nativeProfile
}
