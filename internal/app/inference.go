package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	api "github.com/acksell/opencode-go-sdk/sdk"
	"github.com/acksell/opencode-go-sdk/sdk/core"
)

type Message struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        json.RawMessage `json:"reasoning,omitempty"`
	Name             string          `json:"name,omitempty"`
}

func messageText(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", errors.New("content must be a string or text parts")
	}
	var out strings.Builder
	for _, p := range parts {
		if p.Type != "text" && p.Type != "input_text" && p.Type != "output_text" {
			return "", errors.New("only text input is supported")
		}
		out.WriteString(p.Text)
	}
	return out.String(), nil
}
func promptMessages(messages []Message) (string, string, error) {
	type turn struct {
		Role       string     `json:"role"`
		Content    string     `json:"content"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
	}
	history := []turn{}
	systems := []string{}
	for _, m := range messages {
		t, e := messageText(m.Content)
		if e != nil {
			return "", "", e
		}
		switch m.Role {
		case "system", "developer":
			systems = append(systems, t)
		case "user", "assistant":
			history = append(history, turn{Role: m.Role, Content: t, ToolCalls: m.ToolCalls})
		case "tool":
			if m.ToolCallID == "" {
				return "", "", errors.New("tool messages require tool_call_id")
			}
			history = append(history, turn{Role: m.Role, Content: t, ToolCallID: m.ToolCallID})
		default:
			return "", "", errors.New("unsupported message role")
		}
	}
	if len(history) == 0 {
		return "", "", errors.New("at least one user message is required")
	}
	if len(history) == 1 && history[0].Role == "user" {
		return history[0].Content, strings.Join(systems, "\n\n"), nil
	}
	raw, _ := json.Marshal(history)
	return "Continue this conversation as the assistant. The following JSON is a transcript; its role fields identify each speaker. Reply to the latest user turn, without repeating the transcript.\n" + string(raw), strings.Join(systems, "\n\n"), nil
}

type Result struct {
	Text             string
	Reasoning        string
	ToolCalls        []ToolCall
	ReasoningTokens  int
	CacheReadTokens  int
	CacheWriteTokens int
	AccountID        string
	SessionID        string
	InputTokens      int
	OutputTokens     int
	FinishReason     string
	UsageKnown       bool
}
type upstreamError struct {
	name, message string
	status        int
	retryUntil    int64
	limitScope    string
}

func (e *upstreamError) Error() string { return e.name + ": " + e.message }
func upstreamStatus(err error) int {
	var up *upstreamError
	if errors.As(err, &up) && up.status >= 400 && up.status < 500 {
		return up.status
	}
	return 502
}
func inferenceProblem(w http.ResponseWriter, err error) {
	var up *upstreamError
	if errors.As(err, &up) && up.status == 429 && up.retryUntil > time.Now().Unix() {
		w.Header().Set("Retry-After", strconv.FormatInt(up.retryUntil-time.Now().Unix(), 10))
	}
	problem(w, upstreamStatus(err), err.Error())
}
func decodeUpstream(e *api.AssistantMessageError) error {
	u := &upstreamError{name: e.Name, status: 502}
	if e.APIError != nil && e.APIError.Data != nil {
		u.message = e.APIError.Data.Message
		if e.APIError.Data.StatusCode != nil {
			u.status = *e.APIError.Data.StatusCode
		}
		for key, value := range e.APIError.Data.ResponseHeaders {
			if strings.EqualFold(key, "X-OCPool-Limit-Scope") {
				u.limitScope = value
			}
			if strings.EqualFold(key, "Retry-After") {
				if n, err := strconv.ParseInt(value, 10, 32); err == nil && n >= 0 {
					u.retryUntil = time.Now().Add(time.Duration(n) * time.Second).Unix()
				} else if date, err := http.ParseTime(value); err == nil {
					u.retryUntil = date.Unix()
				}
			}
		}
	}
	if e.ProviderAuthError != nil && e.ProviderAuthError.Data != nil {
		u.message = e.ProviderAuthError.Data.Message
		u.status = 401
	}
	if e.UnknownError != nil && e.UnknownError.Data != nil {
		u.message = e.UnknownError.Data.Message
	}
	if e.StructuredOutputError != nil && e.StructuredOutputError.Data != nil {
		u.message = e.StructuredOutputError.Data.Message
	}
	return u
}
func (a *App) generate(ctx context.Context, model string, messages []Message, account, session string, retain bool, opts GenerationOptions, emit func(string) error) (out Result, err error) {
	out.FinishReason = "stop"
	modelParts := strings.SplitN(model, "/", 2)
	if a.egress != nil && len(modelParts) == 2 {
		if until := a.egress.limitedUntil(modelParts[1]); until > time.Now().Unix() {
			return out, &upstreamError{name: "RateLimitError", message: "利用できる出口はIP制限の休止中です", status: 429, retryUntil: until, limitScope: "egress"}
		}
	}
	prompt, system, err := promptMessages(messages)
	if err != nil {
		return out, err
	}
	system = "This is an API conversation. Answer in the response text, following the client's instructions. Do not use OpenCode's built-in filesystem, shell, task or network tools. A request to print or output text means to write that text in your answer. Use the client tools in the ocpool MCP namespace when client tools are provided.\n\n" + system
	queueCtx, cancelQueue := context.WithTimeout(ctx, time.Duration(a.store.Settings().QueueSeconds)*time.Second)
	r, err := a.pool.acquire(queueCtx, model, account, ctx)
	cancelQueue()
	if err != nil {
		return out, err
	}
	healthy := false
	defer func() { a.pool.release(r, healthy) }()
	out.AccountID = r.ID
	if opts.Tools != nil {
		cleanup, e := opts.Tools.connect(ctx, r)
		if e != nil {
			return out, e
		}
		defer func() {
			if cleanup() != nil {
				healthy = false
			}
		}()
	}
	rules := nativeToolPermissions(modelParts[0], opts.Tools)
	forked := session != ""
	if !forked {
		s, e := r.Client.Session.Create(ctx, &api.SessionCreateRequest{Directory: &r.Dir, Permission: &rules})
		if e != nil {
			return out, e
		}
		session = s.ID
	} else {
		s, e := r.Client.Session.Fork(ctx, &api.SessionForkRequest{SessionID: session, Directory: &r.Dir})
		if e != nil {
			return out, e
		}
		session = s.ID
	}
	out.SessionID = session
	defer func() {
		if err != nil {
			abortSession(r, session)
		}
		if !retain || err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = r.Client.Session.Delete(cleanup, &api.SessionDeleteRequest{SessionID: session, Directory: &r.Dir})
		}
	}()
	if forked {
		// Forked sessions do not inherit OpenCode permissions. Apply the new
		// request's policy before generating so prior tool grants cannot leak.
		if _, e := r.Client.Session.Update(ctx, &api.SessionUpdateRequest{SessionID: session, Directory: &r.Dir, Permission: &rules}); e != nil {
			return out, e
		}
	}
	cleanupParams, e := opts.prepare(r, session)
	if e != nil {
		return out, e
	}
	defer cleanupParams()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, unsubscribe := r.subscribe(session, cancel)
	defer unsubscribe()
	type answer struct {
		response *api.SessionPromptResponse
		err      error
	}
	done := make(chan answer, 1)
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 {
		return out, errors.New("model must use provider/model")
	}
	request := &api.SessionPromptRequest{SessionID: session, Directory: &r.Dir, Model: &api.SessionPromptRequestModel{ProviderID: parts[0], ModelID: parts[1]}, Parts: []*api.SessionPromptRequestPartsItem{{Type: "text", Text: &api.TextPartInput{Text: prompt}}}}
	agent := "build"
	request.Agent = &agent
	if system != "" {
		request.System = &system
	}
	if opts.Reasoning != nil && opts.Reasoning.Effort != "" {
		request.Variant = &opts.Reasoning.Effort
	}
	if opts.Tools != nil {
		system += opts.Tools.prompt()
		request.System = &system
	}
	startPrompt := func() {
		go func() { v, e := r.Client.Session.Prompt(runCtx, request); done <- answer{v, e} }()
	}
	startPrompt()
	sent := map[string]string{}
	partTypes := map[string]string{}
	messageRoles := map[string]string{}
	deferredTools := map[string]bool{}
	internalDenied := false
	internalTool := ""
	corrections := 0
	committedText := ""
	ignoredMessages := map[string]bool{}
	reasoningSent := map[string]string{}
	var reasoning strings.Builder
	sendReasoning := func(id, text string) error {
		old := reasoningSent[id]
		if !strings.HasPrefix(text, old) {
			return errors.New("upstream reasoning was revised during streaming")
		}
		delta := strings.TrimPrefix(text, old)
		reasoningSent[id] = text
		reasoning.WriteString(delta)
		if delta != "" && opts.ReasoningDelta != nil {
			return opts.ReasoningDelta(delta)
		}
		return nil
	}
	var combined strings.Builder
	var visible strings.Builder
	filter := stopBuffer{patterns: opts.Stop}
	stopReached := errors.New("stop sequence reached")
	usage := func(response *api.SessionPromptResponse) {
		if response != nil && response.Info != nil && response.Info.Tokens != nil {
			out.InputTokens += int(response.Info.Tokens.Input)
			if response.Info.Tokens.Cache != nil {
				out.CacheReadTokens += int(response.Info.Tokens.Cache.Read)
				out.CacheWriteTokens += int(response.Info.Tokens.Cache.Write)
				out.InputTokens += int(response.Info.Tokens.Cache.Read + response.Info.Tokens.Cache.Write)
			}
			out.OutputTokens += int(response.Info.Tokens.Output + response.Info.Tokens.Reasoning)
			out.ReasoningTokens += int(response.Info.Tokens.Reasoning)
			out.UsageKnown = out.InputTokens+out.OutputTokens > 0
		}
	}
	finishStop := func() (Result, error) {
		abortSession(r, session)
		select {
		case answer := <-done:
			usage(answer.response)
			healthy = answer.err == nil
		case <-time.After(2 * time.Second):
		}
		out.Text = visible.String()
		out.Reasoning = reasoning.String()
		if e := opts.verify(r, session); e != nil {
			return out, e
		}
		return out, nil
	}
	send := func(id, text string) error {
		old := sent[id]
		if text == old {
			return nil
		}
		if !strings.HasPrefix(text, old) {
			return errors.New("upstream text was revised during streaming")
		}
		delta := strings.TrimPrefix(text, old)
		sent[id] = text
		combined.WriteString(delta)
		allowed, stopped := filter.feed(delta)
		visible.WriteString(allowed)
		if emit != nil && allowed != "" {
			if e := emit(allowed); e != nil {
				return e
			}
		}
		if stopped {
			return stopReached
		}
		return nil
	}
	for {
		select {
		case <-runCtx.Done():
			return out, runCtx.Err()
		case e := <-events:
			switch e.Type {
			case "ocpool.egress.failure":
				if e.Properties.EgressModel == parts[1] && e.Properties.EgressError != nil {
					healthy = true
					a.markFailure(r.ID, e.Properties.EgressError)
					return out, e.Properties.EgressError
				}
			case "message.updated":
				messageRoles[e.Properties.Info.ID] = e.Properties.Info.Role
			case "permission.asked":
				_, clientTool := opts.Tools.toolName(e.Properties.Permission)
				if clientTool && e.Properties.Tool.CallID != "" {
					deferredTools[e.Properties.Tool.CallID] = true
				}
				id := e.Properties.ID
				if id != "" {
					denyCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
					_, replyErr := r.Client.Permission.Reply(denyCtx, &api.PermissionReplyRequest{RequestID: id, Directory: &r.Dir, Reply: api.PermissionReplyRequestReply("reject")})
					stop()
					if replyErr != nil {
						var apiErr *core.APIError
						if !errors.As(replyErr, &apiErr) || apiErr.StatusCode != 404 {
							return out, replyErr
						}
					}
				}
				if !clientTool {
					internalDenied = true
					internalTool = e.Properties.Permission
				}
			case "question.asked":
				// Native question/plan tools ask the CLI directly rather than
				// using the permission service. Decline those as well so an API
				// request cannot wait indefinitely for a native terminal reply.
				denyCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				_, rejectErr := r.Client.Question.Reject(denyCtx, &api.QuestionRejectRequest{RequestID: e.Properties.ID, Directory: &r.Dir})
				stop()
				if rejectErr != nil {
					var apiErr *core.APIError
					if !errors.As(rejectErr, &apiErr) || apiErr.StatusCode != 404 {
						return out, rejectErr
					}
				}
				internalDenied = true
				internalTool = "question"
			case "message.part.updated":
				part := e.Properties.Part
				if ignoredMessages[part.MessageID] || messageRoles[part.MessageID] != "assistant" {
					continue
				}
				partTypes[part.ID] = part.Type
				if part.Type == "reasoning" {
					if err = sendReasoning(part.ID, part.Text); err != nil {
						return out, err
					}
				}
				if part.Type == "text" {
					if err = send(part.ID, part.Text); err != nil {
						if errors.Is(err, stopReached) {
							return finishStop()
						}
						return out, err
					}
				}
			case "message.part.delta":
				if ignoredMessages[e.Properties.MessageID] {
					continue
				}
				if e.Properties.Field == "text" && partTypes[e.Properties.PartID] == "reasoning" {
					if err = sendReasoning(e.Properties.PartID, reasoningSent[e.Properties.PartID]+e.Properties.Delta); err != nil {
						return out, err
					}
				}
				if e.Properties.Field == "text" && partTypes[e.Properties.PartID] == "text" {
					if err = send(e.Properties.PartID, sent[e.Properties.PartID]+e.Properties.Delta); err != nil {
						if errors.Is(err, stopReached) {
							return finishStop()
						}
						return out, err
					}
				}
			}
		case answer := <-done:
			if answer.err != nil {
				return out, answer.err
			}
			response := answer.response
			if response.Info != nil && response.Info.Error != nil && response.Info.Error.Name != "MessageOutputLengthError" {
				err = decodeUpstream(response.Info.Error)
				a.markFailure(r.ID, err)
				var quota *upstreamError
				if errors.As(err, &quota) && quota.status == 429 && quota.limitScope == "egress" {
					healthy = true
				}
				return out, err
			}
			if e := opts.verify(r, session); e != nil {
				return out, e
			}
			usage(response)
			if response.Info != nil {
				if (response.Info.Finish != nil && *response.Info.Finish == "length") || (response.Info.Error != nil && response.Info.Error.Name == "MessageOutputLengthError") {
					out.FinishReason = "length"
				}
			}
			var text strings.Builder
			for _, part := range response.Parts {
				if part.Reasoning != nil {
					if err = sendReasoning(part.Reasoning.ID, part.Reasoning.Text); err != nil {
						return out, err
					}
				}
				if part.Text != nil {
					text.WriteString(part.Text.Text)
				}
			}
			fullText := committedText + text.String()
			out.Reasoning = reasoning.String()
			if internalDenied && len(deferredTools) == 0 && out.FinishReason != "length" {
				if corrections >= 2 {
					return out, fmt.Errorf("the model repeatedly requested internal tool %q instead of a declared client tool or response text", clip(internalTool, 64))
				}
				already := combined.String()
				if !strings.HasPrefix(fullText, already) {
					return out, errors.New("stream and corrective response do not match")
				}
				remaining := strings.TrimPrefix(fullText, already)
				allowed, stopped := filter.feed(remaining)
				visible.WriteString(allowed)
				if emit != nil && allowed != "" {
					if e := emit(allowed); e != nil {
						return out, e
					}
				}
				if stopped {
					out.Text = visible.String()
					healthy = true
					return out, nil
				}
				combined.WriteString(remaining)
				committedText = fullText
				if opts.MaxOutputTokens != nil {
					budget := *opts.MaxOutputTokens - out.OutputTokens
					if budget <= 0 {
						tail := filter.flush()
						visible.WriteString(tail)
						if emit != nil && tail != "" {
							if e := emit(tail); e != nil {
								return out, e
							}
						}
						out.Text = visible.String()
						out.FinishReason = "length"
						healthy = true
						return out, nil
					}
					phaseOptions := opts
					phaseOptions.MaxOutputTokens = &budget
					if _, e := phaseOptions.prepare(r, session); e != nil {
						return out, e
					}
				}
				if response.Info != nil {
					ignoredMessages[response.Info.ID] = true
				}
				corrections++
				reminder := "The preceding OpenCode internal tool request was rejected. Finish the original API user's request. Do not call OpenCode's internal tools again. "
				if opts.Tools == nil {
					reminder += "No client tools are available. Write the requested answer directly in the response text."
				} else {
					reminder += "If a tool is needed, call only a declared tool in the ocpool MCP namespace, using its client schema and the exact client paths." + opts.Tools.prompt()
				}
				request.Parts = []*api.SessionPromptRequestPartsItem{{Type: "text", Text: &api.TextPartInput{Text: reminder}}}
				internalDenied = false
				deferredTools = map[string]bool{}
				startPrompt()
				continue
			}
			if opts.Tools != nil && out.FinishReason != "length" {
				out.ToolCalls, err = opts.Tools.capture(response.Parts, deferredTools)
				if err != nil {
					return out, err
				}
				if len(out.ToolCalls) > 0 {
					out.FinishReason = "tool_calls"
				}
			}
			already := combined.String()
			if !strings.HasPrefix(fullText, already) {
				if emit != nil {
					return out, errors.New("stream and final response do not match")
				}
				filter = stopBuffer{patterns: opts.Stop}
				visible.Reset()
				already = ""
			}
			allowed, stopped := filter.feed(strings.TrimPrefix(fullText, already))
			allowed += filter.flush()
			visible.WriteString(allowed)
			if emit != nil && allowed != "" {
				if err = emit(allowed); err != nil {
					return out, err
				}
			}
			out.Text = visible.String()
			if stopped {
				out.FinishReason = "stop"
			}
			if out.Text == "" && len(out.ToolCalls) == 0 && out.FinishReason != "length" && !filter.stopped {
				return out, errors.New("upstream returned an empty response")
			}
			healthy = true
			a.pool.mu.Lock()
			a.pool.failures = 0
			a.pool.blockedUntil = time.Time{}
			a.pool.mu.Unlock()
			_, _ = a.store.db.Exec("UPDATE accounts SET last_error='' WHERE id=?", r.ID)
			return out, nil
		}
	}
}
func (a *App) markFailure(id string, err error) {
	var up *upstreamError
	if !errors.As(err, &up) {
		return
	}
	status := "ready"
	cooldown := int64(0)
	switch up.status {
	case 401:
		status = "login_required"
	case 402:
		status = "exhausted"
	case 429:
		if up.limitScope == "egress" {
			return
		}
		cooldown = time.Now().Add(time.Minute).Unix()
		if up.retryUntil > time.Now().Unix() {
			cooldown = up.retryUntil
		}
	}
	_, _ = a.store.db.Exec("UPDATE accounts SET status=?,cooldown_until=?,last_error=?,updated_at=? WHERE id=?", status, cooldown, clip(up.Error(), 1000), time.Now().Unix(), id)
	if up.status >= 500 {
		a.pool.mu.Lock()
		a.pool.failures++
		if a.pool.failures >= 3 {
			a.pool.blockedUntil = time.Now().Add(10 * time.Second)
		}
		a.pool.mu.Unlock()
	}
}

type chatRequest struct {
	ClientHints
	Model         string    `json:"model"`
	Messages      []Message `json:"messages"`
	Stream        bool      `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools               json.RawMessage   `json:"tools"`
	ToolChoice          json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls   *bool             `json:"parallel_tool_calls"`
	Reasoning           *ReasoningOptions `json:"reasoning"`
	ReasoningEffort     string            `json:"reasoning_effort"`
	Store               *bool             `json:"store"`
	Temperature         *float64          `json:"temperature"`
	TopP                *float64          `json:"top_p"`
	MaxTokens           *int              `json:"max_tokens"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	N                   *int              `json:"n"`
	Stop                json.RawMessage   `json:"stop"`
}

// A heartbeat and the generation callback share one response writer. Serializing
// complete SSE frames prevents comments from splitting an event or its JSON.
type sseStreamWriter struct {
	w      http.ResponseWriter
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newSSEStreamWriter(ctx context.Context, w http.ResponseWriter, cancel context.CancelFunc, interval time.Duration) *sseStreamWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	s := &sseStreamWriter{w: w, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case <-ticker.C:
				if s.frame(": keep-alive\n\n") != nil {
					return
				}
			}
		}
	}()
	return s
}

func (s *sseStreamWriter) write(write func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if err := write(); err != nil {
		s.err = err
		s.cancel()
	}
	return s.err
}

func (s *sseStreamWriter) event(name string, value any) error {
	return s.write(func() error { return sendSSE(s.w, name, value) })
}

func (s *sseStreamWriter) frame(value string) error {
	return s.write(func() error {
		_ = http.NewResponseController(s.w).SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprint(s.w, value); err != nil {
			return err
		}
		return http.NewResponseController(s.w).Flush()
	})
}

func (s *sseStreamWriter) stopAndJoin() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

func inferenceStreamError(err error) map[string]any {
	status := upstreamStatus(err)
	typeName, code := "api_error", "upstream_error"
	switch status {
	case 400, 422:
		typeName, code = "invalid_request_error", "upstream_invalid_request"
	case 401:
		typeName, code = "authentication_error", "upstream_authentication_failed"
	case 403:
		typeName, code = "permission_error", "upstream_permission_denied"
	case 429:
		typeName, code = "rate_limit_error", "rate_limit_exceeded"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		code = "request_timeout"
	} else if errors.Is(err, context.Canceled) {
		code = "request_canceled"
	}
	detail := map[string]any{"message": err.Error(), "type": typeName, "code": code, "status": status}
	var up *upstreamError
	if errors.As(err, &up) && up.retryUntil > time.Now().Unix() {
		detail["retry_after"] = up.retryUntil - time.Now().Unix()
	}
	return map[string]any{"error": detail}
}

func (a *App) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := readJSON(w, r, &req); err != nil {
		problem(w, 400, err.Error())
		return
	}
	toolPlan, toolError := parseTools(req.Tools, req.ToolChoice, req.ParallelToolCalls)
	if toolError != nil {
		problem(w, 400, toolError.Error())
		return
	}
	maximum := req.MaxCompletionTokens
	if maximum == nil {
		maximum = req.MaxTokens
	}
	if req.MaxTokens != nil && req.MaxCompletionTokens != nil && *req.MaxTokens != *req.MaxCompletionTokens {
		problem(w, 400, "max_tokens and max_completion_tokens must agree when both are provided")
		return
	}
	stops, e := parseStop(req.Stop)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	if req.ReasoningEffort != "" {
		if req.Reasoning != nil && req.Reasoning.Effort != "" && req.Reasoning.Effort != req.ReasoningEffort {
			problem(w, 400, "reasoning and reasoning_effort must agree")
			return
		}
		if req.Reasoning == nil {
			req.Reasoning = &ReasoningOptions{}
		}
		req.Reasoning.Effort = req.ReasoningEffort
	}
	opts := GenerationOptions{Temperature: req.Temperature, TopP: req.TopP, MaxOutputTokens: maximum, Stop: stops, Reasoning: req.Reasoning, Tools: toolPlan}
	if e = opts.validate(); e != nil {
		problem(w, 400, e.Error())
		return
	}
	count := 1
	if req.N != nil {
		count = *req.N
	}
	if count < 1 || count > 8 {
		problem(w, 400, "n must be between 1 and 8")
		return
	}
	if req.StreamOptions != nil && !req.Stream {
		problem(w, 400, "stream_options requires stream=true")
		return
	}
	if req.Model == "" {
		req.Model = a.store.Settings().DefaultModel
	}
	if provider, model, ok := strings.Cut(req.Model, "/"); !ok || provider == "" || model == "" {
		problem(w, 400, "model must use provider/model")
		return
	}
	if _, _, err := promptMessages(req.Messages); err != nil {
		problem(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(a.store.Settings().RequestSeconds)*time.Second)
	defer cancel()
	id := newID("chatcmpl")
	started := time.Now()
	created := started.Unix()
	var stream *sseStreamWriter
	if req.Stream {
		stream = newSSEStreamWriter(ctx, w, cancel, 10*time.Second)
		defer stream.stopAndJoin()
	}
	choiceIndex := 0
	chunk := func(delta map[string]any, finish any) error {
		payload := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{map[string]any{"index": choiceIndex, "delta": delta, "finish_reason": finish}}}
		if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
			payload["usage"] = nil
		}
		return stream.event("", payload)
	}
	choices := []any{}
	totalInput, totalOutput, totalReasoning, totalCacheRead, totalCacheWrite := 0, 0, 0, 0, 0
	known := true
	lastAccount := ""
	for choiceIndex = 0; choiceIndex < count; choiceIndex++ {
		var emit func(string) error
		if req.Stream {
			// Flush the role before cold worker startup, queuing, or provider
			// prefill. Some clients otherwise treat a long first delta as a dead
			// connection and cancel an otherwise healthy reasoning request.
			if err := chunk(map[string]any{"role": "assistant"}, nil); err != nil {
				return
			}
			opts.ReasoningDelta = func(text string) error {
				return chunk(map[string]any{"reasoning_content": text}, nil)
			}
			emit = func(text string) error {
				return chunk(map[string]any{"content": text}, nil)
			}
		}
		result, err := a.generate(ctx, req.Model, req.Messages, "", "", false, opts, emit)
		lastAccount = result.AccountID
		if err != nil {
			a.recordRequest(id, lastAccount, req.Model, upstreamStatus(err), time.Since(started), err)
			if req.Stream {
				stream.stopAndJoin()
				_ = stream.event("error", inferenceStreamError(err))
				_ = stream.frame("data: [DONE]\n\n")
			} else {
				inferenceProblem(w, err)
			}
			return
		}
		totalInput += result.InputTokens
		totalOutput += result.OutputTokens
		totalReasoning += result.ReasoningTokens
		totalCacheRead += result.CacheReadTokens
		totalCacheWrite += result.CacheWriteTokens
		known = known && result.UsageKnown
		message := map[string]any{"role": "assistant", "content": result.Text}
		if result.Reasoning != "" {
			message["reasoning_content"] = result.Reasoning
		}
		if len(result.ToolCalls) > 0 {
			message["tool_calls"] = result.ToolCalls
		}
		choices = append(choices, map[string]any{"index": choiceIndex, "message": message, "finish_reason": result.FinishReason})
		if req.Stream {
			for i, call := range result.ToolCalls {
				if err = chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": call.ID, "type": "function", "function": call.Function}}}, nil); err != nil {
					return
				}
			}
			if err = chunk(map[string]any{}, result.FinishReason); err != nil {
				return
			}
		}
	}
	a.recordRequest(id, lastAccount, req.Model, 200, time.Since(started), nil)
	var usage any
	if known {
		usage = map[string]any{"prompt_tokens": totalInput, "completion_tokens": totalOutput, "total_tokens": totalInput + totalOutput, "prompt_tokens_details": map[string]int{"cached_tokens": totalCacheRead, "cache_write_tokens": totalCacheWrite}, "completion_tokens_details": map[string]int{"reasoning_tokens": totalReasoning}}
	}
	if req.Stream {
		stream.stopAndJoin()
		if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
			if stream.event("", map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []any{}, "usage": usage}) != nil {
				return
			}
		}
		_ = stream.frame("data: [DONE]\n\n")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "object": "chat.completion", "created": created, "model": req.Model, "choices": choices, "usage": usage})
}
func sendSSE(w http.ResponseWriter, name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
	if name != "" {
		if _, err = fmt.Fprintf(w, "event: %s\n", name); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}
func (a *App) recordRequest(id, account, model string, status int, d time.Duration, err error) {
	message := ""
	if err != nil {
		message = clip(err.Error(), 1000)
	}
	_, _ = a.store.db.Exec("INSERT INTO requests(id,account_id,model,status,milliseconds,created_at,error) VALUES(?,?,?,?,?,?,?)", id, account, model, status, d.Milliseconds(), time.Now().Unix(), message)
}
