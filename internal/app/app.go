package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

type App struct {
	Root, Profiles, State string
	store                 *Store
	pool                  *Pool
	egress                *Egress
	adminSecret           string
	manager               *flock.Flock
	ctx                   context.Context
	cancel                context.CancelFunc
	wg                    sync.WaitGroup
	httpWG                sync.WaitGroup
	jobsMu                sync.Mutex
	jobs                  map[string]*jobRun
	admitMu               sync.Mutex
	pending               int
	pendingBytes          int64
	loginTimes            []time.Time
	started               time.Time
}

func New(root string) (*App, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	state := filepath.Join(root, ".state", "server")
	if err = os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	if err = secureDirectory(filepath.Dir(state)); err != nil {
		return nil, fmt.Errorf("protect state directory: %w", err)
	}
	lock := flock.New(filepath.Join(state, "manager.lock"))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		return nil, errors.New("another ocpool server is using this state directory")
	}
	store, err := openStore(filepath.Join(state, "pool.db"))
	if err != nil {
		lock.Unlock()
		return nil, err
	}
	path := filepath.Join(state, "admin-key.txt")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		raw = []byte(secret())
		err = atomicWrite(path, raw)
	}
	if err == nil && len(strings.TrimSpace(string(raw))) < 32 {
		err = errors.New("invalid admin key file")
	}
	if err != nil {
		store.db.Close()
		lock.Unlock()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{Root: root, State: state, Profiles: filepath.Join(root, ".state", "onboarding"), store: store, adminSecret: strings.TrimSpace(string(raw)), manager: lock, ctx: ctx, cancel: cancel, jobs: map[string]*jobRun{}, started: time.Now()}
	a.egress, err = newEgress(store, state)
	if err != nil {
		cancel()
		store.db.Close()
		lock.Unlock()
		return nil, err
	}
	a.pool = newPool(store, a.Profiles, defaultExecutable())
	a.pool.egress = a.egress
	a.egress.onFailure = a.pool.egressFailure
	_, _ = store.Import(a.Profiles)
	store.reconcileJobs(a.Profiles)
	a.wg.Add(3)
	go func() { defer a.wg.Done(); a.jobLoop() }()
	go func() { defer a.wg.Done(); a.pool.janitor(ctx) }()
	go func() { defer a.wg.Done(); a.egress.maintainVPNs(ctx) }()
	return a, nil
}
func (a *App) Close() {
	a.cancel()
	a.jobsMu.Lock()
	for _, j := range a.jobs {
		j.cancel()
	}
	a.jobsMu.Unlock()
	a.pool.close()
	a.wg.Wait()
	a.httpWG.Wait()
	a.egress.close()
	a.store.db.Close()
	a.manager.Unlock()
}
func (a *App) AccessFile(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	link := "http://" + net.JoinHostPort(host, port) + "/#access=" + a.adminSecret
	return os.WriteFile(filepath.Join(a.State, "access-url.txt"), []byte(link+"\n"), 0600)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(hash(a)), []byte(hash(b))) == 1
}
func (a *App) sign(value string) string {
	h := hmac.New(sha256.New, []byte(a.adminSecret))
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}
func (a *App) isAdmin(r *http.Request) bool {
	if bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); bearer != "" && equal(bearer, a.adminSecret) {
		return true
	}
	cookie, err := r.Cookie("ocpool_session")
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return false
	}
	expires, _ := strconv.ParseInt(parts[1], 10, 64)
	return expires > time.Now().Unix() && equal(parts[2], a.sign(parts[0]+"."+parts[1]))
}
func (a *App) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAdmin(r) {
			problem(w, 401, "管理画面へログインしてください")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Authorization") == "" {
			origin, e := url.Parse(r.Header.Get("Origin"))
			if e != nil || origin.Host != r.Host {
				problem(w, 403, "origin mismatch")
				return
			}
		}
		next(w, r)
	}
}

type keyContext struct{}

func (a *App) api(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyID := "admin"
		if !a.isAdmin(r) {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" {
				problem(w, 401, "API key required")
				return
			}
			if a.store.db.QueryRow("SELECT id FROM api_keys WHERE hash=? AND enabled=1", hash(token)).Scan(&keyID) != nil {
				problem(w, 401, "invalid API key")
				return
			}
			_, _ = a.store.db.Exec("UPDATE api_keys SET last_used=? WHERE id=?", time.Now().Unix(), keyID)
		}
		if keyID == "admin" && r.Header.Get("Authorization") == "" && r.Method != "GET" {
			o, e := url.Parse(r.Header.Get("Origin"))
			if e != nil || o.Host != r.Host {
				problem(w, 403, "origin mismatch")
				return
			}
		}
		if r.Method == "POST" {
			cfg := a.store.Settings()
			n := r.ContentLength
			if n <= 0 {
				n = 2 << 20
			}
			if n > 2<<20 {
				problem(w, 413, "request exceeds 2 MiB")
				return
			}
			a.admitMu.Lock()
			if a.pending >= cfg.QueueLimit+cfg.MaxWorkers || a.pendingBytes+n > 64<<20 {
				a.admitMu.Unlock()
				w.Header().Set("Retry-After", "2")
				problem(w, 429, "request queue is full")
				return
			}
			a.pending++
			a.pendingBytes += n
			a.admitMu.Unlock()
			defer func() { a.admitMu.Lock(); a.pending--; a.pendingBytes -= n; a.admitMu.Unlock() }()
		}
		next(w, r.WithContext(context.WithValue(r.Context(), keyContext{}, keyID)))
	}
}
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	m.HandleFunc("POST /api/login", a.login)
	m.HandleFunc("POST /api/logout", a.admin(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ocpool_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("GET /api/overview", a.admin(a.overview))
	m.HandleFunc("GET /api/accounts", a.admin(a.accounts))
	m.HandleFunc("POST /api/accounts/import", a.admin(a.importAccounts))
	m.HandleFunc("POST /api/accounts", a.admin(a.createAccounts))
	m.HandleFunc("PATCH /api/accounts/{id}", a.admin(a.updateAccount))
	m.HandleFunc("GET /api/accounts/{id}", a.admin(a.getAccount))
	m.HandleFunc("POST /api/accounts/{id}/jobs", a.admin(a.accountJob))
	m.HandleFunc("DELETE /api/accounts/{id}", a.admin(a.archiveAccount))
	m.HandleFunc("GET /api/jobs", a.admin(a.listJobs))
	m.HandleFunc("POST /api/jobs/cancel-queued", a.admin(func(w http.ResponseWriter, r *http.Request) {
		result, e := a.store.db.Exec("UPDATE jobs SET status='cancelled',updated_at=? WHERE status='queued'", time.Now().Unix())
		if e != nil {
			problem(w, 500, e.Error())
			return
		}
		n, _ := result.RowsAffected()
		writeJSON(w, 200, map[string]int64{"cancelled": n})
	}))
	m.HandleFunc("GET /api/jobs/{id}", a.admin(a.getJob))
	m.HandleFunc("POST /api/jobs/{id}/cancel", a.admin(a.cancelJob))
	m.HandleFunc("POST /api/jobs/{id}/resume", a.admin(a.resumeJob))
	m.HandleFunc("GET /api/jobs/{id}/frame", a.admin(a.jobFrame))
	m.HandleFunc("POST /api/jobs/{id}/input", a.admin(a.jobInput))
	m.HandleFunc("GET /api/settings", a.admin(a.getSettings))
	m.HandleFunc("PUT /api/settings", a.admin(a.putSettings))
	m.HandleFunc("GET /api/egress", a.admin(a.getEgress))
	m.HandleFunc("PUT /api/egress", a.admin(a.putEgress))
	m.HandleFunc("POST /api/egress/{id}/probe", a.admin(a.probeEgress))
	m.HandleFunc("POST /api/egress/{id}/rotate", a.admin(a.rotateEgress))
	m.HandleFunc("GET /api/keys", a.admin(a.listKeys))
	m.HandleFunc("POST /api/keys", a.admin(a.createKey))
	m.HandleFunc("DELETE /api/keys/{id}", a.admin(a.revokeKey))
	m.HandleFunc("GET /api/models", a.admin(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.store.ModelIDs()) }))
	m.HandleFunc("POST /api/runtimes/{id}/stop", a.admin(func(w http.ResponseWriter, r *http.Request) {
		a.pool.drain(r.PathValue("id"))
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("GET /v1/models", a.api(func(w http.ResponseWriter, r *http.Request) {
		models := []any{}
		for _, id := range a.store.ModelIDs() {
			models = append(models, map[string]any{"id": id, "object": "model", "owned_by": "opencode"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": models})
	}))
	m.HandleFunc("POST /v1/chat/completions", a.api(a.chat))
	m.HandleFunc("POST /v1/responses", a.api(a.responses))
	m.HandleFunc("GET /v1/responses/{id}", a.api(a.getResponse))
	assets, _ := fs.Sub(webAssets, "web")
	m.Handle("/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.httpWG.Add(1)
		defer a.httpWG.Done()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(a.ctx, cancel)
		defer stop()
		defer cancel()
		r = r.WithContext(ctx)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if host, _, e := net.SplitHostPort(r.RemoteAddr); e == nil && net.ParseIP(host).IsLoopback() {
			h := r.Host
			if x, _, e := net.SplitHostPort(h); e == nil {
				h = x
			}
			if h != "localhost" && h != "127.0.0.1" && h != "::1" {
				problem(w, 403, "invalid local host")
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	a.admitMu.Lock()
	now := time.Now()
	fresh := a.loginTimes[:0]
	for _, t := range a.loginTimes {
		if now.Sub(t) < time.Minute {
			fresh = append(fresh, t)
		}
	}
	a.loginTimes = fresh
	if len(fresh) >= 20 {
		a.admitMu.Unlock()
		problem(w, 429, "しばらく待ってから再試行してください")
		return
	}
	a.loginTimes = append(a.loginTimes, now)
	a.admitMu.Unlock()
	var v struct {
		Key string `json:"key"`
	}
	if readJSON(w, r, &v) != nil || !equal(v.Key, a.adminSecret) {
		problem(w, 401, "管理キーが違います")
		return
	}
	value := secret() + "." + strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)
	value += "." + a.sign(value)
	http.SetCookie(w, &http.Cookie{Name: "ocpool_session", Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) overview(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	rows, e := a.store.db.Query("SELECT status,count(*) FROM accounts GROUP BY status")
	if e == nil {
		for rows.Next() {
			var k string
			var n int
			_ = rows.Scan(&k, &n)
			counts[k] = n
		}
		rows.Close()
	}
	var total, success int
	_ = a.store.db.QueryRow("SELECT count(*),coalesce(sum(status=200),0) FROM requests WHERE created_at>?", time.Now().Add(-24*time.Hour).Unix()).Scan(&total, &success)
	a.admitMu.Lock()
	pending := a.pending
	a.admitMu.Unlock()
	writeJSON(w, 200, map[string]any{"accounts": counts, "requests_24h": total, "success_24h": success, "pending": pending, "runtimes": a.pool.snapshots(), "settings": a.store.Settings(), "uptime_seconds": int(time.Since(a.started).Seconds()), "models": a.store.ModelIDs()})
}
func (a *App) accounts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	list, total, e := a.store.Accounts(r.URL.Query().Get("q"), r.URL.Query().Get("status"), limit, offset)
	if e != nil {
		problem(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": list, "total": total, "limit": limit, "offset": offset})
}
func (a *App) getAccount(w http.ResponseWriter, r *http.Request) {
	v, e := a.store.Account(r.PathValue("id"))
	if e != nil {
		problem(w, 404, "account not found")
		return
	}
	writeJSON(w, 200, v)
}
func (a *App) importAccounts(w http.ResponseWriter, r *http.Request) {
	n, e := a.store.Import(a.Profiles)
	if e != nil {
		problem(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]int{"imported": n})
}
func (a *App) createAccounts(w http.ResponseWriter, r *http.Request) {
	bridge := a.bridge()
	if bridge.URL == "" || bridge.Token == "" || bridge.Domain == "" {
		problem(w, 503, "設定画面で確認メールの接続先を登録してください")
		return
	}
	var v struct {
		Count int      `json:"count"`
		Name  string   `json:"name"`
		Email string   `json:"email"`
		Tags  []string `json:"tags"`
	}
	if e := readJSON(w, r, &v); e != nil {
		problem(w, 400, e.Error())
		return
	}
	if v.Count < 1 || v.Count > 1000 || len(v.Name) > 100 || len(v.Tags) > 20 || (v.Count > 1 && v.Email != "") {
		problem(w, 400, "件数は1〜1000。メール指定は1件のみです")
		return
	}
	if v.Email != "" && !strings.HasSuffix(strings.ToLower(v.Email), "@"+a.bridgeDomain()) {
		problem(w, 400, "受信ドメインとメールアドレスが一致しません")
		return
	}
	v.Email = strings.ToLower(strings.TrimSpace(v.Email))
	if v.Email != "" {
		var count int
		_ = a.store.db.QueryRow("SELECT count(*) FROM accounts WHERE lower(email)=? AND status<>'archived'", v.Email).Scan(&count)
		if count > 0 {
			problem(w, 409, "このメールアドレスは登録済みです")
			return
		}
	}
	for i, tag := range v.Tags {
		v.Tags[i] = clip(strings.TrimSpace(tag), 64)
	}
	tx, e := a.store.db.Begin()
	if e != nil {
		problem(w, 500, e.Error())
		return
	}
	defer tx.Rollback()
	jobs := []Job{}
	for i := 0; i < v.Count; i++ {
		id := newID("acc")
		name := strings.TrimSpace(v.Name)
		if name == "" {
			name = "Account"
		}
		if v.Count > 1 {
			name += fmt.Sprintf(" %03d", i+1)
		}
		tags, _ := json.Marshal(v.Tags)
		now := time.Now().Unix()
		if _, e := tx.Exec("INSERT INTO accounts(id,name,email,status,enabled,tags,created_at,updated_at) VALUES(?,?,?,'new',1,?,?,?)", id, name, v.Email, string(tags), now, now); e != nil {
			problem(w, 500, e.Error())
			return
		}
		j := Job{ID: newID("job"), AccountID: id, Kind: "create", Status: "queued", Stage: "queued", CreatedAt: now, UpdatedAt: now}
		_, e := tx.Exec("INSERT INTO jobs(id,account_id,kind,status,stage,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", j.ID, j.AccountID, j.Kind, j.Status, j.Stage, j.CreatedAt, j.UpdatedAt)
		if e != nil {
			problem(w, 500, e.Error())
			return
		}
		jobs = append(jobs, j)
	}
	if e = tx.Commit(); e != nil {
		problem(w, 500, e.Error())
		return
	}
	writeJSON(w, 202, jobs)
}
func (a *App) updateAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, e := a.store.Account(id)
	if e != nil {
		problem(w, 404, "account not found")
		return
	}
	var patch struct {
		Name          *string   `json:"name"`
		Enabled       *bool     `json:"enabled"`
		Tags          *[]string `json:"tags"`
		ResetCooldown bool      `json:"reset_cooldown"`
	}
	if e = readJSON(w, r, &patch); e != nil {
		problem(w, 400, e.Error())
		return
	}
	if patch.Name != nil {
		v.Name = clip(*patch.Name, 100)
	}
	if patch.Enabled != nil {
		v.Enabled = *patch.Enabled
	}
	if patch.Tags != nil {
		if len(*patch.Tags) > 20 {
			problem(w, 400, "too many tags")
			return
		}
		v.Tags = *patch.Tags
	}
	tags, _ := json.Marshal(v.Tags)
	_, e = a.store.db.Exec("UPDATE accounts SET name=?,enabled=?,tags=?,updated_at=? WHERE id=?", v.Name, v.Enabled, string(tags), time.Now().Unix(), id)
	if e != nil {
		problem(w, 500, e.Error())
		return
	}
	if patch.ResetCooldown {
		_, _ = a.store.db.Exec("UPDATE accounts SET cooldown_until=0,last_error='' WHERE id=?", id)
	}
	if !v.Enabled {
		a.pool.drain(id)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) archiveAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, e := a.store.db.Exec("UPDATE accounts SET status='archived',enabled=0,updated_at=? WHERE id=?", time.Now().Unix(), id)
	if e != nil {
		problem(w, 500, e.Error())
		return
	}
	_, _ = a.store.db.Exec("UPDATE jobs SET status='cancelled' WHERE account_id=? AND status='queued'", id)
	a.pool.drain(id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) accountJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, e := a.store.Account(id); e != nil {
		problem(w, 404, "account not found")
		return
	}
	var v struct {
		Kind string `json:"kind"`
	}
	if readJSON(w, r, &v) != nil || (v.Kind != "probe" && v.Kind != "login" && v.Kind != "create" && v.Kind != "browser") {
		problem(w, 400, "invalid job kind")
		return
	}
	j, e := a.store.CreateJob(id, v.Kind)
	if e != nil {
		problem(w, 409, "このアカウントには実行待ちのジョブがあります")
		return
	}
	writeJSON(w, 202, j)
}

type bridgeConfig struct {
	URL    string `json:"base_url"`
	Domain string `json:"mail_domain"`
	Token  string `json:"api_token"`
}

func (a *App) bridge() bridgeConfig {
	var v bridgeConfig
	raw, _ := os.ReadFile(filepath.Join(a.Root, ".state", "email-bridge.json"))
	_ = json.Unmarshal(raw, &v)
	return v
}
func (a *App) bridgeDomain() string { return a.bridge().Domain }
func (a *App) getSettings(w http.ResponseWriter, r *http.Request) {
	b := a.bridge()
	writeJSON(w, 200, map[string]any{"pool": a.store.Settings(), "email": map[string]any{"base_url": b.URL, "mail_domain": b.Domain, "configured": b.Token != ""}, "opencode_binary": a.pool.executable})
}
func (a *App) putSettings(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Pool  Settings      `json:"pool"`
		Email *bridgeConfig `json:"email"`
	}
	if e := readJSON(w, r, &v); e != nil {
		problem(w, 400, e.Error())
		return
	}
	if e := v.Pool.Validate(); e != nil {
		problem(w, 400, e.Error())
		return
	}
	if v.Email != nil {
		u, e := url.Parse(v.Email.URL)
		if e != nil || u.Scheme != "https" || u.Host == "" || strings.ContainsAny(v.Email.Domain, "/@ \\:") || v.Email.Domain == "" {
			problem(w, 400, "Worker URLと受信ドメインを確認してください")
			return
		}
		if v.Email.Token == "" {
			v.Email.Token = a.bridge().Token
		}
		raw, _ := json.MarshalIndent(v.Email, "", "  ")
		if e = atomicWrite(filepath.Join(a.Root, ".state", "email-bridge.json"), raw); e != nil {
			problem(w, 500, e.Error())
			return
		}
	}
	if e := a.store.SaveSettings(v.Pool); e != nil {
		problem(w, 500, e.Error())
		return
	}
	a.pool.mu.Lock()
	excess := len(a.pool.workers) + len(a.pool.external) - v.Pool.MaxWorkers
	ids := []string{}
	for id := range a.pool.workers {
		if excess <= 0 {
			break
		}
		ids = append(ids, id)
		excess--
	}
	a.pool.mu.Unlock()
	for _, id := range ids {
		a.pool.drain(id)
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) listKeys(w http.ResponseWriter, r *http.Request) {
	rows, e := a.store.db.Query("SELECT id,name,prefix,enabled,created_at,last_used FROM api_keys ORDER BY created_at DESC")
	if e != nil {
		problem(w, 500, e.Error())
		return
	}
	defer rows.Close()
	list := []any{}
	for rows.Next() {
		var id, name, prefix string
		var enabled bool
		var created, used int64
		_ = rows.Scan(&id, &name, &prefix, &enabled, &created, &used)
		list = append(list, map[string]any{"id": id, "name": name, "prefix": prefix, "enabled": enabled, "created_at": created, "last_used": used})
	}
	writeJSON(w, 200, list)
}
func (a *App) createKey(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Name string `json:"name"`
	}
	if readJSON(w, r, &v) != nil || strings.TrimSpace(v.Name) == "" {
		problem(w, 400, "キーの名前を入力してください")
		return
	}
	id := newID("key")
	token := "ocp_" + secret()
	_, e := a.store.db.Exec("INSERT INTO api_keys(id,name,hash,prefix,created_at) VALUES(?,?,?,?,?)", id, clip(v.Name, 100), hash(token), token[:12], time.Now().Unix())
	if e != nil {
		problem(w, 500, e.Error())
		return
	}
	writeJSON(w, 201, map[string]string{"id": id, "token": token, "name": v.Name})
}
func (a *App) revokeKey(w http.ResponseWriter, r *http.Request) {
	_, _ = a.store.db.Exec("UPDATE api_keys SET enabled=0 WHERE id=?", r.PathValue("id"))
	writeJSON(w, 200, map[string]bool{"ok": true})
}
