package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPooledEgressPublicMetadataAndLifecycle(t *testing.T) {
	e := testEgress(t)
	poolHealth := func() map[string]any {
		return map[string]any{
			"healthy": true, "connected": true, "backend": "wireguard", "public_ip": "198.51.100.1", "ip_scope": "opencode.ai",
			"pooled": true, "candidate_count": 100, "pool_size": 3, "ready_count": 2, "cooling_count": 1,
			"private_key": "root-private-sentinel",
			"slots": []any{map[string]any{
				"id": "wg0", "healthy": true, "connected": true, "public_ip": "198.51.100.1", "ip_scope": "opencode.ai",
				"backend": "wireguard", "vpn_server": "vpn.example", "vpn_endpoint": "198.51.100.2:51820",
				"latest_handshake": time.Now().Unix(), "active_flows": 2, "requests": 3, "successes": 2, "failures": 1,
				"private_key": "slot-private-sentinel", "config": map[string]any{"token": "nested-private-sentinel"},
			}},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/peer/") {
			writeJSON(w, 200, map[string]any{"healthy": true, "public_ip": "198.51.100.1", "ip_scope": "opencode.ai"})
			return
		}
		if r.URL.Path == "/rotate" {
			var options RotationOptions
			if err := json.NewDecoder(r.Body).Decode(&options); err != nil {
				t.Error(err)
			}
			newIP := "198.51.100.9"
			if options.SameServer {
				newIP = "198.51.100.2"
			}
			writeJSON(w, 200, map[string]any{"healthy": true, "changed": true, "old_ip": "198.51.100.2", "new_ip": newIP, "ip_scope": "opencode.ai", "health": poolHealth()})
			return
		}
		writeJSON(w, 200, poolHealth())
	}))
	defer server.Close()
	config := defaultEgressConfig()
	config.Mode = "spread"
	config.Endpoints = []EgressEndpoint{
		{ID: "pool", Name: "VPN pool", Kind: "vpn", URL: server.URL, Enabled: true, RotateOnLimit: true},
		{ID: "vpn-peer", Kind: "vpn", URL: server.URL + "/peer", Enabled: true},
	}
	if err := e.Save(config); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Probe(context.Background(), "pool"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Probe(context.Background(), "vpn-peer"); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	r := e.routes["pool"]
	pooled, size, ready, cooling, candidates := r.Pooled, r.PoolSize, r.ReadyCount, r.CoolingCount, r.CandidateCount
	key, peerHealthy := routeKey(r), e.routes["vpn-peer"].Healthy
	e.mu.Unlock()
	if !pooled || size != 3 || ready != 2 || cooling != 1 || candidates != 100 || key != "exit:pool" || !peerHealthy {
		t.Fatal("pooled health or logical route identity was lost")
	}
	raw, err := json.Marshal(e.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-sentinel", "private_key", "nested-private"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("public health retained secret metadata: %s", forbidden)
		}
	}
	if !strings.Contains(string(raw), `"active_flows":2`) || !strings.Contains(string(raw), `"candidate_count":100`) {
		t.Fatal("public pool details were omitted")
	}
	changed, err := e.Rotate(context.Background(), "pool")
	if err != nil || changed["changed"] != true {
		t.Fatalf("per-slot measured IP change lost: %v, %v", changed, err)
	}
	same, err := e.Rotate(context.Background(), "pool", RotationOptions{SameServer: true})
	if err != nil || same["changed"] != false {
		t.Fatalf("same-slot IP was falsely reported as changed: %v, %v", same, err)
	}
	e.mu.Lock()
	representative, slotCount := r.IP, len(r.PoolSlots)
	e.mu.Unlock()
	if representative != "198.51.100.1" || slotCount != 1 {
		t.Fatal("nested rotation health was not refreshed independently of the rotated slot")
	}
	config.Endpoints[0].URL = server.URL + "/replacement"
	if err := e.Save(config); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	r = e.routes["pool"]
	reset := !r.Pooled && len(r.PoolSlots) == 0 && r.PoolSize == 0 && r.ReadyCount == 0 && r.CandidateCount == 0
	e.mu.Unlock()
	if !reset {
		t.Fatal("changing the managed service retained old pool metadata")
	}
}

func TestPooledBusyRotationPreservesLiveHealth(t *testing.T) {
	e := testEgress(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Relay-Auth") != "pool-test-token" {
			t.Error("managed health request missing authentication")
		}
		if r.URL.Path == "/rotate" {
			writeJSON(w, 409, map[string]any{"changed": false, "error": "all VPN devices are streaming; tunnels preserved"})
			return
		}
		writeJSON(w, 200, map[string]any{"healthy": true, "pooled": true, "public_ip": "198.51.100.1", "ip_scope": "opencode.ai", "pool_size": 1, "ready_count": 1, "slots": []any{map[string]any{"id": "wg0", "healthy": true, "connected": true, "active_flows": 3, "public_ip": "198.51.100.1"}}})
	}))
	defer server.Close()
	config := defaultEgressConfig()
	config.Mode = "spread"
	config.Endpoints = []EgressEndpoint{{ID: "pool", Kind: "vpn", URL: server.URL, Secret: "pool-test-token", Enabled: true}}
	if err := e.Save(config); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Probe(context.Background(), "pool"); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	previousProbe := e.routes["pool"].LastProbe
	e.mu.Unlock()
	if _, err := e.Rotate(context.Background(), "pool"); err == nil {
		t.Fatal("busy rotation should have been rejected")
	}
	e.mu.Lock()
	r := e.routes["pool"]
	preserved := r.Healthy && r.Pooled && r.IP == "198.51.100.1" && len(r.PoolSlots) == 1 && r.PoolSlots[0].ActiveFlows == 3 && r.LastProbe.Equal(previousProbe)
	e.mu.Unlock()
	if !preserved {
		t.Fatal("rejecting a busy manual rotation demoted or changed live pool status")
	}
}

func TestPooledAdminPollingRefreshesFlowsAndKeepsCacheOnFailure(t *testing.T) {
	e := testEgress(t)
	var flows atomic.Int32
	var failing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			writeJSON(w, 503, map[string]string{"error": "temporary management failure"})
			return
		}
		writeJSON(w, 200, map[string]any{"healthy": true, "pooled": true, "public_ip": "198.51.100.1", "ip_scope": "opencode.ai", "pool_size": 1, "ready_count": 1, "slots": []any{map[string]any{"id": "wg0", "healthy": true, "connected": true, "active_flows": flows.Load(), "public_ip": "198.51.100.1"}}})
	}))
	defer server.Close()
	config := defaultEgressConfig()
	config.Mode = "spread"
	config.Endpoints = []EgressEndpoint{{ID: "pool", Kind: "vpn", URL: server.URL, Enabled: true}}
	if err := e.Save(config); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Probe(context.Background(), "pool"); err != nil {
		t.Fatal(err)
	}
	flows.Store(3)
	app := &App{egress: e}
	for _, fail := range []bool{false, true} {
		failing.Store(fail)
		writer := httptest.NewRecorder()
		app.getEgress(writer, httptest.NewRequest("GET", "/api/egress", nil))
		if writer.Code != 200 || !strings.Contains(writer.Body.String(), `"active_flows":3`) {
			t.Fatal("admin polling did not refresh live flows or preserve its cached snapshot")
		}
		e.mu.Lock()
		healthy := e.routes["pool"].Healthy
		e.mu.Unlock()
		if !healthy {
			t.Fatal("temporary admin polling failure demoted a live tunnel")
		}
	}
}
