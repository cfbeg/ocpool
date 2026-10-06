package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errCandidateUnhealthy = errors.New("candidate tunnel verification failed; prior tunnel restored")

type vpnDetails struct {
	Endpoint, Server string
	Handshake        int64
}
type wgCandidate struct {
	ID        string   `json:"id"`
	Endpoint  string   `json:"endpoint"`
	PublicKey string   `json:"public_key"`
	Hostname  string   `json:"hostname,omitempty"`
	DNS       []string `json:"dns,omitempty"`
}
type wgBase struct {
	privateKey, presharedKey   string
	addresses, dns, allowedIPs []string
	mtu                        int
	original                   wgCandidate
}

var wgInterfaceName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,15}$`)

func validateWGInterface(interfaceName string, routingTable, rulePriority int) error {
	if !wgInterfaceName.MatchString(interfaceName) || routingTable < 1 || routingTable > 2147483647 || (routingTable >= 253 && routingTable <= 255) || rulePriority < 1 || rulePriority >= 32765 {
		return errors.New("invalid WireGuard interface or policy routing allocation")
	}
	return nil
}

// The same WireGuard client identity must not connect concurrently to two IPs
// of a single server: its peer can roam between those sessions. A hostname is
// preferred for human-facing catalog metadata; the peer key is the fallback.
func wgCandidateHostKey(candidate wgCandidate) string {
	if candidate.Hostname != "" {
		return strings.ToLower(candidate.Hostname)
	}
	return candidate.PublicKey
}

func wgHostAddresses(addresses []string) []string {
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address)
		if err != nil {
			continue
		}
		prefix := "/128"
		if ip.To4() != nil {
			prefix = "/32"
		}
		result = append(result, ip.String()+prefix)
	}
	return result
}

func validWGKey(value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func commaValues(value string) []string {
	var values []string
	for _, entry := range strings.Split(value, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			values = append(values, entry)
		}
	}
	return values
}

func parseWGBase(raw []byte) (wgBase, error) {
	var config wgBase
	config.original.ID = "base"
	section := ""
	peers := 0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if line != "[Interface]" && line != "[Peer]" {
				return config, errors.New("unsupported WireGuard config section")
			}
			section = line
			if line == "[Peer]" {
				peers++
			}
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found {
			return config, errors.New("invalid WireGuard config syntax")
		}
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch section + ":" + name {
		case "[Interface]:PrivateKey":
			config.privateKey = value
		case "[Interface]:Address":
			config.addresses = append(config.addresses, commaValues(value)...)
		case "[Interface]:DNS":
			config.dns = append(config.dns, commaValues(value)...)
		case "[Interface]:MTU":
			var err error
			config.mtu, err = strconv.Atoi(value)
			if err != nil || config.mtu < 576 || config.mtu > 9000 {
				return config, errors.New("invalid WireGuard MTU")
			}
		case "[Peer]:PublicKey":
			config.original.PublicKey = value
		case "[Peer]:PresharedKey":
			config.presharedKey = value
		case "[Peer]:AllowedIPs":
			config.allowedIPs = append(config.allowedIPs, commaValues(value)...)
		case "[Peer]:Endpoint":
			config.original.Endpoint = value
			// Shell hooks, SaveConfig, and other supplied settings never enter the
			// runtime template. All interface setup is controlled by this service.
		}
	}
	if scanner.Err() != nil || peers != 1 || !validWGKey(config.privateKey) || (config.presharedKey != "" && !validWGKey(config.presharedKey)) {
		return config, errors.New("invalid WireGuard key or peer configuration")
	}
	if len(config.addresses) == 0 || len(config.allowedIPs) == 0 {
		return config, errors.New("WireGuard address and allowed IPs are required")
	}
	for _, address := range append(append([]string(nil), config.addresses...), config.allowedIPs...) {
		if _, _, err := net.ParseCIDR(address); err != nil {
			return config, errors.New("invalid WireGuard IP configuration")
		}
	}
	for _, address := range config.dns {
		if net.ParseIP(address) == nil {
			return config, errors.New("WireGuard DNS must contain IP addresses")
		}
	}
	if len(config.dns) == 0 {
		return config, errors.New("WireGuard DNS is required")
	}
	if err := validateWGCandidate(config.original); err != nil {
		return config, err
	}
	return config, nil
}

func validateWGCandidate(candidate wgCandidate) error {
	host, port, err := net.SplitHostPort(candidate.Endpoint)
	if err != nil || host == "" || strings.ContainsAny(host, "\r\n /\\;\t") {
		return errors.New("invalid VPN endpoint")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || !validWGKey(candidate.PublicKey) {
		return errors.New("invalid VPN endpoint or peer key")
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast()) {
		return errors.New("invalid VPN endpoint address")
	}
	for _, address := range candidate.DNS {
		if net.ParseIP(address) == nil {
			return errors.New("invalid VPN DNS")
		}
	}
	return nil
}

func renderWGConfig(config wgBase, candidate wgCandidate) (string, error) {
	if err := validateWGCandidate(candidate); err != nil {
		return "", err
	}
	var text strings.Builder
	// Keep the client address but scope it to a host route. Concurrent interfaces
	// may share the provided identity/address; subnet routes in the main table
	// would otherwise overlap. Each slot's default route lives in its own table.
	// DNS is resolved by Go through the explicitly bound interface.
	fmt.Fprintf(&text, "[Interface]\nPrivateKey = %s\nAddress = %s\nTable = off\n", config.privateKey, strings.Join(wgHostAddresses(config.addresses), ", "))
	if config.mtu != 0 {
		fmt.Fprintf(&text, "MTU = %d\n", config.mtu)
	}
	fmt.Fprintf(&text, "\n[Peer]\nPublicKey = %s\n", candidate.PublicKey)
	if config.presharedKey != "" {
		fmt.Fprintf(&text, "PresharedKey = %s\n", config.presharedKey)
	}
	fmt.Fprintf(&text, "AllowedIPs = %s\nEndpoint = %s\nPersistentKeepalive = 25\n", strings.Join(config.allowedIPs, ", "), candidate.Endpoint)
	return text.String(), nil
}

func freshHandshake(timestamp int64, now time.Time) bool {
	return timestamp > 0 && timestamp <= now.Unix()+5 && now.Unix()-timestamp <= 180
}

type wgBackend struct {
	base          wgBase
	candidates    []wgCandidate
	initial       wgCandidate
	runtimeFile   string
	interfaceName string
	routingTable  int
	rulePriority  int
	command       func(context.Context, string, ...string) (string, error)
	verify        func(context.Context) error
	closeIdle     func()
	mu            sync.Mutex
	current       wgCandidate
	failedUntil   map[string]time.Time
	connectWait   time.Duration
}

func newWGBackend(configFile, catalogFile, initialID, initialEndpoint string) (*wgBackend, error) {
	return newWGBackendForInterface(configFile, catalogFile, initialID, initialEndpoint, "wg0", 51820, 1000)
}

func newWGBackendForInterface(configFile, catalogFile, initialID, initialEndpoint, interfaceName string, routingTable, rulePriority int) (*wgBackend, error) {
	if err := validateWGInterface(interfaceName, routingTable, rulePriority); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(configFile)
	if err != nil || len(raw) > 64<<10 {
		return nil, errors.New("cannot read WireGuard base configuration")
	}
	base, err := parseWGBase(raw)
	if err != nil {
		return nil, err
	}
	backend := &wgBackend{base: base, interfaceName: interfaceName, routingTable: routingTable, rulePriority: rulePriority, runtimeFile: filepath.Join("/run/ocpool", interfaceName+".conf"), command: runWGCommand, failedUntil: make(map[string]time.Time), connectWait: 12 * time.Second}
	if catalogFile != "" {
		catalog, readErr := os.ReadFile(catalogFile)
		if readErr != nil || len(catalog) > 4<<20 || json.Unmarshal(catalog, &backend.candidates) != nil || len(backend.candidates) > 2048 {
			return nil, errors.New("cannot read VPN candidate catalog")
		}
	}
	if len(backend.candidates) == 0 {
		backend.candidates = []wgCandidate{base.original}
	}
	seen := map[string]bool{}
	var candidates []wgCandidate
	for _, candidate := range backend.candidates {
		if err := validateWGCandidate(candidate); err != nil {
			continue
		}
		if seen[candidate.Endpoint] {
			continue
		}
		seen[candidate.Endpoint] = true
		if candidate.ID == "" {
			candidate.ID = candidate.Endpoint
		}
		candidates = append(candidates, candidate)
	}
	backend.candidates = candidates
	if len(candidates) == 0 {
		return nil, errors.New("VPN catalog has no validated peer candidates")
	}
	if initialID != "" || initialEndpoint != "" {
		for _, candidate := range candidates {
			if (initialID != "" && candidate.ID == initialID) || (initialEndpoint != "" && candidate.Endpoint == initialEndpoint) {
				backend.initial = candidate
				break
			}
		}
		if backend.initial.Endpoint == "" {
			return nil, errors.New("initial VPN server is not in validated catalog")
		}
	} else {
		backend.initial = candidates[rand.IntN(len(candidates))]
	}
	return backend, nil
}

// Clone parsed base configuration/catalog without rereading the credential
// file for every slot. Runtime state, cooldowns, commands and sockets remain
// independent. The pool selects a candidate and sets initial before start.
func cloneWGBackendForInterface(template *wgBackend, interfaceName string, routingTable, rulePriority int) (*wgBackend, error) {
	if template == nil {
		return nil, errors.New("WireGuard template is required")
	}
	if err := validateWGInterface(interfaceName, routingTable, rulePriority); err != nil {
		return nil, err
	}
	template.mu.Lock()
	defer template.mu.Unlock()
	base := template.base
	base.addresses = append([]string(nil), base.addresses...)
	base.dns = append([]string(nil), base.dns...)
	base.allowedIPs = append([]string(nil), base.allowedIPs...)
	candidates := append([]wgCandidate(nil), template.candidates...)
	for i := range candidates {
		candidates[i].DNS = append([]string(nil), candidates[i].DNS...)
	}
	return &wgBackend{base: base, candidates: candidates, interfaceName: interfaceName, routingTable: routingTable, rulePriority: rulePriority, runtimeFile: filepath.Join("/run/ocpool", interfaceName+".conf"), command: template.command, failedUntil: make(map[string]time.Time), connectWait: template.connectWait}, nil
}

func (backend *wgBackend) interfaceRouting() (string, string, string, string) {
	name, table, priority := backend.interfaceName, backend.routingTable, backend.rulePriority
	if name == "" {
		name = "wg0"
	}
	if table == 0 {
		table = 51820
	}
	if priority == 0 {
		priority = 1000
	}
	return name, strconv.Itoa(table), strconv.Itoa(priority), strconv.Itoa(priority + 1)
}

func runWGCommand(ctx context.Context, name string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, name, args...)
	var output bytes.Buffer
	// Capture only explicitly public peer status and ping timing. Configuration
	// operations suppress all output, so a malformed key cannot reach logs.
	if name == "wg" || name == "ping" {
		command.Stdout = &output
	}
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("%s operation failed", name)
	}
	return output.String(), nil
}

func (backend *wgBackend) details(ctx context.Context) (vpnDetails, error) {
	interfaceName, _, _, _ := backend.interfaceRouting()
	backend.mu.Lock()
	candidate := backend.current
	backend.mu.Unlock()
	details := vpnDetails{Endpoint: candidate.Endpoint, Server: candidate.Hostname}
	if details.Server == "" {
		details.Server, _, _ = net.SplitHostPort(candidate.Endpoint)
	}
	if candidate.Endpoint == "" {
		return details, errors.New("VPN peer not initialized")
	}
	handshakes, err := backend.command(ctx, "wg", "show", interfaceName, "latest-handshakes")
	if err != nil {
		return details, err
	}
	for _, line := range strings.Split(strings.TrimSpace(handshakes), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == candidate.PublicKey {
			details.Handshake, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	endpoints, err := backend.command(ctx, "wg", "show", interfaceName, "endpoints")
	if err != nil {
		return details, err
	}
	for _, line := range strings.Split(strings.TrimSpace(endpoints), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == candidate.PublicKey {
			details.Endpoint = fields[1]
		}
	}
	return details, nil
}

func (backend *wgBackend) writeConfig(candidate wgCandidate) error {
	config, err := renderWGConfig(backend.base, candidate)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(backend.runtimeFile), 0700); err != nil {
		return errors.New("cannot prepare private VPN runtime directory")
	}
	if err := os.WriteFile(backend.runtimeFile, []byte(config), 0600); err != nil {
		return errors.New("cannot write private VPN runtime configuration")
	}
	return nil
}

func (backend *wgBackend) down(ctx context.Context) {
	interfaceName, table, priority, prohibitPriority := backend.interfaceRouting()
	for _, family := range []string{"-4", "-6"} {
		_, _ = backend.command(ctx, "ip", family, "rule", "del", "priority", priority, "oif", interfaceName, "table", table)
		_, _ = backend.command(ctx, "ip", family, "rule", "del", "priority", prohibitPriority, "oif", interfaceName, "prohibit")
		_, _ = backend.command(ctx, "ip", family, "route", "flush", "table", table)
	}
	_, _ = backend.command(ctx, "wg-quick", "down", backend.runtimeFile)
	if backend.closeIdle != nil {
		backend.closeIdle()
	}
}

func (backend *wgBackend) connect(ctx context.Context, candidate wgCandidate) error {
	interfaceName, table, priority, prohibitPriority := backend.interfaceRouting()
	if err := backend.writeConfig(candidate); err != nil {
		return err
	}
	backend.mu.Lock()
	backend.current = candidate
	backend.mu.Unlock()
	if _, err := backend.command(ctx, "wg-quick", "up", backend.runtimeFile); err != nil {
		return err
	}
	families := []string{"-4"}
	for _, address := range backend.base.addresses {
		if strings.Contains(address, ":") {
			families = append(families, "-6")
			break
		}
	}
	for _, family := range families {
		// Only sockets explicitly bound to this slot select these routes. Relay replies
		// and encrypted WireGuard UDP keep the ordinary container route. A second
		// prohibit rule prevents bound sockets falling through if the table empties.
		for _, args := range [][]string{
			{family, "route", "replace", "default", "dev", interfaceName, "table", table},
			{family, "rule", "add", "priority", priority, "oif", interfaceName, "table", table},
			{family, "rule", "add", "priority", prohibitPriority, "oif", interfaceName, "prohibit"},
		} {
			if _, err := backend.command(ctx, "ip", args...); err != nil {
				return err
			}
		}
	}
	if backend.closeIdle != nil {
		backend.closeIdle()
	}
	wait := backend.connectWait
	if wait == 0 {
		wait = 12 * time.Second
	}
	readinessCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		if err := backend.verify(readinessCtx); err == nil {
			return nil
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-readinessCtx.Done():
			return errors.New("VPN handshake and target verification timed out")
		}
	}
}

func (backend *wgBackend) start(ctx context.Context) error {
	backend.down(ctx)
	if err := backend.connect(ctx, backend.initial); err != nil {
		backend.down(ctx)
		return err
	}
	return nil
}

func (backend *wgBackend) rotate(ctx context.Context, excluded []string, sameServer bool) error {
	backend.mu.Lock()
	previous := backend.current
	excludedSet := map[string]bool{previous.Endpoint: true}
	for _, endpoint := range excluded {
		excludedSet[endpoint] = true
	}
	var choices []wgCandidate
	for _, candidate := range backend.candidates {
		if !excludedSet[candidate.Endpoint] && time.Now().After(backend.failedUntil[candidate.Endpoint]) {
			choices = append(choices, candidate)
		}
	}
	backend.mu.Unlock()
	if len(choices) == 0 && !sameServer {
		return errors.New("no alternate validated VPN endpoints available")
	}
	candidate := previous
	if !sameServer {
		candidate = choices[rand.IntN(len(choices))]
	}
	if candidate.Endpoint == "" {
		return errors.New("VPN peer not initialized")
	}
	// ICMP is a latency hint. VPN servers often drop it, so the actual handshake
	// and target request decide readiness even when this bounded ping fails.
	host, _, _ := net.SplitHostPort(candidate.Endpoint)
	pingCtx, pingCancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	_, _ = backend.command(pingCtx, "ping", "-n", "-c", "1", "-W", "1", host)
	pingCancel()
	backend.down(ctx)
	if err := backend.connect(ctx, candidate); err == nil {
		return nil
	}
	backend.mu.Lock()
	backend.failedUntil[candidate.Endpoint] = time.Now().Add(5 * time.Minute)
	backend.mu.Unlock()
	backend.down(ctx)
	// A failed candidate must not strand a previously working fleet member.
	restoreCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := backend.connect(restoreCtx, previous); err != nil {
		backend.down(restoreCtx)
		return errors.New("candidate failed and prior VPN tunnel could not be restored")
	}
	return errCandidateUnhealthy
}

func (backend *wgBackend) dnsServers() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.current.DNS) != 0 {
		return append([]string(nil), backend.current.DNS...)
	}
	return append([]string(nil), backend.base.dns...)
}

func wireGuardTransport(dnsServers func() []string) *http.Transport {
	transport, _ := wireGuardTransportForInterface("wg0", dnsServers)
	return transport
}

func wireGuardTransportForInterface(interfaceName string, dnsServers func() []string) (*http.Transport, error) {
	control, err := wireGuardDeviceControl(interfaceName)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: control}
	var dnsNext atomic.Uint32
	dialer.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		resolverDialer := &net.Dialer{Timeout: 4 * time.Second, Control: control}
		dns := dnsServers()
		if len(dns) == 0 {
			return nil, errors.New("VPN DNS is not configured")
		}
		// DNS uses the same slot as inference. No host resolver fallback is permitted.
		server := dns[int(dnsNext.Add(1)-1)%len(dns)]
		resolverNetwork := "udp4"
		if net.ParseIP(server).To4() == nil {
			resolverNetwork = "udp6"
		}
		return resolverDialer.DialContext(ctx, resolverNetwork, net.JoinHostPort(server, "53"))
	}}
	return &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", address)
	}, TLSHandshakeTimeout: 12 * time.Second, ResponseHeaderTimeout: 90 * time.Second, MaxIdleConns: 100, MaxIdleConnsPerHost: 32, IdleConnTimeout: 60 * time.Second, ForceAttemptHTTP2: true}, nil
}
