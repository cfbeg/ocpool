package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStopAcrossChunks(t *testing.T) {
	cases := []struct {
		patterns []string
		chunks   []string
		want     string
		stopped  bool
	}{
		{[]string{"STOP"}, []string{"hello S", "T", "OP leaked"}, "hello ", true},
		{[]string{"END", "END!"}, []string{"a E", "ND!tail"}, "a ", true},
		{[]string{"終わり"}, []string{"こんにちは終", "わ", "り続き"}, "こんにちは", true},
		{[]string{"STOP"}, []string{"ST", "ill complete"}, "STill complete", false},
		{nil, []string{"a", "b"}, "ab", false},
	}
	for _, c := range cases {
		f := stopBuffer{patterns: c.patterns}
		var out strings.Builder
		for _, chunk := range c.chunks {
			x, _ := f.feed(chunk)
			out.WriteString(x)
		}
		out.WriteString(f.flush())
		if out.String() != c.want || f.stopped != c.stopped {
			t.Fatalf("%v -> %q, %v", c.chunks, out.String(), f.stopped)
		}
	}
}
func TestNullAndEmptyStop(t *testing.T) {
	for _, raw := range []string{"null", "[]", " \n null "} {
		values, e := parseStop(json.RawMessage(raw))
		if e != nil || len(values) != 0 {
			t.Fatal(raw, values, e)
		}
	}
	if _, e := parseStop(json.RawMessage(`42`)); e == nil {
		t.Fatal("accepted numeric stop")
	}
}
func TestGenerationOptionsStayIsolated(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{ParamsDir: dir}
	a, b := 0.0, 1.2
	ca, e := (GenerationOptions{Temperature: &a}).prepare(r, "ses_a")
	if e != nil {
		t.Fatal(e)
	}
	defer ca()
	cb, e := (GenerationOptions{Temperature: &b}).prepare(r, "ses_b")
	if e != nil {
		t.Fatal(e)
	}
	defer cb()
	raw, _ := os.ReadFile(filepath.Join(dir, "ses_a.json"))
	if !strings.Contains(string(raw), `"temperature":0`) {
		t.Fatal(string(raw))
	}
	if _, e := (GenerationOptions{}).prepare(r, "../outside"); e == nil {
		t.Fatal("unsafe session id")
	}
}
func TestNativeParameterPlugin(t *testing.T) {
	if os.Getenv("OCPOOL_NATIVE_TEST") != "1" {
		t.Skip("native OpenCode integration test")
	}
	s := testStore(t)
	p := newPool(s, t.TempDir(), defaultExecutable())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, e := p.start(ctx, "native-parameter-test")
	if e != nil {
		t.Fatal(e)
	}
	defer r.close()
	raw, e := os.ReadFile(filepath.Join(r.ParamsDir, "ready.json"))
	if e != nil || !strings.Contains(string(raw), r.ParamsRun) {
		t.Fatal(string(raw), e)
	}
	plan, e := parseTools(json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]`), nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	cleanup, e := plan.connect(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	if e = cleanup(); e != nil {
		t.Fatal(e)
	}
}
