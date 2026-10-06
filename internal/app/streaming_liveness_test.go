package app

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acksell/opencode-go-sdk/sdk/client"
	"github.com/acksell/opencode-go-sdk/sdk/option"
)

func streamingFixture(t *testing.T, prompt http.HandlerFunc) (*App, *Runtime) {
	t.Helper()
	s := testStore(t)
	addTestAccount(t, s, "stream-account")
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/session":
			writeJSON(w, 200, map[string]string{"id": "ses_stream"})
		case r.Method == "POST" && r.URL.Path == "/session/ses_stream/message":
			_, _ = io.Copy(io.Discard, r.Body)
			prompt(w, r)
		default:
			writeJSON(w, 200, true)
		}
	}))
	t.Cleanup(native.Close)
	p := newPool(s, t.TempDir(), "")
	t.Cleanup(p.close)
	runtime := &Runtime{ID: "stream-account", Dir: t.TempDir(), ParamsDir: t.TempDir(), cancel: func() {}, subs: map[string]*subscription{}, Client: client.NewClient(option.WithBaseURL(native.URL), option.WithMaxAttempts(1))}
	p.startFn = func(context.Context, string) (*Runtime, error) { return runtime, nil }
	return &App{store: s, pool: p}, runtime
}

func syntheticAnswer(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{
		"info":  map[string]any{"id": "msg_stream", "role": "assistant", "finish": "stop", "tokens": map[string]int{"input": 10, "output": 3, "reasoning": 0}},
		"parts": []any{map[string]any{"id": "part_stream", "type": "text", "text": "SYNTHETIC_OK"}},
	})
}

func TestStreamingBeginsBeforeNativeGenerationCompletes(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			a, _ := streamingFixture(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
					syntheticAnswer(w)
				case <-r.Context().Done():
				}
			})
			handler := a.chat
			body := `{"model":"opencode/test-free","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`
			first, completed := `"role":"assistant"`, `[DONE]`
			if responses {
				handler = a.responses
				body = `{"model":"opencode/test-free","input":"hello","stream":true,"store":false}`
				first, completed = "response.created", "response.completed"
			}
			server := httptest.NewServer(http.HandlerFunc(handler))
			defer server.Close()
			request, _ := http.NewRequest("POST", server.URL, strings.NewReader(body))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			request = request.WithContext(ctx)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal("response headers waited for the blocked native request:", err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatal(response.StatusCode, response.Header)
			}
			reader := bufio.NewReader(response.Body)
			var initial strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				initial.WriteString(line)
				if line == "\n" {
					break
				}
			}
			if !strings.Contains(initial.String(), first) {
				t.Fatal("missing initial stream event:", initial.String())
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("native generation never began")
			}
			once.Do(func() { close(release) })
			rest, err := io.ReadAll(reader)
			if err != nil || !strings.Contains(string(rest), "SYNTHETIC_OK") || !strings.Contains(string(rest), completed) {
				t.Fatalf("stream did not finish normally: %v %s", err, rest)
			}
		})
	}
}

type brokenStreamWriter struct {
	*httptest.ResponseRecorder
	writes, allowed int
}

func (w *brokenStreamWriter) Write(value []byte) (int, error) {
	w.writes++
	if w.writes > w.allowed {
		return 0, errors.New("fixture stream disconnected")
	}
	return w.ResponseRecorder.Write(value)
}

func TestBrokenStreamCancelsBlockedNativeGeneration(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			a, runtime := streamingFixture(t, func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
				close(canceled)
			})
			w := &brokenStreamWriter{ResponseRecorder: httptest.NewRecorder(), allowed: 1}
			handler := a.chat
			body := `{"model":"opencode/test-free","messages":[{"role":"user","content":"hello"}],"stream":true}`
			if responses {
				handler = a.responses
				body = `{"model":"opencode/test-free","input":"hello","store":false,"stream":true}`
				w.allowed = 2 // event: response.created followed by its data frame.
			}
			request := httptest.NewRequest("POST", "/", strings.NewReader(body))
			returned := make(chan struct{})
			go func() { handler(w, request); close(returned) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("native generation did not begin")
			}
			runtime.mu.Lock()
			sub := runtime.subs["ses_stream"]
			runtime.mu.Unlock()
			if sub == nil {
				t.Fatal("native session was not subscribed")
			}
			var role event
			role.Type = "message.updated"
			role.Properties.Info.ID, role.Properties.Info.Role = "msg_stream", "assistant"
			var delta event
			delta.Type = "message.part.updated"
			delta.Properties.Part.MessageID, delta.Properties.Part.ID = "msg_stream", "part_stream"
			delta.Properties.Part.Type, delta.Properties.Part.Text = "text", "SYNTHETIC_OK"
			sub.ch <- role
			sub.ch <- delta
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("broken stream did not cancel the native HTTP request")
			}
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("handler did not join its heartbeat and return")
			}
		})
	}
}

func TestStreamingValidationAndHistoryErrorsRemainHTTP(t *testing.T) {
	a, _ := streamingFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached generation") })
	for _, fixture := range []struct {
		handler http.HandlerFunc
		body    string
		status  int
	}{
		{a.chat, `{"model":"opencode/test-free","messages":[{"role":"invalid","content":"hello"}],"stream":true}`, 400},
		{a.responses, `{"model":"opencode/test-free","input":[{"role":"invalid","content":"hello"}],"stream":true}`, 400},
		{a.responses, `{"input":"hello","previous_response_id":"resp_missing","stream":true}`, 404},
	} {
		w := httptest.NewRecorder()
		fixture.handler(w, httptest.NewRequest("POST", "/", strings.NewReader(fixture.body)))
		if w.Code != fixture.status || strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
	}
}

func TestStreamErrorsRetainTypedStatusAndNonstreamHTTPStatus(t *testing.T) {
	a, _ := streamingFixture(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"info": map[string]any{"error": map[string]any{"name": "APIError", "data": map[string]any{"message": "fixture IP limit", "statusCode": 429}}}, "parts": []any{}})
	})
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			body := `{"model":"opencode/test-free","messages":[{"role":"user","content":"hello"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
			handler := a.chat
			if responses {
				body = `{"model":"opencode/test-free","input":"hello","store":false,"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
				handler = a.responses
			}
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest("POST", "/", strings.NewReader(body)))
			if stream {
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":429`) || !strings.Contains(w.Body.String(), `"code":"rate_limit_exceeded"`) {
					t.Fatalf("typed streaming failure lost: responses=%v %d %s", responses, w.Code, w.Body.String())
				}
				if !responses && !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
					t.Fatal("Chat failure did not terminate with DONE:", w.Body.String())
				}
				if responses && !strings.Contains(w.Body.String(), "response.failed") {
					t.Fatal("Responses failure did not terminate with response.failed")
				}
			} else if w.Code != 429 {
				t.Fatal("nonstream HTTP status changed:", w.Code, w.Body.String())
			}
			// An account-scoped fixture 429 cools the metadata account; restore it
			// so the next API variant reaches the same synthetic native error.
			_, _ = a.store.db.Exec("UPDATE accounts SET cooldown_until=0 WHERE id='stream-account'")
		}
	}
}

type heartbeatRecorder struct {
	*httptest.ResponseRecorder
	comments atomic.Int32
}

func (w *heartbeatRecorder) Write(value []byte) (int, error) {
	if strings.HasPrefix(string(value), ": keep-alive") {
		w.comments.Add(1)
	}
	return w.ResponseRecorder.Write(value)
}

func TestHeartbeatStopsAndJoinsBeforeTerminalFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &heartbeatRecorder{ResponseRecorder: httptest.NewRecorder()}
	s := newSSEStreamWriter(ctx, w, cancel, time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for w.comments.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w.comments.Load() == 0 {
		t.Fatal("heartbeat did not send a keep-alive comment")
	}
	s.stopAndJoin()
	if err := s.event("error", map[string]string{"message": "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := s.frame("data: [DONE]\n\n"); err != nil {
		t.Fatal(err)
	}
	count := w.comments.Load()
	time.Sleep(5 * time.Millisecond)
	if w.comments.Load() != count || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatal("heartbeat wrote after terminal frame or handler join")
	}
}
