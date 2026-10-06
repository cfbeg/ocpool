package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/acksell/opencode-go-sdk/sdk/client"
	"github.com/acksell/opencode-go-sdk/sdk/option"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testEgress(t *testing.T) *Egress {
	t.Helper()
	e, err := newEgress(testStore(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.close)
	return e
}
func fakeWarp(t *testing.T, ip string, fetch http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			writeJSON(w, 200, map[string]any{"healthy": true, "connected": true, "public_ip": ip, "ip_scope": "opencode.ai"})
		case "/fetch":
			fetch(w, r)
		case "/rotate":
			writeJSON(w, 200, map[string]any{"healthy": true, "public_ip": ip, "changed": false})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	return s
}
func testEgressRequest(t *testing.T, e *Egress, target string, body string, ctx context.Context) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, "POST", e.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-OCPOOL-Egress-Auth", e.token)
	r.Header.Set("X-OCPOOL-Target", target)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer native-upstream-key")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func saveWarpExits(t *testing.T, e *Egress, endpoints ...EgressEndpoint) {
	t.Helper()
	c := defaultEgressConfig()
	c.Mode = "sticky"
	c.Endpoints = endpoints
	if err := e.Save(c); err != nil {
		t.Fatal(err)
	}
}

func TestSameIPAliasesShareCooldownAndPreserve429(t *testing.T) {
	e := testEgress(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer up.Close()
	e.allowedOrigin = up.URL
	var a, b atomic.Int32
	first := fakeWarp(t, "198.51.100.1", func(w http.ResponseWriter, r *http.Request) {
		a.Add(1)
		w.Header().Set("Retry-After", "120")
		writeJSON(w, 429, map[string]any{"error": map[string]string{"type": "FreeUsageLimitError", "message": "IP limit"}})
	})
	second := fakeWarp(t, "198.51.100.1", func(w http.ResponseWriter, r *http.Request) {
		b.Add(1)
		writeJSON(w, 200, map[string]bool{"wrong": true})
	})
	saveWarpExits(t, e, EgressEndpoint{ID: "a", Kind: "warp", URL: first.URL, Enabled: true}, EgressEndpoint{ID: "b", Kind: "warp", URL: second.URL, Enabled: true})
	for i := 0; i < 2; i++ {
		res := testEgressRequest(t, e, up.URL+"/zen/v1/chat/completions", `{"model":"m"}`, context.Background())
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
			t.Fatal(res.StatusCode, string(raw))
		}
	}
	if a.Load() != 1 || b.Load() != 0 {
		t.Fatalf("same public IP retried: %d, %d", a.Load(), b.Load())
	}
}
func Test429SurvivesNextExitConnectionFailure(t *testing.T) {
	e := testEgress(t)
	e.allowedOrigin = "http://example.invalid"
	first := fakeWarp(t, "198.51.100.1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "37")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"keep me"}}`)
	})
	saveWarpExits(t, e, EgressEndpoint{ID: "a", Kind: "warp", URL: first.URL, Enabled: true}, EgressEndpoint{ID: "b", Kind: "warp", URL: "http://127.0.0.1:1", Enabled: true})
	res := testEgressRequest(t, e, "http://example.invalid/zen/v1/chat/completions", `{"model":"m"}`, context.Background())
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != "37" || !bytes.Contains(raw, []byte("keep me")) {
		t.Fatal(res.StatusCode, res.Header, string(raw))
	}
}
func TestFailoverKeepsNativeBodyAndAuth(t *testing.T) {
	e := testEgress(t)
	e.allowedOrigin = "http://example.invalid"
	expected := `{"model":"m","tools":[{"name":"read"}],"reasoning":{"effort":"low"},"unknown_extension":1}`
	first := fakeWarp(t, "198.51.100.1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, 429, map[string]any{"error": map[string]string{"type": "rate_limit_error"}})
	})
	second := fakeWarp(t, "198.51.100.2", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if string(raw) != expected || r.Header.Get("Authorization") != "Bearer native-upstream-key" || r.Header.Get("X-OCPOOL-Egress-Auth") != "" {
			t.Error("native payload or headers changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: exact\n\ndata: [DONE]\n\n")
	})
	saveWarpExits(t, e, EgressEndpoint{ID: "a", Kind: "warp", URL: first.URL, Enabled: true}, EgressEndpoint{ID: "b", Kind: "warp", URL: second.URL, Enabled: true})
	res := testEgressRequest(t, e, "http://example.invalid/zen/v1/chat/completions", expected, context.Background())
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("X-OCPool-Egress") != "b" || string(raw) != "data: exact\n\ndata: [DONE]\n\n" {
		t.Fatal(res.StatusCode, string(raw))
	}
}
func TestAccountQuotaDoesNotChangeExit(t *testing.T) {
	e := testEgress(t)
	e.allowedOrigin = "http://example.invalid"
	var calls atomic.Int32
	first := fakeWarp(t, "198.51.100.1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 429, map[string]any{"error": map[string]string{"type": "GoUsageLimitError"}})
	})
	second := fakeWarp(t, "198.51.100.2", func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) })
	saveWarpExits(t, e, EgressEndpoint{ID: "a", Kind: "warp", URL: first.URL, Enabled: true}, EgressEndpoint{ID: "b", Kind: "warp", URL: second.URL, Enabled: true})
	res := testEgressRequest(t, e, "http://example.invalid/zen/v1/responses", `{"model":"m"}`, context.Background())
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 429 || res.Header.Get("X-OCPool-Limit-Scope") != "account" || calls.Load() != 0 {
		t.Fatal("account quota rerouted")
	}
}
func TestCooldownIsMonotonicAndPersisted(t *testing.T) {
	e := testEgress(t)
	r := e.routes["direct"]
	r.IP = "198.51.100.7"
	long := time.Now().Add(10 * time.Minute).Unix()
	short := time.Now().Add(time.Minute).Unix()
	var wg sync.WaitGroup
	for _, until := range []int64{long, short, short, long, short} {
		wg.Add(1)
		go func(until int64) { defer wg.Done(); e.limit(r, "m", "egress_model", "limit", until) }(until)
	}
	wg.Wait()
	e.mu.Lock()
	got := e.cooldown(r, "m").Until
	e.mu.Unlock()
	var stored int64
	if err := e.store.db.QueryRow("SELECT until_at FROM egress_limits WHERE egress_key=? AND model='m'", "ip:"+r.IP).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if got != long || stored != long {
		t.Fatal(got, stored)
	}
	if retryDeadline("1e100", 60) <= time.Now().Unix() {
		t.Fatal("Retry-After overflow")
	}
}
func TestStreamLeaseHeldUntilDisconnect(t *testing.T) {
	e := testEgress(t)
	e.allowedOrigin = "http://example.invalid"
	finished := make(chan struct{})
	first := fakeWarp(t, "198.51.100.1", func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	saveWarpExits(t, e, EgressEndpoint{ID: "a", Kind: "warp", URL: first.URL, Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	res := testEgressRequest(t, e, "http://example.invalid/zen/v1/responses", `{"model":"m"}`, ctx)
	raw := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(res.Body, raw); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	active := e.routes["a"].Active
	e.mu.Unlock()
	if active != 1 {
		t.Fatal(active)
	}
	config := e.Config()
	if err := e.Save(config); err == nil {
		t.Fatal("changed settings during stream")
	}
	cancel()
	res.Body.Close()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect did not cancel upstream")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		active = e.routes["a"].Active
		e.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream lease leaked")
}
func TestNoReplayAfterStreamHeaders(t *testing.T) {
	e := testEgress(t)
	e.allowedOrigin = "http://example.invalid"
	var secondCalls atomic.Int32
	first := fakeWarp(t, "198.51.100.1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: partial\n\n")
	})
	second := fakeWarp(t, "198.51.100.2", func(w http.ResponseWriter, r *http.Request) { secondCalls.Add(1) })
	saveWarpExits(t, e, EgressEndpoint{ID: "a", Kind: "warp", URL: first.URL, Enabled: true}, EgressEndpoint{ID: "b", Kind: "warp", URL: second.URL, Enabled: true})
	res := testEgressRequest(t, e, "http://example.invalid/zen/v1/responses", `{"model":"m"}`, context.Background())
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if secondCalls.Load() != 0 {
		t.Fatal("streamed response replayed")
	}
}
func TestEgressConfigMasksSecretsAndRoundTrips(t *testing.T) {
	e := testEgress(t)
	c := defaultEgressConfig()
	c.Mode = "sticky"
	c.Endpoints = []EgressEndpoint{{ID: "p", Kind: "proxy", URL: "http://user:password@127.0.0.1:8080", Secret: "shared-private", Enabled: true}}
	if err := e.Save(c); err != nil {
		t.Fatal(err)
	}
	public := e.Config()
	raw, _ := json.Marshal(public)
	if bytes.Contains(raw, []byte("password")) || bytes.Contains(raw, []byte("shared-private")) || !public.Endpoints[0].SecretConfigured {
		t.Fatal(string(raw))
	}
	if err := e.Save(public); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	private := e.routes["p"].Endpoint
	e.mu.Unlock()
	if private.URL != c.Endpoints[0].URL || private.Secret != c.Endpoints[0].Secret {
		t.Fatal("secret was lost during form save")
	}
}
func TestNativeEgressTargetAndAuthGuard(t *testing.T) {
	e := testEgress(t)
	request := httptest.NewRequest("POST", "/", nil)
	w := httptest.NewRecorder()
	e.serve(w, request)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	request = httptest.NewRequest("POST", "/", nil)
	request.Header.Set("X-OCPOOL-Egress-Auth", e.token)
	request.Header.Set("X-OCPOOL-Target", "http://169.254.169.254/latest/meta-data")
	w = httptest.NewRecorder()
	e.serve(w, request)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	u, _ := url.Parse("https://opencode.ai/zen/v1/responses")
	if !e.allowed(u) {
		t.Fatal("normal Zen origin rejected")
	}
	u, _ = url.Parse("https://opencode.ai/inference/openai/v1/responses")
	if !e.allowed(u) {
		t.Fatal("Console inference origin rejected")
	}
}
func TestUpstream429StatusAndAccountStatePreserved(t *testing.T) {
	s := testStore(t)
	addTestAccount(t, s, "a")
	a := &App{store: s}
	err := &upstreamError{name: "APIError", message: "IP quota", status: 429, retryUntil: time.Now().Add(time.Minute).Unix(), limitScope: "egress"}
	a.markFailure("a", err)
	account, _ := s.Account("a")
	if account.Status != "ready" || account.CooldownUntil != 0 {
		t.Fatal(account)
	}
	w := httptest.NewRecorder()
	inferenceProblem(w, err)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "rate_limit_error") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestGatewayQuotaStopsNativeRetryWithoutWaiting(t *testing.T) {
	e := testEgress(t)
	e.allowedOrigin = "http://example.invalid"
	warp := fakeWarp(t, "198.51.100.3", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		writeJSON(w, 429, map[string]any{"error": map[string]string{"type": "FreeUsageLimitError", "message": "IP quota"}})
	})
	saveWarpExits(t, e, EgressEndpoint{ID: "a", Kind: "warp", URL: warp.URL, Enabled: true})
	addTestAccount(t, e.store, "a")
	pool := newPool(e.store, t.TempDir(), "")
	defer pool.close()
	a := &App{store: e.store, pool: pool, egress: e}
	e.onFailure = pool.egressFailure
	finished := make(chan struct{})
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/session":
			writeJSON(w, 200, map[string]string{"id": "ses_fake"})
		case r.Method == "POST" && r.URL.Path == "/session/ses_fake/message":
			defer close(finished)
			io.Copy(io.Discard, r.Body)
			request, _ := http.NewRequestWithContext(r.Context(), "POST", e.URL, strings.NewReader(`{"model":"test-free"}`))
			request.Header.Set("X-OCPOOL-Egress-Auth", e.token)
			request.Header.Set("X-OCPOOL-Target", "http://example.invalid/zen/v1/chat/completions")
			request.Header.Set("X-Opencode-Session-Id", "ses_fake")
			request.Header.Set("X-OCPOOL-Agent", "build")
			response, err := http.DefaultClient.Do(request)
			if err == nil {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
			<-r.Context().Done() // Native engine otherwise waits Retry-After for an hour.
		default:
			writeJSON(w, 200, true)
		}
	}))
	defer native.Close()
	pool.startFn = func(ctx context.Context, id string) (*Runtime, error) {
		return &Runtime{ID: id, Dir: t.TempDir(), ParamsDir: t.TempDir(), cancel: func() {}, subs: map[string]*subscription{}, Client: client.NewClient(option.WithBaseURL(native.URL), option.WithMaxAttempts(1))}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	begin := time.Now()
	_, err := a.generate(ctx, "opencode/test-free", []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}, "", "", false, GenerationOptions{}, nil)
	var quota *upstreamError
	if !errors.As(err, &quota) || quota.status != 429 || time.Since(begin) > 2*time.Second {
		t.Fatalf("quota did not stop promptly: %v (%v)", err, time.Since(begin))
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("native pending request was not canceled")
	}
	if until := e.limitedUntil("test-free"); until <= time.Now().Unix() {
		t.Fatal("shared IP cooldown was not retained")
	}
}
