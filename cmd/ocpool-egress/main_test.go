package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "test-control-token-with-at-least-32-characters"

func TestWireGuardPoolSizeValidation(t *testing.T) {
	for _, value := range []string{"1", "4", "32", " 4 "} {
		if _, err := parseWGPoolSize(value); err != nil {
			t.Fatalf("valid pool size %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"", "0", "-1", "33", "4.0", "all"} {
		if _, err := parseWGPoolSize(value); err == nil {
			t.Fatalf("invalid pool size %q accepted", value)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func authenticatedRequest(t *testing.T, method, target, path string) *http.Request {
	t.Helper()
	var body io.Reader
	if path != "" {
		body = strings.NewReader(`{"model":"test","stream":true}`)
	}
	r, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Relay-Auth", testToken)
	if path != "" {
		r.Header.Set("X-Relay-Target", "https://opencode.ai")
		r.Header.Set("X-Relay-Path", path)
	}
	return r
}

func TestZenOriginAndPathGuard(t *testing.T) {
	for _, tc := range []struct {
		origin, path, method string
		allowed              bool
	}{
		{"https://opencode.ai", "/zen/v1/chat/completions", "POST", true},
		{"https://opencode.ai", "/zen/v1/responses", "POST", true},
		{"https://opencode.ai", "/zen/v1/messages", "POST", true},
		{"https://opencode.ai", "/zen/v1/models", "", true},
		{"https://opencode.ai", "/inference/openai/v1/chat/completions", "POST", true},
		{"https://opencode.ai", "/inference/openai/v1/responses", "POST", true},
		{"https://opencode.ai", "/inference/openai/v1/models", "", true},
		{"https://opencode.ai", "/inference/anthropic/v1/messages", "POST", true},
		{"https://opencode.ai", "/inference/google/v1beta/models", "GET", true},
		{"https://opencode.ai", "/inference/google/v1beta/models/gemini-2.5-pro", "GET", true},
		{"https://opencode.ai", "/inference/google/v1beta/models/gemini-2.5-pro:generateContent", "POST", true},
		{"https://opencode.ai", "/inference/google/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", "POST", true},
		{"https://opencode.ai", "/inference/google/v1beta/models/gemini-2.5-pro:countTokens", "POST", true},
		{"https://opencode.ai", "/inference/openai/v1/responses?key=invalid", "POST", false},
		{"https://opencode.ai", "/inference/anthropic/v1/messages", "GET", false},
		{"https://opencode.ai", "/inference/google/v1beta/models/gemini-2.5-pro:delete", "POST", false},
		{"https://opencode.ai", "/inference/google/v1beta/models/gemini-2.5-pro:generateContent", "DELETE", false},
		{"https://opencode.ai", "/inference/google/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse&key=invalid", "POST", false},
		{"https://opencode.ai", "/inference/google/v1beta/models/../console:generateContent", "POST", false},
		{"https://opencode.ai", "/inference/google/v1beta/models/%2E%2E:generateContent", "POST", false},
		{"https://opencode.ai", "/inference/google/v1beta/models/gemini-2.5-pro:generateContent?alt=sse", "POST", false},
		{"https://opencode.ai", "/zen/v1/models", "POST", false},
		{"https://opencode.ai.evil.invalid", "/zen/v1/responses", "POST", false},
		{"https://opencode.ai", "/zen/v1/responses?url=http://127.0.0.1", "POST", false},
		{"https://opencode.ai", "/zen/v1/../console", "POST", false},
		{"https://opencode.ai", "/console/api/user", "POST", false},
		{"https://opencode.ai", "https://evil.invalid", "POST", false},
	} {
		r := authenticatedRequest(t, "POST", "http://relay/fetch", "")
		r.Header.Set("X-Relay-Target", tc.origin)
		r.Header.Set("X-Relay-Path", tc.path)
		r.Header.Set("X-Relay-Method", tc.method)
		_, _, err := targetRequest(r)
		if (err == nil) != tc.allowed {
			t.Errorf("origin=%s path=%s method=%s: %v", tc.origin, tc.path, tc.method, err)
		}
	}
	s := newRelay(testToken, nil)
	r := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauthenticated health: %d", w.Code)
	}
}

func TestRelayPreservesBodyStatusAndHeaders(t *testing.T) {
	s := newRelay(testToken, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://opencode.ai/zen/v1/messages" {
			t.Errorf("target %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Opencode-Session") != "session-test" || r.Header.Get("X-Opencode-Org-Id") != "org-test" {
			t.Error("upstream headers lost")
		}
		if r.Header.Get("X-Relay-Auth") != "" || r.Header.Get("X-Relay-Target") != "" || r.Header.Get("X-Hop") != "" {
			t.Error("control or hop headers leaked")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"test","stream":true}` {
			t.Errorf("body changed: %s", body)
		}
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"17"}, "Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`))}, nil
	})})
	s.state.Connected = true
	r := authenticatedRequest(t, "POST", "http://relay/fetch", "/zen/v1/messages")
	r.Header.Set("Authorization", "Bearer upstream-secret")
	r.Header.Set("X-Opencode-Session", "session-test")
	r.Header.Set("X-Opencode-Org-Id", "org-test")
	r.Header.Set("Connection", "X-Hop")
	r.Header.Set("X-Hop", "private")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") != "17" || w.Body.String() != `{"error":"rate_limited"}` {
		t.Fatalf("upstream response not preserved: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if s.snapshot().ActiveFlows != 0 {
		t.Fatal("completed response retained lease")
	}
}

func TestRelayRateLimitCooldownIsKeyedByPublicIP(t *testing.T) {
	var upstream atomic.Int32
	s := newRelay(testToken, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstream.Add(1)
		return &http.Response{StatusCode: http.StatusTooManyRequests,
			Header: http.Header{"Retry-After": []string{"120"}, "Content-Type": []string{"application/json"}},
			Body:   io.NopCloser(strings.NewReader(`{"type":"FreeUsageLimit","error":"rate_limited"}`))}, nil
	})})
	s.state.Connected = true
	s.state.Healthy = true
	s.state.Backend = "wireguard"
	s.state.PublicIP = "192.0.2.41"
	s.state.IPScope = "opencode.ai"
	r := authenticatedRequest(t, "POST", "http://relay/fetch", "/zen/v1/messages")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Body.String() != `{"type":"FreeUsageLimit","error":"rate_limited"}` || upstream.Load() != 1 {
		t.Fatalf("first upstream 429 was not preserved: status=%d body=%s calls=%d", w.Code, w.Body, upstream.Load())
	}
	second := authenticatedRequest(t, "POST", "http://relay/fetch", "/zen/v1/messages")
	w2 := httptest.NewRecorder()
	s.ServeHTTP(w2, second)
	if w2.Code != http.StatusTooManyRequests || upstream.Load() != 1 || w2.Header().Get("Retry-After") == "" {
		t.Fatalf("cooldown did not short-circuit: status=%d calls=%d headers=%v body=%s", w2.Code, upstream.Load(), w2.Header(), w2.Body)
	}
	// A newly measured IP has an independent budget and is admitted.
	s.mu.Lock()
	s.state.PublicIP = "192.0.2.42"
	s.mu.Unlock()
	third := authenticatedRequest(t, "POST", "http://relay/fetch", "/zen/v1/messages")
	w3 := httptest.NewRecorder()
	s.ServeHTTP(w3, third)
	if w3.Code != http.StatusTooManyRequests || upstream.Load() != 2 {
		t.Fatalf("new public IP inherited old cooldown: status=%d calls=%d", w3.Code, upstream.Load())
	}
}

func TestRelayDoesNotTurnAccountQuotaIntoIPCooldown(t *testing.T) {
	var upstream atomic.Int32
	s := newRelay(testToken, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstream.Add(1)
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader(`{"type":"AccountUsageLimit","message":"subscription quota"}`))}, nil
	})})
	s.state.Connected = true
	s.state.Healthy = true
	s.state.PublicIP = "192.0.2.45"
	s.state.IPScope = "opencode.ai"
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, authenticatedRequest(t, "POST", "http://relay/fetch", "/zen/v1/messages"))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d status: %d", i, w.Code)
		}
	}
	if upstream.Load() != 2 {
		t.Fatalf("account quota was incorrectly converted to relay cooldown: upstream calls=%d", upstream.Load())
	}
}

func TestRelayRateLimitTriggersCatalogRotationAfterFlow(t *testing.T) {
	rotated := make(chan struct{}, 1)
	s := newRelay(testToken, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"1"}}, Body: io.NopCloser(strings.NewReader(`{"type":"FreeUsageLimit","error":"rate_limited"}`))}, nil
	})})
	s.state.Connected = true
	s.state.Healthy = true
	s.state.Backend = "wireguard"
	s.state.PublicIP = "192.0.2.43"
	s.state.IPScope = "opencode.ai"
	s.rotateVPN = func(context.Context, []string, bool) error {
		rotated <- struct{}{}
		return nil
	}
	s.probe = func(context.Context) (string, string, string, error) {
		return "192.0.2.43", "off", "opencode.ai", nil
	}
	s.vpnDetails = func(context.Context) (vpnDetails, error) {
		return vpnDetails{Endpoint: "198.51.100.1:51820", Handshake: time.Now().Unix()}, nil
	}
	s.onRateLimit = s.rotateOnRateLimit
	r := authenticatedRequest(t, "POST", "http://relay/fetch", "/zen/v1/messages")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("upstream 429 not returned: %d", w.Code)
	}
	select {
	case <-rotated:
	case <-time.After(time.Second):
		t.Fatal("rate-limit rotation callback did not run")
	}
}

func streamFixture(t *testing.T) (*relay, *httptest.Server, context.CancelFunc, <-chan struct{}, *atomic.Int32) {
	t.Helper()
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"delta\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, _ := url.Parse(upstream.URL)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		cloned := r.Clone(r.Context())
		cloned.URL.Scheme = upstreamURL.Scheme
		cloned.URL.Host = upstreamURL.Host
		return http.DefaultTransport.RoundTrip(cloned)
	})}
	s := newRelay(testToken, client)
	s.state.Connected = true
	s.state.Healthy = true
	s.state.PublicIP = "192.0.2.1"
	s.state.IPScope = "opencode.ai"
	count := new(atomic.Int32)
	s.reconnect = func(context.Context) error {
		if s.snapshot().ActiveFlows != 0 {
			t.Error("reconnect ran while SSE active")
		}
		count.Add(1)
		return nil
	}
	s.probe = func(context.Context) (string, string, string, error) { return "192.0.2.2", "on", "opencode.ai", nil }
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := authenticatedRequest(t, "POST", server.URL+"/fetch", "/zen/v1/chat/completions").WithContext(ctx)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "first") {
		t.Fatalf("SSE first chunk: %q %v", line, err)
	}
	if s.snapshot().ActiveFlows != 1 {
		t.Fatal("lease released before SSE body finished")
	}
	return s, server, cancel, upstreamCanceled, count
}

func TestRotationDrainsSSEAndCancelsUpstream(t *testing.T) {
	s, server, cancel, upstreamCanceled, reconnects := streamFixture(t)
	rotation := make(chan *http.Response, 1)
	go func() {
		resp, _ := http.DefaultClient.Do(authenticatedRequest(t, "POST", server.URL+"/rotate", ""))
		rotation <- resp
	}()
	deadline := time.Now().Add(time.Second)
	for !s.snapshot().Rotating && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.snapshot().Rotating {
		t.Fatal("rotation did not start")
	}
	if reconnects.Load() != 0 {
		t.Fatal("reconnected before stream drained")
	}
	blocked, err := http.DefaultClient.Do(authenticatedRequest(t, "POST", server.URL+"/fetch", "/zen/v1/chat/completions"))
	if err != nil {
		t.Fatal(err)
	}
	_ = blocked.Body.Close()
	if blocked.StatusCode != 503 || blocked.Header.Get("Retry-After") == "" {
		t.Fatal("new flow admitted during rotation")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("caller disconnect did not cancel upstream")
	}
	select {
	case result := <-rotation:
		if result == nil {
			t.Fatal("rotation request failed")
		}
		data, _ := io.ReadAll(result.Body)
		_ = result.Body.Close()
		if result.StatusCode != 200 || !strings.Contains(string(data), `"changed":true`) {
			t.Fatalf("rotation %d %s", result.StatusCode, data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rotation did not resume after drain")
	}
	if reconnects.Load() != 1 || s.snapshot().Rotations != 1 || s.snapshot().ActiveFlows != 0 {
		t.Fatalf("rotation state: %+v", s.snapshot())
	}
}

func TestRotationDrainTimeoutPreservesTunnel(t *testing.T) {
	s, server, cancel, _, reconnects := streamFixture(t)
	s.drainWait = 30 * time.Millisecond
	resp, err := http.DefaultClient.Do(authenticatedRequest(t, "POST", server.URL+"/rotate", ""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 409 || reconnects.Load() != 0 || !s.snapshot().Connected || s.snapshot().ActiveFlows != 1 || s.snapshot().Rotating {
		t.Fatalf("drain timeout disturbed stream: %+v status=%d", s.snapshot(), resp.StatusCode)
	}
	cancel()
}

func TestRotationDoesNotClaimSameIPChanged(t *testing.T) {
	s := newRelay(testToken, nil)
	s.state.Connected = true
	s.state.PublicIP = "192.0.2.1"
	s.state.IPScope = "opencode.ai"
	s.reconnect = func(context.Context) error { return nil }
	s.probe = func(context.Context) (string, string, string, error) { return "192.0.2.1", "on", "opencode.ai", nil }
	w := httptest.NewRecorder()
	s.ServeHTTP(w, authenticatedRequest(t, "POST", "http://relay/rotate", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"changed":false`) || s.snapshot().Rotations != 0 {
		t.Fatalf("unverified IP rotation claimed: %s", w.Body)
	}
}

func TestConfigureWARPRenewalSequenceAndFailure(t *testing.T) {
	var calls []string
	command := func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	if err := configureWARP(context.Background(), command, true); err != nil {
		t.Fatal(err)
	}
	expected := []string{"disconnect", "registration delete", "registration new", "mode proxy", "proxy port 40000", "connect"}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatalf("registration renewal sequence: %v", calls)
	}
	calls = nil
	command = func(_ context.Context, args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if call == "registration new" {
			return errors.New("registration failed")
		}
		return nil
	}
	if err := configureWARP(context.Background(), command, true); err == nil {
		t.Fatal("registration error lost")
	}
	if !reflect.DeepEqual(calls, expected[:3]) {
		t.Fatalf("continued after failed registration: %v", calls)
	}
}

func TestRenewalRotationIsBoundedAndStopsWhenIPChanges(t *testing.T) {
	for _, changeAt := range []int{0, 1, 2} {
		s := newRelay(testToken, nil)
		s.state.Connected = true
		s.state.PublicIP = "192.0.2.1"
		s.state.IPScope = "opencode.ai"
		calls := 0
		s.renew = func(context.Context) error { calls++; return nil }
		s.probe = func(context.Context) (string, string, string, error) {
			if changeAt != 0 && calls >= changeAt {
				return "192.0.2.2", "on", "opencode.ai", nil
			}
			return "192.0.2.1", "on", "opencode.ai", nil
		}
		r := authenticatedRequest(t, "POST", "http://relay/rotate", "")
		r.Body = io.NopCloser(strings.NewReader(`{"renew_registration":true,"max_attempts":2}`))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		expectedCalls := 2
		if changeAt == 1 {
			expectedCalls = 1
		}
		if w.Code != 200 || calls != expectedCalls || s.snapshot().RegistrationRenewals != uint64(expectedCalls) {
			t.Fatalf("changeAt=%d, calls=%d, status=%d, body=%s", changeAt, calls, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), `"changed":`+map[bool]string{true: "true", false: "false"}[changeAt != 0]) {
			t.Fatalf("changeAt=%d, result=%s", changeAt, w.Body)
		}
	}
	for _, body := range []string{`{"renew_registration":true,"max_attempts":3}`, `{"renew_registration":true,"max_attempts":-1}`, `{"max_attempts":2}`, `{"unknown":true}`, `{} {}`} {
		s := newRelay(testToken, nil)
		s.renew = func(context.Context) error { t.Fatal("invalid options executed renewal"); return nil }
		r := authenticatedRequest(t, "POST", "http://relay/rotate", "")
		r.Body = io.NopCloser(strings.NewReader(body))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("accepted invalid options %s: %d", body, w.Code)
		}
	}
}

func TestTracePrefersTargetAndLabelsFallback(t *testing.T) {
	for _, targetAvailable := range []bool{true, false} {
		var hosts []string
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)
			if r.URL.Host == "opencode.ai" && !targetAvailable {
				return nil, errors.New("target unavailable")
			}
			ip := "192.0.2.1"
			if r.URL.Host == "www.cloudflare.com" {
				ip = "192.0.2.2"
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ip=" + ip + "\nwarp=on\n"))}, nil
		})}
		ip, warp, scope, err := traceProbe(client)(context.Background())
		expectedIP, expectedScope, expectedCalls := "192.0.2.1", "opencode.ai", 1
		if !targetAvailable {
			expectedIP, expectedScope, expectedCalls = "192.0.2.2", "cloudflare.com", 2
		}
		if err != nil || ip != expectedIP || scope != expectedScope || warp != "on" || len(hosts) != expectedCalls || hosts[0] != "opencode.ai" {
			t.Fatalf("targetAvailable=%v ip=%s warp=%s scope=%s calls=%v err=%v", targetAvailable, ip, warp, scope, hosts, err)
		}
	}
}

func TestFallbackScopeDoesNotClaimTargetIPChanged(t *testing.T) {
	s := newRelay(testToken, nil)
	s.state.Connected = true
	s.state.PublicIP = "192.0.2.1"
	s.state.IPScope = "opencode.ai"
	s.reconnect = func(context.Context) error { return nil }
	s.probe = func(context.Context) (string, string, string, error) { return "192.0.2.2", "on", "cloudflare.com", nil }
	w := httptest.NewRecorder()
	s.ServeHTTP(w, authenticatedRequest(t, "POST", "http://relay/rotate", ""))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"changed":false`) || s.snapshot().Rotations != 0 {
		t.Fatalf("generic fallback IP falsely marked target changed: %s", w.Body)
	}
}

func TestExplicitDrainBlocksNewFlowsUntilResume(t *testing.T) {
	s, server, cancel, _, _ := streamFixture(t)
	drained := make(chan *http.Response, 1)
	go func() {
		response, _ := http.DefaultClient.Do(authenticatedRequest(t, "POST", server.URL+"/drain", ""))
		drained <- response
	}()
	deadline := time.Now().Add(time.Second)
	for !s.snapshot().Rotating && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, ok := s.acquire(); ok {
		t.Fatal("new flow admitted after drain started")
	}
	cancel()
	select {
	case response := <-drained:
		if response == nil {
			t.Fatal("drain request failed")
		}
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 || !strings.Contains(string(data), `"drained":true`) {
			t.Fatalf("drain %d %s", response.StatusCode, data)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after caller disconnect")
	}
	if _, ok := s.acquire(); ok {
		t.Fatal("explicit drain resumed itself")
	}
	resume, err := http.DefaultClient.Do(authenticatedRequest(t, "POST", server.URL+"/resume", ""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resume.Body.Close()
	if resume.StatusCode != 200 {
		t.Fatal("resume failed")
	}
	release, ok := s.acquire()
	if !ok {
		t.Fatal("resumed relay still blocked")
	}
	release()
}
