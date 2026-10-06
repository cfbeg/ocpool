package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)

func validID(s string) bool { return idPattern.MatchString(s) }
func newID(prefix string) string {
	b := make([]byte, 12)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + "_" + hex.EncodeToString(b)
}
func secret() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func problem(w http.ResponseWriter, status int, message string) {
	typ := "invalid_request_error"
	if status == 429 {
		typ = "rate_limit_error"
	} else if status >= 500 {
		typ = "api_error"
	} else if status == 401 {
		typ = "authentication_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": typ}})
}
func safePath(root, id string) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid id")
	}
	base, e := filepath.Abs(root)
	if e != nil {
		return "", e
	}
	p := filepath.Join(base, id)
	rel, e := filepath.Rel(base, p)
	if e != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", errors.New("path outside profile root")
	}
	if info, e := os.Lstat(p); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("symlink profile is not supported")
	}
	return p, nil
}

func atomicWrite(path string, raw []byte) error {
	file, e := os.CreateTemp(filepath.Dir(path), ".ocpool-*.tmp")
	if e != nil {
		return e
	}
	name := file.Name()
	defer os.Remove(name)
	if e = file.Chmod(0600); e != nil {
		file.Close()
		return e
	}
	if _, e = file.Write(raw); e != nil {
		file.Close()
		return e
	}
	if e = file.Sync(); e != nil {
		file.Close()
		return e
	}
	if e = file.Close(); e != nil {
		return e
	}
	return os.Rename(name, path)
}
