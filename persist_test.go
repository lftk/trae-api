package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestSessionStore(t *testing.T, idleTimeout time.Duration) *sessionStore {
	t.Helper()
	dir := t.TempDir()
	return newSessionStore(dir, idleTimeout)
}

func TestSessionStoreDisabledNoOps(t *testing.T) {
	s := newSessionStore("", time.Hour)
	if s.enabled() {
		t.Fatal("store with empty dir should be disabled")
	}
	if rec, err := s.loadRecord("any"); err != nil || rec != nil {
		t.Fatalf("load on disabled store: rec=%v err=%v", rec, err)
	}
	if err := s.storeRecord(&sessionRecord{ExternalID: "any", AcpSessionID: "x", Cwd: "/"}); err != nil {
		t.Fatalf("store on disabled store: err=%v", err)
	}
	s.deleteRecord("any")
	if err := s.touchLastUsed("any", time.Now()); err != nil {
		t.Fatalf("touch on disabled store: err=%v", err)
	}
	s.pruneExpired(time.Now())
}

func TestSessionStoreRoundTrip(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	now := time.Now().Truncate(time.Second)
	rec := &sessionRecord{
		ExternalID:   "client-id-1",
		AcpSessionID: "acp-uuid-1",
		Cwd:          "/project",
		Model:        "GLM-5",
		CreatedAt:    now,
		LastUsedAt:   now,
	}
	if err := s.storeRecord(rec); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.loadRecord("client-id-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatal("record not found after store")
	}
	if loaded.AcpSessionID != "acp-uuid-1" || loaded.Cwd != "/project" || loaded.Model != "GLM-5" {
		t.Fatalf("loaded record mismatch: %+v", loaded)
	}
	if !loaded.CreatedAt.Equal(now) {
		t.Fatalf("created_at = %v, want %v", loaded.CreatedAt, now)
	}
}

func TestSessionStoreMissingReturnsNil(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	rec, err := s.loadRecord("absent")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatalf("expected nil for absent record, got %+v", rec)
	}
}

func TestSessionStoreDeleteIsIdempotent(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	_ = s.storeRecord(&sessionRecord{ExternalID: "x", AcpSessionID: "u", Cwd: "/", CreatedAt: time.Now(), LastUsedAt: time.Now()})
	s.deleteRecord("x")
	if rec, _ := s.loadRecord("x"); rec != nil {
		t.Fatal("record survived delete")
	}
	s.deleteRecord("x") // must not error
}

func TestSessionStoreTouchLastUsed(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	old := time.Now().Add(-30 * time.Minute)
	_ = s.storeRecord(&sessionRecord{ExternalID: "x", AcpSessionID: "u", Cwd: "/", CreatedAt: old, LastUsedAt: old})
	fresh := time.Now()
	if err := s.touchLastUsed("x", fresh); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.loadRecord("x")
	if !rec.LastUsedAt.Equal(fresh) {
		t.Fatalf("last_used_at = %v, want %v", rec.LastUsedAt, fresh)
	}
	if !rec.CreatedAt.Equal(old) {
		t.Fatalf("created_at should be unchanged: %v, want %v", rec.CreatedAt, old)
	}
}

func TestSessionStoreTouchMissingIsTolerated(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	if err := s.touchLastUsed("nope", time.Now()); err != nil {
		t.Fatalf("touch missing record should not error: %v", err)
	}
}

func TestSessionStorePruneExpired(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	stale := time.Now().Add(-2 * time.Hour)
	fresh := time.Now()
	_ = s.storeRecord(&sessionRecord{ExternalID: "old", AcpSessionID: "u1", Cwd: "/", CreatedAt: stale, LastUsedAt: stale})
	_ = s.storeRecord(&sessionRecord{ExternalID: "new", AcpSessionID: "u2", Cwd: "/", CreatedAt: fresh, LastUsedAt: fresh})
	s.pruneExpired(time.Now())
	if rec, _ := s.loadRecord("old"); rec != nil {
		t.Fatal("expired record was not pruned")
	}
	if rec, _ := s.loadRecord("new"); rec == nil {
		t.Fatal("fresh record was pruned")
	}
}

func TestSessionStorePruneSweepsTmpFiles(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	tmp := filepath.Join(s.dir, "deadbeef.json.tmp")
	if err := os.WriteFile(tmp, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.pruneExpired(time.Now())
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("tmp file should have been swept, stat err=%v", err)
	}
}

func TestSessionStoreCorruptRecordSkipped(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	path := s.pathFor("broken")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadRecord("broken"); err == nil {
		t.Fatal("expected error parsing corrupt record")
	}
	// prune should not crash or remove (kept for inspection); verify no panic.
	s.pruneExpired(time.Now())
}

func TestSessionStoreFilenameIsHashed(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	_ = s.storeRecord(&sessionRecord{ExternalID: "client-id", AcpSessionID: "u", Cwd: "/", CreatedAt: time.Now(), LastUsedAt: time.Now()})
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "client-id.json" {
			t.Fatalf("external id leaked into filename: %s", name)
		}
	}
	// path traversal ids must not escape the directory
	for _, id := range []string{"../../etc/passwd", "a/b/c", "with spaces & %"} {
		p := s.pathFor(id)
		if filepath.Dir(p) != s.dir {
			t.Fatalf("path for %q escaped store dir: %s", id, p)
		}
	}
}

func TestSessionStoreModelRoundTripped(t *testing.T) {
	s := newTestSessionStore(t, time.Hour)
	_ = s.storeRecord(&sessionRecord{ExternalID: "x", AcpSessionID: "u", Cwd: "/", Model: "GLM-5", CreatedAt: time.Now(), LastUsedAt: time.Now()})
	rec, _ := s.loadRecord("x")
	// ensure omitempty model still valid; round-trip value matches
	if rec.Model != "GLM-5" {
		t.Fatalf("model = %q, want GLM-5", rec.Model)
	}
	// no-model record stores fine
	s2 := newTestSessionStore(t, time.Hour)
	_ = s2.storeRecord(&sessionRecord{ExternalID: "y", AcpSessionID: "u", Cwd: "/", CreatedAt: time.Now(), LastUsedAt: time.Now()})
	raw, _ := os.ReadFile(s2.pathFor("y"))
	var probe map[string]any
	_ = json.Unmarshal(raw, &probe)
	if _, ok := probe["model"]; ok {
		t.Fatal("empty model should be omitted, not stored as null")
	}
}

func TestResolveStateDirExplictEmpty(t *testing.T) {
	t.Setenv("TRAE_API_STATE_DIR", "")
	if dir := resolveStateDir(); dir != "" {
		t.Fatalf("explicit empty state dir should be empty, got %q", dir)
	}
}

func TestResolveStateDirExplicitValue(t *testing.T) {
	t.Setenv("TRAE_API_STATE_DIR", "/custom/state")
	if dir := resolveStateDir(); dir != "/custom/state" {
		t.Fatalf("explicit state dir = %q, want /custom/state", dir)
	}
}

func TestDefaultStateDirHonorsXDG(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	t.Setenv("HOME", "/should/not/be/used")
	if dir := defaultStateDir(); dir != filepath.Join(xdg, "trae-api") {
		t.Fatalf("default state dir = %q, want %q", dir, filepath.Join(xdg, "trae-api"))
	}
}

func TestNewSessionStoreDirCreationFailureDisables(t *testing.T) {
	// a path under a file (not a dir) cannot be created as a directory
	parent := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(parent, "sub", "trae-api")
	s := newSessionStore(bad, time.Hour)
	if s.enabled() {
		t.Fatal("store should be disabled when dir creation fails")
	}
}
