package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type EgressEndpoint struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	URL              string `json:"url"`
	Secret           string `json:"secret,omitempty"`
	SecretConfigured bool   `json:"secret_configured,omitempty"`
	Enabled          bool   `json:"enabled"`
	RotateOnLimit    bool   `json:"rotate_on_limit"`
}
type EgressConfig struct {
	Mode                string           `json:"mode"`
	AllowDirectFallback bool             `json:"allow_direct_fallback"`
	MaxAttempts         int              `json:"max_attempts"`
	CooldownSeconds     int              `json:"cooldown_seconds"`
	Endpoints           []EgressEndpoint `json:"endpoints"`
}

func defaultEgressConfig() EgressConfig {
	return EgressConfig{Mode: "direct", MaxAttempts: 2, CooldownSeconds: 60, Endpoints: []EgressEndpoint{}}
}
func safeEgressURL(raw string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return ""
	}
	u.User = nil
	return u.String()
}
func (c EgressConfig) public() EgressConfig {
	out := c
	out.Endpoints = append([]EgressEndpoint{}, c.Endpoints...)
	for i := range out.Endpoints {
		e := &out.Endpoints[i]
		u, _ := url.Parse(e.URL)
		e.SecretConfigured = e.Secret != "" || (u != nil && u.User != nil)
		e.Secret = ""
		e.URL = safeEgressURL(e.URL)
	}
	return out
}
func (c EgressConfig) validate() error {
	if c.Mode != "direct" && c.Mode != "sticky" && c.Mode != "spread" {
		return errors.New("出口モードを確認してください")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 4 || c.CooldownSeconds < 1 || c.CooldownSeconds > 86400 || len(c.Endpoints) > 32 {
		return errors.New("出口設定値が範囲外です")
	}
	ids := map[string]bool{}
	enabled := false
	for _, e := range c.Endpoints {
		if !validID(e.ID) || ids[e.ID] || len(e.Name) > 100 || len(e.Secret) > 4096 {
			return errors.New("出口ID・名前・共有キーを確認してください")
		}
		ids[e.ID] = true
		if e.Kind == "direct" {
			if e.ID != "direct" || e.URL != "" {
				return errors.New("直通のIDはdirect、URLは空にしてください")
			}
			continue
		}
		u, err := url.Parse(e.URL)
		if err != nil || u.Hostname() == "" || u.Fragment != "" {
			return errors.New("出口URLを確認してください")
		}
		switch e.Kind {
		case "warp", "vpn", "relay":
			if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
				return errors.New("WARP・relayはHTTP URLを指定してください")
			}
		case "proxy":
			if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
				return errors.New("プロキシの形式を確認してください")
			}
		default:
			return errors.New("出口の種類を確認してください")
		}
		if e.Enabled {
			enabled = true
		}
	}
	if c.Mode != "direct" && !enabled && !c.AllowDirectFallback {
		return errors.New("有効な出口を追加してください")
	}
	return nil
}

type egressLimit struct {
	Until   int64  `json:"until"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
}
type egressRoute struct {
	Endpoint                                           EgressEndpoint
	IP                                                 string
	IPScope                                            string
	Backend, VPNEndpoint, VPNServer                    string
	LatestHandshake                                    int64
	LastMaintenance                                    time.Time
	MaintenanceFailures                                int
	Active, Requests, Successes, Failures, Rotations   int
	Healthy, Rotating                                  bool
	LastError                                          string
	client                                             *http.Client
	LastProbe                                          time.Time
	duplicateBlocked                                   bool
	Pooled                                             bool
	CandidateCount, PoolSize, ReadyCount, CoolingCount int
	PoolSlots                                          []EgressVPNPoolSlot
}

// Public health data from managed VPN services. Deliberately exclude config,
// credentials and all unrecognized backend fields from the management API.
type EgressVPNPoolSlot struct {
	ID              string `json:"id"`
	Healthy         bool   `json:"healthy"`
	Connected       bool   `json:"connected"`
	PublicIP        string `json:"public_ip"`
	IPScope         string `json:"ip_scope"`
	Backend         string `json:"backend"`
	VPNServer       string `json:"vpn_server"`
	VPNEndpoint     string `json:"vpn_endpoint"`
	LatestHandshake int64  `json:"latest_handshake"`
	ActiveFlows     int    `json:"active_flows"`
	Rotating        bool   `json:"rotating"`
	CooldownUntil   int64  `json:"cooldown_until"`
	LastError       string `json:"last_error"`
	Requests        int64  `json:"requests"`
	Successes       int64  `json:"successes"`
	Failures        int64  `json:"failures"`
}
type Egress struct {
	mu            sync.Mutex
	vpnRotateMu   sync.Mutex
	config        EgressConfig
	routes        map[string]*egressRoute
	limits        map[string]map[string]egressLimit
	store         *Store
	path          string
	token, URL    string
	server        *http.Server
	notify        chan struct{}
	sticky        string
	spread        uint64
	allowedOrigin string // isolated tests only; production always uses opencode.ai
	onFailure     func(string, string, *upstreamError)
}

func newEgress(store *Store, state string) (*Egress, error) {
	e := &Egress{store: store, path: filepath.Join(state, "egress.json"), token: secret(), routes: map[string]*egressRoute{}, limits: map[string]map[string]egressLimit{}, notify: make(chan struct{})}
	c := defaultEgressConfig()
	raw, err := os.ReadFile(e.path)
	if err == nil {
		if err = json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err = c.validate(); err != nil {
		return nil, err
	}
	_, err = store.db.Exec(`CREATE TABLE IF NOT EXISTS egress_limits(egress_key TEXT NOT NULL,model TEXT NOT NULL,until_at INTEGER NOT NULL,scope TEXT NOT NULL,message TEXT NOT NULL,PRIMARY KEY(egress_key,model)); CREATE TABLE IF NOT EXISTS egress_events(id INTEGER PRIMARY KEY AUTOINCREMENT,created_at INTEGER NOT NULL,endpoint_id TEXT NOT NULL,model TEXT NOT NULL,public_ip TEXT NOT NULL,status INTEGER NOT NULL,kind TEXT NOT NULL,message TEXT NOT NULL); CREATE INDEX IF NOT EXISTS egress_event_time ON egress_events(created_at);`)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.Query("SELECT egress_key,model,until_at,scope,message FROM egress_limits WHERE until_at>?", time.Now().Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key, model string
		var l egressLimit
		if err = rows.Scan(&key, &model, &l.Until, &l.Scope, &l.Message); err != nil {
			rows.Close()
			return nil, err
		}
		if e.limits[key] == nil {
			e.limits[key] = map[string]egressLimit{}
		}
		e.limits[key][model] = l
	}
	rows.Close()
	e.apply(c)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	e.URL = "http://" + listener.Addr().String()
	e.server = &http.Server{Handler: http.HandlerFunc(e.serve), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() { _ = e.server.Serve(listener) }()
	return e, nil
}
func (e *Egress) close() {
	_ = e.server.Close()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.routes {
		r.client.CloseIdleConnections()
	}
}
func egressClient(endpoint EgressEndpoint) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.ForceAttemptHTTP2 = true
	transport.ResponseHeaderTimeout = 60 * time.Second
	if endpoint.Kind == "proxy" {
		u, _ := url.Parse(endpoint.URL)
		if u.User == nil && endpoint.Secret != "" {
			username, password, _ := strings.Cut(endpoint.Secret, ":")
			u.User = url.UserPassword(username, password)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (e *Egress) signal() { close(e.notify); e.notify = make(chan struct{}) }
func (e *Egress) apply(c EgressConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.applyLocked(c)
}
func (e *Egress) applyLocked(c EgressConfig) {
	e.config = c
	all := append([]EgressEndpoint{}, c.Endpoints...)
	hasDirect := false
	for _, x := range all {
		if x.ID == "direct" {
			hasDirect = true
		}
	}
	if !hasDirect {
		all = append(all, EgressEndpoint{ID: "direct", Name: "直通", Kind: "direct", Enabled: true})
	}
	next := map[string]*egressRoute{}
	for _, endpoint := range all {
		r := e.routes[endpoint.ID]
		if r == nil {
			r = &egressRoute{Healthy: true}
		} else {
			r.client.CloseIdleConnections()
			if r.Endpoint.URL != endpoint.URL || r.Endpoint.Secret != endpoint.Secret || r.Endpoint.Kind != endpoint.Kind {
				r.IP = ""
				r.IPScope = ""
				r.LastProbe = time.Time{}
				r.Backend, r.VPNEndpoint, r.VPNServer = "", "", ""
				r.LatestHandshake = 0
				r.LastMaintenance = time.Time{}
				r.MaintenanceFailures = 0
				r.LastError = ""
				r.Healthy = true
				clearVPNPoolMetadata(r)
				r.duplicateBlocked = false
			}
		}
		r.Endpoint = endpoint
		r.client = egressClient(endpoint)
		next[endpoint.ID] = r
	}
	for id, r := range e.routes {
		if next[id] == nil {
			r.client.CloseIdleConnections()
		}
	}
	e.routes = next
	e.reconcileVPNDuplicatesLocked()
	e.signal()
}
func (e *Egress) Config() EgressConfig { e.mu.Lock(); defer e.mu.Unlock(); return e.config.public() }
func (e *Egress) Enabled() bool        { e.mu.Lock(); defer e.mu.Unlock(); return e.config.Mode != "direct" }
func (e *Egress) limitedUntil(model string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.config.Mode == "direct" {
		return 0
	}
	until := int64(0)
	count := 0
	ids := []string{}
	for _, endpoint := range e.config.Endpoints {
		if endpoint.Enabled && endpoint.Kind != "direct" {
			ids = append(ids, endpoint.ID)
		}
	}
	if e.config.AllowDirectFallback {
		ids = append(ids, "direct")
	}
	for _, id := range ids {
		r := e.routes[id]
		if r == nil {
			continue
		}
		l := e.cooldown(r, model)
		if l.Until <= time.Now().Unix() {
			return 0
		}
		count++
		if until == 0 || l.Until < until {
			until = l.Until
		}
	}
	if count == 0 {
		return 0
	}
	return until
}
func (e *Egress) Save(c EgressConfig) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.routes {
		if r.Active > 0 || r.Rotating {
			return errors.New("処理中の出口があります。配信・切り替えの完了後に保存してください")
		}
	}
	for i := range c.Endpoints {
		x := &c.Endpoints[i]
		if old := e.routes[x.ID]; old != nil {
			if x.Secret == "" {
				x.Secret = old.Endpoint.Secret
			}
			if x.URL == safeEgressURL(old.Endpoint.URL) {
				x.URL = old.Endpoint.URL
			}
		}
		x.SecretConfigured = false
	}
	if err := c.validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err = atomicWrite(e.path, raw); err != nil {
		return err
	}
	e.applyLocked(c)
	return nil
}
func routeKey(r *egressRoute) string {
	if r.Pooled {
		return "exit:" + r.Endpoint.ID
	}
	if r.IP != "" && (r.IPScope == "" || r.IPScope == "opencode.ai") {
		return "ip:" + r.IP
	}
	return "exit:" + r.Endpoint.ID
}
func (e *Egress) cooldown(r *egressRoute, model string) egressLimit {
	all := e.limits[routeKey(r)]
	l := all[model]
	if all[""].Until > l.Until {
		l = all[""]
	}
	return l
}
func (e *Egress) event(id, model, ip string, status int, kind, message string) {
	if e.store == nil {
		return
	}
	_, _ = e.store.db.Exec("INSERT INTO egress_events(created_at,endpoint_id,model,public_ip,status,kind,message) VALUES(?,?,?,?,?,?,?)", time.Now().Unix(), id, model, ip, status, kind, clip(message, 700))
}
func (e *Egress) Snapshot() map[string]any {
	e.mu.Lock()
	routes := []any{}
	for _, r := range e.routes {
		until := int64(0)
		cooldowns := []any{}
		for model, l := range e.limits[routeKey(r)] {
			if l.Until > time.Now().Unix() {
				until = max(until, l.Until)
				cooldowns = append(cooldowns, map[string]any{"model": model, "until": l.Until, "scope": l.Scope, "message": l.Message})
			}
		}
		routes = append(routes, map[string]any{"id": r.Endpoint.ID, "name": r.Endpoint.Name, "kind": r.Endpoint.Kind, "url": safeEgressURL(r.Endpoint.URL), "enabled": r.Endpoint.Enabled, "public_ip": r.IP, "ip_scope": r.IPScope, "backend": r.Backend, "vpn_server": r.VPNServer, "vpn_endpoint": r.VPNEndpoint, "latest_handshake": r.LatestHandshake, "active": r.Active, "requests": r.Requests, "successes": r.Successes, "failures": r.Failures, "cooldown_until": until, "cooldowns": cooldowns, "last_error": r.LastError, "healthy": r.Healthy, "rotating": r.Rotating, "rotations": r.Rotations, "pooled": r.Pooled, "candidate_count": r.CandidateCount, "pool_size": r.PoolSize, "ready_count": r.ReadyCount, "cooling_count": r.CoolingCount, "slots": append([]EgressVPNPoolSlot{}, r.PoolSlots...)})
	}
	config := e.config.public()
	e.mu.Unlock()
	events := []any{}
	rows, err := e.store.db.Query("SELECT created_at,endpoint_id,model,public_ip,status,kind,message FROM egress_events ORDER BY id DESC LIMIT 40")
	if err == nil {
		for rows.Next() {
			var ts int64
			var id, model, ip, kind, message string
			var status int
			if rows.Scan(&ts, &id, &model, &ip, &status, &kind, &message) == nil {
				events = append(events, map[string]any{"created_at": ts, "endpoint_id": id, "model": model, "public_ip": ip, "status": status, "kind": kind, "message": message})
			}
		}
		rows.Close()
	}
	return map[string]any{"config": config, "routes": routes, "events": events}
}
func (e *Egress) endpoint(id string) (*egressRoute, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.routes[id]
	if r == nil {
		return nil, errors.New("出口が見つかりません")
	}
	r.Active++
	return r, nil
}
func (e *Egress) control(ctx context.Context, r *egressRoute, path string, method string, payload ...any) (map[string]any, error) {
	var reader io.Reader
	if len(payload) > 0 {
		raw, err := json.Marshal(payload[0])
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.Endpoint.URL, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Relay-Auth", r.Endpoint.Secret)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := r.client.Do(req)
	if err != nil {
		return nil, errors.New("出口の管理APIへ接続できません")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 65536))
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("出口の管理APIがJSONを返しませんでした")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return body, &egressControlError{status: res.StatusCode}
	}
	return body, nil
}

type egressControlError struct{ status int }

func (err *egressControlError) Error() string {
	return fmt.Sprintf("出口の管理API: HTTP %d", err.status)
}

func (e *Egress) Probe(ctx context.Context, id string, preserveOnError ...bool) (map[string]any, error) {
	r, err := e.endpoint(id)
	if err != nil {
		return nil, err
	}
	defer e.release(r)
	var body map[string]any
	if r.Endpoint.Kind == "warp" || r.Endpoint.Kind == "vpn" {
		body, err = e.control(ctx, r, "/health", "GET")
	} else {
		target, _ := url.Parse("https://opencode.ai/cdn-cgi/trace")
		if e.allowedOrigin != "" {
			target, _ = url.Parse(e.allowedOrigin + "/cdn-cgi/trace")
		}
		req, buildErr := e.build(ctx, r, target, "GET", nil, http.Header{})
		if buildErr != nil {
			return nil, buildErr
		}
		res, fetchErr := r.client.Do(req)
		if fetchErr != nil {
			err = errors.New("出口のIP確認に失敗しました")
		} else {
			raw, readErr := io.ReadAll(io.LimitReader(res.Body, 32768))
			res.Body.Close()
			if readErr != nil {
				err = readErr
			} else if res.StatusCode != 200 {
				err = fmt.Errorf("IP確認: HTTP %d", res.StatusCode)
			} else {
				ip := ""
				for _, line := range strings.Split(string(raw), "\n") {
					if strings.HasPrefix(line, "ip=") {
						ip = strings.TrimPrefix(line, "ip=")
					}
				}
				if net.ParseIP(ip) == nil {
					err = errors.New("出口IPを確認できませんでした")
				} else {
					body = map[string]any{"healthy": true, "connected": true, "public_ip": ip}
				}
			}
		}
	}
	if err != nil && len(preserveOnError) > 0 && preserveOnError[0] {
		return body, err
	}
	e.mu.Lock()
	r.LastProbe = time.Now()
	if err == nil {
		healthy, _ := body["healthy"].(bool)
		r.Healthy = healthy
		ip, _ := body["public_ip"].(string)
		if net.ParseIP(ip) != nil {
			r.IP = ip
		}
		r.IPScope, _ = body["ip_scope"].(string)
		if healthy && net.ParseIP(ip) != nil && r.IPScope == "opencode.ai" {
			r.duplicateBlocked = false
		}
		updateVPNMetadata(r, body)
		r.LastError = ""
		if !healthy {
			r.LastError = "出口が未接続です"
		}
		e.reconcileVPNDuplicatesLocked()
		body["healthy"] = r.Healthy
	} else {
		r.Healthy = false
		r.LastError = err.Error()
	}
	e.signal()
	ip := r.IP
	healthy := r.Healthy
	e.mu.Unlock()
	if err != nil || !healthy {
		if err == nil {
			e.event(id, "", ip, 503, "probe_failed", "出口を利用できません")
			return body, nil
		}
		e.event(id, "", ip, 0, "probe_failed", err.Error())
	} else {
		e.event(id, "", ip, 200, "probe", "出口IPを確認しました")
	}
	return body, err
}

type RotationOptions struct {
	RenewRegistration bool     `json:"renew_registration"`
	MaxAttempts       int      `json:"max_attempts"`
	ExcludeEndpoints  []string `json:"exclude_endpoints,omitempty"`
	SameServer        bool     `json:"same_server,omitempty"`
	idleOnly          bool
	expected          EgressEndpoint
}

func (e *Egress) Rotate(ctx context.Context, id string, options ...RotationOptions) (map[string]any, error) {
	e.vpnRotateMu.Lock()
	defer e.vpnRotateMu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var opt RotationOptions
	if len(options) > 0 {
		opt = options[0]
	}
	if opt.MaxAttempts == 0 {
		opt.MaxAttempts = 1
	}
	e.mu.Lock()
	r := e.routes[id]
	if r == nil {
		e.mu.Unlock()
		return nil, errors.New("出口が見つかりません")
	}
	if opt.idleOnly && (e.config.Mode == "direct" || !r.Endpoint.Enabled || !r.Endpoint.RotateOnLimit || r.Active > 0 || r.Endpoint.URL != opt.expected.URL || r.Endpoint.Secret != opt.expected.Secret || r.Endpoint.Kind != opt.expected.Kind) {
		e.mu.Unlock()
		return nil, errVPNMaintenanceSkipped
	}
	if r.Endpoint.Kind != "warp" && r.Endpoint.Kind != "vpn" {
		e.mu.Unlock()
		return nil, errors.New("この出口はWARPの切り替えに対応していません")
	}
	maximum := 2
	if r.Endpoint.Kind == "vpn" {
		maximum = 3
		if opt.RenewRegistration {
			e.mu.Unlock()
			return nil, errors.New("WireGuardは登録更新ではなくVPNサーバーを切り替えます")
		}
		for id, other := range e.routes {
			if id != r.Endpoint.ID && other.Endpoint.Kind == "vpn" && other.VPNEndpoint != "" {
				opt.ExcludeEndpoints = append(opt.ExcludeEndpoints, other.VPNEndpoint)
			}
		}
	}
	if opt.MaxAttempts < 1 || opt.MaxAttempts > maximum {
		e.mu.Unlock()
		return nil, errors.New("切り替えの試行回数を確認してください")
	}
	if r.Rotating {
		e.mu.Unlock()
		return nil, errors.New("出口の切り替え中です")
	}
	r.Rotating = true
	e.signal()
	oldIP := r.IP
	oldScope := r.IPScope
	oldProbe := r.LastProbe
	e.mu.Unlock()
	body, err := e.control(ctx, r, "/rotate", "POST", opt)
	e.mu.Lock()
	r.Rotating = false
	r.LastProbe = time.Now()
	if err == nil {
		changed, _ := body["changed"].(bool)
		healthy, _ := body["healthy"].(bool)
		r.Healthy = healthy
		ip, _ := body["public_ip"].(string)
		if ip == "" {
			ip, _ = body["new_ip"].(string)
		}
		if net.ParseIP(ip) != nil {
			r.IP = ip
		}
		if scope, ok := body["ip_scope"].(string); ok {
			r.IPScope = scope
		}
		if healthy && net.ParseIP(ip) != nil && r.IPScope == "opencode.ai" {
			r.duplicateBlocked = false
		}
		updateVPNMetadata(r, body)
		if oldIP == "" {
			oldIP, _ = body["old_ip"].(string)
		}
		e.reconcileVPNDuplicatesLocked()
		verifiedIP := r.IP
		if r.Pooled {
			// A pool rotates one device. Its representative IP may belong to a
			// different device, so use the measured pair for that operation.
			oldIP, _ = body["old_ip"].(string)
			verifiedIP, _ = body["new_ip"].(string)
		}
		verifiedChange := changed && r.Healthy && net.ParseIP(oldIP) != nil && net.ParseIP(verifiedIP) != nil && verifiedIP != oldIP && (oldScope == "" || r.IPScope == oldScope)
		body["healthy"] = r.Healthy
		body["changed"] = verifiedChange
		if verifiedChange {
			r.Rotations++
			r.LastError = ""
		} else if r.Healthy {
			r.LastError = "再接続後も出口IPは同じです"
		}
	} else if managementError := new(egressControlError); errors.As(err, &managementError) && managementError.status == http.StatusConflict && body["healthy"] == nil {
		// No idle device was available. Existing live devices were preserved;
		// rejecting a manual operation is not evidence of a tunnel failure.
		r.LastProbe = oldProbe
		r.LastError = err.Error()
	} else {
		r.Healthy = false
		if restored, ok := body["healthy"].(bool); ok && restored {
			r.Healthy = true
			updateVPNMetadata(r, body)
			if ip, ok := body["public_ip"].(string); ok && net.ParseIP(ip) != nil {
				r.IP = ip
			}
			if scope, ok := body["ip_scope"].(string); ok {
				r.IPScope = scope
			}
			if ip, _ := body["public_ip"].(string); net.ParseIP(ip) != nil && r.IPScope == "opencode.ai" {
				r.duplicateBlocked = false
			}
		}
		r.LastError = err.Error()
		e.reconcileVPNDuplicatesLocked()
		if body != nil {
			body["healthy"] = r.Healthy
			if !r.Healthy {
				body["changed"] = false
			}
		}
	}
	e.signal()
	ip := r.IP
	message := r.LastError
	e.mu.Unlock()
	e.event(id, "", ip, 0, "rotation", message)
	return body, err
}

func (e *Egress) duplicateVPNLocked(r *egressRoute) bool {
	if r.Pooled || r.IP == "" || r.IPScope != "opencode.ai" {
		return false
	}
	for id, other := range e.routes {
		if id < r.Endpoint.ID && !other.Pooled && other.Endpoint.Kind == "vpn" && other.Endpoint.Enabled && other.Healthy && other.IPScope == r.IPScope && other.IP == r.IP {
			return true
		}
	}
	return false
}

// A newly verified lower-ID VPN can reclaim an IP held by a higher-ID VPN.
// Reconcile the whole set immediately, rather than checking only the updated
// route. Demotion survives old probes and in-flight responses until a fresh
// health check or rotation verifies that route again.
func (e *Egress) reconcileVPNDuplicatesLocked() bool {
	changed := false
	for _, r := range e.routes {
		if r.Pooled || r.Endpoint.Kind != "vpn" || !r.Endpoint.Enabled || !r.Healthy || !e.duplicateVPNLocked(r) {
			continue
		}
		r.Healthy = false
		r.duplicateBlocked = true
		r.LastError = "出口IPが別のVPNと重複しています"
		if e.sticky == r.Endpoint.ID {
			e.sticky = ""
		}
		changed = true
	}
	return changed
}

func updateVPNMetadata(r *egressRoute, body map[string]any) {
	if health, ok := body["health"].(map[string]any); ok {
		body = health
	}
	if backend, ok := body["backend"].(string); ok {
		r.Backend = backend
	}
	if endpoint, ok := body["vpn_endpoint"].(string); ok {
		r.VPNEndpoint = endpoint
	}
	if server, ok := body["vpn_server"].(string); ok {
		r.VPNServer = server
	}
	if timestamp, ok := body["latest_handshake"].(float64); ok {
		r.LatestHandshake = int64(timestamp)
	}
	if pooled, ok := body["pooled"].(bool); ok {
		clearVPNPoolMetadata(r)
		r.Pooled = pooled
		if pooled {
			if ip, ok := body["public_ip"].(string); ok && net.ParseIP(ip) != nil {
				r.IP = ip
			}
			if scope, ok := body["ip_scope"].(string); ok {
				r.IPScope = scope
			}
			r.CandidateCount = egressPublicCount(body["candidate_count"])
			r.PoolSize = egressPublicCount(body["pool_size"])
			r.ReadyCount = min(r.PoolSize, egressPublicCount(body["ready_count"]))
			r.CoolingCount = min(r.PoolSize, egressPublicCount(body["cooling_count"]))
			if raw, err := json.Marshal(body["slots"]); err == nil {
				var slots []EgressVPNPoolSlot
				if json.Unmarshal(raw, &slots) == nil {
					for _, slot := range slots[:min(128, len(slots))] {
						slot.ID = clip(slot.ID, 64)
						slot.LastError = clip(slot.LastError, 700)
						slot.ActiveFlows = max(0, slot.ActiveFlows)
						slot.Requests, slot.Successes, slot.Failures = max(0, slot.Requests), max(0, slot.Successes), max(0, slot.Failures)
						r.PoolSlots = append(r.PoolSlots, slot)
					}
				}
			}
		}
	}
}

func clearVPNPoolMetadata(r *egressRoute) {
	r.Pooled = false
	r.CandidateCount, r.PoolSize, r.ReadyCount, r.CoolingCount = 0, 0, 0, 0
	r.PoolSlots = nil
}

func egressPublicCount(value any) int {
	raw, err := json.Marshal(value)
	var count int
	if err != nil || json.Unmarshal(raw, &count) != nil {
		return 0
	}
	return max(0, count)
}
