package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type responseRequest struct {
	ClientHints
	Model             string            `json:"model"`
	Input             json.RawMessage   `json:"input"`
	Instructions      string            `json:"instructions"`
	Previous          string            `json:"previous_response_id"`
	Stream            bool              `json:"stream"`
	Store             *bool             `json:"store"`
	Tools             json.RawMessage   `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Reasoning         *ReasoningOptions `json:"reasoning"`
	Temperature       *float64          `json:"temperature"`
	TopP              *float64          `json:"top_p"`
	MaxOutputTokens   *int              `json:"max_output_tokens"`
}

func responseMessages(raw json.RawMessage, instructions string) ([]Message, error) {
	out := []Message{}
	if instructions != "" {
		b, _ := json.Marshal(instructions)
		out = append(out, Message{Role: "system", Content: b})
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		b, _ := json.Marshal(plain)
		return append(out, Message{Role: "user", Content: b}), nil
	}
	var list []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil, errors.New("input must be text or a list of text messages")
	}
	for _, item := range list {
		if item.Type == "reasoning" {
			continue
		}
		if item.Type == "function_call" {
			if item.CallID == "" || !functionName.MatchString(item.Name) || !json.Valid([]byte(item.Arguments)) {
				return nil, errors.New("invalid function_call input")
			}
			call := ToolCall{ID: item.CallID, Type: "function"}
			call.Function.Name = item.Name
			call.Function.Arguments = item.Arguments
			out = append(out, Message{Role: "assistant", Content: json.RawMessage(`null`), ToolCalls: []ToolCall{call}})
			continue
		}
		if item.Type == "function_call_output" {
			if item.CallID == "" {
				return nil, errors.New("function_call_output requires call_id")
			}
			out = append(out, Message{Role: "tool", Content: item.Output, ToolCallID: item.CallID})
			continue
		}
		if item.Type != "" && item.Type != "message" {
			return nil, errors.New("only message input is supported")
		}
		out = append(out, Message{Role: item.Role, Content: item.Content})
	}
	return out, nil
}
func (a *App) responses(w http.ResponseWriter, r *http.Request) {
	var req responseRequest
	if e := readJSON(w, r, &req); e != nil {
		problem(w, 400, e.Error())
		return
	}
	plan, e := parseTools(req.Tools, req.ToolChoice, req.ParallelToolCalls)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	opts := GenerationOptions{Temperature: req.Temperature, TopP: req.TopP, MaxOutputTokens: req.MaxOutputTokens, Reasoning: req.Reasoning, Tools: plan}
	if e := opts.validate(); e != nil {
		problem(w, 400, e.Error())
		return
	}
	messages, e := responseMessages(req.Input, req.Instructions)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	if _, _, e = promptMessages(messages); e != nil {
		problem(w, 400, e.Error())
		return
	}
	if req.Model == "" && req.Previous == "" {
		req.Model = a.store.Settings().DefaultModel
	}
	account, session := "", ""
	keyID, _ := r.Context().Value(keyContext{}).(string)
	if req.Previous != "" {
		var model string
		e = a.store.db.QueryRow("SELECT account_id,session_id,model FROM responses WHERE id=? AND expires_at>? AND key_id=?", req.Previous, time.Now().Unix(), keyID).Scan(&account, &session, &model)
		if e != nil {
			problem(w, 404, "previous response not found or expired")
			return
		}
		if req.Model == "" {
			req.Model = model
		}
		if model != req.Model {
			problem(w, 400, "model must match the previous response")
			return
		}
	}
	if provider, model, ok := strings.Cut(req.Model, "/"); !ok || provider == "" || model == "" {
		problem(w, 400, "model must use provider/model")
		return
	}
	id := newID("resp")
	created := time.Now().Unix()
	store := req.Store == nil || *req.Store
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(a.store.Settings().RequestSeconds)*time.Second)
	defer cancel()
	sse := responseStream{w: w, enabled: req.Stream, id: id, model: req.Model, created: created}
	var emit func(string) error
	if req.Stream {
		sse.writer = newSSEStreamWriter(ctx, w, cancel, 10*time.Second)
		defer sse.writer.stopAndJoin()
		if e := sse.start(); e != nil {
			return
		}
		emit = func(delta string) error { return sse.delta("message", delta) }
		opts.ReasoningDelta = func(delta string) error { return sse.delta("reasoning", delta) }
	}
	begin := time.Now()
	result, e := a.generate(ctx, req.Model, messages, account, session, store, opts, emit)
	status := 200
	if e != nil {
		status = upstreamStatus(e)
	}
	a.recordRequest(id, result.AccountID, req.Model, status, time.Since(begin), e)
	if e != nil {
		if req.Stream {
			sse.writer.stopAndJoin()
			detail := inferenceStreamError(e)["error"].(map[string]any)
			if status >= 500 {
				detail["code"] = "server_error"
			}
			_ = sse.event("response.failed", map[string]any{"response": map[string]any{"id": id, "object": "response", "created_at": created, "model": req.Model, "status": "failed", "output": []any{}, "error": detail}})
		} else {
			inferenceProblem(w, e)
		}
		return
	}
	output, e := sse.finish(result)
	if e != nil {
		return
	}
	body := map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": req.Model, "output": output, "output_text": result.Text, "previous_response_id": req.Previous, "usage": map[string]any{"input_tokens": result.InputTokens, "output_tokens": result.OutputTokens, "total_tokens": result.InputTokens + result.OutputTokens, "input_tokens_details": map[string]int{"cached_tokens": result.CacheReadTokens, "cache_write_tokens": result.CacheWriteTokens}, "output_tokens_details": map[string]int{"reasoning_tokens": result.ReasoningTokens}}}
	if result.FinishReason == "length" {
		body["status"] = "incomplete"
		body["incomplete_details"] = map[string]string{"reason": "max_output_tokens"}
	}
	if store {
		raw, _ := json.Marshal(body)
		_, e = a.store.db.Exec("INSERT INTO responses(id,account_id,session_id,model,body,expires_at,key_id) VALUES(?,?,?,?,?,?,?)", id, result.AccountID, result.SessionID, req.Model, string(raw), time.Now().Add(30*time.Minute).Unix(), keyID)
		if e != nil {
			if req.Stream {
				sse.writer.stopAndJoin()
				_ = sse.event("response.failed", map[string]any{"response": map[string]any{"id": id, "object": "response", "created_at": created, "model": req.Model, "status": "failed", "output": output, "error": map[string]any{"code": "server_error", "type": "api_error", "status": 500, "message": "response persistence failed"}}})
			} else {
				problem(w, 500, "response persistence failed")
			}
			return
		}
	}
	if req.Stream {
		sse.writer.stopAndJoin()
		eventName := "response.completed"
		if result.FinishReason == "length" {
			eventName = "response.incomplete"
		}
		_ = sse.event(eventName, map[string]any{"response": body})
		return
	}
	writeJSON(w, 200, body)
}
func (a *App) getResponse(w http.ResponseWriter, r *http.Request) {
	key, _ := r.Context().Value(keyContext{}).(string)
	var raw string
	if a.store.db.QueryRow("SELECT body FROM responses WHERE id=? AND expires_at>? AND key_id=?", r.PathValue("id"), time.Now().Unix(), key).Scan(&raw) != nil {
		problem(w, 404, "response not found or expired")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(raw))
}
