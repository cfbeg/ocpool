package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := openStore(filepath.Join(t.TempDir(), "pool.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}
func addTestAccount(t *testing.T, s *Store, id string) {
	t.Helper()
	if e := s.PutAccount(Account{ID: id, Name: id, Status: "ready", Enabled: true, Tags: []string{"test"}}); e != nil {
		t.Fatal(e)
	}
	if e := s.SetModels(id, []string{"opencode/test-free"}); e != nil {
		t.Fatal(e)
	}
}
func TestThousandAccountsStayMetadataOnly(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 1000; i++ {
		addTestAccount(t, s, fmt.Sprintf("account-%04d", i))
	}
	page, total, e := s.Accounts("", "", 50, 950)
	if e != nil || total != 1000 || len(page) != 50 {
		t.Fatalf("pagination: %d %d %v", total, len(page), e)
	}
	candidates, e := s.Candidates("opencode/test-free", "")
	if e != nil || len(candidates) != 1000 {
		t.Fatal(len(candidates), e)
	}
	p := newPool(s, t.TempDir(), "not-started")
	if len(p.snapshots()) != 0 {
		t.Fatal("metadata listing started a process")
	}
}
func TestPoolEnforcesBoundAndReusesWorkers(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 12; i++ {
		addTestAccount(t, s, fmt.Sprintf("a%d", i))
	}
	cfg := defaultSettings()
	cfg.MaxWorkers = 3
	cfg.LeaseRequests = 1000
	if e := s.SaveSettings(cfg); e != nil {
		t.Fatal(e)
	}
	p := newPool(s, t.TempDir(), "")
	defer p.close()
	var active, peak, starts atomic.Int32
	p.startFn = func(ctx context.Context, id string) (*Runtime, error) {
		n := active.Add(1)
		starts.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		return &Runtime{ID: id, cancel: func() {}, kill: func() { active.Add(-1) }, subs: map[string]*subscription{}}, nil
	}
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, e := p.acquire(ctx, "opencode/test-free", "")
			if e != nil {
				t.Error(e)
				return
			}
			time.Sleep(5 * time.Millisecond)
			p.release(r, true)
		}()
	}
	wg.Wait()
	if peak.Load() > 3 {
		t.Fatalf("process bound exceeded: %d", peak.Load())
	}
	if starts.Load() > 3 {
		t.Fatalf("unnecessary worker churn: %d", starts.Load())
	}
}
func TestCreatorReservationSharesBudgetAndReleasesLock(t *testing.T) {
	s := testStore(t)
	addTestAccount(t, s, "a")
	cfg := defaultSettings()
	cfg.MaxWorkers = 1
	s.SaveSettings(cfg)
	root := t.TempDir()
	p := newPool(s, root, "")
	defer p.close()
	p.startFn = func(ctx context.Context, id string) (*Runtime, error) {
		return &Runtime{ID: id, cancel: func() {}, subs: map[string]*subscription{}}, nil
	}
	release, e := p.reserve(context.Background(), "creator")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, e = p.acquire(ctx, "opencode/test-free", ""); e == nil {
		t.Fatal("inference exceeded shared process budget")
	}
	release()
	r, e := p.acquire(context.Background(), "opencode/test-free", "")
	if e != nil {
		t.Fatal(e)
	}
	p.release(r, true)
	release, e = p.reserve(context.Background(), "creator")
	if e != nil {
		t.Fatal(e)
	}
	release()
}
func TestImportDoesNotResetPersistedAccountState(t *testing.T) {
	s := testStore(t)
	addTestAccount(t, s, "existing")
	s.db.Exec("UPDATE accounts SET status='login_required',enabled=0 WHERE id='existing'")
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "existing"), 0700)
	os.WriteFile(filepath.Join(root, "existing", "job.json"), []byte(`{"email":"test@example.com","stage":"ready"}`), 0600)
	n, e := s.Import(root)
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
	a, e := s.Account("existing")
	if e != nil || a.Status != "login_required" || a.Enabled {
		t.Fatal(a, e)
	}
}
func TestAdminAuthCSRFAndSecretIsolation(t *testing.T) {
	root := t.TempDir()
	a, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	cfg := defaultSettings()
	cfg.CreatorPaused = true
	a.store.SaveSettings(cfg)
	handler := a.Handler()
	request := func(method, path, body, auth, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if request("GET", "/api/overview", "", "", "", nil).Code != 401 {
		t.Fatal("missing auth allowed")
	}
	login := request("POST", "/api/login", `{"key":"`+a.adminSecret+`"}`, "", "", nil)
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	if request("POST", "/api/accounts/import", "{}", "", "https://wrong.example", cookie).Code != 403 {
		t.Fatal("cross-origin write allowed")
	}
	if request("GET", "/.state/server/admin-key.txt", "", a.adminSecret, "", nil).Code != 404 {
		t.Fatal("state file exposed")
	}
	created := request("POST", "/api/keys", `{"name":"client"}`, a.adminSecret, "", nil)
	var key map[string]string
	json.Unmarshal(created.Body.Bytes(), &key)
	if key["token"] == "" {
		t.Fatal(created.Body.String())
	}
	listed := request("GET", "/api/keys", "", a.adminSecret, "", nil)
	if strings.Contains(listed.Body.String(), key["token"]) {
		t.Fatal("key secret exposed in listing")
	}
	if request("GET", "/v1/models", "", key["token"], "", nil).Code != 200 {
		t.Fatal("API key does not work")
	}
	request("DELETE", "/api/keys/"+key["id"], "", a.adminSecret, "", nil)
	if request("GET", "/v1/models", "", key["token"], "", nil).Code != 401 {
		t.Fatal("revoked key remains valid")
	}
}
func TestPathAndInputValidation(t *testing.T) {
	for _, id := range []string{"../secrets", "a/b", "a\\b", "C:\\tmp", ".."} {
		if _, e := safePath(t.TempDir(), id); e == nil {
			t.Fatal("unsafe path accepted", id)
		}
	}
	if _, _, e := promptMessages([]Message{{Role: "tool", Content: json.RawMessage(`"x"`)}}); e == nil {
		t.Fatal("unsupported role silently accepted")
	}
	if _, e := messageText(json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://example.com"}}]`)); e == nil {
		t.Fatal("non-text input silently accepted")
	}
}

func TestJobPaginationAndResponseExpiry(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("job-account-%04d", i)
		addTestAccount(t, s, id)
		if _, e := s.CreateJob(id, "probe"); e != nil {
			t.Fatal(e)
		}
	}
	list, total, e := s.JobPage("queued", 50, 950)
	if e != nil || total != 1000 || len(list) != 50 {
		t.Fatal(total, len(list), e)
	}
	now := time.Now().Unix()
	_, e = s.db.Exec("INSERT INTO responses(id,account_id,session_id,model,body,expires_at,key_id) VALUES('old','job-account-0000','old-session','opencode/test-free','{}',?,'key'),('new','job-account-0000','new-session','opencode/test-free','{}',?,'key')", now-1, now+3600)
	if e != nil {
		t.Fatal(e)
	}
	s.expireResponses()
	var count int
	s.db.QueryRow("SELECT count(*) FROM responses").Scan(&count)
	if count != 1 {
		t.Fatal("live response removed or expired response retained", count)
	}
	var session string
	if e = s.db.QueryRow("SELECT session_id FROM session_gc").Scan(&session); e != nil || session != "old-session" {
		t.Fatal(session, e)
	}
}

func TestAtomicStateReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if e := atomicWrite(path, []byte(`{"stage":"first"}`)); e != nil {
		t.Fatal(e)
	}
	if e := atomicWrite(path, []byte(`{"stage":"second"}`)); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(path)
	if e != nil || string(raw) != `{"stage":"second"}` {
		t.Fatal(string(raw), e)
	}
}
