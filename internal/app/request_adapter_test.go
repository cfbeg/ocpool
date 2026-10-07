package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acksell/opencode-go-sdk/sdk/client"
	"github.com/acksell/opencode-go-sdk/sdk/option"
)

func TestEnvelopePreservesOriginalValuesAndClassifiesEveryExtension(t *testing.T) {
	raw := " {\n\"model\":\"opencode/test-free\",\"input\":\"hello\",\"client_metadata\":{\"integer\":9007199254740993,\"future_hint\":{\"flag\":true}},\"future_generation\":{\"budget\":999}} \n"
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(raw))
	w := httptest.NewRecorder()
	var req responseRequest
	e, err := readCompatJSON(w, r, &req)
	if err != nil {
		t.Fatal(err)
	}
	if string(e.raw) != raw || string(e.fields["future_generation"]) != `{"budget":999}` || !strings.Contains(string(req.ClientMetadata), "9007199254740993") {
		t.Fatal("normalization modified or discarded the original request")
	}
	r, accepted := e.adapt(w, r, true)
	if accepted || w.Code != 422 || envelopeFromContext(r.Context()) != e {
		t.Fatal("unsupported semantics were accepted or envelope lost", w.Code)
	}
	if !strings.Contains(w.Header().Get("X-OCPool-Adapter"), "client_metadata=retained-local") {
		t.Fatal(w.Header())
	}
	for _, secret := range []string{"9007199254740993", "future_hint", "budget", "999"} {
		if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Header().Get("X-OCPool-Adapter"), secret) {
			t.Fatal("a request value leaked into diagnostics")
		}
	}
}

func TestAdapterRejectsAllUnsupportedFieldsBeforeStartingStreamOrRuntime(t *testing.T) {
	// A zero App deliberately has no store or process pool. Any side effect or
	// attempt to generate before validation would panic instead of returning 422.
	a := &App{}
	for _, responses := range []bool{false, true} {
		for _, fixture := range []struct{ extra, param string }{
			{`"future_generation":{"secret":"UNSAFE_TO_ECHO"}`, "future_generation"},
			{`"reasoning":{"effort":"low","future_budget":7}`, "reasoning.future_budget"},
			{`"include":["reasoning.encrypted_content"]`, "include[0]"},
			{`"prompt_cache_retention":"24h"`, "prompt_cache_retention"},
			{`"prompt_cache_options":{"mode":"explicit"}`, "prompt_cache_options.mode"},
			{`"service_tier":"priority"`, "service_tier"},
			{`"tools":[{"type":"function","name":"read","strict":true,"parameters":{"type":"object"}}]`, "tools[0].strict"},
			{`"tools":[{"type":"function","function":{"name":"read","new_policy":true}}]`, "tools[0].function.new_policy"},
			{`"tool_choice":{"type":"function","name":"read","future":true}`, "tool_choice.future"},
			{`"truncation":"auto"`, "truncation"},
		} {
			t.Run(fixture.param+map[bool]string{false: "/chat", true: "/responses"}[responses], func(t *testing.T) {
				body := `{"model":"opencode/test-free","stream":true,` + fixture.extra
				handler := a.chat
				if responses {
					body += `,"input":"hello"}`
					handler = a.responses
				} else {
					body += `,"messages":[{"role":"user","content":"hello"}]}`
				}
				w := httptest.NewRecorder()
				handler(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
				assertAdapterRejection(t, w, fixture.param)
			})
		}
	}
	for _, fixture := range []struct {
		body, param string
		responses   bool
	}{
		{`{"input":"hello","text":{"verbosity":"high","format":{"type":"json_schema","schema":{}}},"access_programs":["x"],"stream":true}`, "text.verbosity", true},
		{`{"input":[{"role":"user","content":[{"type":"input_text","text":"hi","future_flag":true}]}],"stream":true}`, "input[0].content[0].future_flag", true},
		{`{"input":[{"type":"reasoning","encrypted_content":"UNSAFE_TO_ECHO"},{"role":"user","content":"hello"}]}`, "input[0].encrypted_content", true},
		{`{"input":[{"type":"compaction","encrypted_content":"UNSAFE_TO_ECHO"}]}`, "input[0]", true},
		{`{"messages":[{"role":"user","content":"hi","future_flag":true}],"stream":true}`, "messages[0].future_flag", false},
		{`{"messages":[{"role":"user","content":"hi"}],"stream":true,"store":true}`, "store", false},
		{`{"messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"new_option":true}}`, "stream_options.new_option", false},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader(fixture.body))
		if fixture.responses {
			a.responses(w, r)
		} else {
			a.chat(w, r)
		}
		assertAdapterRejection(t, w, fixture.param)
		if fixture.param == "text.verbosity" {
			for _, param := range []string{"text.format.type", "text.format.schema", "access_programs[0]"} {
				if !strings.Contains(w.Body.String(), `"param":"`+param+`"`) {
					t.Fatal("did not aggregate unsupported fields", w.Body.String())
				}
			}
		}
	}
}

func assertAdapterRejection(t *testing.T, w *httptest.ResponseRecorder, param string) {
	t.Helper()
	if w.Code != 422 || strings.Contains(w.Header().Get("Content-Type"), "event-stream") || !strings.Contains(w.Body.String(), `"code":"unsupported_parameter"`) || !strings.Contains(w.Body.String(), `"param":"`+param+`"`) {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	if strings.Contains(w.Body.String(), "UNSAFE_TO_ECHO") {
		t.Fatal("echoed an unsupported value")
	}
}

func TestEnvelopeRejectsAmbiguousMalformedAndOversizedJSON(t *testing.T) {
	a := &App{}
	for _, body := range []string{
		`null`, `[]`, `{} {}`, `{} trailing`, `{"input":"hello","input":"changed"}`,
		`{"client_metadata":{"key":1,"key":2}}`, `{"model":23}`,
		`{"input":"hello","text":true}`, `{"input":"hello","access_programs":{}}`,
	} {
		w := httptest.NewRecorder()
		a.responses(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	a.responses(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"input":"`+strings.Repeat("x", 2<<20)+`"}`)))
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
}

func TestAdapterHeaderEscapesNamesAndBoundsSize(t *testing.T) {
	fields := map[string]any{"input": "hello", "evil\r\nX-Fake: yes": true}
	for i := 0; i < 300; i++ {
		fields[strings.Repeat("long_field_", i+1)] = true
	}
	raw, _ := json.Marshal(fields)
	r := httptest.NewRequest("POST", "/", strings.NewReader(string(raw)))
	w := httptest.NewRecorder()
	var req responseRequest
	e, err := readCompatJSON(w, r, &req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.adapt(w, r, true)
	header := w.Header().Get("X-OCPool-Adapter")
	if strings.ContainsAny(header, "\r\n") || len(header) > 4096 || w.Header().Get("X-OCPool-Adapter-Truncated") != "true" {
		t.Fatal("invalid adapter header", len(header), w.Header())
	}
}

func TestSchemasRetainLargeNumbersAndClientMessageIdentity(t *testing.T) {
	raw := `{"input":[{"role":"assistant","phase":"commentary","content":"working"},{"role":"user","content":"continue"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"value":{"const":9007199254740993}},"new_schema_keyword":{"anything":true}}}],"text":{"format":{"type":"text"}},"access_programs":[]}`
	r := httptest.NewRequest("POST", "/", strings.NewReader(raw))
	w := httptest.NewRecorder()
	var req responseRequest
	e, err := readCompatJSON(w, r, &req)
	if err != nil {
		t.Fatal(err)
	}
	if _, accepted := e.adapt(w, r, true); !accepted {
		t.Fatal(w.Body.String())
	}
	plan, err := parseTools(req.Tools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, _ := json.Marshal(plan.definitions[0].Parameters)
	if !strings.Contains(string(schema), "9007199254740993") || !strings.Contains(string(schema), "new_schema_keyword") {
		t.Fatal(string(schema))
	}
	messages, err := responseMessages(req.Input, "")
	if err != nil {
		t.Fatal(err)
	}
	prompt, _, err := promptMessages(messages)
	if err != nil || !strings.Contains(prompt, `"phase":"commentary"`) {
		t.Fatal(prompt, err)
	}
	prompt, _, err = promptMessages([]Message{{Role: "user", Content: json.RawMessage(`"hello"`), Name: "alice"}})
	if err != nil || !strings.Contains(prompt, `"name":"alice"`) {
		t.Fatal(prompt, err)
	}
}

// Exercise both real HTTP handlers against a fake OpenCode SDK endpoint. This
// checks the actual outbound payload and parameter file, rather than asserting
// that the field classifier merely labels metadata as local.
func TestMetadataIsolationAndResponsesStorageAcrossHTTPAndSSE(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(map[bool]string{false: "chat", true: "responses"}[responses]+map[bool]string{false: "/json", true: "/sse"}[stream], func(t *testing.T) {
				s := testStore(t)
				addTestAccount(t, s, "account")
				params := t.TempDir()
				nativeBodies := make(chan string, 1)
				native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/session" && r.Method == "POST":
						writeJSON(w, 200, map[string]string{"id": "ses_adapter"})
					case r.URL.Path == "/session/ses_adapter/message" && r.Method == "POST":
						body, _ := io.ReadAll(r.Body)
						nativeBodies <- string(body)
						options, err := os.ReadFile(filepath.Join(params, "ses_adapter.json"))
						if err != nil {
							t.Error(err)
						}
						if strings.Contains(string(options), "PRIVATE") || strings.Contains(string(options), "cache") {
							t.Error("raw envelope entered parameter file")
						}
						syntheticAnswer(w)
					default:
						writeJSON(w, 200, true)
					}
				}))
				defer native.Close()
				p := newPool(s, t.TempDir(), "")
				defer p.close()
				p.startFn = func(ctx context.Context, id string) (*Runtime, error) {
					e := envelopeFromContext(ctx)
					if e == nil || !strings.Contains(string(e.fields["client_metadata"]), "PRIVATE_CLIENT") {
						t.Error("envelope did not survive to runtime acquisition")
					}
					return &Runtime{ID: id, Dir: t.TempDir(), ParamsDir: params, cancel: func() {}, subs: map[string]*subscription{}, Client: client.NewClient(option.WithBaseURL(native.URL), option.WithMaxAttempts(1))}, nil
				}
				a := &App{store: s, pool: p}
				body := `{"model":"opencode/test-free","client_metadata":{"secret":"PRIVATE_CLIENT"},"user":"PRIVATE_USER","safety_identifier":"PRIVATE_SAFETY","prompt_cache_key":"PRIVATE_CACHE","metadata":{"label":"PRIVATE_LABEL"},"stream":` + map[bool]string{false: "false", true: "true"}[stream]
				handler := a.chat
				if responses {
					body += `,"input":"hello","store":true}`
					handler = a.responses
				} else {
					body += `,"messages":[{"role":"user","content":"hello"}],"store":false}`
				}
				r := httptest.NewRequest("POST", "/", strings.NewReader(body))
				r = r.WithContext(context.WithValue(r.Context(), keyContext{}, "owner"))
				w := httptest.NewRecorder()
				handler(w, r)
				if w.Code != 200 || !strings.Contains(w.Body.String(), "SYNTHETIC_OK") {
					t.Fatal(w.Code, w.Body.String())
				}
				nativeBody := <-nativeBodies
				if strings.Contains(nativeBody, "PRIVATE") || !strings.Contains(nativeBody, "hello") {
					t.Fatal("metadata leaked or input was lost", nativeBody)
				}
				if !strings.Contains(w.Header().Get("X-OCPool-Adapter"), "client_metadata=retained-local") || strings.Contains(w.Header().Get("X-OCPool-Adapter"), "PRIVATE") {
					t.Fatal(w.Header())
				}
				var saved, id string
				err := s.db.QueryRow("SELECT id,body FROM responses").Scan(&id, &saved)
				if responses {
					if err != nil || !strings.Contains(saved, `"metadata":{"label":"PRIVATE_LABEL"}`) || strings.Contains(saved, "PRIVATE_CLIENT") || strings.Contains(saved, "PRIVATE_CACHE") {
						t.Fatal("unexpected persisted metadata", saved, err)
					}
					get := httptest.NewRequest("GET", "/v1/responses/"+id, nil)
					get.SetPathValue("id", id)
					get = get.WithContext(context.WithValue(get.Context(), keyContext{}, "owner"))
					got := httptest.NewRecorder()
					a.getResponse(got, get)
					if got.Code != 200 || got.Body.String() != saved {
						t.Fatal("stored response did not round trip", got.Code)
					}
					get = get.WithContext(context.WithValue(get.Context(), keyContext{}, "other-owner"))
					got = httptest.NewRecorder()
					a.getResponse(got, get)
					if got.Code != 404 {
						t.Fatal("response metadata crossed API keys")
					}
				} else if err == nil {
					t.Fatal("chat unexpectedly persisted its raw request")
				}
			})
		}
	}
}

func TestStoreFalseDoesNotPersistEnvelopeOrResponseMetadata(t *testing.T) {
	for _, stream := range []bool{false, true} {
		a, _ := streamingFixture(t, func(w http.ResponseWriter, _ *http.Request) { syntheticAnswer(w) })
		body := `{"model":"opencode/test-free","input":"hello","store":false,"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"metadata":{"label":"private-response"},"client_metadata":{"local":"private-client"}}`
		w := httptest.NewRecorder()
		a.responses(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "private-response") || strings.Contains(w.Body.String(), "private-client") {
			t.Fatal(w.Code, w.Body.String())
		}
		var count int
		if err := a.store.db.QueryRow("SELECT COUNT(*) FROM responses").Scan(&count); err != nil || count != 0 {
			t.Fatal("store=false retained a response", count, err)
		}
		var requestError string
		if err := a.store.db.QueryRow("SELECT error FROM requests").Scan(&requestError); err != nil || requestError != "" {
			t.Fatal("request log unexpectedly contains metadata", requestError, err)
		}
	}
}

func TestCacheAffinityIsScopedStableAndRespectsPreviousResponseAccount(t *testing.T) {
	ctx := context.WithValue(context.Background(), keyContext{}, "key_a")
	affinity := cacheAffinity(ctx, "opencode/test-free", "private-cache-key")
	if affinity == "" || strings.Contains(affinity, "private") || affinity == cacheAffinity(ctx, "opencode/other", "private-cache-key") || affinity == cacheAffinity(context.WithValue(ctx, keyContext{}, "key_b"), "opencode/test-free", "private-cache-key") || cacheAffinity(ctx, "opencode/test-free", "") != "" {
		t.Fatal("affinity is not scoped")
	}
	s := testStore(t)
	for _, id := range []string{"a", "b", "c"} {
		addTestAccount(t, s, id)
	}
	p := newPool(s, t.TempDir(), "")
	defer p.close()
	p.startFn = func(_ context.Context, id string) (*Runtime, error) {
		return &Runtime{ID: id, cancel: func() {}, subs: map[string]*subscription{}}, nil
	}
	r, err := p.acquireWithAffinity(ctx, "opencode/test-free", "", affinity)
	if err != nil {
		t.Fatal(err)
	}
	first := r.ID
	p.release(r, true)
	for i := 0; i < 3; i++ {
		r, err = p.acquireWithAffinity(ctx, "opencode/test-free", "", affinity)
		if err != nil || r.ID != first {
			t.Fatal("LRU state changed the cache account", err)
		}
		p.release(r, true)
	}
	other := "a"
	if first == other {
		other = "b"
	}
	r, err = p.acquireWithAffinity(ctx, "opencode/test-free", other, affinity)
	if err != nil || r.ID != other {
		t.Fatal("affinity overrode previous_response_id account", err)
	}
	p.release(r, true)
	_, err = s.db.Exec("UPDATE accounts SET cooldown_until=? WHERE id=?", time.Now().Add(time.Hour).Unix(), first)
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.acquireWithAffinity(ctx, "opencode/test-free", "", affinity)
	if err != nil || r.ID == first {
		t.Fatal("affinity routed to cooling account", err)
	}
	p.release(r, true)
}
