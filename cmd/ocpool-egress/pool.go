package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A request leases a particular device through the end of its response body.
// Retiring a device prevents new leases without interrupting existing streams.
type wgPoolSlot struct {
	ID                                     string
	IP                                     string
	Healthy                                bool
	Candidate                              wgCandidate
	Handshake                              int64
	Active                                 int
	Rotating                               bool
	Probing                                bool
	LastProbe                              time.Time
	LastMaintenance                        time.Time
	Failures                               int
	LastError                              string
	Requests, Successes, Errors, Rotations int
	backend                                *wgBackend
	client                                 *http.Client
	probe                                  func(context.Context) (string, string, string, error)
	details                                func(context.Context) (vpnDetails, error)
	replace                                func(context.Context, wgCandidate) error
}

type poolLimit struct {
	IP    string `json:"ip"`
	Model string `json:"model,omitempty"`
	Until int64  `json:"until"`
	Scope string `json:"scope"`
}

type wgPool struct {
	token           string
	mu              sync.Mutex
	slots           []*wgPoolSlot
	candidates      []wgCandidate
	limits          map[string]poolLimit
	failedEndpoints map[string]time.Time
	next            uint64
	changed         chan struct{}
	wake            chan struct{}
	draining        bool
	closed          bool
	stateFile       string
	saveMu          sync.Mutex
	maintenanceDone chan struct{}
}

type poolSlotHealth struct {
	health
	ID            string `json:"id"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Requests      int    `json:"requests"`
	Successes     int    `json:"successes"`
	Failures      int    `json:"failures"`
}

type poolHealth struct {
	health
	Pooled         bool             `json:"pooled"`
	CandidateCount int              `json:"candidate_count"`
	PoolSize       int              `json:"pool_size"`
	ReadyCount     int              `json:"ready_count"`
	CoolingCount   int              `json:"cooling_count"`
	Slots          []poolSlotHealth `json:"slots"`
}

func newWGPool(ctx context.Context, token, configFile, catalogFile string, size int) (*wgPool, error) {
	if size < 1 || size > 32 {
		return nil, errors.New("invalid WireGuard pool size")
	}
	seed, err := newWGBackend(configFile, catalogFile, "", "")
	if err != nil {
		return nil, err
	}
	p := &wgPool{token: token, candidates: seed.candidates, limits: map[string]poolLimit{}, failedEndpoints: map[string]time.Time{}, changed: make(chan struct{}), wake: make(chan struct{}, 1), stateFile: os.Getenv("OCPOOL_VPN_STATE_FILE"), maintenanceDone: make(chan struct{})}
	if err := p.loadLimits(); err != nil {
		return nil, err
	}
	groups := map[string]bool{}
	for _, candidate := range p.candidates {
		groups[wgCandidateHostKey(candidate)] = true
	}
	if size > len(groups) {
		size = len(groups)
	}
	if size == 0 {
		return nil, errors.New("VPN catalog has no independent peer groups")
	}
	for index := 0; index < size; index++ {
		id := fmt.Sprintf("wg%d", index)
		backend, err := cloneWGBackendForInterface(seed, id, 51820+index, 1000+2*index)
		if err != nil {
			return nil, err
		}
		transport, err := wireGuardTransportForInterface(id, backend.dnsServers)
		if err != nil {
			return nil, err
		}
		client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		slot := &wgPoolSlot{ID: id, backend: backend, client: client, probe: traceProbe(client), details: backend.details}
		backend.closeIdle = transport.CloseIdleConnections
		backend.verify = func(check context.Context) error {
			ip, _, scope, err := slot.probe(check)
			if err != nil || net.ParseIP(ip) == nil || scope != "opencode.ai" {
				return errors.New("VPN target trace verification failed")
			}
			details, err := slot.details(check)
			if err != nil || !freshHandshake(details.Handshake, time.Now()) {
				return errors.New("VPN handshake verification failed")
			}
			return nil
		}
		slot.replace = func(check context.Context, candidate wgCandidate) error {
			backend.down(check)
			if err := backend.connect(check, candidate); err != nil {
				backend.down(check)
				return err
			}
			return nil
		}
		p.slots = append(p.slots, slot)
	}
	// Warm each device separately. Every reservation excludes peer groups used
	// by the others, including candidates whose handshake is still in progress.
	var warm sync.WaitGroup
	parallel := make(chan struct{}, 4)
	for index := range p.slots {
		warm.Add(1)
		go func(index int) {
			defer warm.Done()
			select {
			case parallel <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-parallel }()
			check, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_ = p.rotateSlot(check, index, false, 2)
		}(index)
	}
	warm.Wait()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, slot := range p.slots {
			slot.backend.down(cleanup)
		}
		return nil, ctx.Err()
	}
	return p, nil
}

func poolLimitKey(ip, model string) string { return ip + "\x00" + model }

func (p *wgPool) initLocked() {
	if p.changed == nil {
		p.changed = make(chan struct{})
	}
	if p.wake == nil {
		p.wake = make(chan struct{}, 1)
	}
	if p.limits == nil {
		p.limits = map[string]poolLimit{}
	}
	if p.failedEndpoints == nil {
		p.failedEndpoints = map[string]time.Time{}
	}
}
func (p *wgPool) signalLocked() {
	p.initLocked()
	close(p.changed)
	p.changed = make(chan struct{})
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *wgPool) limitLocked(ip, model string) poolLimit {
	global := p.limits[poolLimitKey(ip, "")]
	specific := p.limits[poolLimitKey(ip, model)]
	if specific.Until > global.Until {
		return specific
	}
	return global
}

func (p *wgPool) snapshot() poolHealth {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	result := poolHealth{health: health{Backend: "wireguard", Warp: "off", IPScope: "opencode.ai", Rotating: p.draining}, Pooled: true, CandidateCount: len(p.candidates), PoolSize: len(p.slots), Slots: []poolSlotHealth{}}
	now := time.Now()
	for _, slot := range p.slots {
		limit := p.limitLocked(slot.IP, "")
		connected := slot.Healthy && net.ParseIP(slot.IP) != nil && freshHandshake(slot.Handshake, now)
		ready := connected && !slot.Rotating && !p.draining && limit.Until <= now.Unix()
		item := poolSlotHealth{health: health{Backend: "wireguard", Warp: "off", Connected: connected, Healthy: ready, PublicIP: slot.IP, IPScope: "opencode.ai", VPNServer: slot.Candidate.Hostname, VPNEndpoint: slot.Candidate.Endpoint, LatestHandshake: slot.Handshake, ActiveFlows: slot.Active, Rotating: slot.Rotating, Rotations: uint64(slot.Rotations)}, ID: slot.ID, CooldownUntil: limit.Until, LastError: slot.LastError, Requests: slot.Requests, Successes: slot.Successes, Failures: slot.Errors}
		if limit.Until > now.Unix() {
			result.CoolingCount++
			item.RateLimitUntil = limit.Until
			item.RateLimitIP = slot.IP
		}
		result.ActiveFlows += slot.Active
		result.Rotations += uint64(slot.Rotations)
		if connected {
			result.Connected = true
		}
		if ready {
			result.ReadyCount++
			if !result.Healthy {
				result.PublicIP = slot.IP
				result.VPNServer = slot.Candidate.Hostname
				result.VPNEndpoint = slot.Candidate.Endpoint
				result.LatestHandshake = slot.Handshake
			}
			result.Healthy = true
		}
		result.Slots = append(result.Slots, item)
	}
	if result.PublicIP == "" && len(p.slots) > 0 {
		slot := p.slots[0]
		result.PublicIP = slot.IP
		result.VPNServer = slot.Candidate.Hostname
		result.VPNEndpoint = slot.Candidate.Endpoint
		result.LatestHandshake = slot.Handshake
	}
	if !result.Healthy {
		result.Error = "no ready WireGuard devices"
	}
	return result
}

func (p *wgPool) acquire(model string, tried map[string]bool) (*wgPoolSlot, string, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	if p.draining || p.closed {
		return nil, "", 0, errors.New("VPN pool is drained")
	}
	now := time.Now()
	soon := int64(0)
	var selected *wgPoolSlot
	if len(p.slots) == 0 {
		return nil, "", 0, errors.New("VPN pool has no devices")
	}
	start := int(p.next % uint64(len(p.slots)))
	p.next++
	for offset := 0; offset < len(p.slots); offset++ {
		slot := p.slots[(start+offset)%len(p.slots)]
		if tried[slot.ID] || !slot.Healthy || slot.Rotating || !freshHandshake(slot.Handshake, now) {
			continue
		}
		limit := p.limitLocked(slot.IP, model)
		if limit.Until > now.Unix() {
			if soon == 0 || limit.Until < soon {
				soon = limit.Until
			}
			continue
		}
		if slot.Active >= 32 {
			continue
		}
		if selected == nil || slot.Active < selected.Active {
			selected = slot
		}
	}
	if selected == nil {
		return nil, "", soon, errors.New("no ready WireGuard device for this request")
	}
	selected.Active++
	selected.Requests++
	return selected, selected.IP, 0, nil
}
func (p *wgPool) release(slot *wgPoolSlot) {
	p.mu.Lock()
	slot.Active--
	p.signalLocked()
	p.mu.Unlock()
}

func poolLimitScope(body []byte) string {
	var parsed struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	kind := strings.ToLower(parsed.Type + " " + parsed.Error.Type + " " + parsed.Error.Code)
	for _, marker := range []string{"gousagelimit", "blackusagelimit", "account", "subscription", "insufficient_quota"} {
		if strings.Contains(kind, marker) {
			return "account"
		}
	}
	if isIPRateLimitBody(body) {
		return "ip"
	}
	return "egress_model"
}
func (p *wgPool) noteLimit(ip, model, scope, retryAfter string) {
	if scope == "account" {
		return
	}
	if scope == "ip" {
		model = ""
	}
	deadline := time.Now().Add(retryAfterDuration(retryAfter)).Unix()
	p.mu.Lock()
	p.initLocked()
	key := poolLimitKey(ip, model)
	if previous := p.limits[key]; previous.Until > deadline {
		deadline = previous.Until
	}
	p.limits[key] = poolLimit{IP: ip, Model: model, Scope: scope, Until: deadline}
	p.signalLocked()
	p.mu.Unlock()
	_ = p.saveLimits()
}

func (p *wgPool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Relay-Auth")), []byte(p.token)) != 1 {
		writeJSON(w, 401, map[string]string{"error": "relay authentication required"})
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/health":
		writeJSON(w, 200, p.snapshot())
	case r.Method == "POST" && r.URL.Path == "/fetch":
		p.fetch(w, r)
	case r.Method == "POST" && r.URL.Path == "/rotate":
		p.rotate(w, r)
	case r.Method == "POST" && r.URL.Path == "/drain":
		p.drain(w, r)
	case r.Method == "POST" && r.URL.Path == "/resume":
		p.mu.Lock()
		if !p.draining {
			p.mu.Unlock()
			writeJSON(w, 409, map[string]string{"error": "relay is not explicitly drained"})
			return
		}
		p.draining = false
		p.signalLocked()
		p.mu.Unlock()
		writeJSON(w, 200, p.snapshot())
	default:
		writeJSON(w, 404, map[string]string{"error": "unknown relay route"})
	}
}

type poolResponse struct {
	status    int
	headers   http.Header
	body      []byte
	ip, scope string
}

func writePoolResponse(w http.ResponseWriter, response *poolResponse) {
	copyHeaders(w.Header(), response.headers)
	w.Header().Set("Content-Length", strconv.Itoa(len(response.body)))
	w.Header().Set("X-Relay-Exit-IP", response.ip)
	if response.status == 429 && response.scope != "account" {
		w.Header().Set("X-Relay-Rate-Limit", "internal")
		w.Header().Set("X-OCPool-Limit-Scope", response.scope)
	}
	w.WriteHeader(response.status)
	_, _ = w.Write(response.body)
}

func (p *wgPool) fetch(w http.ResponseWriter, r *http.Request) {
	target, method, err := targetRequest(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, body, 64<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		status := 400
		if errors.As(err, &tooLarge) {
			status = 413
		}
		writeJSON(w, status, map[string]string{"error": "cannot read request body"})
		return
	}
	var input struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &input)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	tried := map[string]bool{}
	var last429, lastTransient *poolResponse
	var retryUntil int64
	for attempt := 0; attempt < 4; attempt++ {
		slot, ip, until, pickErr := p.acquire(input.Model, tried)
		if pickErr != nil {
			retryUntil = until
			break
		}
		tried[slot.ID] = true
		request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
		if err != nil {
			p.release(slot)
			writeJSON(w, 400, map[string]string{"error": "invalid upstream request"})
			return
		}
		copyHeaders(request.Header, r.Header)
		request.Header.Del("Accept-Encoding")
		response, err := slot.client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				p.release(slot)
				return
			}
			p.mu.Lock()
			slot.Errors++
			slot.LastError = "upstream connection failed"
			slot.Healthy = false
			p.mu.Unlock()
			p.release(slot)
			continue
		}
		if response.StatusCode == 429 || response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504 || response.StatusCode >= 520 {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
			_ = response.Body.Close()
			if readErr != nil || len(body) > 2<<20 {
				p.release(slot)
				if last429 != nil {
					writePoolResponse(w, last429)
				} else {
					writeJSON(w, 502, map[string]string{"error": "cannot read upstream error"})
				}
				return
			}
			captured := &poolResponse{status: response.StatusCode, headers: response.Header.Clone(), body: body, ip: ip}
			p.mu.Lock()
			slot.Errors++
			p.mu.Unlock()
			if response.StatusCode == 429 {
				captured.scope = poolLimitScope(body)
				if captured.scope != "account" {
					p.noteLimit(ip, input.Model, captured.scope, response.Header.Get("Retry-After"))
				}
				p.release(slot)
				if captured.scope == "account" {
					writePoolResponse(w, captured)
					return
				}
				last429 = captured
			} else {
				lastTransient = captured
				p.release(slot)
			}
			continue
		}
		p.mu.Lock()
		if response.StatusCode < 400 {
			slot.Successes++
		} else {
			slot.Errors++
		}
		p.mu.Unlock()
		copyHeaders(w.Header(), response.Header)
		w.Header().Set("X-Relay-Exit-IP", ip)
		w.WriteHeader(response.StatusCode)
		controller := http.NewResponseController(w)
		_ = controller.Flush()
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := response.Body.Read(buffer)
			if n > 0 {
				if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
					cancel()
					break
				}
				_ = controller.Flush()
			}
			if readErr != nil {
				break
			}
		}
		_ = response.Body.Close()
		p.release(slot)
		return
	}
	if last429 != nil {
		writePoolResponse(w, last429)
		return
	}
	if lastTransient != nil {
		writePoolResponse(w, lastTransient)
		return
	}
	if retryUntil > time.Now().Unix() {
		w.Header().Set("X-Relay-Rate-Limit", "internal")
		w.Header().Set("X-OCPool-Limit-Scope", "ip")
		w.Header().Set("Retry-After", strconv.FormatInt(max(1, retryUntil-time.Now().Unix()), 10))
		writeJSON(w, 429, map[string]any{"error": map[string]string{"type": "rate_limit_error", "message": "all ready WireGuard devices are cooling down for this model"}})
	} else {
		w.Header().Set("Retry-After", "2")
		writeJSON(w, 503, map[string]string{"error": "VPN pool has no ready devices"})
	}
}

func (p *wgPool) candidateLocked(index int, same bool) (wgCandidate, bool) {
	current := p.slots[index].Candidate
	if same && current.Endpoint != "" {
		return current, true
	}
	groups := map[string]bool{}
	peerKeys := map[string]bool{}
	endpoints := map[string]bool{}
	for i, slot := range p.slots {
		if i != index && slot.Candidate.Endpoint != "" {
			groups[wgCandidateHostKey(slot.Candidate)] = true
			peerKeys[slot.Candidate.PublicKey] = true
			endpoints[slot.Candidate.Endpoint] = true
		}
	}
	choices := []wgCandidate{}
	for _, candidate := range p.candidates {
		if candidate.Endpoint != current.Endpoint && !endpoints[candidate.Endpoint] && !groups[wgCandidateHostKey(candidate)] && !peerKeys[candidate.PublicKey] && !p.failedEndpoints[candidate.Endpoint].After(time.Now()) {
			choices = append(choices, candidate)
		}
	}
	if len(choices) == 0 {
		return wgCandidate{}, false
	}
	return choices[rand.IntN(len(choices))], true
}

func (p *wgPool) rotateSlot(ctx context.Context, index int, same bool, attempts int) error {
	p.mu.Lock()
	p.initLocked()
	if p.closed || p.draining || index < 0 || index >= len(p.slots) {
		p.mu.Unlock()
		return errors.New("VPN pool is unavailable")
	}
	slot := p.slots[index]
	if slot.Active != 0 || slot.Rotating || slot.Probing {
		p.mu.Unlock()
		return errors.New("VPN device still has active flows")
	}
	slot.Rotating = true
	slot.LastMaintenance = time.Now()
	p.signalLocked()
	p.mu.Unlock()
	defer func() { p.mu.Lock(); slot.Rotating = false; p.signalLocked(); p.mu.Unlock() }()
	var lastError error
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.mu.Lock()
		candidate, ok := p.candidateLocked(index, same)
		if ok {
			slot.Candidate = candidate
			slot.Healthy = false
		}
		p.mu.Unlock()
		if !ok {
			return errors.New("no unused WireGuard candidates are ready")
		}
		if slot.replace == nil {
			return errors.New("VPN replacement is unavailable")
		}
		err := slot.replace(ctx, candidate)
		if err == nil {
			err = p.refreshSlot(ctx, index)
		}
		if err == nil {
			p.mu.Lock()
			cooled := p.limitLocked(slot.IP, "").Until > time.Now().Unix()
			duplicate := false
			for i, other := range p.slots {
				if i != index && other.Healthy && other.IP == slot.IP {
					duplicate = true
					break
				}
			}
			if duplicate {
				slot.Healthy = false
				slot.LastError = "public IP duplicates another device"
			}
			if !cooled && !duplicate {
				slot.Failures = 0
				slot.Rotations++
				p.mu.Unlock()
				return nil
			}
			p.mu.Unlock()
			err = errors.New("replacement public IP is cooling down or duplicated")
		}
		lastError = err
		p.mu.Lock()
		p.failedEndpoints[candidate.Endpoint] = time.Now().Add(5 * time.Minute)
		slot.LastError = "VPN candidate verification failed"
		slot.Healthy = false
		slot.Failures++
		p.mu.Unlock()
		if same {
			break
		}
	}
	return lastError
}

func (p *wgPool) refreshSlot(ctx context.Context, index int) error {
	slot := p.slots[index]
	if slot.probe == nil || slot.details == nil {
		return errors.New("VPN probe is unavailable")
	}
	ip, _, scope, err := slot.probe(ctx)
	var details vpnDetails
	if err == nil {
		details, err = slot.details(ctx)
	}
	if err == nil && (scope != "opencode.ai" || net.ParseIP(ip) == nil || !freshHandshake(details.Handshake, time.Now())) {
		err = errors.New("VPN target IP or handshake is unverified")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	slot.LastProbe = time.Now()
	slot.Healthy = err == nil
	if err == nil {
		slot.IP = ip
		slot.Handshake = details.Handshake
		slot.Candidate.Endpoint = details.Endpoint
		if details.Server != "" {
			slot.Candidate.Hostname = details.Server
		}
		slot.LastError = ""
		for i, other := range p.slots {
			if i != index && other.Healthy && other.IP == slot.IP {
				slot.Healthy = false
				slot.LastError = "public IP duplicates another device"
				err = errors.New("VPN public IP duplicates another device")
				break
			}
		}
	} else {
		slot.LastError = "VPN probe failed"
	}
	p.signalLocked()
	return err
}

func (p *wgPool) maintain(ctx context.Context) {
	if p.maintenanceDone != nil {
		defer close(p.maintenanceDone)
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.wake:
		}
		for index, slot := range p.slots {
			if ctx.Err() != nil {
				return
			}
			p.mu.Lock()
			now := time.Now()
			backoff := time.Duration(1<<min(slot.Failures, 4)) * 10 * time.Second
			available := !slot.Rotating && !slot.Probing && !p.draining && !p.closed
			needsRotate := !slot.Healthy || p.limitLocked(slot.IP, "").Until > now.Unix()
			needsProbe := time.Since(slot.LastProbe) > 45*time.Second
			eligible := available && time.Since(slot.LastMaintenance) >= backoff && (!needsRotate || slot.Active == 0)
			if eligible && (needsRotate || needsProbe) {
				slot.LastMaintenance = now
			}
			p.mu.Unlock()
			if !eligible {
				continue
			}
			if needsRotate {
				op, cancel := context.WithTimeout(ctx, 45*time.Second)
				_ = p.rotateSlot(op, index, false, 3)
				cancel()
			} else if needsProbe {
				// Probe reads the existing tunnel. Model streams keep their leases;
				// replacing the device waits for both probes and leases to finish.
				p.mu.Lock()
				if slot.Rotating || slot.Probing || p.draining || p.closed {
					p.mu.Unlock()
					continue
				}
				slot.Probing = true
				p.mu.Unlock()
				op, cancel := context.WithTimeout(ctx, 6*time.Second)
				_ = p.refreshSlot(op, index)
				cancel()
				p.mu.Lock()
				slot.Probing = false
				p.signalLocked()
				p.mu.Unlock()
			}
		}
	}
}

func (p *wgPool) rotate(w http.ResponseWriter, r *http.Request) {
	var options struct {
		SameServer        bool     `json:"same_server"`
		MaxAttempts       int      `json:"max_attempts"`
		ExcludeEndpoints  []string `json:"exclude_endpoints"`
		RenewRegistration bool     `json:"renew_registration"`
	}
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, body, 2048))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, 400, map[string]string{"error": "invalid rotation options"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, 400, map[string]string{"error": "unexpected rotation data"})
		return
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 3
	}
	if options.RenewRegistration || options.MaxAttempts < 1 || options.MaxAttempts > 3 {
		writeJSON(w, 400, map[string]string{"error": "invalid WireGuard rotation options"})
		return
	}
	p.mu.Lock()
	index := -1
	for i, slot := range p.slots {
		if slot.Active == 0 && !slot.Rotating && !slot.Probing {
			index = i
			if !slot.Healthy || p.limitLocked(slot.IP, "").Until > time.Now().Unix() {
				break
			}
		}
	}
	if index < 0 {
		p.mu.Unlock()
		writeJSON(w, 409, map[string]any{"changed": false, "error": "all VPN devices are streaming; tunnels preserved"})
		return
	}
	oldIP := p.slots[index].IP
	p.mu.Unlock()
	op, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	err := p.rotateSlot(op, index, options.SameServer, options.MaxAttempts)
	cancel()
	current := p.snapshot()
	p.mu.Lock()
	newIP := p.slots[index].IP
	p.mu.Unlock()
	result := map[string]any{"changed": err == nil && oldIP != "" && oldIP != newIP, "old_ip": oldIP, "new_ip": newIP, "ip_scope": "opencode.ai", "healthy": current.Healthy, "health": current}
	if err != nil {
		result["error"] = "VPN device replacement did not become ready"
		writeJSON(w, 502, result)
	} else {
		writeJSON(w, 200, result)
	}
}

func (p *wgPool) drain(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.draining = true
	p.signalLocked()
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	for {
		p.mu.Lock()
		active := 0
		rotating := false
		for _, slot := range p.slots {
			active += slot.Active
			rotating = rotating || slot.Rotating || slot.Probing
		}
		wait := p.changed
		p.mu.Unlock()
		if active == 0 && !rotating {
			writeJSON(w, 200, map[string]any{"drained": true, "active_flows": 0})
			return
		}
		select {
		case <-wait:
		case <-ctx.Done():
			writeJSON(w, 409, map[string]any{"drained": false, "active_flows": active, "error": "existing flows did not drain"})
			return
		}
	}
}

func (p *wgPool) close(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	p.draining = true
	p.signalLocked()
	p.mu.Unlock()
	if p.maintenanceDone != nil {
		select {
		case <-p.maintenanceDone:
		case <-ctx.Done():
		}
	}
	for _, slot := range p.slots {
		if slot.client != nil {
			slot.client.CloseIdleConnections()
		}
		if slot.backend != nil {
			slot.backend.down(ctx)
		}
	}
	_ = p.saveLimits()
}

func (p *wgPool) loadLimits() error {
	if p.stateFile == "" {
		return nil
	}
	raw, err := os.ReadFile(p.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || len(raw) > 4<<20 {
		return errors.New("cannot read VPN cooldown state")
	}
	var state struct {
		Version int         `json:"version"`
		Limits  []poolLimit `json:"limits"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Version != 1 {
		return errors.New("invalid VPN cooldown state")
	}
	for _, limit := range state.Limits {
		if net.ParseIP(limit.IP) != nil && limit.Until > time.Now().Unix() && (limit.Scope == "ip" || limit.Scope == "egress_model") {
			p.limits[poolLimitKey(limit.IP, limit.Model)] = limit
		}
	}
	return nil
}
func (p *wgPool) saveLimits() error {
	if p.stateFile == "" {
		return nil
	}
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	entries := []poolLimit{}
	for _, limit := range p.limits {
		if limit.Until > time.Now().Unix() {
			entries = append(entries, limit)
		}
	}
	p.mu.Unlock()
	raw, err := json.Marshal(struct {
		Version int         `json:"version"`
		Limits  []poolLimit `json:"limits"`
	}{1, entries})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.stateFile), 0700); err != nil {
		return err
	}
	path := p.stateFile + ".tmp"
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return err
	}
	return os.Rename(path, p.stateFile)
}
