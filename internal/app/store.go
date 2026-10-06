package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Account struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Email         string   `json:"email"`
	Status        string   `json:"status"`
	Enabled       bool     `json:"enabled"`
	Tags          []string `json:"tags"`
	Models        []string `json:"models"`
	LastError     string   `json:"last_error"`
	CooldownUntil int64    `json:"cooldown_until"`
	LastUsed      int64    `json:"last_used"`
	Requests      int64    `json:"requests"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
}
type Job struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Stage       string `json:"stage"`
	Error       string `json:"error"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	ResumedFrom string `json:"resumed_from,omitempty"`
	ResolvedBy  string `json:"resolved_by,omitempty"`
}
type Settings struct {
	MaxWorkers     int    `json:"max_workers"`
	QueueLimit     int    `json:"queue_limit"`
	QueueSeconds   int    `json:"queue_seconds"`
	IdleSeconds    int    `json:"idle_seconds"`
	LeaseSeconds   int    `json:"lease_seconds"`
	LeaseRequests  int    `json:"lease_requests"`
	RequestSeconds int    `json:"request_seconds"`
	DefaultModel   string `json:"default_model"`
	CreatorPaused  bool   `json:"creator_paused"`
}

func defaultSettings() Settings {
	return Settings{4, 128, 30, 300, 600, 32, 180, "opencode/big-pickle", false}
}
func (s Settings) Validate() error {
	if s.MaxWorkers < 1 || s.MaxWorkers > 32 || s.QueueLimit < 1 || s.QueueLimit > 4096 || s.QueueSeconds < 1 || s.QueueSeconds > 300 || s.IdleSeconds < 10 || s.IdleSeconds > 86400 || s.LeaseSeconds < 10 || s.LeaseSeconds > 86400 || s.LeaseRequests < 1 || s.LeaseRequests > 10000 || s.RequestSeconds < 10 || s.RequestSeconds > 1800 || len(s.DefaultModel) > 200 {
		return errors.New("設定値が範囲外です")
	}
	return nil
}

type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS accounts(id TEXT PRIMARY KEY,name TEXT NOT NULL,email TEXT NOT NULL DEFAULT '',status TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,tags TEXT NOT NULL DEFAULT '[]',last_error TEXT NOT NULL DEFAULT '',cooldown_until INTEGER NOT NULL DEFAULT 0,last_used INTEGER NOT NULL DEFAULT 0,requests INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS account_eligible ON accounts(enabled,status,cooldown_until,last_used);
 CREATE TABLE IF NOT EXISTS models(account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,model_id TEXT NOT NULL,PRIMARY KEY(account_id,model_id));
 CREATE INDEX IF NOT EXISTS model_lookup ON models(model_id,account_id);
 CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,account_id TEXT NOT NULL REFERENCES accounts(id),kind TEXT NOT NULL,status TEXT NOT NULL,stage TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS job_queue ON jobs(status,created_at);
 CREATE INDEX IF NOT EXISTS job_account_time ON jobs(account_id,created_at);
 CREATE UNIQUE INDEX IF NOT EXISTS one_account_job ON jobs(account_id) WHERE status IN ('queued','running','waiting');
 CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT,job_id TEXT NOT NULL REFERENCES jobs(id),stage TEXT NOT NULL,message TEXT NOT NULL,created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS settings(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS api_keys(id TEXT PRIMARY KEY,name TEXT NOT NULL,hash TEXT NOT NULL UNIQUE,prefix TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,created_at INTEGER NOT NULL,last_used INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY,account_id TEXT,model TEXT,status INTEGER NOT NULL,milliseconds INTEGER NOT NULL,created_at INTEGER NOT NULL,error TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS responses(id TEXT PRIMARY KEY,account_id TEXT NOT NULL,session_id TEXT NOT NULL,model TEXT NOT NULL,body TEXT NOT NULL,expires_at INTEGER NOT NULL,key_id TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS response_expiry ON responses(expires_at);
 CREATE TABLE IF NOT EXISTS session_gc(account_id TEXT NOT NULL,session_id TEXT NOT NULL,PRIMARY KEY(account_id,session_id));
 CREATE INDEX IF NOT EXISTS request_time ON requests(created_at);
 UPDATE jobs SET status='interrupted',error='サーバー再起動により中断。状態を確認して再開してください。' WHERE status IN ('running','waiting');`)
	if err != nil {
		db.Close()
		return nil, err
	}
	columns := map[string]bool{}
	rows, e := db.Query("PRAGMA table_info(jobs)")
	if e != nil {
		db.Close()
		return nil, e
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if e = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); e != nil {
			rows.Close()
			db.Close()
			return nil, e
		}
		columns[name] = true
	}
	rows.Close()
	for _, name := range []string{"resumed_from", "resolved_by"} {
		if !columns[name] {
			if _, e = db.Exec("ALTER TABLE jobs ADD COLUMN " + name + " TEXT NOT NULL DEFAULT ''"); e != nil {
				db.Close()
				return nil, e
			}
		}
	}
	raw, _ := json.Marshal(defaultSettings())
	_, err = db.Exec("INSERT OR IGNORE INTO settings(id,value) VALUES(1,?)", string(raw))
	return &Store{db}, err
}
func (s *Store) Settings() Settings {
	v := defaultSettings()
	var raw string
	if s.db.QueryRow("SELECT value FROM settings WHERE id=1").Scan(&raw) == nil {
		_ = json.Unmarshal([]byte(raw), &v)
	}
	return v
}
func (s *Store) SaveSettings(v Settings) error {
	if err := v.Validate(); err != nil {
		return err
	}
	raw, _ := json.Marshal(v)
	_, err := s.db.Exec("UPDATE settings SET value=? WHERE id=1", string(raw))
	return err
}

const accountCols = "id,name,email,status,enabled,tags,last_error,cooldown_until,last_used,requests,created_at,updated_at"

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var tags string
	err := row.Scan(&a.ID, &a.Name, &a.Email, &a.Status, &a.Enabled, &tags, &a.LastError, &a.CooldownUntil, &a.LastUsed, &a.Requests, &a.CreatedAt, &a.UpdatedAt)
	_ = json.Unmarshal([]byte(tags), &a.Tags)
	if a.Tags == nil {
		a.Tags = []string{}
	}
	a.Models = []string{}
	return a, err
}
func (s *Store) Account(id string) (Account, error) {
	a, err := scanAccount(s.db.QueryRow("SELECT "+accountCols+" FROM accounts WHERE id=?", id))
	if err != nil {
		return a, err
	}
	rows, err := s.db.Query("SELECT model_id FROM models WHERE account_id=? ORDER BY model_id", id)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		_ = rows.Scan(&m)
		a.Models = append(a.Models, m)
	}
	return a, rows.Err()
}
func (s *Store) Accounts(q, state string, limit, offset int) ([]Account, int, error) {
	where := "status<>'archived'"
	args := []any{}
	if q != "" {
		where += " AND (name LIKE ? OR email LIKE ? OR tags LIKE ?)"
		for range 3 {
			args = append(args, "%"+q+"%")
		}
	}
	if state != "" {
		where += " AND status=?"
		args = append(args, state)
	}
	var total int
	_ = s.db.QueryRow("SELECT count(*) FROM accounts WHERE "+where, args...).Scan(&total)
	rows, err := s.db.Query("SELECT "+accountCols+" FROM accounts WHERE "+where+" ORDER BY created_at DESC,id LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	list := []Account{}
	for rows.Next() {
		a, e := scanAccount(rows)
		if e != nil {
			rows.Close()
			return nil, 0, e
		}
		list = append(list, a)
	}
	err = rows.Err()
	rows.Close()
	return list, total, err
}
func (s *Store) PutAccount(a Account) error {
	if !validID(a.ID) {
		return errors.New("invalid account id")
	}
	if a.Name == "" {
		a.Name = a.ID
	}
	if a.Status == "" {
		a.Status = "new"
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	a.UpdatedAt = time.Now().Unix()
	tags, _ := json.Marshal(a.Tags)
	_, err := s.db.Exec("INSERT INTO accounts(id,name,email,status,enabled,tags,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING", a.ID, a.Name, a.Email, a.Status, a.Enabled, string(tags), a.CreatedAt, a.UpdatedAt)
	return err
}
func (s *Store) SetModels(id string, models []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM models WHERE account_id=?", id); err != nil {
		return err
	}
	for _, m := range models {
		if len(m) > 200 {
			continue
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO models(account_id,model_id) VALUES(?,?)", id, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Candidates(model, account string) ([]Account, error) {
	q := "SELECT "
	for i, col := range strings.Split(accountCols, ",") {
		if i > 0 {
			q += ","
		}
		q += "a." + col
	}
	q += " FROM accounts a JOIN models m ON m.account_id=a.id WHERE a.enabled=1 AND a.status='ready' AND a.cooldown_until<=? AND m.model_id=?"
	args := []any{time.Now().Unix(), model}
	if account != "" {
		q += " AND a.id=?"
		args = append(args, account)
	}
	q += " ORDER BY a.last_used,a.requests,a.id"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Account{}
	for rows.Next() {
		a, e := scanAccount(rows)
		if e != nil {
			return nil, e
		}
		list = append(list, a)
	}
	return list, rows.Err()
}
func (s *Store) ModelIDs() []string {
	rows, err := s.db.Query("SELECT DISTINCT m.model_id FROM models m JOIN accounts a ON a.id=m.account_id WHERE a.enabled=1 AND a.status='ready' ORDER BY m.model_id")
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		result = append(result, id)
	}
	return result
}
func (s *Store) CreateJob(account, kind string, parent ...string) (Job, error) {
	j := Job{ID: newID("job"), AccountID: account, Kind: kind, Status: "queued", Stage: "queued", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}
	if len(parent) > 0 {
		j.ResumedFrom = parent[0]
	}
	_, err := s.db.Exec("INSERT INTO jobs(id,account_id,kind,status,stage,created_at,updated_at,resumed_from) VALUES(?,?,?,?,?,?,?,?)", j.ID, j.AccountID, j.Kind, j.Status, j.Stage, j.CreatedAt, j.UpdatedAt, j.ResumedFrom)
	return j, err
}
func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.AccountID, &j.Kind, &j.Status, &j.Stage, &j.Error, &j.CreatedAt, &j.UpdatedAt, &j.ResumedFrom, &j.ResolvedBy)
	return j, err
}

const jobCols = "id,account_id,kind,status,stage,error,created_at,updated_at,resumed_from,resolved_by"

func (s *Store) Job(id string) (Job, error) {
	return scanJob(s.db.QueryRow("SELECT "+jobCols+" FROM jobs WHERE id=?", id))
}
func (s *Store) Jobs(limit int) []Job {
	rows, e := s.db.Query("SELECT "+jobCols+" FROM jobs ORDER BY created_at DESC,id DESC LIMIT ?", limit)
	if e != nil {
		return []Job{}
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, e := scanJob(rows)
		if e == nil {
			out = append(out, j)
		}
	}
	return out
}
func (s *Store) NextJob() (Job, error) {
	return scanJob(s.db.QueryRow("SELECT " + jobCols + " FROM jobs WHERE status='queued' ORDER BY created_at,id LIMIT 1"))
}
func (s *Store) JobState(id, state, stage, message string) {
	_, _ = s.db.Exec("UPDATE jobs SET status=?,stage=?,error=?,updated_at=? WHERE id=?", state, stage, clip(message, 1500), time.Now().Unix(), id)
}

func (s *Store) completeJob(id string) {
	s.JobState(id, "completed", "ready", "")
	child, e := s.Job(id)
	if e != nil {
		return
	}
	parent := child.ResumedFrom
	seen := map[string]bool{id: true}
	for parent != "" && !seen[parent] {
		seen[parent] = true
		previous, e := s.Job(parent)
		if e != nil || previous.AccountID != child.AccountID {
			break
		}
		s.resolveJob(parent, id)
		parent = previous.ResumedFrom
	}
}
func (s *Store) resolveJob(id, success string) {
	tx, e := s.db.Begin()
	if e != nil {
		return
	}
	defer tx.Rollback()
	var oldError, state string
	if e = tx.QueryRow("SELECT status,error FROM jobs WHERE id=?", id).Scan(&state, &oldError); e != nil {
		return
	}
	if state == "running" || state == "waiting" || state == "queued" || state == "completed" || state == "resolved" {
		return
	}
	now := time.Now().Unix()
	if oldError != "" {
		_, _ = tx.Exec("INSERT INTO events(job_id,stage,message,created_at) VALUES(?,?,?,?)", id, "previous_error", oldError, now)
	}
	if _, e = tx.Exec("UPDATE jobs SET status='resolved',stage='ready',error='',resolved_by=?,updated_at=? WHERE id=?", success, now, id); e != nil {
		return
	}
	_, _ = tx.Exec("INSERT INTO events(job_id,stage,message,created_at) VALUES(?,?,?,?)", id, "resolved", "再開先のジョブ "+success+" で完了しました", now)
	_ = tx.Commit()
}
func (s *Store) reconcileJobs(profiles string) {
	rows, e := s.db.Query("SELECT id,account_id FROM jobs WHERE status='interrupted'")
	if e != nil {
		return
	}
	type run struct{ ID, Account string }
	runs := []run{}
	for rows.Next() {
		var r run
		if rows.Scan(&r.ID, &r.Account) == nil {
			runs = append(runs, r)
		}
	}
	rows.Close()
	for _, r := range runs {
		path, e := safePath(profiles, r.Account)
		if e != nil {
			continue
		}
		raw, e := os.ReadFile(filepath.Join(path, "job.json"))
		if e != nil {
			continue
		}
		var state struct {
			Stage        string `json:"stage"`
			CompletedRun string `json:"completed_run"`
		}
		if json.Unmarshal(raw, &state) == nil && state.Stage == "ready" && state.CompletedRun == r.ID {
			s.completeJob(r.ID)
		}
	}
	rows, e = s.db.Query(`SELECT old.id,(SELECT done.id FROM jobs done WHERE done.account_id=old.account_id AND done.status='completed' AND done.kind IN ('create','retry') AND done.created_at>old.created_at ORDER BY done.created_at,done.id LIMIT 1)
 FROM jobs old JOIN accounts a ON a.id=old.account_id WHERE old.status IN ('needs_attention','failed','interrupted') AND old.kind IN ('create','retry') AND a.status='ready'`)
	if e != nil {
		return
	}
	type pair struct{ Old, Done string }
	pairs := []pair{}
	for rows.Next() {
		var id string
		var done sql.NullString
		if rows.Scan(&id, &done) == nil && done.Valid {
			pairs = append(pairs, pair{id, done.String})
		}
	}
	rows.Close()
	for _, p := range pairs {
		s.resolveJob(p.Old, p.Done)
	}
}
func (s *Store) Event(id, stage, message string) {
	_, _ = s.db.Exec("INSERT INTO events(job_id,stage,message,created_at) VALUES(?,?,?,?)", id, stage, clip(message, 1500), time.Now().Unix())
	_, _ = s.db.Exec("DELETE FROM events WHERE job_id=? AND id NOT IN (SELECT id FROM events WHERE job_id=? ORDER BY id DESC LIMIT 200)", id, id)
}
func (s *Store) Import(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		var exists int
		if s.db.QueryRow("SELECT 1 FROM accounts WHERE id=?", entry.Name()).Scan(&exists) == nil {
			continue
		}
		if err := s.SyncProfile(root, entry.Name()); err == nil {
			count++
		}
	}
	return count, nil
}
func (s *Store) SyncProfile(root, id string) error {
	raw, err := os.ReadFile(filepath.Join(root, id, "job.json"))
	if err != nil {
		return err
	}
	var data struct {
		Email string `json:"email"`
		Stage string `json:"stage"`
		Probe struct {
			Models []string `json:"zero_cost_catalog_models"`
		} `json:"probe"`
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		return err
	}
	status := data.Stage
	if status == "" {
		status = "new"
	}
	if err = s.PutAccount(Account{ID: id, Name: id, Email: data.Email, Status: status, Enabled: true, Tags: []string{}}); err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE accounts SET email=?,status=?,last_error='',cooldown_until=0,updated_at=? WHERE id=? AND status<>'archived'", data.Email, status, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if data.Probe.Models != nil {
		return s.SetModels(id, data.Probe.Models)
	}
	return nil
}
func clip(v string, n int) string {
	if len(v) > n {
		return v[:n] + "…"
	}
	return v
}

func (s *Store) JobPage(state string, limit, offset int) ([]Job, int, error) {
	where := "1=1"
	args := []any{}
	if state != "" {
		where = "status=?"
		args = append(args, state)
	}
	var total int
	if e := s.db.QueryRow("SELECT count(*) FROM jobs WHERE "+where, args...).Scan(&total); e != nil {
		return nil, 0, e
	}
	rows, e := s.db.Query("SELECT "+jobCols+" FROM jobs WHERE "+where+" ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			return nil, 0, e
		}
		out = append(out, j)
	}
	return out, total, rows.Err()
}

func (s *Store) expireResponses() {
	tx, e := s.db.Begin()
	if e != nil {
		return
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, e = tx.Exec("INSERT OR IGNORE INTO session_gc(account_id,session_id) SELECT account_id,session_id FROM responses WHERE expires_at<=?", now); e != nil {
		return
	}
	if _, e = tx.Exec("DELETE FROM responses WHERE expires_at<=?", now); e != nil {
		return
	}
	_, _ = tx.Exec("DELETE FROM requests WHERE created_at<?", time.Now().Add(-7*24*time.Hour).Unix())
	_, _ = tx.Exec("DELETE FROM requests WHERE id NOT IN (SELECT id FROM requests ORDER BY created_at DESC LIMIT 10000)")
	_ = tx.Commit()
}
func sqlError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("database operation failed: %w", err)
}
