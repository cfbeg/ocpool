package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

// Raw bytes never become model input, logs, or database records. Keeping them
// separate from the typed request prevents normalization from losing fields.
// Ownership is confined to one HTTP request, including its streaming lifetime.
type requestEnvelope struct {
	raw    json.RawMessage
	fields map[string]json.RawMessage
	report adapterReport
}

type envelopeContext struct{}

func envelopeFromContext(ctx context.Context) *requestEnvelope {
	e, _ := ctx.Value(envelopeContext{}).(*requestEnvelope)
	return e
}

func readCompatJSON(w http.ResponseWriter, r *http.Request, v any) (*requestEnvelope, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, errors.New("body must contain exactly one valid UTF-8 JSON object")
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	check.UseNumber()
	if err := uniqueJSONMembers(check, 0); err != nil {
		return nil, err
	}
	e := &requestEnvelope{raw: raw}
	if err := json.Unmarshal(raw, &e.fields); err != nil || e.fields == nil {
		return nil, errors.New("body must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		// Do not echo invalid values from a client request into diagnostics.
		return nil, errors.New("request field has an invalid JSON type")
	}
	return e, nil
}

func compatDecodeProblem(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	problem(w, status, err.Error())
}

// A duplicate member would otherwise be overwritten before the adapter can
// classify it. Check even opaque metadata and JSON Schemas for ambiguity.
func uniqueJSONMembers(d *json.Decoder, depth int) error {
	if depth > 128 {
		return errors.New("JSON nesting exceeds 128 levels")
	}
	token, err := d.Token()
	if err != nil {
		return errors.New("invalid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return errors.New("invalid JSON object")
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON member")
			}
			seen[name] = true
		}
		if err := uniqueJSONMembers(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

type fieldDecision struct {
	Param  string `json:"param"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

type adapterReport struct {
	Route     string          `json:"route"`
	Decisions []fieldDecision `json:"fields"`
	invalid   bool
}

func (a *adapterReport) add(path, action, reason string) {
	a.Decisions = append(a.Decisions, fieldDecision{path, action, reason})
	if action == "invalid" {
		a.invalid = true
	}
}

func (a *adapterReport) unsupported(path, reason string) {
	a.add(path, "unsupported", reason)
}

// Header names/values are encoded and bounded. Neither field values nor
// arbitrary JSON can be injected into HTTP headers or error descriptions.
func (a *adapterReport) writeHeaders(w http.ResponseWriter) {
	w.Header().Set("X-OCPool-Adapter-Route", a.Route)
	var entries []string
	size := 0
	for _, d := range a.Decisions {
		entry := url.QueryEscape(d.Param) + "=" + d.Action
		if size+len(entry)+2 > 4096 {
			w.Header().Set("X-OCPool-Adapter-Truncated", "true")
			break
		}
		entries = append(entries, entry)
		size += len(entry) + 2
	}
	w.Header().Set("X-OCPool-Adapter", strings.Join(entries, ", "))
}

func (a *adapterReport) accept(w http.ResponseWriter) bool {
	sort.Slice(a.Decisions, func(i, j int) bool { return a.Decisions[i].Param < a.Decisions[j].Param })
	a.writeHeaders(w)
	issues := []fieldDecision{}
	for _, d := range a.Decisions {
		if d.Action == "unsupported" || d.Action == "invalid" {
			issues = append(issues, d)
		}
	}
	if len(issues) == 0 {
		return true
	}
	status, code := http.StatusUnprocessableEntity, "unsupported_parameter"
	message := "The OpenCode session adapter cannot preserve the requested semantics; see unsupported_parameters."
	if a.invalid {
		status, code = http.StatusBadRequest, "invalid_parameter"
		message = "A request field has an invalid JSON shape; see unsupported_parameters."
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"type": "invalid_request_error", "code": code, "param": issues[0].Param,
		"message": message, "unsupported_parameters": issues, "adapter": a,
	}})
	return false
}

type fieldRule func(*adapterReport, string, json.RawMessage)
type fieldRules map[string]fieldRule

func disposition(action string) fieldRule {
	return func(a *adapterReport, path string, _ json.RawMessage) { a.add(path, action, "") }
}

var mappedField = disposition("mapped")
var localField = disposition("retained-local")
var modelField = disposition("mapped-model-dependent")

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func objectRule(fields fieldRules) fieldRule {
	return func(a *adapterReport, path string, raw json.RawMessage) {
		if isNull(raw) {
			a.add(path, "mapped-default", "")
			return
		}
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil || values == nil {
			a.add(path, "invalid", "Expected an object.")
			return
		}
		if len(values) == 0 {
			a.add(path, "mapped-default", "")
		}
		for key, value := range values {
			name := key
			if path != "" {
				name = path + "." + key
			}
			if rule, ok := fields[key]; ok {
				rule(a, name, value)
			} else {
				a.unsupported(name, "No mapping is defined for this field.")
			}
		}
	}
}

func arrayRule(item fieldRule) fieldRule {
	return func(a *adapterReport, path string, raw json.RawMessage) {
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			a.add(path, "invalid", "Expected an array.")
			return
		}
		if len(values) == 0 {
			a.add(path, "mapped-default", "")
		}
		for i, value := range values {
			item(a, fmt.Sprintf("%s[%d]", path, i), value)
		}
	}
}

func emptyOnly(reason string) fieldRule {
	return func(a *adapterReport, path string, raw json.RawMessage) {
		if isNull(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte(`""`)) {
			a.add(path, "mapped-default", "")
			return
		}
		a.unsupported(path, reason)
	}
}

func stringChoice(allowed ...string) fieldRule {
	return func(a *adapterReport, path string, raw json.RawMessage) {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			a.add(path, "invalid", "Expected a string.")
			return
		}
		if isNull(raw) {
			a.add(path, "mapped-default", "")
			return
		}
		for _, choice := range allowed {
			if value == choice {
				a.add(path, "mapped", "")
				return
			}
		}
		a.unsupported(path, "This value is not supported by the OpenCode session adapter.")
	}
}

func falseOnly(reason string) fieldRule {
	return func(a *adapterReport, path string, raw json.RawMessage) {
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			a.add(path, "invalid", "Expected a boolean.")
		} else if value {
			a.unsupported(path, reason)
		} else {
			a.add(path, "mapped-default", "")
		}
	}
}

// Capabilities describe the implemented downstream route, not an inference
// from a model's name. All currently registered models use Session.Prompt;
// claiming native Responses support here would reintroduce silent loss.
func sessionRules(responses bool) fieldRules {
	rules := fieldRules{
		"model": mappedField, "stream": mappedField, "temperature": mappedField,
		"top_p": mappedField, "tools": arrayRule(toolRule), "tool_choice": toolChoiceRule,
		"parallel_tool_calls": disposition("mapped-validated"),
		"client_metadata":     localField, "metadata": localField, "user": localField,
		"safety_identifier": localField, "prompt_cache_key": disposition("mapped-account-affinity"),
		"prompt_cache_retention": emptyOnly("The upstream cache lifetime cannot be configured through Session.Prompt."),
		"prompt_cache_options":   objectRule(fieldRules{}),
		"service_tier":           stringChoice("", "auto"),
		"include": arrayRule(func(a *adapterReport, path string, _ json.RawMessage) {
			a.unsupported(path, "The session route cannot return native Responses include data, including encrypted reasoning or logprobs.")
		}),
		"reasoning": objectRule(fieldRules{"effort": modelField, "summary": modelField}),
	}
	if responses {
		rules["input"] = inputRule
		rules["instructions"] = mappedField
		rules["previous_response_id"] = disposition("mapped-session-fork")
		rules["store"] = disposition("mapped-local-store")
		rules["metadata"] = disposition("mapped-response-metadata")
		rules["max_output_tokens"] = mappedField
		rules["text"] = objectRule(fieldRules{
			"format":    objectRule(fieldRules{"type": stringChoice("text")}),
			"verbosity": emptyOnly("No text verbosity mapping is available on the session route."),
		})
		rules["background"] = falseOnly("Background Responses jobs are not implemented.")
		rules["access_programs"] = arrayRule(func(a *adapterReport, path string, _ json.RawMessage) {
			a.unsupported(path, "Access programs cannot be represented by an OpenCode session.")
		})
	} else {
		rules["messages"] = arrayRule(chatMessageRule)
		rules["store"] = falseOnly("Chat Completions storage is not implemented; use Responses with store=true.")
		rules["max_tokens"], rules["max_completion_tokens"] = mappedField, mappedField
		rules["n"], rules["stop"] = mappedField, mappedField
		rules["reasoning_effort"] = modelField
		rules["stream_options"] = objectRule(fieldRules{"include_usage": mappedField})
		rules["response_format"] = objectRule(fieldRules{"type": stringChoice("text")})
	}
	return rules
}

func (e *requestEnvelope) adapt(w http.ResponseWriter, r *http.Request, responses bool) (*http.Request, bool) {
	e.report = adapterReport{Route: "opencode-session"}
	objectRule(sessionRules(responses))(&e.report, "", e.raw)
	r = r.WithContext(context.WithValue(r.Context(), envelopeContext{}, e))
	return r, e.report.accept(w)
}

func contentRule(a *adapterReport, path string, raw json.RawMessage) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		mappedField(a, path, raw)
		return
	}
	arrayRule(objectRule(fieldRules{
		"type": stringChoice("text", "input_text", "output_text"), "text": mappedField,
		"annotations": localField, "logprobs": localField,
	}))(a, path, raw)
}

func chatMessageRule(a *adapterReport, path string, raw json.RawMessage) {
	var message struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(raw, &message) // Typed request decoding validates the shape.
	fields := fieldRules{
		"role": mappedField, "content": contentRule, "tool_call_id": mappedField,
		"name": mappedField, "phase": mappedField, "reasoning_content": localField,
		"reasoning": localField,
		"tool_calls": arrayRule(objectRule(fieldRules{
			"id": mappedField, "type": stringChoice("function"),
			"function": objectRule(fieldRules{"name": mappedField, "arguments": mappedField}),
		})),
	}
	if message.Role != "assistant" {
		fields["phase"] = emptyOnly("Only assistant messages can have a phase.")
	}
	objectRule(fields)(a, path, raw)
}

func inputRule(a *adapterReport, path string, raw json.RawMessage) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		mappedField(a, path, raw)
		return
	}
	arrayRule(func(a *adapterReport, path string, raw json.RawMessage) {
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &item) != nil || isNull(raw) {
			a.add(path, "invalid", "Expected an input item object.")
			return
		}
		fields := fieldRules{"type": mappedField, "id": localField, "status": localField}
		switch item.Type {
		case "", "message":
			fields["role"], fields["content"] = mappedField, contentRule
			fields["phase"] = mappedField
			if item.Role != "assistant" {
				fields["phase"] = emptyOnly("Only assistant messages can have a phase.")
			}
		case "function_call":
			fields["call_id"], fields["name"], fields["arguments"] = mappedField, mappedField, mappedField
		case "function_call_output":
			fields["call_id"], fields["output"] = mappedField, contentRule
		case "reasoning":
			fields["summary"], fields["content"] = localField, localField
			fields["encrypted_content"] = emptyOnly("Provider-encrypted reasoning cannot be replayed through an OpenCode session.")
		default:
			a.unsupported(path, "This input item type cannot be represented by the session adapter.")
			return
		}
		objectRule(fields)(a, path, raw)
	})(a, path, raw)
}

func toolRule(a *adapterReport, path string, raw json.RawMessage) {
	var item struct {
		Function json.RawMessage `json:"function"`
	}
	if json.Unmarshal(raw, &item) != nil || isNull(raw) {
		a.add(path, "invalid", "Expected a tool object.")
		return
	}
	definition := fieldRules{
		"name": mappedField, "description": mappedField, "parameters": mappedField,
		"strict": falseOnly("The MCP tool route cannot guarantee strict JSON Schema generation."),
	}
	if len(item.Function) > 0 && !isNull(item.Function) {
		objectRule(fieldRules{"type": stringChoice("function"), "function": objectRule(definition)})(a, path, raw)
		return
	}
	definition["type"] = stringChoice("function")
	objectRule(definition)(a, path, raw)
}

func toolChoiceRule(a *adapterReport, path string, raw json.RawMessage) {
	var choice string
	if json.Unmarshal(raw, &choice) == nil {
		if choice == "auto" || choice == "" {
			mappedField(a, path, raw)
		} else {
			a.add(path, "mapped-validated", "")
		}
		return
	}
	var item struct {
		Function json.RawMessage `json:"function"`
	}
	if json.Unmarshal(raw, &item) != nil {
		a.add(path, "invalid", "Expected a tool choice object.")
		return
	}
	fields := fieldRules{"type": stringChoice("function")}
	if len(item.Function) > 0 && !isNull(item.Function) {
		fields["function"] = objectRule(fieldRules{"name": disposition("mapped-validated")})
	} else {
		fields["name"] = disposition("mapped-validated")
	}
	objectRule(fields)(a, path, raw)
}

// Only a digest enters the scheduler. Cache affinity never reuses another
// conversation's session and is scoped to the authenticated API key and model.
func cacheAffinity(ctx context.Context, model, key string) string {
	if key == "" {
		return ""
	}
	owner, _ := ctx.Value(keyContext{}).(string)
	raw, _ := json.Marshal([]string{owner, model, key})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func affinityAccount(candidates []Account, affinity string) Account {
	var selected Account
	best := ""
	for _, a := range candidates {
		sum := sha256.Sum256([]byte(affinity + ":" + a.ID))
		score := hex.EncodeToString(sum[:])
		if selected.ID == "" || score > best {
			selected, best = a, score
		}
	}
	return selected
}
