package app

import (
	"encoding/json"
	api "github.com/acksell/opencode-go-sdk/sdk"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPiResponsesEnvelopeAndHistory(t *testing.T) {
	raw := `{"model":"opencode/big-pickle","stream":true,"store":false,"reasoning":{"effort":"low","summary":"auto"},"include":["reasoning.encrypted_content"],"prompt_cache_key":"pi-session","prompt_cache_retention":"24h","service_tier":"auto","input":[{"role":"developer","content":"help"},{"role":"user","content":[{"type":"input_text","text":"lookup"}]},{"type":"reasoning","id":"rs_test","summary":[]},{"type":"function_call","call_id":"call_one","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_one","output":"FOUND"}]}`
	var req responseRequest
	if e := readJSON(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", strings.NewReader(raw)), &req); e != nil {
		t.Fatal(e)
	}
	messages, e := responseMessages(req.Input, req.Instructions)
	if e != nil {
		t.Fatal(e)
	}
	prompt, _, e := promptMessages(messages)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(prompt, `"tool_call_id":"call_one"`) || !strings.Contains(prompt, "FOUND") {
		t.Fatal(prompt)
	}
}

func TestEnvelopeDecodesCodexExtensionsBeforeCapabilityValidation(t *testing.T) {
	raw := `{"model":"opencode/big-pickle","input":"hello","client_metadata":{"x-codex-installation-id":"test"},"include":["reasoning.encrypted_content"],"text":{"verbosity":"high"},"access_programs":[],"future_field":{"kept_by_client":true}}`
	var req responseRequest
	if _, e := readCompatJSON(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", strings.NewReader(raw)), &req); e != nil {
		t.Fatal(e)
	}
	if string(req.ClientMetadata) != `{"x-codex-installation-id":"test"}` {
		t.Fatalf("client metadata was not accepted: %s", req.ClientMetadata)
	}
	if req.Model != "opencode/big-pickle" || string(req.Input) != `"hello"` {
		t.Fatalf("typed request fields were not decoded: %+v", req)
	}
}

func TestManagementJSONRemainsStrict(t *testing.T) {
	var req struct {
		Name string `json:"name"`
	}
	if e := readJSON(httptest.NewRecorder(), httptest.NewRequest("POST", "/admin", strings.NewReader(`{"name":"x","future_field":true}`)), &req); e == nil {
		t.Fatal("strict management decoder accepted an unknown field")
	}
}

func TestToolChoiceAndCapturedCalls(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]`)
	parallel := false
	p, e := parseTools(raw, json.RawMessage(`{"type":"function","name":"lookup"}`), &parallel)
	if e != nil {
		t.Fatal(e)
	}
	part := &api.Part{Type: "tool", Tool: &api.ToolPart{CallID: "call_native", Tool: "ocpool_lookup", State: &api.ToolState{Status: "error", Error: &api.ToolStateError{Input: map[string]any{}, Error: "The user rejected permission to use this specific tool call."}}}}
	calls, e := p.capture([]*api.Part{part}, map[string]bool{"call_native": true})
	if e != nil || len(calls) != 1 || calls[0].Function.Arguments != "{}" {
		t.Fatal(calls, e)
	}
	if calls[0].ID != "call_native" {
		t.Fatal("native call ID was lost")
	}
	if _, e = p.capture(nil, nil); e == nil {
		t.Fatal("accepted missing required tool")
	}
	if _, ok := p.toolName("ocpool_unlisted"); ok {
		t.Fatal("accepted undeclared tool")
	}
	if _, e = parseTools(raw, json.RawMessage(`{"type":"function","name":"missing"}`), nil); e == nil {
		t.Fatal("accepted unknown tool choice")
	}
}

func TestMCPBridgeSchemasAndExecutionBoundary(t *testing.T) {
	p, e := parseTools(json.RawMessage(`[{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]`), nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	b := &toolBridge{plan: p, token: "test-private-key"}
	call := func(method, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		return w
	}
	if call("tools/list", "").Code != 401 {
		t.Fatal("unauthenticated schema access")
	}
	var result struct {
		Result struct {
			Tools []struct {
				Name   string         `json:"name"`
				Schema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if e = json.Unmarshal(call("tools/list", "Bearer test-private-key").Body.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	if len(result.Result.Tools) != 1 || result.Result.Tools[0].Name != "read" || result.Result.Tools[0].Schema["properties"].(map[string]any)["path"].(map[string]any)["type"] != "string" {
		t.Fatal(result)
	}
	if !strings.Contains(call("tools/call", "Bearer test-private-key").Body.String(), "gateway execution is disabled") {
		t.Fatal("MCP endpoint executed a client tool")
	}
}

func TestClientToolAliasesAndParallelPermissionHandoff(t *testing.T) {
	longName := strings.Repeat("a", 64)
	if len(clientToolPrefix+clientToolAlias(longName)) > 64 {
		t.Fatal("tool alias exceeds provider limit")
	}
	p := &toolPlan{definitions: []functionDefinition{{Name: longName}}, parallel: true, choice: "auto"}
	if got, ok := p.toolName(clientToolPrefix + clientToolAlias(longName)); !ok || got != longName {
		t.Fatal(got, ok)
	}
	parts := []*api.Part{}
	for _, id := range []string{"call_a", "call_b"} {
		parts = append(parts, &api.Part{Type: "tool", Tool: &api.ToolPart{CallID: id, Tool: clientToolPrefix + clientToolAlias(longName), State: &api.ToolState{Status: "error", Error: &api.ToolStateError{Input: map[string]any{"id": id}, Error: "The user rejected permission to use this specific tool call."}}}})
	}
	calls, e := p.capture(parts, map[string]bool{"call_a": true})
	if e != nil || len(calls) != 2 {
		t.Fatal(calls, e)
	}
	p.parallel = false
	if _, e = p.capture(parts, map[string]bool{"call_a": true}); e == nil {
		t.Fatal("parallel_tool_calls=false was ignored")
	}
}

func TestResponsesSSEEmptyLengthAndToolIndexes(t *testing.T) {
	for _, withReasoning := range []bool{false, true} {
		w := httptest.NewRecorder()
		s := responseStream{w: w, enabled: true, id: "resp_test"}
		result := Result{FinishReason: "length"}
		if withReasoning {
			result.Reasoning = "reason"
			if e := s.delta("reasoning", "reason"); e != nil {
				t.Fatal(e)
			}
		}
		output, e := s.finish(result)
		if e != nil {
			t.Fatal(e)
		}
		if len(output) == 0 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatal(w.Result().Header)
		}
		if !strings.Contains(w.Body.String(), "response.created") || !strings.Contains(w.Body.String(), "response.output_item.done") {
			t.Fatal(w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s := responseStream{w: w, enabled: true, id: "resp_tools"}
	call := ToolCall{ID: "call_one", Type: "function"}
	call.Function.Name = "lookup"
	call.Function.Arguments = `{"city":"Tokyo"}`
	output, e := s.finish(Result{ToolCalls: []ToolCall{call}, FinishReason: "tool_calls"})
	if e != nil || len(output) != 1 {
		t.Fatal(output, e)
	}
	sequence := 0
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if e = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); e != nil {
			t.Fatal(e)
		}
		if event["sequence_number"] != float64(sequence) {
			t.Fatal(event)
		}
		sequence++
		if index, ok := event["output_index"]; ok && index != float64(0) {
			t.Fatal(event)
		}
	}
}
