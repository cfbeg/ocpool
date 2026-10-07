package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	api "github.com/acksell/opencode-go-sdk/sdk"
	"github.com/acksell/opencode-go-sdk/sdk/client"
	"github.com/acksell/opencode-go-sdk/sdk/core"
	"github.com/acksell/opencode-go-sdk/sdk/option"
	"github.com/gofrs/flock"
)

type event struct {
	Type       string `json:"type"`
	Properties struct {
		SessionID   string         `json:"sessionID"`
		MessageID   string         `json:"messageID"`
		PartID      string         `json:"partID"`
		Field       string         `json:"field"`
		Delta       string         `json:"delta"`
		ID          string         `json:"id"`
		EgressError *upstreamError `json:"-"`
		EgressModel string         `json:"-"`
		Permission  string         `json:"permission"`
		Tool        struct {
			MessageID string `json:"messageID"`
			CallID    string `json:"callID"`
		} `json:"tool"`
		Part struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			MessageID string `json:"messageID"`
			Type      string `json:"type"`
			Text      string `json:"text"`
		} `json:"part"`
		Info struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
			Role      string `json:"role"`
		} `json:"info"`
	} `json:"properties"`
}
type subscription struct {
	ch     chan event
	cancel context.CancelFunc
}
type Runtime struct {
	ID, Dir, URL          string
	ParamsDir, ParamsRun  string
	Client                *client.Client
	authorization         string
	cmd                   *exec.Cmd
	cancel                context.CancelFunc
	kill                  func()
	lock                  *flock.Flock
	done                  chan struct{}
	mu                    sync.Mutex
	subs                  map[string]*subscription
	created, last         time.Time
	uses                  int
	busy, starting, drain bool
	closed                bool
}

func (r *Runtime) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	if r.kill != nil {
		r.kill()
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	}
	if r.lock != nil {
		_ = r.lock.Unlock()
	}
}
func (r *Runtime) subscribe(id string, cancel context.CancelFunc) (<-chan event, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &subscription{make(chan event, 128), cancel}
	r.subs[id] = s
	return s.ch, func() { r.mu.Lock(); delete(r.subs, id); r.mu.Unlock() }
}
func (r *Runtime) events(ctx context.Context, recv func() ([]byte, error)) {
	for {
		raw, err := recv()
		if err != nil {
			r.mu.Lock()
			for _, s := range r.subs {
				s.cancel()
			}
			r.mu.Unlock()
			return
		}
		var wrap struct {
			Directory string          `json:"directory"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &wrap) != nil {
			continue
		}
		if wrap.Directory != "" && !strings.EqualFold(filepath.Clean(wrap.Directory), filepath.Clean(r.Dir)) {
			continue
		}
		var e event
		if json.Unmarshal(wrap.Payload, &e) != nil {
			continue
		}
		id := e.Properties.SessionID
		if id == "" {
			id = e.Properties.Part.SessionID
		}
		if id == "" {
			id = e.Properties.Info.SessionID
		}
		r.mu.Lock()
		if s := r.subs[id]; s != nil {
			select {
			case s.ch <- e:
			default:
				s.cancel()
			}
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

type Pool struct {
	mu               sync.Mutex
	store            *Store
	egress           *Egress
	root, executable string
	workers          map[string]*Runtime
	external         map[string]bool
	notify           chan struct{}
	starting         bool
	closing          bool
	failures         int
	blockedUntil     time.Time
	startFn          func(context.Context, string) (*Runtime, error)
}

func newPool(store *Store, root, exe string) *Pool {
	p := &Pool{store: store, root: root, executable: exe, workers: map[string]*Runtime{}, external: map[string]bool{}, notify: make(chan struct{})}
	p.startFn = p.start
	return p
}
func (p *Pool) egressFailure(session, model string, err *upstreamError) {
	p.mu.Lock()
	workers := []*Runtime{}
	for _, r := range p.workers {
		workers = append(workers, r)
	}
	p.mu.Unlock()
	for _, r := range workers {
		r.mu.Lock()
		if sub := r.subs[session]; sub != nil {
			var event event
			event.Type = "ocpool.egress.failure"
			event.Properties.SessionID = session
			event.Properties.EgressModel = model
			event.Properties.EgressError = err
			select {
			case sub.ch <- event:
			default:
				sub.cancel()
			}
		}
		r.mu.Unlock()
	}
}
func (p *Pool) signal() { close(p.notify); p.notify = make(chan struct{}) }
func (p *Pool) snapshots() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []map[string]any{}
	for id, r := range p.workers {
		state := "idle"
		if r.busy {
			state = "busy"
		}
		if r.starting {
			state = "starting"
		}
		if r.drain {
			state = "draining"
		}
		pid := 0
		if r.cmd != nil && r.cmd.Process != nil {
			pid = r.cmd.Process.Pid
		}
		out = append(out, map[string]any{"account_id": id, "state": state, "pid": pid, "requests": r.uses, "started_at": r.created.Unix()})
	}
	for id := range p.external {
		out = append(out, map[string]any{"account_id": id, "state": "creator", "requests": 0})
	}
	return out
}
func (p *Pool) acquire(ctx context.Context, model, pinned string, requestContexts ...context.Context) (*Runtime, error) {
	return p.acquireWithAffinity(ctx, model, pinned, "", requestContexts...)
}

func (p *Pool) acquireWithAffinity(ctx context.Context, model, pinned, affinity string, requestContexts ...context.Context) (*Runtime, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		cfg := p.store.Settings()
		candidates, err := p.store.Candidates(model, pinned)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return nil, errors.New("このモデルを利用できるアカウントがありません。状態・クールダウンを確認してください")
		}
		if pinned == "" && affinity != "" {
			// Rendezvous hashing keeps an eligible account stable across process
			// restarts and least-recently-used changes without an unbounded cache.
			// Busy accounts wait within the normal queue budget. Unavailable or
			// cooling accounts are already excluded by Candidates.
			candidates = []Account{affinityAccount(candidates, affinity)}
		}
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			return nil, errors.New("server shutting down")
		}
		if time.Now().Before(p.blockedUntil) {
			wait := p.notify
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
			case <-time.After(time.Second):
			}
			continue
		}
		cold := ""
		for _, a := range candidates {
			if p.workers[a.ID] == nil && !p.external[a.ID] {
				cold = a.ID
				break
			}
		}
		for _, a := range candidates {
			r := p.workers[a.ID]
			if r == nil || r.busy || r.starting || r.drain {
				continue
			}
			if pinned == "" && cold != "" && (time.Since(r.created) > time.Duration(cfg.LeaseSeconds)*time.Second || r.uses >= cfg.LeaseRequests) {
				continue
			}
			r.busy = true
			r.last = time.Now()
			p.mu.Unlock()
			return r, nil
		}
		if cold != "" && !p.starting && len(p.workers)+len(p.external) < cfg.MaxWorkers {
			p.starting = true
			placeholder := &Runtime{ID: cold, busy: true, starting: true, created: time.Now()}
			p.workers[cold] = placeholder
			p.mu.Unlock()
			// The queue deadline bounds waiting for capacity. Once capacity is
			// reserved, cold-start initialization uses the request deadline.
			startCtx := ctx
			if len(requestContexts) > 0 {
				startCtx = requestContexts[0]
			}
			r, err := p.startFn(startCtx, cold)
			p.mu.Lock()
			p.starting = false
			if err != nil {
				delete(p.workers, cold)
			} else {
				r.busy = true
				r.drain = placeholder.drain || p.closing
				r.created = time.Now()
				r.last = time.Now()
				p.workers[cold] = r
			}
			p.signal()
			p.mu.Unlock()
			if err != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					_, _ = p.store.db.Exec("UPDATE accounts SET last_error=?,cooldown_until=? WHERE id=?", clip(err.Error(), 1000), time.Now().Add(30*time.Second).Unix(), cold)
				}
				return nil, err
			}
			return r, nil
		}
		var evict *Runtime
		for _, r := range p.workers {
			if !r.busy && !r.starting {
				if evict == nil || r.last.Before(evict.last) {
					evict = r
				}
			}
		}
		if cold != "" && evict != nil {
			evict.busy = true
			evict.drain = true
			p.mu.Unlock()
			evict.close()
			p.mu.Lock()
			delete(p.workers, evict.ID)
			p.signal()
			p.mu.Unlock()
			continue
		}
		wait := p.notify
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		case <-time.After(time.Second):
		}
	}
}
func (p *Pool) release(r *Runtime, healthy bool) {
	p.mu.Lock()
	r.uses++
	r.last = time.Now()
	if !healthy || r.drain || p.closing {
		r.drain = true
		p.mu.Unlock()
		r.close()
		p.mu.Lock()
		delete(p.workers, r.ID)
	} else {
		r.busy = false
	}
	p.signal()
	p.mu.Unlock()
	_, _ = p.store.db.Exec("UPDATE accounts SET last_used=?,requests=requests+1 WHERE id=?", time.Now().Unix(), r.ID)
}
func (p *Pool) reserve(ctx context.Context, id string) (func(), error) {
	for {
		cfg := p.store.Settings()
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			return nil, errors.New("closing")
		}
		var victim *Runtime
		if r := p.workers[id]; r != nil {
			if !r.busy && !r.starting {
				victim = r
			}
		} else if !p.external[id] && len(p.workers)+len(p.external) < cfg.MaxWorkers {
			p.external[id] = true
			p.mu.Unlock()
			path, err := safePath(p.root, id)
			if err != nil {
				return nil, err
			}
			_ = os.MkdirAll(path, 0700)
			lock := flock.New(filepath.Join(path, "runtime.lock"))
			ok, e := lock.TryLock()
			if e != nil || !ok {
				p.mu.Lock()
				delete(p.external, id)
				p.signal()
				p.mu.Unlock()
				return nil, errors.New("profile is already in use")
			}
			return func() { _ = lock.Unlock(); p.mu.Lock(); delete(p.external, id); p.signal(); p.mu.Unlock() }, nil
		} else {
			for _, r := range p.workers {
				if !r.busy && !r.starting {
					victim = r
					break
				}
			}
		}
		if victim != nil {
			victim.busy = true
			victim.drain = true
			p.mu.Unlock()
			victim.close()
			p.mu.Lock()
			delete(p.workers, victim.ID)
			p.signal()
			p.mu.Unlock()
			continue
		}
		wait := p.notify
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		case <-time.After(time.Second):
		}
	}
}
func (p *Pool) drain(id string) {
	p.mu.Lock()
	r := p.workers[id]
	if r == nil {
		p.mu.Unlock()
		return
	}
	r.drain = true
	if r.busy || r.starting {
		p.mu.Unlock()
		return
	}
	r.busy = true
	p.mu.Unlock()
	r.close()
	p.mu.Lock()
	delete(p.workers, id)
	p.signal()
	p.mu.Unlock()
}
func (p *Pool) close() {
	p.mu.Lock()
	p.closing = true
	list := []*Runtime{}
	for _, r := range p.workers {
		if !r.starting {
			list = append(list, r)
		}
	}
	p.signal()
	p.mu.Unlock()
	for _, r := range list {
		r.close()
	}
}
func (p *Pool) janitor(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			p.store.expireResponses()
			cfg := p.store.Settings()
			p.mu.Lock()
			ids := []string{}
			for id, r := range p.workers {
				if !r.busy && !r.starting && time.Since(r.last) > time.Duration(cfg.IdleSeconds)*time.Second {
					ids = append(ids, id)
				}
			}
			p.mu.Unlock()
			for _, id := range ids {
				p.drain(id)
			}
			p.mu.Lock()
			var cleanup *Runtime
			for _, r := range p.workers {
				if !r.busy && !r.starting && !r.drain {
					r.busy = true
					cleanup = r
					break
				}
			}
			p.mu.Unlock()
			if cleanup != nil {
				p.collectSessions(ctx, cleanup)
				p.mu.Lock()
				cleanup.busy = false
				drain := cleanup.drain
				p.signal()
				p.mu.Unlock()
				if drain {
					p.drain(cleanup.ID)
				}
			}
		}
	}
}
func profileEnvironment(root, password string) ([]string, error) {
	for _, name := range []string{"data", "config", "cache", "state", "temp", "home", "workspace", "params"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return nil, err
		}
	}
	cfg := filepath.Join(root, "config", "opencode.json")
	pluginPath := filepath.Join(root, "config", "ocpool-parameters.mjs")
	if err := atomicWrite(pluginPath, parameterPlugin); err != nil {
		return nil, err
	}
	urlPath := filepath.ToSlash(pluginPath)
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	pluginURL := (&url.URL{Scheme: "file", Path: urlPath}).String()
	configBytes, _ := json.Marshal(map[string]any{"permission": map[string]string{"*": "ask"}, "plugin": []string{pluginURL}, "mcp": map[string]any{}, "experimental": map[string]any{"continue_loop_on_deny": false}})
	if err := atomicWrite(cfg, configBytes); err != nil {
		return nil, err
	}
	env := []string{}
	for _, v := range os.Environ() {
		key := strings.ToUpper(strings.SplitN(v, "=", 2)[0])
		switch key {
		case "PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC", "APPDATA", "LOCALAPPDATA", "USERPROFILE", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMDATA", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY":
			env = append(env, v)
		}
	}
	// OpenCode derives Global.Path.config as XDG_CONFIG_HOME/opencode. Keep
	// OPENCODE_CONFIG_DIR on that same directory so ConfigPaths deduplicates it;
	// pointing it at the parent config directory makes OpenCode install a second
	// copy of @opencode-ai/plugin for every account profile.
	for k, v := range map[string]string{"XDG_DATA_HOME": "data", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state", "TEMP": "temp", "TMP": "temp", "TMPDIR": "temp", "OPENCODE_TEST_HOME": "home", "OPENCODE_CONFIG_DIR": filepath.Join("config", "opencode")} {
		env = append(env, k+"="+filepath.Join(root, v))
	}
	env = append(env, "OPENCODE_CONFIG="+cfg, "OPENCODE_DISABLE_PROJECT_CONFIG=true", "OPENCODE_SERVER_PASSWORD="+password, "NO_COLOR=1")
	return env, nil
}
func defaultExecutable() string {
	if v := os.Getenv("OPENCODE_BINARY"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "npm", "node_modules", "opencode-ai", "bin", "opencode.exe")
	}
	return "opencode"
}
func (p *Pool) start(ctx context.Context, id string) (*Runtime, error) {
	ctx, cancelStartup := context.WithTimeout(ctx, 60*time.Second)
	defer cancelStartup()
	path, err := safePath(p.root, id)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(path, "runtime.lock"))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		return nil, errors.New("profile is already in use")
	}
	root := filepath.Join(path, "opencode")
	password := secret()
	paramsRun := newID("params")
	env, err := profileEnvironment(root, password)
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	runctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(runctx, p.executable, "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd.Dir = filepath.Join(root, "workspace")
	cmd.Env = append(env, "OCPOOL_PARAMS_DIR="+filepath.Join(root, "params"), "OCPOOL_PARAMS_RUN="+paramsRun)
	cmd.Env = append(cmd.Env, "OCPOOL_SMALL_MODEL="+p.store.Settings().DefaultModel)
	if p.egress != nil && p.egress.Enabled() {
		cmd.Env = append(cmd.Env, "OCPOOL_EGRESS_URL="+p.egress.URL, "OCPOOL_EGRESS_TOKEN="+p.egress.token)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	prepareCommand(cmd)
	r := &Runtime{ID: id, Dir: cmd.Dir, URL: fmt.Sprintf("http://127.0.0.1:%d", port), ParamsDir: filepath.Join(root, "params"), ParamsRun: paramsRun, cmd: cmd, cancel: cancel, lock: lock, done: make(chan struct{}), subs: map[string]*subscription{}}
	if err = cmd.Start(); err != nil {
		cancel()
		lock.Unlock()
		return nil, err
	}
	r.kill, err = ownProcess(cmd)
	go func() { _ = cmd.Wait(); close(r.done) }()
	if err != nil {
		r.close()
		return nil, err
	}
	header := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password))}}
	r.Client = client.NewClient(option.WithBaseURL(r.URL), option.WithHTTPHeader(header), option.WithMaxAttempts(1))
	r.authorization = header.Get("Authorization")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			r.close()
			return nil, ctx.Err()
		}
		select {
		case <-r.done:
			r.close()
			return nil, errors.New("OpenCode exited during startup")
		default:
		}
		probe, stop := context.WithTimeout(ctx, time.Second)
		_, e := r.Client.Global.Health(probe)
		stop()
		if e == nil {
			_, e = r.Client.Config.Providers(ctx, &api.ConfigProvidersRequest{Directory: &r.Dir})
			if e != nil {
				r.close()
				return nil, e
			}
			readyRaw, e := os.ReadFile(filepath.Join(r.ParamsDir, "ready.json"))
			var ready struct {
				Run string `json:"run"`
			}
			if e != nil || json.Unmarshal(readyRaw, &ready) != nil || ready.Run != paramsRun {
				r.close()
				return nil, errors.New("OpenCode parameter plugin did not initialize")
			}
			stream, e := r.Client.Global.Event(runctx)
			if e != nil {
				r.close()
				return nil, e
			}
			go func() { defer stream.Close(); r.events(runctx, stream.RecvRaw) }()
			p.collectSessions(ctx, r)
			return r, nil
		}
		select {
		case <-ctx.Done():
			r.close()
			return nil, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	r.close()
	return nil, errors.New("OpenCode startup timeout")
}
func abortSession(r *Runtime, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = r.Client.Session.Abort(ctx, &api.SessionAbortRequest{SessionID: id, Directory: &r.Dir})
}

func (p *Pool) collectSessions(ctx context.Context, r *Runtime) {
	rows, e := p.store.db.Query("SELECT session_id FROM session_gc g WHERE account_id=? AND NOT EXISTS (SELECT 1 FROM responses s WHERE s.session_id=g.session_id AND s.expires_at>?) LIMIT 10", r.ID, time.Now().Unix())
	if e != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		timeout, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, e := r.Client.Session.Delete(timeout, &api.SessionDeleteRequest{SessionID: id, Directory: &r.Dir})
		cancel()
		var apiErr *core.APIError
		if e == nil || (errors.As(e, &apiErr) && apiErr.StatusCode == 404) {
			_, _ = p.store.db.Exec("DELETE FROM session_gc WHERE account_id=? AND session_id=?", r.ID, id)
		}
	}
}
