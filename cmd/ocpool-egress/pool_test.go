package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureWGPool(t *testing.T, transports ...roundTripFunc) *wgPool {
	t.Helper()
	p := &wgPool{token: testToken, limits: map[string]poolLimit{}, failedEndpoints: map[string]time.Time{}, changed: make(chan struct{}), wake: make(chan struct{}, 1)}
	for index, transport := range transports {
		candidate := wgCandidate{ID: fmt.Sprintf("server-%d", index), Endpoint: fmt.Sprintf("198.51.100.%d:51820", index+1), PublicKey: fixtureWGKey(byte(index + 2)), Hostname: fmt.Sprintf("city-%d.prod.surfshark.com", index)}
		p.candidates = append(p.candidates, candidate)
		p.slots = append(p.slots, &wgPoolSlot{ID: fmt.Sprintf("wg%d", index), IP: fmt.Sprintf("192.0.2.%d", index+1), Healthy: true, Candidate: candidate, Handshake: time.Now().Unix(), LastProbe: time.Now(), client: &http.Client{Transport: transport}})
	}
	return p
}

func poolRequest(t *testing.T, serverURL, model string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, serverURL+"/fetch", strings.NewReader(`{"model":"`+model+`","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Relay-Auth", testToken)
	r.Header.Set("X-Relay-Target", "https://opencode.ai")
	r.Header.Set("X-Relay-Path", "/inference/openai/v1/chat/completions")
	r.Header.Set("X-Relay-Method", http.MethodPost)
	return r
}

func fixturePoolResponse(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{"Content-Type": []string{"application/json"}}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestWGPoolRateLimitFailoverKeepsOtherSSE(t *testing.T) {
	writerReady := make(chan *io.PipeWriter, 1)
	canceled := make(chan struct{})
	var limited, spare, replaced atomic.Int32
	p := fixtureWGPool(t,
		func(r *http.Request) (*http.Response, error) {
			reader, writer := io.Pipe()
			writerReady <- writer
			go func() {
				_, _ = writer.Write([]byte("data: first\n\n"))
				<-r.Context().Done()
				_ = writer.CloseWithError(r.Context().Err())
				close(canceled)
			}()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
		},
		func(r *http.Request) (*http.Response, error) {
			limited.Add(1)
			return fixturePoolResponse(http.StatusTooManyRequests, `{"error":{"type":"FreeUsageLimitError","message":"IP address limit"}}`, http.Header{"Retry-After": []string{"120"}, "Content-Type": []string{"application/json"}}), nil
		},
		func(r *http.Request) (*http.Response, error) {
			spare.Add(1)
			return fixturePoolResponse(http.StatusOK, "data: spare\n\ndata: [DONE]\n\n", http.Header{"Content-Type": []string{"text/event-stream"}}), nil
		},
		func(r *http.Request) (*http.Response, error) {
			return fixturePoolResponse(http.StatusOK, "fourth", nil), nil
		},
	)
	p.slots[0].replace = func(context.Context, wgCandidate) error { replaced.Add(1); return nil }
	server := httptest.NewServer(p)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, err := http.DefaultClient.Do(poolRequest(t, server.URL, "m").WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first SSE: %q %v", line, err)
	}
	_, _ = reader.ReadString('\n')
	writer := <-writerReady
	start := time.Now()
	second, err := http.DefaultClient.Do(poolRequest(t, server.URL, "m"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(second.Body)
	_ = second.Body.Close()
	if err != nil || second.StatusCode != http.StatusOK || !strings.Contains(string(body), "spare") || limited.Load() != 1 || spare.Load() != 1 {
		t.Fatalf("spare did not serve limit request: status=%d body=%s calls=%d/%d err=%v", second.StatusCode, body, limited.Load(), spare.Load(), err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("failover waited for unrelated SSE stream")
	}
	if replaced.Load() != 0 {
		t.Fatal("active SSE tunnel was replaced")
	}
	go func() { _, _ = writer.Write([]byte("data: still-active\n\n")) }()
	if line, err := reader.ReadString('\n'); err != nil || line != "data: still-active\n" {
		t.Fatalf("first SSE was interrupted by failover: %q %v", line, err)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not cancel leased upstream")
	}
}

func TestWGPoolAccountQuotaReturnsWithoutExitRetry(t *testing.T) {
	var first, spare atomic.Int32
	p := fixtureWGPool(t,
		func(r *http.Request) (*http.Response, error) {
			first.Add(1)
			return fixturePoolResponse(http.StatusTooManyRequests, `{"error":{"type":"GoUsageLimitError","message":"account subscription quota"}}`, http.Header{"Retry-After": []string{"120"}}), nil
		},
		func(r *http.Request) (*http.Response, error) {
			spare.Add(1)
			return fixturePoolResponse(http.StatusOK, "spare", nil), nil
		},
	)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, poolRequest(t, "http://relay", "m"))
	if w.Code != http.StatusTooManyRequests || first.Load() != 1 || spare.Load() != 0 || !strings.Contains(w.Body.String(), "GoUsageLimitError") {
		t.Fatalf("account quota rotated exit: status=%d first=%d spare=%d body=%s", w.Code, first.Load(), spare.Load(), w.Body)
	}
}

func TestWGPoolReturnsOriginal429WhenSpareFails(t *testing.T) {
	p := fixtureWGPool(t,
		func(r *http.Request) (*http.Response, error) {
			return fixturePoolResponse(http.StatusTooManyRequests, `{"error":{"type":"FreeUsageLimitError","message":"original limit"}}`, http.Header{"Retry-After": []string{"37"}}), nil
		},
		func(r *http.Request) (*http.Response, error) { return nil, errors.New("unreachable spare") },
	)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, poolRequest(t, "http://relay", "m"))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "37" || !strings.Contains(w.Body.String(), "original limit") {
		t.Fatalf("original 429 lost after spare failure: status=%d header=%v body=%s", w.Code, w.Header(), w.Body)
	}
}

func TestWGPoolUnknown429CooldownOnlyBlocksItsModel(t *testing.T) {
	var first, spare atomic.Int32
	p := fixtureWGPool(t,
		func(r *http.Request) (*http.Response, error) {
			first.Add(1)
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"model":"other"`) {
				return fixturePoolResponse(http.StatusOK, "other model", nil), nil
			}
			return fixturePoolResponse(http.StatusTooManyRequests, `{"error":{"type":"rate_limit_error","message":"model throttle"}}`, http.Header{"Retry-After": []string{"120"}}), nil
		},
		func(r *http.Request) (*http.Response, error) {
			spare.Add(1)
			return fixturePoolResponse(http.StatusOK, "spare", nil), nil
		},
	)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, poolRequest(t, "http://relay", "m"))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d did not use spare: status=%d body=%s", i, w.Code, w.Body)
		}
	}
	if first.Load() != 1 || spare.Load() != 2 {
		t.Fatalf("model-scoped cooldown did not exclude limited exit: first=%d spare=%d", first.Load(), spare.Load())
	}
	// Return the round-robin cursor to the first slot; it remains available for
	// a model outside this IP+model cooldown.
	p.next = 0
	w := httptest.NewRecorder()
	p.ServeHTTP(w, poolRequest(t, "http://relay", "other"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "other model") || first.Load() != 2 {
		t.Fatalf("other model inherited unknown429 cooldown: status=%d body=%s calls=%d", w.Code, w.Body, first.Load())
	}
}

func TestWGPoolRotationSkipsBusyDevice(t *testing.T) {
	p := fixtureWGPool(t, func(*http.Request) (*http.Response, error) { return fixturePoolResponse(http.StatusOK, "ok", nil), nil })
	var replacements atomic.Int32
	p.slots[0].replace = func(context.Context, wgCandidate) error { replacements.Add(1); return nil }
	p.slots[0].Active = 1
	p.noteLimit(p.slots[0].IP, "", "ip", "120")
	err := p.rotateSlot(context.Background(), 0, false, 3)
	if err == nil || replacements.Load() != 0 || !p.slots[0].Healthy || p.slots[0].Rotating {
		t.Fatalf("busy cooling device was disturbed: replacements=%d healthy=%v rotating=%v err=%v", replacements.Load(), p.slots[0].Healthy, p.slots[0].Rotating, err)
	}
}

func TestWGPoolCooldownStateSurvivesReloadAndKeepsModelScope(t *testing.T) {
	p := fixtureWGPool(t, func(*http.Request) (*http.Response, error) { return fixturePoolResponse(http.StatusOK, "ok", nil), nil })
	p.stateFile = filepath.Join(t.TempDir(), "private", "cooldowns.json")
	ip := p.slots[0].IP
	p.noteLimit(ip, "m", "egress_model", "120")
	p.noteLimit("192.0.2.55", "other", "ip", "300")
	reloaded := &wgPool{stateFile: p.stateFile, limits: map[string]poolLimit{}}
	if err := reloaded.loadLimits(); err != nil {
		t.Fatal(err)
	}
	if reloaded.limitLocked(ip, "m").Until <= time.Now().Unix() || reloaded.limitLocked(ip, "other").Until != 0 {
		t.Fatal("persisted model cooldown changed scope")
	}
	if reloaded.limitLocked("192.0.2.55", "another").Until <= time.Now().Unix() {
		t.Fatal("persisted IP-wide cooldown lost global scope")
	}
}

func TestWGPoolCandidatesExcludeLivePeerGroupAliases(t *testing.T) {
	p := fixtureWGPool(t,
		func(*http.Request) (*http.Response, error) { return fixturePoolResponse(http.StatusOK, "ok", nil), nil },
		func(*http.Request) (*http.Response, error) { return fixturePoolResponse(http.StatusOK, "ok", nil), nil },
	)
	alias := p.slots[1].Candidate
	alias.ID, alias.Endpoint = "second-IP-same-peer", "198.51.100.99:51820"
	allowed := wgCandidate{ID: "new-independent-peer", Endpoint: "198.51.100.88:51820", PublicKey: fixtureWGKey(9), Hostname: "other.prod.surfshark.com"}
	p.candidates = append(p.candidates, alias, allowed)
	for i := 0; i < 20; i++ {
		candidate, ok := p.candidateLocked(0, false)
		if !ok || candidate.Endpoint != allowed.Endpoint {
			t.Fatalf("live peer hostname alias was reused: %+v", candidate)
		}
	}
}

func TestWGPoolAllCoolingRequestsReturnFastRetryAfter(t *testing.T) {
	var calls atomic.Int32
	p := fixtureWGPool(t,
		func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return fixturePoolResponse(http.StatusOK, "unexpected", nil), nil
		},
		func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return fixturePoolResponse(http.StatusOK, "unexpected", nil), nil
		},
	)
	for _, slot := range p.slots {
		p.noteLimit(slot.IP, "", "ip", "120")
	}
	w := httptest.NewRecorder()
	start := time.Now()
	p.ServeHTTP(w, poolRequest(t, "http://relay", "m"))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || calls.Load() != 0 || time.Since(start) > time.Second {
		t.Fatalf("all-cooling request reached upstream or blocked: status=%d headers=%v calls=%d duration=%v", w.Code, w.Header(), calls.Load(), time.Since(start))
	}
}

func TestWGPoolCanceledHeaderWaitKeepsSharedDeviceHealthy(t *testing.T) {
	started := make(chan struct{})
	p := fixtureWGPool(t, func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		p.ServeHTTP(httptest.NewRecorder(), poolRequest(t, "http://relay", "m").WithContext(ctx))
		close(finished)
	}()
	<-started
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("canceled header wait did not return")
	}
	p.mu.Lock()
	healthy, active := p.slots[0].Healthy, p.slots[0].Active
	p.mu.Unlock()
	if !healthy || active != 0 {
		t.Fatalf("caller cancellation tainted shared VPN: healthy=%v active=%d", healthy, active)
	}
}

func TestWGPoolFailoverPreservesNativeRequestAndStripsControlHeaders(t *testing.T) {
	original := `{"model":"m","tools":[{"name":"read"}],"reasoning":{"effort":"low"},"extension":true}`
	check := func(r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != original || r.Header.Get("Authorization") != "Bearer fixture-upstream-key" || r.Header.Get("X-Opencode-Org-Id") != "fixture-org" {
			t.Error("retry changed native request or credentials")
		}
		if r.Header.Get("X-Relay-Auth") != "" || r.Header.Get("X-Relay-Path") != "" || r.Header.Get("X-Hop") != "" {
			t.Error("relay control or hop header reached upstream")
		}
	}
	p := fixtureWGPool(t,
		func(r *http.Request) (*http.Response, error) {
			check(r)
			return fixturePoolResponse(http.StatusTooManyRequests, `{"error":{"type":"FreeUsageLimitError"}}`, http.Header{"Retry-After": []string{"120"}}), nil
		},
		func(r *http.Request) (*http.Response, error) {
			check(r)
			return fixturePoolResponse(http.StatusOK, "replayed", nil), nil
		},
	)
	r := poolRequest(t, "http://relay", "m")
	r.Body = io.NopCloser(strings.NewReader(original))
	r.Header.Set("Authorization", "Bearer fixture-upstream-key")
	r.Header.Set("X-Opencode-Org-Id", "fixture-org")
	r.Header.Set("Connection", "X-Hop")
	r.Header.Set("X-Hop", "fixture-control")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "replayed" || w.Header().Get("X-Relay-Exit-IP") != p.slots[1].IP {
		t.Fatalf("successful replay did not identify actual exit: status=%d body=%s headers=%v", w.Code, w.Body, w.Header())
	}
}

func TestWGPoolRefreshesHandshakeDuringActiveSSEWithoutReplacement(t *testing.T) {
	p := fixtureWGPool(t, func(*http.Request) (*http.Response, error) {
		return fixturePoolResponse(http.StatusOK, "unused", nil), nil
	})
	slot := p.slots[0]
	slot.Active = 1
	slot.Handshake = time.Now().Add(-181 * time.Second).Unix()
	slot.LastProbe = time.Now().Add(-46 * time.Second)
	slot.LastMaintenance = time.Time{}
	var replacements atomic.Int32
	slot.replace = func(context.Context, wgCandidate) error { replacements.Add(1); return nil }
	slot.probe = func(context.Context) (string, string, string, error) { return slot.IP, "off", "opencode.ai", nil }
	slot.details = func(context.Context) (vpnDetails, error) {
		return vpnDetails{Endpoint: slot.Candidate.Endpoint, Server: slot.Candidate.Hostname, Handshake: time.Now().Unix()}, nil
	}
	if leased, _, _, _ := p.acquire("m", map[string]bool{}); leased != nil {
		p.release(leased)
		t.Fatal("stale handshake admitted a new lease before refresh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() { p.maintain(ctx); close(finished) }()
	p.wake <- struct{}{}
	deadline := time.Now().Add(time.Second)
	refreshed := false
	for time.Now().Before(deadline) {
		p.mu.Lock()
		refreshed = freshHandshake(slot.Handshake, time.Now()) && !slot.Probing
		active := slot.Active
		p.mu.Unlock()
		if active != 1 {
			t.Fatal("health refresh changed active SSE leases")
		}
		if refreshed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !refreshed || replacements.Load() != 0 {
		t.Fatalf("active SSE device did not refresh in place: refreshed=%v replacements=%d", refreshed, replacements.Load())
	}
	leased, _, _, err := p.acquire("m", map[string]bool{})
	if err != nil || leased != slot {
		t.Fatalf("refreshed device remained unavailable: %v", err)
	}
	p.release(leased)
	p.mu.Lock()
	active := slot.Active
	p.mu.Unlock()
	if active != 1 {
		t.Fatalf("new lease disturbed existing SSE: active=%d", active)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not stop")
	}
}

func TestWGPoolSameDevice429RetriesSpareWhileItsSSEContinues(t *testing.T) {
	writerReady := make(chan *io.PipeWriter, 1)
	canceled := make(chan struct{})
	var firstDeviceCalls, spareCalls, replacements atomic.Int32
	p := fixtureWGPool(t,
		func(r *http.Request) (*http.Response, error) {
			if firstDeviceCalls.Add(1) > 1 {
				return fixturePoolResponse(http.StatusTooManyRequests, `{"error":{"type":"FreeUsageLimitError","message":"IP address limit"}}`, http.Header{"Retry-After": []string{"120"}}), nil
			}
			reader, writer := io.Pipe()
			writerReady <- writer
			go func() {
				_, _ = writer.Write([]byte("data: first\n\n"))
				<-r.Context().Done()
				_ = writer.CloseWithError(r.Context().Err())
				close(canceled)
			}()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
		},
		func(*http.Request) (*http.Response, error) {
			spareCalls.Add(1)
			return fixturePoolResponse(http.StatusOK, "spare", nil), nil
		},
	)
	p.slots[0].replace = func(context.Context, wgCandidate) error { replacements.Add(1); return nil }
	server := httptest.NewServer(p)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := http.DefaultClient.Do(poolRequest(t, server.URL, "m").WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	reader := bufio.NewReader(first.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first SSE: %q %v", line, err)
	}
	_, _ = reader.ReadString('\n')
	writer := <-writerReady
	// Represent more outstanding work on the spare so the least-active policy
	// sends request two to WG0, which already owns the first streaming lease.
	p.mu.Lock()
	p.next = 0
	p.slots[1].Active = 2
	p.mu.Unlock()
	start := time.Now()
	second, err := http.DefaultClient.Do(poolRequest(t, server.URL, "m"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(second.Body)
	_ = second.Body.Close()
	if err != nil || second.StatusCode != http.StatusOK || string(body) != "spare" || firstDeviceCalls.Load() != 2 || spareCalls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("same-device limit did not retry spare immediately: status=%d body=%s first=%d spare=%d duration=%v err=%v", second.StatusCode, body, firstDeviceCalls.Load(), spareCalls.Load(), time.Since(start), err)
	}
	p.mu.Lock()
	active := p.slots[0].Active
	cooling := p.limitLocked(p.slots[0].IP, "").Until > time.Now().Unix()
	p.mu.Unlock()
	if active != 1 || !cooling || p.rotateSlot(context.Background(), 0, false, 3) == nil || replacements.Load() != 0 {
		t.Fatalf("cooling streaming device was replaced: active=%d cooling=%v replacements=%d", active, cooling, replacements.Load())
	}
	go func() { _, _ = writer.Write([]byte("data: still-active\n\n")) }()
	if line, err := reader.ReadString('\n'); err != nil || line != "data: still-active\n" {
		t.Fatalf("same WG SSE was interrupted by its other request's limit: %q %v", line, err)
	}
	p.mu.Lock()
	p.slots[1].Active = 0
	p.next = 0
	p.mu.Unlock()
	third, err := http.DefaultClient.Do(poolRequest(t, server.URL, "m"))
	if err != nil {
		t.Fatal(err)
	}
	thirdBody, err := io.ReadAll(third.Body)
	_ = third.Body.Close()
	if err != nil || third.StatusCode != http.StatusOK || string(thirdBody) != "spare" || firstDeviceCalls.Load() != 2 || spareCalls.Load() != 2 {
		t.Fatalf("new request reused cooling WG: status=%d body=%s first=%d spare=%d err=%v", third.StatusCode, thirdBody, firstDeviceCalls.Load(), spareCalls.Load(), err)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("first upstream did not cancel")
	}
}
