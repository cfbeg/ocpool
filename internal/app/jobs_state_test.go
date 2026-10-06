package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResumedJobResolvesItsParents(t *testing.T) {
	s := testStore(t)
	addTestAccount(t, s, "a")
	first, e := s.CreateJob("a", "create")
	if e != nil {
		t.Fatal(e)
	}
	s.JobState(first.ID, "needs_attention", "new", "old failure")
	second, e := s.CreateJob("a", "create", first.ID)
	if e != nil {
		t.Fatal(e)
	}
	s.completeJob(second.ID)
	old, _ := s.Job(first.ID)
	if old.Status != "resolved" || old.ResolvedBy != second.ID || old.Error != "" {
		t.Fatal(old)
	}
	var message string
	if e = s.db.QueryRow("SELECT message FROM events WHERE job_id=? AND stage='previous_error'", first.ID).Scan(&message); e != nil || message != "old failure" {
		t.Fatal(message, e)
	}
}
func TestLegacyFailedCreationResolvesOnlyAfterLaterSuccess(t *testing.T) {
	s := testStore(t)
	addTestAccount(t, s, "a")
	first, _ := s.CreateJob("a", "create")
	s.JobState(first.ID, "needs_attention", "new", "failed")
	second, _ := s.CreateJob("a", "create")
	s.JobState(second.ID, "completed", "ready", "")
	s.db.Exec("UPDATE jobs SET created_at=? WHERE id=?", time.Now().Unix()+1, second.ID)
	s.reconcileJobs(t.TempDir())
	old, _ := s.Job(first.ID)
	if old.Status != "resolved" || old.ResolvedBy != second.ID {
		t.Fatal(old)
	}
}
func TestInterruptedJobDoesNotReuseAnotherRunsReadyState(t *testing.T) {
	s := testStore(t)
	addTestAccount(t, s, "a")
	job, _ := s.CreateJob("a", "probe")
	s.JobState(job.ID, "interrupted", "sdk_probe_starting", "")
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a"), 0700)
	path := filepath.Join(root, "a", "job.json")
	os.WriteFile(path, []byte(`{"stage":"ready","completed_run":"a-different-run"}`), 0600)
	s.reconcileJobs(root)
	j, _ := s.Job(job.ID)
	if j.Status != "interrupted" {
		t.Fatal(j)
	}
	os.WriteFile(path, []byte(`{"stage":"ready","completed_run":"`+job.ID+`"}`), 0600)
	s.reconcileJobs(root)
	j, _ = s.Job(job.ID)
	if j.Status != "completed" {
		t.Fatal(j)
	}
}
