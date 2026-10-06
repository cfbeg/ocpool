// OCPool egress relays OpenCode inference requests through a dedicated WARP client.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type health struct {
	Healthy                     bool   `json:"healthy"`
	Connected                   bool   `json:"connected"`
	PublicIP                    string `json:"public_ip"`
	IPScope                     string `json:"ip_scope"`
	Warp                        string `json:"warp"`
	ActiveFlows                 int    `json:"active_flows"`
	Rotating                    bool   `json:"rotating"`
	Rotations                   uint64 `json:"rotations"`
	RotationAttempts            uint64 `json:"rotation_attempts"`
	RegistrationRenewals        uint64 `json:"registration_renewals"`
	SupportsRegistrationRenewal bool   `json:"supports_registration_renewal"`
	Error                       string `json:"error,omitempty"`
	Backend                     string `json:"backend"`
	VPNEndpoint                 string `json:"vpn_endpoint,omitempty"`
	VPNServer                   string `json:"vpn_server,omitempty"`
	LatestHandshake             int64  `json:"latest_handshake,omitempty"`
	RateLimitUntil              int64  `json:"rate_limit_until,omitempty"`
	RateLimitIP                 string `json:"rate_limit_ip,omitempty"`
}

type relay struct {
	token      string
	client     *http.Client
	probe      func(context.Context) (string, string, string, error)
	reconnect  func(context.Context) error
	renew      func(context.Context) error
	rotateVPN  func(context.Context, []string, bool) error
	vpnDetails func(context.Context) (vpnDetails, error)
	// onRateLimit runs after a complete upstream 429 response has been sent and
	// the flow lease has been released. WireGuard relays use it to move to a
	// different catalog candidate; WARP leaves it nil and the caller can use
	// the normal /rotate API.
	onRateLimit func(context.Context)
	idle        func()
	drainWait   time.Duration
	rotateWait  time.Duration
	mu          sync.Mutex
	state       health
	changed     chan struct{}
	draining    bool
	// cooldowns are keyed by the measured public IP. A server reconnect that
	// happens to retain the same IP therefore remains paused, while a new IP
	// can be used immediately. This state intentionally lives inside the relay
	// so a single-container catalog pool enforces the limit before requests
	// reach OpenCode.
	cooldowns map[string]time.Time
}

func newRelay(token string, client *http.Client) *relay {
	return &relay{token: token, client: client, drainWait: 30 * time.Second,
		rotateWait: 90 * time.Second, changed: make(chan struct{}), cooldowns: make(map[string]time.Time), state: health{Warp: "off", Backend: "warp", SupportsRegistrationRenewal: true}}
}

func (s *relay) snapshot() health {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state
	if state.PublicIP != "" {
		if until := s.cooldowns[state.PublicIP]; until.After(time.Now()) {
			state.RateLimitUntil = until.Unix()
			state.RateLimitIP = state.PublicIP
		}
	}
	return state
}

func retryAfterDuration(value string) time.Duration {
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && seconds > 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
		seconds = math.Ceil(seconds)
		if seconds > 24*60*60 {
			seconds = 24 * 60 * 60
		}
		return time.Duration(seconds) * time.Second
	}
	if timestamp, err := http.ParseTime(strings.TrimSpace(value)); err == nil {
		if delay := time.Until(timestamp); delay > 0 {
			if delay > 24*time.Hour {
				return 24 * time.Hour
			}
			return delay
		}
	}
	return 60 * time.Second
}

// noteRateLimit records an upstream 429 against the current measured public
// IP. The longest cooldown wins under concurrent requests. The relay keeps
// forwarding that response unchanged; only subsequent requests are rejected
// locally with the same Retry-After semantics.
func (s *relay) noteRateLimit(header string) (string, time.Time, bool) {
	duration := retryAfterDuration(header)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ip := s.state.PublicIP
	if ip == "" {
		return "", time.Time{}, false
	}
	until := now.Add(duration)
	if previous := s.cooldowns[ip]; previous.After(until) {
		until = previous
	}
	if s.cooldowns == nil {
		s.cooldowns = make(map[string]time.Time)
	}
	s.cooldowns[ip] = until
	s.notifyLocked()
	return ip, until, true
}

// isIPRateLimitBody mirrors the app's limit-scope classification. Account or
// subscription quotas must remain account-scoped: cooling the relay's public
// IP for those responses would incorrectly suppress every other account in a
// shared pool. Unknown 429s are left to the app's model-scoped handling.
func isIPRateLimitBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	for _, marker := range []string{"gousagelimit", "blackusagelimit", "account", "subscription", "insufficient_quota"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, marker := range []string{"freeusagelimit", "freetier", "ip_rate", "ip_limit", "ip address"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (s *relay) cooldownForCurrentIP() (time.Duration, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ip := s.state.PublicIP
	if ip == "" || s.cooldowns == nil {
		return 0, ""
	}
	until := s.cooldowns[ip]
	if !until.After(time.Now()) {
		if !until.IsZero() {
			delete(s.cooldowns, ip)
		}
		return 0, ""
	}
	return time.Until(until), ip
}

func (s *relay) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }

func (s *relay) refresh(ctx context.Context) health {
	ip, warp, scope, err := s.probe(ctx)
	var details vpnDetails
	if s.vpnDetails != nil {
		var detailErr error
		details, detailErr = s.vpnDetails(ctx)
		if err == nil {
			err = detailErr
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Connected = err == nil && (warp == "on" || warp == "plus") && net.ParseIP(ip) != nil
	if s.state.Backend == "wireguard" {
		s.state.VPNEndpoint = details.Endpoint
		s.state.VPNServer = details.Server
		s.state.LatestHandshake = details.Handshake
		s.state.Connected = err == nil && scope == "opencode.ai" && net.ParseIP(ip) != nil && freshHandshake(details.Handshake, time.Now())
	}
	s.state.Healthy = s.state.Connected && !s.state.Rotating
	s.state.Warp = warp
	if err == nil {
		s.state.PublicIP = ip
		s.state.IPScope = scope
	}
	s.state.Error = ""
	if err != nil {
		s.state.Error = "VPN verification failed"
	} else if !s.state.Connected {
		s.state.Error = "VPN tunnel is not active"
	}
	return s.state
}

func (s *relay) acquire() (func(), bool) {
	s.mu.Lock()
	if s.state.Rotating || !s.state.Connected {
		s.mu.Unlock()
		return nil, false
	}
	s.state.ActiveFlows++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.state.ActiveFlows--
			s.notifyLocked()
		})
	}, true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Relay-Auth")), []byte(s.token)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "relay authentication required"})
		return
	}
	switch {
	case r.URL.Path == "/health" && r.Method == http.MethodGet:
		if s.snapshot().Rotating {
			writeJSON(w, 200, s.snapshot())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		writeJSON(w, 200, s.refresh(ctx))
	case r.URL.Path == "/fetch" && r.Method == http.MethodPost:
		s.fetch(w, r)
	case r.URL.Path == "/rotate" && r.Method == http.MethodPost:
		s.rotate(w, r)
	case r.URL.Path == "/drain" && r.Method == http.MethodPost:
		s.drain(w, r)
	case r.URL.Path == "/resume" && r.Method == http.MethodPost:
		s.mu.Lock()
		if !s.draining {
			s.mu.Unlock()
			writeJSON(w, 409, map[string]string{"error": "relay is not explicitly drained"})
			return
		}
		s.draining = false
		s.state.Rotating = false
		s.state.Healthy = s.state.Connected
		s.notifyLocked()
		current := s.state
		s.mu.Unlock()
		writeJSON(w, 200, current)
	default:
		writeJSON(w, 404, map[string]string{"error": "unknown relay route"})
	}
}

func (s *relay) drain(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.state.Rotating && !s.draining {
		s.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "rotation already in progress"})
		return
	}
	s.draining = true
	s.state.Rotating = true
	s.state.Healthy = false
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), s.drainWait)
	defer cancel()
	for {
		s.mu.Lock()
		active, changed := s.state.ActiveFlows, s.changed
		s.mu.Unlock()
		if active == 0 {
			writeJSON(w, 200, map[string]any{"drained": true, "active_flows": 0})
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			writeJSON(w, 409, map[string]any{"drained": false, "active_flows": active, "error": "existing streams did not drain; new flows remain paused"})
			return
		}
	}
}

func targetRequest(r *http.Request) (string, string, error) {
	if r.Header.Get("X-Relay-Target") != "https://opencode.ai" {
		return "", "", errors.New("only the opencode.ai inference origin is allowed")
	}
	path := r.Header.Get("X-Relay-Path")
	parsed, err := url.ParseRequestURI(path)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawPath != "" || parsed.Fragment != "" {
		return "", "", errors.New("invalid inference path")
	}
	route := parsed.Path
	query := parsed.RawQuery
	method := r.Header.Get("X-Relay-Method")
	if method == "" {
		method = http.MethodPost
		if route == "/zen/v1/models" || route == "/inference/openai/v1/models" || route == "/inference/google/v1beta/models" {
			method = http.MethodGet
		}
	}
	switch route {
	case "/zen/v1/models", "/inference/openai/v1/models", "/inference/google/v1beta/models":
		if method != http.MethodGet || query != "" {
			return "", "", errors.New("models requires GET")
		}
	case "/zen/v1/chat/completions", "/zen/v1/responses", "/zen/v1/messages",
		"/inference/openai/v1/chat/completions", "/inference/openai/v1/responses", "/inference/anthropic/v1/messages":
		if method != http.MethodPost || query != "" {
			return "", "", errors.New("inference requires POST")
		}
	default:
		if !allowedGoogleModelOperation(route, query, method) {
			return "", "", errors.New("unsupported inference path")
		}
	}
	return "https://opencode.ai" + path, method, nil
}

var googleModelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func allowedGoogleModelOperation(path, query, method string) bool {
	modelOperation, found := strings.CutPrefix(path, "/inference/google/v1beta/models/")
	if !found {
		return false
	}
	model, operation, hasOperation := strings.Cut(modelOperation, ":")
	if !googleModelName.MatchString(model) {
		return false
	}
	if !hasOperation {
		return method == http.MethodGet && query == ""
	}
	if method != http.MethodPost {
		return false
	}
	switch operation {
	case "generateContent", "countTokens":
		return query == ""
	case "streamGenerateContent":
		return query == "" || query == "alt=sse"
	default:
		return false
	}
}

func copyHeaders(dst, src http.Header) {
	hop := map[string]bool{"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
		"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true}
	for _, value := range src.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			hop[http.CanonicalHeaderKey(strings.TrimSpace(key))] = true
		}
	}
	for key, values := range src {
		if hop[http.CanonicalHeaderKey(key)] || strings.HasPrefix(strings.ToLower(key), "x-relay-") || strings.EqualFold(key, "X-OCPool-Limit-Scope") || strings.EqualFold(key, "Host") {
			continue
		}
		dst[key] = append([]string(nil), values...)
	}
}

func (s *relay) fetch(w http.ResponseWriter, r *http.Request) {
	target, method, err := targetRequest(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if delay, ip := s.cooldownForCurrentIP(); delay > 0 {
		w.Header().Set("X-Relay-Rate-Limit", "internal")
		seconds := int64(delay / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		w.Header().Set("X-OCPool-Limit-Scope", "ip")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"type": "error", "error": map[string]any{"type": "ip_rate_limit", "message": "egress public IP is cooling down", "public_ip": ip, "retry_after": seconds}})
		return
	}
	release, ok := s.acquire()
	if !ok {
		w.Header().Set("Retry-After", "2")
		writeJSON(w, 503, map[string]string{"error": "VPN unavailable or rotating"})
		return
	}
	defer release()
	// The caller context lives until the entire upstream body has drained. A caller
	// disconnect cancels the upstream transport, including an in-progress SSE read.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, 64<<20)
	defer body.Close()
	upstream, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid upstream request"})
		return
	}
	copyHeaders(upstream.Header, r.Header)
	upstream.Header.Del("Accept-Encoding")
	resp, err := s.client.Do(upstream)
	if err != nil {
		if r.Context().Err() == nil {
			writeJSON(w, 502, map[string]string{"error": "WARP upstream request failed"})
		}
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	// A 429 body is normally a small JSON error. Buffer it so the response can
	// remain byte-for-byte identical while recording the Retry-After deadline
	// before the next request is admitted. Other responses retain streaming
	// behavior and lease the tunnel until EOF.
	if resp.StatusCode == http.StatusTooManyRequests {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if readErr != nil {
			if r.Context().Err() == nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "rate-limit response could not be read"})
			}
			return
		}
		recorded := false
		if isIPRateLimitBody(body) {
			_, _, recorded = s.noteRateLimit(resp.Header.Get("Retry-After"))
			if recorded {
				w.Header().Set("X-OCPool-Limit-Scope", "ip")
			}
		}
		w.Header().Set("X-Relay-Rate-Limit", "internal")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		if recorded && s.onRateLimit != nil {
			// The current request has released its flow by the time this callback
			// runs. Implementations must drain before replacing the tunnel.
			go s.onRateLimit(context.Background())
		}
		return
	}
	w.WriteHeader(resp.StatusCode)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n != 0 {
			if _, err := w.Write(buffer[:n]); err != nil {
				cancel()
				return
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			return
		}
	}
}

func (s *relay) rotate(w http.ResponseWriter, r *http.Request) {
	var options struct {
		RenewRegistration bool     `json:"renew_registration"`
		MaxAttempts       int      `json:"max_attempts"`
		ExcludeEndpoints  []string `json:"exclude_endpoints,omitempty"`
		SameServer        bool     `json:"same_server,omitempty"`
	}
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, 400, map[string]any{"changed": false, "error": "invalid rotation options"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, 400, map[string]any{"changed": false, "error": "unexpected data after rotation options"})
		return
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 1
	}
	wireguard := s.snapshot().Backend == "wireguard"
	validAttempts := options.MaxAttempts >= 1 && options.MaxAttempts <= 2 && (options.RenewRegistration || options.MaxAttempts == 1)
	if wireguard {
		validAttempts = options.MaxAttempts >= 1 && options.MaxAttempts <= 3 && !options.RenewRegistration
	}
	if len(options.ExcludeEndpoints) > 128 || (!wireguard && (len(options.ExcludeEndpoints) != 0 || options.SameServer)) {
		validAttempts = false
	}
	if !validAttempts {
		writeJSON(w, 400, map[string]any{"changed": false, "error": "renewal supports one or two attempts; reconnect supports one"})
		return
	}
	rotateFunc := s.reconnect
	if wireguard && s.rotateVPN != nil {
		rotateFunc = func(ctx context.Context) error { return s.rotateVPN(ctx, options.ExcludeEndpoints, options.SameServer) }
	}
	if options.RenewRegistration {
		rotateFunc = s.renew
	}
	if rotateFunc == nil {
		writeJSON(w, 400, map[string]any{"changed": false, "error": "requested rotation mode is unavailable"})
		return
	}
	s.mu.Lock()
	if s.state.Rotating {
		s.mu.Unlock()
		writeJSON(w, 409, map[string]any{"changed": false, "error": "rotation already in progress"})
		return
	}
	s.state.Rotating = true
	s.state.Healthy = false
	oldIP := s.state.PublicIP
	oldScope := s.state.IPScope
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.state.Rotating = false
		s.state.Healthy = s.state.Connected
		s.notifyLocked()
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(r.Context(), s.drainWait)
	defer cancel()
	for {
		s.mu.Lock()
		active, changed := s.state.ActiveFlows, s.changed
		s.mu.Unlock()
		if active == 0 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			writeJSON(w, 409, map[string]any{"changed": false, "error": "active streams did not drain; tunnel preserved", "active_flows": active})
			return
		}
	}
	// Once drained, finish reconnecting even if the control client disappears.
	rotateCtx, rotateCancel := context.WithTimeout(context.Background(), s.rotateWait)
	defer rotateCancel()
	var err error
	var current health
	changed := false
	attempts := 0
	for attempts < options.MaxAttempts {
		attempts++
		s.mu.Lock()
		s.state.Connected = false
		s.state.RotationAttempts++
		if options.RenewRegistration {
			s.state.RegistrationRenewals++
		}
		s.mu.Unlock()
		if s.idle != nil {
			s.idle()
		}
		err = rotateFunc(rotateCtx)
		if s.idle != nil {
			s.idle()
		}
		current = s.refresh(rotateCtx)
		changed = current.Connected && oldIP != "" && current.IPScope == oldScope && current.PublicIP != oldIP
		if changed || (!errors.Is(err, errCandidateUnhealthy) && err != nil) || !current.Connected {
			break
		}
	}
	s.mu.Lock()
	if changed {
		s.state.Rotations++
	}
	s.state.Rotating = false
	s.state.Healthy = s.state.Connected
	current = s.state
	s.mu.Unlock()
	result := map[string]any{"changed": changed, "old_ip": oldIP, "new_ip": current.PublicIP, "old_ip_scope": oldScope, "ip_scope": current.IPScope, "health": current, "renew_registration": options.RenewRegistration, "attempts": attempts}
	status := 200
	if err != nil || !current.Connected {
		status = 502
		result["error"] = "VPN did not reconnect successfully"
	}
	writeJSON(w, status, result)
}

// rotateOnRateLimit is the relay-side catalog failover used by the
// single-container WireGuard deployment. It is deliberately separate from
// the HTTP /rotate handler: the handler needs a response writer, while this
// callback runs after a 429 flow has completed. Requests already in flight
// are drained before the WireGuard backend replaces its peer.
func (s *relay) rotateOnRateLimit(parent context.Context) {
	if s.rotateVPN == nil {
		return
	}
	s.mu.Lock()
	if s.state.Rotating || s.draining || !s.state.Connected {
		s.mu.Unlock()
		return
	}
	s.state.Rotating = true
	s.state.Healthy = false
	oldIP, oldScope := s.state.PublicIP, s.state.IPScope
	s.notifyLocked()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.state.Rotating = false
		s.state.Healthy = s.state.Connected
		s.notifyLocked()
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(parent, s.rotateWait)
	defer cancel()
	for {
		s.mu.Lock()
		active, changed := s.state.ActiveFlows, s.changed
		s.mu.Unlock()
		if active == 0 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
	if s.idle != nil {
		s.idle()
	}
	var err error
	var current health
	// A candidate may resolve to an IP that this relay cooled down earlier.
	// Keep trying bounded random catalog peers so a rate-limited IP is not
	// immediately reused after rotation. The catalog itself remains the source
	// of candidates; this loop only validates the measured public IP.
	for attempts := 0; attempts < 8; attempts++ {
		err = s.rotateVPN(ctx, nil, false)
		if s.idle != nil {
			s.idle()
		}
		current = s.refresh(ctx)
		if err != nil || !current.Connected {
			break
		}
		if delay, _ := s.cooldownForCurrentIP(); delay == 0 {
			break
		}
	}
	if err == nil && current.Connected && oldIP != "" && current.IPScope == oldScope && current.PublicIP != oldIP {
		s.mu.Lock()
		s.state.Rotations++
		s.mu.Unlock()
	}
}

func warpCommand(ctx context.Context, args ...string) error {
	commandCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "warp-cli", append([]string{"--accept-tos"}, args...)...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("warp-cli %s failed: %w", strings.Join(args, " "), err)
	}
	return nil
}

func configureWARP(ctx context.Context, command func(context.Context, ...string) error, renew bool) error {
	commands := [][]string{{"disconnect"}}
	if renew {
		commands = append(commands, []string{"registration", "delete"}, []string{"registration", "new"})
	}
	commands = append(commands, []string{"mode", "proxy"}, []string{"proxy", "port", "40000"}, []string{"connect"})
	for _, args := range commands {
		if err := command(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func traceProbe(client *http.Client) func(context.Context) (string, string, string, error) {
	probeEndpoint := func(ctx context.Context, endpoint string) (string, string, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			return "", "off", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", "off", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", "off", fmt.Errorf("trace returned %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		if err != nil {
			return "", "off", err
		}
		values := map[string]string{}
		for _, line := range strings.Split(string(body), "\n") {
			key, value, found := strings.Cut(line, "=")
			if found {
				values[key] = value
			}
		}
		if net.ParseIP(values["ip"]) == nil {
			return "", "off", errors.New("trace missing public IP")
		}
		return values["ip"], values["warp"], nil
	}
	return func(ctx context.Context) (string, string, string, error) {
		// WARP may use different addresses for different destinations. Measure the
		// actual Zen host first, and label a generic fallback as a separate scope.
		targetCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ip, warp, err := probeEndpoint(targetCtx, "https://opencode.ai/cdn-cgi/trace")
		cancel()
		if err == nil {
			return ip, warp, "opencode.ai", nil
		}
		ip, warp, err = probeEndpoint(ctx, "https://www.cloudflare.com/cdn-cgi/trace")
		return ip, warp, "cloudflare.com", err
	}
}

func main() {
	token := strings.TrimSpace(os.Getenv("OCPOOL_RELAY_TOKEN"))
	if tokenFile := os.Getenv("OCPOOL_RELAY_TOKEN_FILE"); tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			log.Fatal("cannot read relay token file")
		}
		token = strings.TrimSpace(string(data))
	}
	if len(token) < 32 {
		log.Fatal("OCPOOL_RELAY_TOKEN must contain at least 32 characters")
	}
	backendKind := os.Getenv("OCPOOL_VPN_BACKEND")
	if backendKind == "" {
		backendKind = "warp"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The pool is enabled explicitly by deployment configuration. Existing
	// WARP and single-peer WireGuard containers retain their original handler
	// and interface names when this variable is absent.
	if backendKind == "wireguard" && os.Getenv("OCPOOL_WG_POOL_SIZE") != "" {
		size, err := parseWGPoolSize(os.Getenv("OCPOOL_WG_POOL_SIZE"))
		if err != nil {
			log.Fatal(err)
		}
		configFile := os.Getenv("OCPOOL_WG_CONFIG_FILE")
		if configFile == "" {
			configFile = "/run/secrets/wg.conf"
		}
		catalogFile := os.Getenv("OCPOOL_WG_CATALOG_FILE")
		if catalogFile == "" {
			catalogFile = "/run/servers.json"
		}
		pool, err := newWGPool(ctx, token, configFile, catalogFile, size)
		if err != nil {
			log.Fatal(err)
		}
		poolCtx, poolStop := context.WithCancel(ctx)
		defer poolStop()
		go pool.maintain(poolCtx)
		serveRelay(ctx, pool, "wireguard pool", func(closeCtx context.Context) {
			poolStop()
			pool.close(closeCtx)
		})
		return
	}
	var transport *http.Transport
	var wireguard *wgBackend
	if backendKind == "wireguard" {
		configFile := os.Getenv("OCPOOL_WG_CONFIG_FILE")
		if configFile == "" {
			configFile = "/run/secrets/wg.conf"
		}
		catalogFile := os.Getenv("OCPOOL_WG_CATALOG_FILE")
		if catalogFile == "" {
			catalogFile = "/run/servers.json"
		}
		var err error
		wireguard, err = newWGBackend(configFile, catalogFile, os.Getenv("OCPOOL_VPN_SERVER_ID"), os.Getenv("OCPOOL_WG_INITIAL_ENDPOINT"))
		if err != nil {
			log.Fatal(err)
		}
		transport = wireGuardTransport(wireguard.dnsServers)
	} else if backendKind == "warp" {
		proxyURL, _ := url.Parse("socks5h://127.0.0.1:40000")
		transport = &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 90 * time.Second, MaxIdleConns: 100, MaxIdleConnsPerHost: 32, IdleConnTimeout: 60 * time.Second, ForceAttemptHTTP2: true}
	} else {
		log.Fatal("unsupported VPN backend")
	}
	// Redirects could otherwise escape the target/path allowlist or carry credentials.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s := newRelay(token, client)
	s.state.Backend = backendKind
	s.probe = traceProbe(client)
	s.idle = transport.CloseIdleConnections
	if wireguard != nil {
		s.state.SupportsRegistrationRenewal = false
		s.vpnDetails = wireguard.details
		s.rotateVPN = wireguard.rotate
		s.onRateLimit = s.rotateOnRateLimit
		wireguard.closeIdle = transport.CloseIdleConnections
		wireguard.verify = func(ctx context.Context) error {
			probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			ip, _, scope, err := s.probe(probeCtx)
			if err != nil || net.ParseIP(ip) == nil || scope != "opencode.ai" {
				return errors.New("VPN target trace verification failed")
			}
			details, err := wireguard.details(probeCtx)
			if err != nil || !freshHandshake(details.Handshake, time.Now()) {
				return errors.New("VPN handshake verification failed")
			}
			return nil
		}
		startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := wireguard.start(startupCtx); err != nil {
			log.Print("initial WireGuard tunnel did not pass handshake and target verification")
		}
		cancel()
	} else {
		rotateWARP := func(ctx context.Context, renew bool) error {
			if err := configureWARP(ctx, warpCommand, renew); err != nil {
				return err
			}
			for {
				probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				ip, warp, _, err := s.probe(probeCtx)
				cancel()
				if err == nil && net.ParseIP(ip) != nil && (warp == "on" || warp == "plus") {
					return nil
				}
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		s.reconnect = func(ctx context.Context) error { return rotateWARP(ctx, false) }
		s.renew = func(ctx context.Context) error { return rotateWARP(ctx, true) }
	}
	go func() {
		for {
			if !s.snapshot().Rotating {
				checkCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
				s.refresh(checkCtx)
				cancel()
			}
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
	var closeRelay func(context.Context)
	if wireguard != nil {
		closeRelay = wireguard.down
	}
	serveRelay(ctx, s, backendKind, closeRelay)
}

func parseWGPoolSize(value string) (int, error) {
	size, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || size < 1 || size > 32 {
		return 0, errors.New("OCPOOL_WG_POOL_SIZE must be an integer from 1 to 32")
	}
	return size, nil
}

func serveRelay(ctx context.Context, handler http.Handler, backend string, closeRelay func(context.Context)) {
	listen := os.Getenv("OCPOOL_RELAY_LISTEN")
	if listen == "" {
		listen = "0.0.0.0:18071"
	}
	server := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	shutdownDone := make(chan struct{})
	serverCtx, serverStop := context.WithCancel(ctx)
	defer serverStop()
	go func() {
		defer close(shutdownDone)
		<-serverCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("OCPool dedicated %s relay listening on %s", backend, listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
		serverStop()
	}
	<-shutdownDone
	if closeRelay != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		closeRelay(stopCtx)
	}
}
