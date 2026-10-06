package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedVPNRotationAndMetadata(t *testing.T) {
	e := testEgress(t)
	var rotations atomic.Int32
	var seen RotationOptions
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rotate" {
			json.NewDecoder(r.Body).Decode(&seen)
			rotations.Add(1)
		}
		writeJSON(w, 200, map[string]any{"healthy": true, "connected": true, "backend": "wireguard", "public_ip": "198.51.100.9", "ip_scope": "opencode.ai", "vpn_server": "vpn.example", "vpn_endpoint": "198.51.100.1:51820", "latest_handshake": time.Now().Unix(), "changed": true})
	}))
	defer server.Close()
	config := defaultEgressConfig()
	config.Mode = "spread"
	config.Endpoints = []EgressEndpoint{{ID: "vpn1", Kind: "vpn", URL: server.URL, Enabled: true, RotateOnLimit: true}}
	if err := e.Save(config); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Probe(context.Background(), "vpn1"); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	r := e.routes["vpn1"]
	serverName, endpoint, handshake := r.VPNServer, r.VPNEndpoint, r.LatestHandshake
	e.mu.Unlock()
	if serverName != "vpn.example" || endpoint != "198.51.100.1:51820" || handshake == 0 {
		t.Fatal("VPN metadata lost")
	}
	body, err := e.Rotate(context.Background(), "vpn1", RotationOptions{SameServer: true, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := body["changed"].(bool); changed {
		t.Fatal("same IP was falsely returned as changed")
	}
	if !seen.SameServer || rotations.Load() != 1 {
		t.Fatal("same server request lost")
	}
}

func TestBackgroundRotationRechecksBusyAndConfigState(t *testing.T) {
	e := testEgress(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, map[string]any{"healthy": true})
	}))
	defer server.Close()
	endpoint := EgressEndpoint{ID: "vpn1", Kind: "vpn", URL: server.URL, Enabled: true, RotateOnLimit: true}
	config := defaultEgressConfig()
	config.Mode = "spread"
	config.Endpoints = []EgressEndpoint{endpoint}
	if err := e.Save(config); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.routes["vpn1"].Active = 1
	e.mu.Unlock()
	if _, err := e.Rotate(context.Background(), "vpn1", RotationOptions{MaxAttempts: 1, idleOnly: true, expected: endpoint}); !errors.Is(err, errVPNMaintenanceSkipped) {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.routes["vpn1"].Active = 0
	e.routes["vpn1"].Endpoint.Enabled = false
	e.mu.Unlock()
	if _, err := e.Rotate(context.Background(), "vpn1", RotationOptions{MaxAttempts: 1, idleOnly: true, expected: endpoint}); !errors.Is(err, errVPNMaintenanceSkipped) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("background operation ignored changed state")
	}
}
func TestVPNMaintenanceSkipsActiveStreamsAndSwitchesLimitedIdleExit(t *testing.T) {
	e := testEgress(t)
	var rotations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rotate" {
			rotations.Add(1)
		}
		writeJSON(w, 200, map[string]any{"healthy": true, "public_ip": "198.51.100.2", "changed": true})
	}))
	defer server.Close()
	config := defaultEgressConfig()
	config.Mode = "spread"
	config.Endpoints = []EgressEndpoint{{ID: "vpn1", Kind: "vpn", URL: server.URL, Enabled: true, RotateOnLimit: true}}
	if err := e.Save(config); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	r := e.routes["vpn1"]
	r.IP = "198.51.100.1"
	r.Active = 1
	e.limits[routeKey(r)] = map[string]egressLimit{"": {Until: time.Now().Add(time.Hour).Unix(), Scope: "ip"}}
	e.mu.Unlock()
	e.maintainVPNPass(context.Background())
	if rotations.Load() != 0 {
		t.Fatal("maintenance rotated an active stream")
	}
	e.mu.Lock()
	r.Active = 0
	e.mu.Unlock()
	e.maintainVPNPass(context.Background())
	if rotations.Load() != 1 {
		t.Fatal("limited idle tunnel was not switched")
	}
	e.mu.Lock()
	oldBucket := e.limits["ip:198.51.100.1"][""].Until
	newUntil := e.cooldown(r, "m").Until
	e.mu.Unlock()
	if oldBucket <= time.Now().Unix() || newUntil > time.Now().Unix() {
		t.Fatal("old cooldown cleared or applied to a new IP")
	}
}
func TestVPNMaintenanceHonorsDisableAndBackoff(t *testing.T) {
	e := testEgress(t)
	var rotations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rotations.Add(1)
		writeJSON(w, 200, map[string]any{"healthy": true, "public_ip": "198.51.100.1", "changed": false})
	}))
	defer server.Close()
	config := defaultEgressConfig()
	config.Mode = "spread"
	config.Endpoints = []EgressEndpoint{{ID: "vpn1", Kind: "vpn", URL: server.URL, Enabled: true, RotateOnLimit: true}}
	if err := e.Save(config); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	r := e.routes["vpn1"]
	r.Healthy = false
	e.mu.Unlock()
	e.maintainVPNPass(context.Background())
	e.maintainVPNPass(context.Background())
	if rotations.Load() != 1 {
		t.Fatal("maintenance did not back off")
	}
}

func TestVPNLowerIDRejoiningDuplicateIPImmediatelyDemotesHigherID(t *testing.T) {
	for _, operation := range []string{"probe", "rotate"} {
		t.Run(operation, func(t *testing.T) {
			e := testEgress(t)
			var lowIP atomic.Value
			lowIP.Store("198.51.100.1")
			low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 200, map[string]any{"healthy": true, "connected": true, "public_ip": lowIP.Load().(string), "ip_scope": "opencode.ai", "changed": true})
			}))
			defer low.Close()
			high := fakeWarp(t, "198.51.100.2", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
			saveWarpExits(t, e, EgressEndpoint{ID: "a", Kind: "vpn", URL: low.URL, Enabled: true}, EgressEndpoint{ID: "b", Kind: "vpn", URL: high.URL, Enabled: true})
			for _, id := range []string{"b", "a"} {
				if _, err := e.Probe(context.Background(), id); err != nil {
					t.Fatal(err)
				}
			}
			lowIP.Store("198.51.100.2")
			if operation == "probe" {
				if _, err := e.Probe(context.Background(), "a"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := e.Rotate(context.Background(), "a"); err != nil {
					t.Fatal(err)
				}
			}
			e.mu.Lock()
			lowHealthy, highHealthy := e.routes["a"].Healthy, e.routes["b"].Healthy
			blocked, lastError := e.routes["b"].duplicateBlocked, e.routes["b"].LastError
			// Even after the preferred route becomes unavailable, merely aging the
			// demoted peer's probe must not make its old duplicate IP selectable.
			e.routes["a"].Healthy = false
			e.routes["b"].LastProbe = time.Now().Add(-20 * time.Second)
			e.mu.Unlock()
			if !lowHealthy || highHealthy || !blocked || !strings.Contains(lastError, "重複") {
				t.Fatalf("winner/demotion incorrect: low=%v high=%v blocked=%v error=%q", lowHealthy, highHealthy, blocked, lastError)
			}
			route, _, err := e.pick(context.Background(), "fixture", map[string]bool{"a": true})
			if route != nil {
				e.release(route)
			}
			if err == nil || route != nil {
				t.Fatal("stale-probe age resurrected duplicate VPN")
			}
			// A fresh successful target probe can recover the now uncontested peer.
			if _, err := e.Probe(context.Background(), "b"); err != nil {
				t.Fatal(err)
			}
			route, _, err = e.pick(context.Background(), "fixture", map[string]bool{"a": true})
			if err != nil || route == nil || route.Endpoint.ID != "b" {
				t.Fatal("fresh verified probe did not recover demoted peer")
			}
			e.release(route)
		})
	}
}

func TestVPNInFlightSuccessCannotRestoreDemotedDuplicate(t *testing.T) {
	e := testEgress(t)
	e.allowedOrigin = "http://fixture.invalid"
	var lowIP atomic.Value
	lowIP.Store("198.51.100.1")
	low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"healthy": true, "connected": true, "public_ip": lowIP.Load().(string), "ip_scope": "opencode.ai"})
	}))
	defer low.Close()
	started, finish := make(chan struct{}), make(chan struct{})
	high := fakeWarp(t, "198.51.100.2", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-finish:
		case <-r.Context().Done():
			return
		}
		writeJSON(w, 200, map[string]bool{"fixture": true})
	})
	// Put the higher ID first so an existing request is already using it when
	// the lower ID's new target-IP probe reveals a collision.
	saveWarpExits(t, e, EgressEndpoint{ID: "b", Kind: "vpn", URL: high.URL, Enabled: true}, EgressEndpoint{ID: "a", Kind: "vpn", URL: low.URL, Enabled: true})
	for _, id := range []string{"b", "a"} {
		if _, err := e.Probe(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	finished := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest("POST", e.URL, strings.NewReader(`{"model":"fixture"}`))
		request.Header.Set("X-OCPOOL-Egress-Auth", e.token)
		request.Header.Set("X-OCPOOL-Target", "http://fixture.invalid/zen/v1/chat/completions")
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != 200 {
				err = errors.New("fixture response failed")
			}
		}
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(finish)
		t.Fatal("fixture request did not start")
	}
	lowIP.Store("198.51.100.2")
	if _, err := e.Probe(context.Background(), "a"); err != nil {
		close(finish)
		t.Fatal(err)
	}
	close(finish)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fixture response did not finish")
	}
	e.mu.Lock()
	highHealthy, blocked, lastError, successes, sticky := e.routes["b"].Healthy, e.routes["b"].duplicateBlocked, e.routes["b"].LastError, e.routes["b"].Successes, e.sticky
	e.mu.Unlock()
	if highHealthy || !blocked || !strings.Contains(lastError, "重複") || successes != 1 || sticky == "b" {
		t.Fatalf("in-flight response restored duplicate: healthy=%v blocked=%v error=%q successes=%d sticky=%s", highHealthy, blocked, lastError, successes, sticky)
	}
}
