package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixtureWGKey(value byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))
}
func fixtureWGConfig() string {
	return "[Interface]\nPrivateKey = " + fixtureWGKey(1) + "\nAddress = 10.14.0.2/16\nDNS = 162.252.172.57,149.154.159.92\nMTU = 1420\nPostUp = echo forbidden-hook\nSaveConfig = true\n\n[Peer]\nPublicKey = " + fixtureWGKey(2) + "\nAllowedIPs = 0.0.0.0/0\nEndpoint = base.surfshark.com:51820\n"
}
func fixtureWGBackend(t *testing.T) *wgBackend {
	t.Helper()
	base, err := parseWGBase([]byte(fixtureWGConfig()))
	if err != nil {
		t.Fatal(err)
	}
	return &wgBackend{base: base, runtimeFile: filepath.Join(t.TempDir(), "private", "wg0.conf"), current: base.original, command: func(context.Context, string, ...string) (string, error) { return "", nil }, failedUntil: make(map[string]time.Time), connectWait: 15 * time.Millisecond}
}

func TestWireGuardTemplateDropsHooksAndUsesCandidatePeerKey(t *testing.T) {
	base, err := parseWGBase([]byte(fixtureWGConfig()))
	if err != nil {
		t.Fatal(err)
	}
	candidate := wgCandidate{ID: "other", Endpoint: "198.51.100.1:51820", PublicKey: fixtureWGKey(3)}
	rendered, err := renderWGConfig(base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PostUp", "forbidden-hook", "SaveConfig", "DNS ="} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("unsafe setting copied: %s", forbidden)
		}
	}
	if !strings.Contains(rendered, "PublicKey = "+candidate.PublicKey) || strings.Contains(rendered, "PublicKey = "+base.original.PublicKey) || !strings.Contains(rendered, "Table = off") {
		t.Fatal("runtime template lost peer identity or routing controls")
	}
	if !strings.Contains(rendered, "PrivateKey = "+base.privateKey) {
		t.Fatal("client identity not preserved")
	}
	backend := fixtureWGBackend(t)
	if err := backend.writeConfig(candidate); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(backend.runtimeFile)
	if err != nil {
		t.Fatal(err)
	}
	// Windows reports translated modes; the actual Linux runtime enforces0600.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("runtime configuration is world writable")
	}
}

func TestWireGuardValidationNeverEchoesKeyMaterial(t *testing.T) {
	badPrivate := "sensitive-value-that-must-not-appear"
	raw := strings.Replace(fixtureWGConfig(), fixtureWGKey(1), badPrivate, 1)
	_, err := parseWGBase([]byte(raw))
	if err == nil || strings.Contains(err.Error(), badPrivate) {
		t.Fatal("invalid private key was accepted or disclosed")
	}
	base, err := parseWGBase([]byte(fixtureWGConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renderWGConfig(base, wgCandidate{Endpoint: "198.51.100.1:51820"}); err == nil {
		t.Fatal("candidate without its peer key reused base key")
	}
	if _, err := renderWGConfig(base, wgCandidate{Endpoint: "198.51.100.1:51820\nPostUp=bad", PublicKey: fixtureWGKey(3)}); err == nil {
		t.Fatal("endpoint allowed config injection")
	}
}

func TestWireGuardCatalogInitialSelectionAndUnknownPeerRejection(t *testing.T) {
	dir := t.TempDir()
	baseFile := filepath.Join(dir, "base.conf")
	catalogFile := filepath.Join(dir, "servers.json")
	if err := os.WriteFile(baseFile, []byte(fixtureWGConfig()), 0600); err != nil {
		t.Fatal(err)
	}
	entries := []wgCandidate{{ID: "a", Endpoint: "198.51.100.1:51820", PublicKey: fixtureWGKey(3)}, {ID: "missing-key", Endpoint: "198.51.100.2:51820"}, {ID: "b", Endpoint: "198.51.100.3:51820", PublicKey: fixtureWGKey(4)}}
	data, _ := json.Marshal(entries)
	if err := os.WriteFile(catalogFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := newWGBackend(baseFile, catalogFile, "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if backend.initial.ID != "b" || len(backend.candidates) != 2 {
		t.Fatal("initial selection or peer filtering incorrect")
	}
	if _, err := newWGBackend(baseFile, catalogFile, "missing-key", ""); err == nil {
		t.Fatal("unverified endpoint selected")
	}
}

func TestWireGuardRotationRespectsExclusionsAndPingIsHint(t *testing.T) {
	backend := fixtureWGBackend(t)
	backend.candidates = []wgCandidate{backend.current, {ID: "busy", Endpoint: "198.51.100.1:51820", PublicKey: fixtureWGKey(3)}, {ID: "available", Endpoint: "198.51.100.2:51820", PublicKey: fixtureWGKey(4)}}
	var operations []string
	backend.command = func(_ context.Context, name string, args ...string) (string, error) {
		operations = append(operations, name+" "+strings.Join(args, " "))
		if name == "ping" {
			return "", errors.New("ICMP dropped")
		}
		return "", nil
	}
	backend.verify = func(context.Context) error {
		if backend.current.ID != "available" {
			t.Error("excluded peer selected")
		}
		return nil
	}
	if err := backend.rotate(context.Background(), []string{"198.51.100.1:51820"}, false); err != nil {
		t.Fatal(err)
	}
	if backend.current.ID != "available" {
		t.Fatal("rotation failed after dropped ICMP")
	}
	joined := strings.Join(operations, "\n")
	if !strings.Contains(joined, "ip -4 rule add priority 1000 oif wg0 table 51820") || !strings.Contains(joined, "ip -4 rule add priority 1001 oif wg0 prohibit") {
		t.Fatal("bound socket routing or fallback prohibition missing")
	}
}

func TestWireGuardFailedCandidateRestoresPriorTunnel(t *testing.T) {
	backend := fixtureWGBackend(t)
	previous := backend.current
	bad := wgCandidate{ID: "bad", Endpoint: "198.51.100.1:51820", PublicKey: fixtureWGKey(3)}
	backend.candidates = []wgCandidate{previous, bad}
	backend.verify = func(context.Context) error {
		if backend.current.ID == "bad" {
			return errors.New("no handshake")
		}
		return nil
	}
	if err := backend.rotate(context.Background(), nil, false); !errors.Is(err, errCandidateUnhealthy) {
		t.Fatalf("failed candidate did not restore: %v", err)
	}
	if backend.current.Endpoint != previous.Endpoint || time.Now().After(backend.failedUntil[bad.Endpoint]) {
		t.Fatal("prior endpoint not restored or failed candidate not cooled down")
	}
	runtime, err := os.ReadFile(backend.runtimeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runtime), "Endpoint = "+previous.Endpoint) {
		t.Fatal("runtime file remained on failed candidate")
	}
}

func TestWireGuardSameServerReconnectsCurrentPeer(t *testing.T) {
	backend := fixtureWGBackend(t)
	previous := backend.current
	ups, downs := 0, 0
	backend.command = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "wg-quick" && args[0] == "up" {
			ups++
		}
		if name == "wg-quick" && args[0] == "down" {
			downs++
		}
		return "", nil
	}
	backend.verify = func(context.Context) error { return nil }
	if err := backend.rotate(context.Background(), []string{previous.Endpoint}, true); err != nil {
		t.Fatal(err)
	}
	if backend.current.Endpoint != previous.Endpoint || ups != 1 || downs != 1 {
		t.Fatal("same_server did not create a fresh tunnel to the current peer")
	}
}

func TestWireGuardConcurrentInterfacesHaveIndependentPolicies(t *testing.T) {
	template := fixtureWGBackend(t)
	template.base.addresses = []string{"10.14.0.2/16", "fd00:14::2/64"}
	template.candidates = []wgCandidate{template.current}
	a, err := cloneWGBackendForInterface(template, "wg7", 51827, 1014)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cloneWGBackendForInterface(template, "wg8", 51828, 1016)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a.runtimeFile, b.runtimeFile = filepath.Join(dir, "wg7.conf"), filepath.Join(dir, "wg8.conf")
	var aOps, bOps []string
	a.command = func(_ context.Context, name string, args ...string) (string, error) {
		aOps = append(aOps, name+" "+strings.Join(args, " "))
		return "", nil
	}
	b.command = func(_ context.Context, name string, args ...string) (string, error) {
		bOps = append(bOps, name+" "+strings.Join(args, " "))
		return "", nil
	}
	a.verify, b.verify = func(context.Context) error { return nil }, func(context.Context) error { return nil }
	a.initial, b.initial = template.current, template.current
	if err := a.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.down(context.Background())
	for _, tc := range []struct {
		ops                                    []string
		name, other, table, priority, prohibit string
	}{
		{aOps, "wg7", "wg8", "51827", "1014", "1015"},
		{bOps, "wg8", "wg7", "51828", "1016", "1017"},
	} {
		joined := strings.Join(tc.ops, "\n")
		for _, family := range []string{"-4", "-6"} {
			for _, want := range []string{
				"ip " + family + " route replace default dev " + tc.name + " table " + tc.table,
				"ip " + family + " rule add priority " + tc.priority + " oif " + tc.name + " table " + tc.table,
				"ip " + family + " rule add priority " + tc.prohibit + " oif " + tc.name + " prohibit",
				"ip " + family + " rule del priority " + tc.priority + " oif " + tc.name + " table " + tc.table,
			} {
				if !strings.Contains(joined, want) {
					t.Errorf("slot policy missing: %s", want)
				}
			}
		}
		if strings.Contains(joined, tc.other) {
			t.Fatal("slot teardown targeted another interface")
		}
	}
	for _, backend := range []*wgBackend{a, b} {
		data, err := os.ReadFile(backend.runtimeFile)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "Address = 10.14.0.2/32, fd00:14::2/128") {
			t.Fatal("shared client addresses installed overlapping main subnet routes")
		}
	}
	a.failedUntil[template.current.Endpoint] = time.Now().Add(time.Hour)
	if len(b.failedUntil) != 0 || len(template.failedUntil) != 0 {
		t.Fatal("slot mutable cooldown state was shared")
	}
	a.base.dns[0] = "1.1.1.1"
	if b.base.dns[0] == "1.1.1.1" || template.base.dns[0] == "1.1.1.1" {
		t.Fatal("slot DNS configuration was shared")
	}
}

func TestWireGuardInterfaceAllocationRejectsUnsafeNamesAndTables(t *testing.T) {
	template := fixtureWGBackend(t)
	for _, name := range []string{"", "wg0\nPostUp=bad", "wg0/../other", "sixteencharnamexx"} {
		if _, err := cloneWGBackendForInterface(template, name, 51820, 1000); err == nil {
			t.Errorf("invalid interface accepted: %q", name)
		}
		if _, err := wireGuardTransportForInterface(name, template.dnsServers); err == nil {
			t.Errorf("invalid socket binding accepted: %q", name)
		}
	}
	for _, tc := range []struct{ table, priority int }{{0, 1000}, {254, 1000}, {51820, 0}, {51820, 32765}} {
		if _, err := cloneWGBackendForInterface(template, "wg1", tc.table, tc.priority); err == nil {
			t.Errorf("system routing allocation accepted: %+v", tc)
		}
	}
	if wgCandidateHostKey(wgCandidate{Hostname: "Paris.Surfshark.com", PublicKey: "key"}) != "paris.surfshark.com" || wgCandidateHostKey(wgCandidate{PublicKey: "key"}) != "key" {
		t.Fatal("pool host identity is not stable")
	}
}

func TestWireGuardDetailsQueryAssignedInterface(t *testing.T) {
	backend := fixtureWGBackend(t)
	backend.interfaceName = "wg3"
	backend.command = func(_ context.Context, name string, args ...string) (string, error) {
		if name != "wg" || len(args) != 3 || args[1] != "wg3" {
			t.Fatal("details queried another slot")
		}
		if args[2] == "latest-handshakes" {
			return backend.current.PublicKey + "\t" + strconv.FormatInt(time.Now().Unix(), 10), nil
		}
		return backend.current.PublicKey + "\t" + backend.current.Endpoint, nil
	}
	if details, err := backend.details(context.Background()); err != nil || details.Endpoint != backend.current.Endpoint || !freshHandshake(details.Handshake, time.Now()) {
		t.Fatal(err)
	}
}

func TestWireGuardHealthRequiresHandshakeAndTargetTrace(t *testing.T) {
	for _, tc := range []struct {
		scope   string
		age     int64
		healthy bool
	}{{"opencode.ai", 10, true}, {"opencode.ai", 181, false}, {"cloudflare.com", 10, false}, {"opencode.ai", -20, false}} {
		s := newRelay(testToken, nil)
		s.state.Backend = "wireguard"
		s.state.SupportsRegistrationRenewal = false
		s.probe = func(context.Context) (string, string, string, error) { return "198.51.100.10", "off", tc.scope, nil }
		s.vpnDetails = func(context.Context) (vpnDetails, error) {
			return vpnDetails{Endpoint: "198.51.100.1:51820", Server: "server.surfshark.com", Handshake: time.Now().Unix() - tc.age}, nil
		}
		h := s.refresh(context.Background())
		if h.Healthy != tc.healthy || h.Connected != tc.healthy {
			t.Errorf("scope=%s age=%d healthy=%v", tc.scope, tc.age, h.Healthy)
		}
	}
}

func TestWireGuardRotateRequestAllowsBoundedSameServer(t *testing.T) {
	s := newRelay(testToken, nil)
	s.state.Backend = "wireguard"
	s.state.Connected = true
	s.state.PublicIP = "198.51.100.10"
	s.state.IPScope = "opencode.ai"
	count := 0
	s.rotateVPN = func(_ context.Context, excluded []string, same bool) error {
		count++
		if !same || len(excluded) != 1 {
			t.Error("WG rotation options lost")
		}
		return nil
	}
	s.probe = func(context.Context) (string, string, string, error) {
		return "198.51.100.10", "off", "opencode.ai", nil
	}
	s.vpnDetails = func(context.Context) (vpnDetails, error) {
		return vpnDetails{Endpoint: "198.51.100.1:51820", Handshake: time.Now().Unix()}, nil
	}
	r := authenticatedRequest(t, "POST", "http://relay/rotate", "")
	r.Body = io.NopCloser(strings.NewReader(`{"same_server":true,"max_attempts":3,"exclude_endpoints":["198.51.100.2:51820"]}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || count != 3 || !strings.Contains(w.Body.String(), `"changed":false`) {
		t.Fatalf("same-IP WG rotation mislabeled: status=%d count=%d", w.Code, count)
	}
}
