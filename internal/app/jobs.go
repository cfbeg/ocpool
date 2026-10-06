package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type jobRun struct {
	mu            sync.Mutex
	cancel        context.CancelFunc
	stdin         io.WriteCloser
	frame         []byte
	width, height int
	path          string
}

func (a *App) jobLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
			if a.store.Settings().CreatorPaused {
				continue
			}
			j, e := a.store.NextJob()
			if e != nil {
				continue
			}
			a.runJob(j)
		}
	}
}
func (a *App) runJob(j Job) {
	result, e := a.store.db.Exec("UPDATE jobs SET status='running',stage='preparing',updated_at=? WHERE id=? AND status='queued'", time.Now().Unix(), j.ID)
	if e != nil {
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 25*time.Minute)
	defer cancel()
	run := &jobRun{cancel: cancel}
	a.jobsMu.Lock()
	a.jobs[j.ID] = run
	a.jobsMu.Unlock()
	defer func() { a.jobsMu.Lock(); delete(a.jobs, j.ID); a.jobsMu.Unlock() }()
	a.store.Event(j.ID, "preparing", "実行枠を確保しています")
	release, e := a.pool.reserve(ctx, j.AccountID)
	if e != nil {
		a.store.JobState(j.ID, "failed", "preparing", e.Error())
		return
	}
	defer release()
	account, e := a.store.Account(j.AccountID)
	if e != nil || (!account.Enabled && (j.Kind == "create" || j.Kind == "retry")) || account.Status == "archived" {
		a.store.JobState(j.ID, "cancelled", "cancelled", "アカウントが無効になっています")
		return
	}
	args := []string{filepath.Join(a.Root, "scripts", "onboard.mjs"), "--id", j.AccountID, "--run-id", j.ID, "--managed", "--human-wait", "--mail-timeout", "300"}
	if account.Email != "" {
		args = append(args, "--email", account.Email)
	}
	if j.Kind == "probe" {
		args = append(args, "--recheck")
	}
	if j.Kind == "login" {
		args = append(args, "--login")
	}
	if j.Kind == "browser" {
		args = append(args, "--browser-only")
	}
	if j.Kind == "retry" {
		args = append(args, "--retry-signup")
	}
	cmd := exec.CommandContext(ctx, "node", args...)
	cmd.Dir = a.Root
	prepareCommand(cmd)
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		a.store.JobState(j.ID, "failed", "start", e.Error())
		return
	}
	stderr, e := cmd.StderrPipe()
	if e != nil {
		a.store.JobState(j.ID, "failed", "start", e.Error())
		return
	}
	stdin, e := cmd.StdinPipe()
	if e != nil {
		a.store.JobState(j.ID, "failed", "start", e.Error())
		return
	}
	run.mu.Lock()
	run.stdin = stdin
	run.mu.Unlock()
	if e = cmd.Start(); e != nil {
		a.store.JobState(j.ID, "failed", "start", e.Error())
		return
	}
	kill, e := ownProcess(cmd)
	if e != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		a.store.JobState(j.ID, "failed", "start", e.Error())
		return
	}
	defer kill()
	var readers sync.WaitGroup
	readers.Add(2)
	for _, reader := range []io.Reader{stdout, stderr} {
		go func(reader io.Reader) {
			defer readers.Done()
			scan := bufio.NewScanner(reader)
			scan.Buffer(make([]byte, 4096), 2<<20)
			for scan.Scan() {
				line := scan.Bytes()
				var v struct {
					Type   string          `json:"type"`
					Stage  string          `json:"stage"`
					Image  string          `json:"image"`
					Width  int             `json:"width"`
					Height int             `json:"height"`
					Path   string          `json:"path"`
					Reason string          `json:"reason"`
					Error  json.RawMessage `json:"error"`
					Job    string          `json:"job"`
				}
				if json.Unmarshal(line, &v) != nil {
					continue
				}
				if v.Type == "frame" {
					image, e := base64.StdEncoding.DecodeString(v.Image)
					if e != nil || len(image) > 1<<20 {
						continue
					}
					run.mu.Lock()
					run.frame = image
					run.width = v.Width
					run.height = v.Height
					run.path = v.Path
					run.mu.Unlock()
					continue
				}
				if v.Stage != "" {
					message := v.Reason
					if len(v.Error) > 0 {
						message = clip(string(v.Error), 1200)
					}
					state := "running"
					if strings.Contains(v.Stage, "confirmation") || strings.Contains(v.Stage, "action_required") {
						state = "waiting"
					}
					a.store.JobState(j.ID, state, v.Stage, message)
					a.store.Event(j.ID, v.Stage, message)
				}
			}
		}(reader)
	}
	waitErr := cmd.Wait()
	readers.Wait()
	run.mu.Lock()
	run.stdin = nil
	run.mu.Unlock()
	_ = a.store.SyncProfile(a.Profiles, j.AccountID)
	account, _ = a.store.Account(j.AccountID)
	current, _ := a.store.Job(j.ID)
	if ctx.Err() != nil {
		state := "cancelled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			state = "failed"
		}
		a.store.JobState(j.ID, state, current.Stage, "ジョブを停止しました")
		return
	}
	if account.Status == "ready" && waitErr == nil {
		a.store.completeJob(j.ID)
		a.store.Event(j.ID, "ready", "登録・認証・モデル疎通確認が完了しました")
		return
	}
	if current.Error == "" && waitErr != nil {
		current.Error = waitErr.Error()
	}
	a.store.JobState(j.ID, "needs_attention", account.Status, current.Error)
}
func (a *App) getJob(w http.ResponseWriter, r *http.Request) {
	j, e := a.store.Job(r.PathValue("id"))
	if e != nil {
		problem(w, 404, "job not found")
		return
	}
	rows, e := a.store.db.Query("SELECT stage,message,created_at FROM events WHERE job_id=? ORDER BY id", j.ID)
	events := []any{}
	if e == nil {
		for rows.Next() {
			var stage, message string
			var created int64
			_ = rows.Scan(&stage, &message, &created)
			events = append(events, map[string]any{"stage": stage, "message": message, "created_at": created})
		}
		rows.Close()
	}
	writeJSON(w, 200, map[string]any{"job": j, "events": events})
}
func (a *App) cancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.jobsMu.Lock()
	run := a.jobs[id]
	if run != nil {
		run.cancel()
	}
	a.jobsMu.Unlock()
	_, _ = a.store.db.Exec("UPDATE jobs SET status='cancelled',updated_at=? WHERE id=? AND status IN ('queued','running','waiting')", time.Now().Unix(), id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) resumeJob(w http.ResponseWriter, r *http.Request) {
	j, e := a.store.Job(r.PathValue("id"))
	if e != nil {
		problem(w, 404, "job not found")
		return
	}
	if j.Status == "running" || j.Status == "waiting" || j.Status == "queued" {
		problem(w, 409, "job is already active")
		return
	}
	if j.Status == "completed" || j.Status == "resolved" {
		problem(w, 409, "このジョブはすでに完了しています")
		return
	}
	var v struct {
		RetrySignup bool `json:"retry_signup"`
	}
	_ = readJSON(w, r, &v)
	kind := j.Kind
	if v.RetrySignup {
		kind = "retry"
	}
	next, e := a.store.CreateJob(j.AccountID, kind, j.ID)
	if e != nil {
		problem(w, 409, "account has an active job")
		return
	}
	writeJSON(w, 202, next)
}
func (a *App) jobFrame(w http.ResponseWriter, r *http.Request) {
	a.jobsMu.Lock()
	run := a.jobs[r.PathValue("id")]
	a.jobsMu.Unlock()
	if run == nil {
		w.WriteHeader(204)
		return
	}
	run.mu.Lock()
	frame := append([]byte(nil), run.frame...)
	width, height := run.width, run.height
	run.mu.Unlock()
	if len(frame) == 0 {
		w.WriteHeader(204)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Viewport-Width", strconv.Itoa(width))
	w.Header().Set("X-Viewport-Height", strconv.Itoa(height))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(frame)
}

func (a *App) listJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	list, total, e := a.store.JobPage(r.URL.Query().Get("status"), limit, offset)
	if e != nil {
		problem(w, 500, e.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": list, "total": total, "limit": limit, "offset": offset})
}
func (a *App) jobInput(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Type   string  `json:"type"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		DeltaX float64 `json:"delta_x"`
		DeltaY float64 `json:"delta_y"`
		Key    string  `json:"key"`
		Text   string  `json:"text"`
	}
	if readJSON(w, r, &v) != nil {
		problem(w, 400, "invalid input")
		return
	}
	switch v.Type {
	case "click", "scroll", "key", "text", "reload":
	default:
		problem(w, 400, "unsupported input")
		return
	}
	if len(v.Text) > 4096 || len(v.Key) > 50 {
		problem(w, 400, "input too large")
		return
	}
	a.jobsMu.Lock()
	run := a.jobs[r.PathValue("id")]
	a.jobsMu.Unlock()
	if run == nil {
		problem(w, 409, "browser is not active")
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.stdin == nil {
		problem(w, 409, "browser is not active")
		return
	}
	raw, _ := json.Marshal(v)
	_, e := run.stdin.Write(append(raw, '\n'))
	if e != nil {
		problem(w, 409, "browser disconnected")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
