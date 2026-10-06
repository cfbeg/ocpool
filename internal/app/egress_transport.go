package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (e *Egress) build(ctx context.Context, r *egressRoute, target *url.URL, method string, body []byte, headers http.Header) (*http.Request, error) {
	destination := target.String()
	h := headers.Clone()
	for _, key := range []string{"Host", "Content-Length", "Connection", "Proxy-Authorization", "X-OCPOOL-Egress-Auth", "X-OCPOOL-Target", "X-OCPOOL-Method", "X-OCPOOL-Agent", "X-Relay-Auth", "X-Relay-Target", "X-Relay-Path", "X-Relay-Method"} {
		h.Del(key)
	}
	if r.Endpoint.Kind == "relay" || r.Endpoint.Kind == "warp" || r.Endpoint.Kind == "vpn" {
		destination = r.Endpoint.URL
		if r.Endpoint.Kind == "warp" || r.Endpoint.Kind == "vpn" {
			destination = strings.TrimRight(destination, "/") + "/fetch"
		}
		h.Set("X-Relay-Target", target.Scheme+"://"+target.Host)
		h.Set("X-Relay-Path", target.RequestURI())
		h.Set("X-Relay-Method", method)
		if r.Endpoint.Secret != "" {
			h.Set("X-Relay-Auth", r.Endpoint.Secret)
		}
		method = "POST"
	}
	request, err := http.NewRequestWithContext(ctx, method, destination, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header = h
	return request, nil
}
func (e *Egress) pick(ctx context.Context, model string, tried map[string]bool) (*egressRoute, int64, error) {
	for {
		e.mu.Lock()
		if e.reconcileVPNDuplicatesLocked() {
			e.signal()
		}
		cfg := e.config
		ids := []string{}
		if cfg.Mode == "direct" {
			ids = append(ids, "direct")
		} else {
			for _, x := range cfg.Endpoints {
				if x.Enabled && x.Kind != "direct" {
					ids = append(ids, x.ID)
				}
			}
			if cfg.Mode == "sticky" && e.sticky != "" {
				for i, id := range ids {
					if id == e.sticky {
						ids[0], ids[i] = ids[i], ids[0]
						break
					}
				}
			}
			if cfg.Mode == "spread" && len(ids) > 0 {
				start := int(e.spread % uint64(len(ids)))
				ids = append(ids[start:], ids[:start]...)
				e.spread++
			}
			if cfg.AllowDirectFallback {
				ids = append(ids, "direct")
			}
		}
		now := time.Now().Unix()
		soon := int64(0)
		waiting := false
		for _, id := range ids {
			r := e.routes[id]
			if r == nil || tried[id] || r.duplicateBlocked {
				continue
			}
			l := e.cooldown(r, model)
			if l.Until > now {
				if soon == 0 || l.Until < soon {
					soon = l.Until
				}
				continue
			}
			// Pooled relays replace only an idle device. Their other devices can
			// keep admitting requests while a manual replacement is in progress.
			if (r.Rotating && !r.Pooled) || r.Active >= 32 {
				waiting = true
				continue
			}
			if !r.Healthy && time.Since(r.LastProbe) < 10*time.Second {
				continue
			}
			r.Active++
			r.Requests++
			e.mu.Unlock()
			return r, 0, nil
		}
		wait := e.notify
		e.mu.Unlock()
		if !waiting {
			return nil, soon, errors.New("利用できる出口がありません。出口の休止時間・接続状態を確認してください")
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-wait:
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (e *Egress) release(r *egressRoute) { e.mu.Lock(); r.Active--; e.signal(); e.mu.Unlock() }
func retryDeadline(header string, fallback int) int64 {
	now := time.Now()
	if n, err := strconv.ParseFloat(strings.TrimSpace(header), 64); err == nil && n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
		return now.Add(time.Duration(min(31536000, max(1, math.Ceil(n)))) * time.Second).Unix()
	}
	if date, err := http.ParseTime(header); err == nil && date.After(now) {
		return date.Unix()
	}
	return now.Add(time.Duration(fallback) * time.Second).Unix()
}
func limitScope(raw []byte) string {
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	kind := strings.ToLower(body.Type + " " + body.Error.Type + " " + body.Error.Code)
	if strings.Contains(kind, "gousagelimit") || strings.Contains(kind, "blackusagelimit") || strings.Contains(kind, "account") || strings.Contains(kind, "subscription") || strings.Contains(kind, "insufficient_quota") {
		return "account"
	}
	if strings.Contains(kind, "freeusagelimit") || strings.Contains(kind, "ip_rate") || strings.Contains(kind, "ip_limit") || strings.Contains(strings.ToLower(body.Error.Message), "ip address") {
		return "ip"
	}
	return "egress_model"
}
func (e *Egress) limit(r *egressRoute, model, scope, message string, until int64) {
	if scope == "account" {
		return
	}
	e.mu.Lock()
	key := routeKey(r)
	if scope == "ip" {
		model = ""
	}
	if e.limits[key] == nil {
		e.limits[key] = map[string]egressLimit{}
	}
	limit := egressLimit{Until: until, Scope: scope, Message: clip(message, 300)}
	if old := e.limits[key][model]; old.Until > limit.Until {
		limit = old
	}
	e.limits[key][model] = limit
	if !r.duplicateBlocked {
		r.LastError = clip(message, 300)
	}
	r.Failures++
	ip := r.IP
	e.signal()
	_, _ = e.store.db.Exec("INSERT INTO egress_limits(egress_key,model,until_at,scope,message) VALUES(?,?,?,?,?) ON CONFLICT(egress_key,model) DO UPDATE SET until_at=MAX(egress_limits.until_at,excluded.until_at),scope=excluded.scope,message=excluded.message", key, model, limit.Until, limit.Scope, limit.Message)
	e.mu.Unlock()
	e.event(r.Endpoint.ID, model, ip, 429, "cooldown", scope+": "+clip(message, 300))
}
func (e *Egress) forward(ctx context.Context, target *url.URL, method string, body []byte, headers http.Header) (*http.Response, *egressRoute, error) {
	var input struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &input)
	model := input.Model
	e.mu.Lock()
	cfg := e.config
	e.mu.Unlock()
	tried := map[string]bool{}
	rotationTried := map[string]bool{}
	var last *http.Response
	var lastRoute *egressRoute
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		r, until, err := e.pick(ctx, model, tried)
		if err != nil {
			if last != nil {
				return last, lastRoute, nil
			}
			status := 503
			if until > time.Now().Unix() {
				status = 429
			}
			raw, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "rate_limit_error", "message": err.Error()}})
			h := http.Header{"Content-Type": {"application/json"}, "X-OCPool-Limit-Scope": {"egress"}}
			if until > 0 {
				h.Set("Retry-After", strconv.FormatInt(max(1, until-time.Now().Unix()), 10))
			}
			return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(raw))}, nil, nil
		}
		e.mu.Lock()
		needProbe := r.IP == "" || time.Since(r.LastProbe) > 30*time.Second
		e.mu.Unlock()
		if needProbe {
			probe, cancel := context.WithTimeout(ctx, 10*time.Second)
			health, probeErr := e.Probe(probe, r.Endpoint.ID)
			cancel()
			healthy, _ := health["healthy"].(bool)
			if probeErr != nil || !healthy {
				e.release(r)
				tried[r.Endpoint.ID] = true
				continue
			}
		}
		e.mu.Lock()
		cooled := e.cooldown(r, model).Until > time.Now().Unix()
		blocked := r.duplicateBlocked
		ip := r.IP
		e.mu.Unlock()
		if blocked {
			e.release(r)
			tried[r.Endpoint.ID] = true
			continue
		}
		if cooled {
			e.mu.Lock()
			limit := e.cooldown(r, model)
			e.mu.Unlock()
			if last == nil {
				last = egressCooldownResponse(limit.Until, limit.Message)
			}
			e.release(r)
			tried[r.Endpoint.ID] = true
			continue
		}
		req, err := e.build(ctx, r, target, method, body, headers)
		if err != nil {
			e.release(r)
			return nil, nil, err
		}
		res, err := r.client.Do(req)
		if err != nil {
			e.mu.Lock()
			r.Failures++
			if !r.duplicateBlocked {
				r.LastError = "出口への接続に失敗しました"
			}
			e.mu.Unlock()
			e.event(r.Endpoint.ID, model, ip, 0, "connection_failed", "出口への接続に失敗しました")
			e.release(r)
			tried[r.Endpoint.ID] = true
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			continue
		}
		// A pooled relay can use a different device for every request. Record
		// its measured request IP without changing the logical route identity.
		if r.Endpoint.Kind == "vpn" || r.Endpoint.Kind == "warp" || r.Endpoint.Kind == "relay" {
			if measured := res.Header.Get("X-Relay-Exit-IP"); net.ParseIP(measured) != nil {
				ip = measured
			}
		}
		res.Header.Set("X-OCPool-Egress", r.Endpoint.ID)
		if last != nil {
			last.Body.Close()
			last = nil
		}
		if res.StatusCode == 429 {
			raw, readErr := io.ReadAll(io.LimitReader(res.Body, 2<<20))
			res.Body.Close()
			if readErr != nil {
				e.release(r)
				return nil, nil, readErr
			}
			// A single Surfshark pool relay owns the IP cooldown and rotates its
			// WireGuard peer internally. Do not pin the OCPool route to the old
			// relay IP; the next request should reach the relay after its own
			// bounded rotation. Other exits retain the normal gateway cooldown.
			internalRelayLimit := (r.Endpoint.Kind == "vpn" || r.Endpoint.Kind == "warp" || r.Endpoint.Kind == "relay") && strings.EqualFold(res.Header.Get("X-Relay-Rate-Limit"), "internal")
			scope := limitScope(raw)
			// A trusted in-container relay may classify an IP-wide limit while
			// preserving the upstream JSON body verbatim. Honor that explicit
			// scope marker so the single-catalog pool rotates immediately rather
			// than waiting for its local cooldown response.
			if (r.Endpoint.Kind == "vpn" || r.Endpoint.Kind == "warp" || r.Endpoint.Kind == "relay") && strings.EqualFold(res.Header.Get("X-OCPool-Limit-Scope"), "ip") {
				scope = "ip"
			}
			until := retryDeadline(res.Header.Get("Retry-After"), cfg.CooldownSeconds)
			if scope != "account" {
				res.Header.Set("X-OCPool-Limit-Scope", "egress")
			} else {
				res.Header.Set("X-OCPool-Limit-Scope", "account")
			}
			if !internalRelayLimit {
				e.limit(r, model, scope, string(raw), until)
			}
			res.Body = io.NopCloser(bytes.NewReader(raw))
			res.Header.Set("Content-Length", strconv.Itoa(len(raw)))
			last, lastRoute = res, nil
			if scope == "account" || internalRelayLimit {
				e.release(r)
				return res, nil, nil
			}
			tried[r.Endpoint.ID] = true
			if scope == "ip" && (r.Endpoint.Kind == "warp" || r.Endpoint.Kind == "vpn") && r.Endpoint.RotateOnLimit && !rotationTried[r.Endpoint.ID] && attempt+1 < cfg.MaxAttempts {
				rotationTried[r.Endpoint.ID] = true
				rotateCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				result, rotateErr := e.Rotate(rotateCtx, r.Endpoint.ID)
				cancel()
				changed, _ := result["changed"].(bool)
				if rotateErr == nil && changed {
					delete(tried, r.Endpoint.ID)
				}
			}
			e.release(r)
			continue
		}
		if res.StatusCode == 502 || res.StatusCode == 503 || res.StatusCode == 504 || res.StatusCode >= 520 {
			raw, readErr := io.ReadAll(io.LimitReader(res.Body, 2<<20))
			res.Body.Close()
			if readErr != nil {
				e.release(r)
				return nil, nil, readErr
			}
			res.Body = io.NopCloser(bytes.NewReader(raw))
			res.Header.Set("Content-Length", strconv.Itoa(len(raw)))
			last, lastRoute = res, nil
			tried[r.Endpoint.ID] = true
			e.mu.Lock()
			r.Failures++
			if !r.duplicateBlocked {
				r.LastError = "上流への一時的な接続エラー"
			}
			e.mu.Unlock()
			e.event(r.Endpoint.ID, model, ip, res.StatusCode, "transient_error", "上流への一時的な接続エラー")
			e.release(r)
			continue
		}
		e.mu.Lock()
		if res.StatusCode < 400 {
			r.Successes++
			if !r.duplicateBlocked {
				r.LastError = ""
				r.Healthy = true
			}
			if e.reconcileVPNDuplicatesLocked() {
				e.signal()
			}
			if r.Healthy {
				e.sticky = r.Endpoint.ID
			}
		} else {
			r.Failures++
		}
		e.mu.Unlock()
		e.event(r.Endpoint.ID, model, ip, res.StatusCode, "request", "")
		return res, r, nil
	}
	if last != nil {
		return last, lastRoute, nil
	}
	return nil, nil, errors.New("出口への接続に失敗しました")
}
func egressCooldownResponse(until int64, message string) *http.Response {
	raw, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "rate_limit_error", "message": "上流の出口IPは休止中です。 " + message}})
	h := http.Header{"Content-Type": {"application/json"}, "X-OCPool-Limit-Scope": {"egress"}, "Retry-After": {strconv.FormatInt(max(1, until-time.Now().Unix()), 10)}}
	return &http.Response{StatusCode: 429, Header: h, Body: io.NopCloser(bytes.NewReader(raw))}
}
func (e *Egress) allowed(target *url.URL) bool {
	if target.User != nil || target.Fragment != "" {
		return false
	}
	if e.allowedOrigin != "" && target.Scheme+"://"+target.Host == e.allowedOrigin {
		return true
	}
	return target.Scheme == "https" && target.Host == "opencode.ai" && (strings.HasPrefix(target.Path, "/zen/v1/") || strings.HasPrefix(target.Path, "/zen/go/v1/") || strings.HasPrefix(target.Path, "/inference/openai/v1/") || strings.HasPrefix(target.Path, "/inference/anthropic/v1/") || strings.HasPrefix(target.Path, "/inference/google/v1beta/"))
}

type egressWriter struct{ http.ResponseWriter }

func (w egressWriter) Write(raw []byte) (int, error) {
	n, err := w.ResponseWriter.Write(raw)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}
func (e *Egress) serve(w http.ResponseWriter, r *http.Request) {
	if !equal(r.Header.Get("X-OCPOOL-Egress-Auth"), e.token) {
		problem(w, 401, "invalid native egress key")
		return
	}
	if r.Method != "POST" {
		problem(w, 405, "POST required")
		return
	}
	target, err := url.Parse(r.Header.Get("X-OCPOOL-Target"))
	if err != nil || !e.allowed(target) {
		problem(w, 400, "unsupported upstream target")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		problem(w, 413, "native request exceeds 8 MiB")
		return
	}
	method := r.Header.Get("X-OCPOOL-Method")
	if method == "" {
		method = "POST"
	}
	if method != "POST" && method != "GET" {
		problem(w, 400, "unsupported method")
		return
	}
	response, route, err := e.forward(r.Context(), target, method, raw, r.Header)
	if err != nil {
		if r.Context().Err() == nil {
			problem(w, 502, err.Error())
		}
		return
	}
	defer response.Body.Close()
	if route != nil {
		defer e.release(route)
	}
	if response.StatusCode == 429 && e.onFailure != nil && r.Header.Get("X-OCPOOL-Agent") == "build" {
		var requestBody struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &requestBody)
		session := r.Header.Get("X-Opencode-Session-Id")
		if session != "" && requestBody.Model != "" {
			payload, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(payload))
			var body struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(payload, &body)
			e.mu.Lock()
			seconds := e.config.CooldownSeconds
			e.mu.Unlock()
			e.onFailure(session, requestBody.Model, &upstreamError{name: "APIError", message: body.Error.Message, status: 429, retryUntil: retryDeadline(response.Header.Get("Retry-After"), seconds), limitScope: response.Header.Get("X-OCPool-Limit-Scope")})
		}
	}
	for key, values := range response.Header {
		if strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(egressWriter{w}, response.Body)
}
