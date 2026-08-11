package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// sessionRecord is the persisted mapping from a caller-visible session ID to
// the trae-cli ACP session. It deliberately stores no transcript: trae-cli owns
// the conversation history on disk and resumes it via session/load.
type sessionRecord struct {
	Version      int       `json:"version"`
	ExternalID   string    `json:"external_id"`
	AcpSessionID string    `json:"acp_session_id"`
	Cwd          string    `json:"cwd"`
	Model        string    `json:"model,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastUsedAt   time.Time `json:"last_used_at"`
}

const sessionRecordVersion = 1

// sessionStore persists stable-session mappings to <stateDir>/sessions. When
// stateDir is empty the store is disabled and every method is a no-op.
type sessionStore struct {
	dir         string // "" when disabled
	idleTimeout time.Duration
}

// resolveStateDir returns the configured state directory. An explicitly empty
// TRAE_API_STATE_DIR disables persistence; an unset value falls back to the
// platform default (see defaultStateDir).
func resolveStateDir() string {
	if v, ok := os.LookupEnv("TRAE_API_STATE_DIR"); ok {
		return v
	}
	return defaultStateDir()
}

// defaultStateDir returns the platform state directory used when
// TRAE_API_STATE_DIR is unset.
func defaultStateDir() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		return filepath.Join(base, "trae-api")
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "trae-api")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "trae-api")
	}
	return ""
}

func newSessionStore(stateDir string, idleTimeout time.Duration) *sessionStore {
	s := &sessionStore{idleTimeout: idleTimeout}
	if stateDir == "" {
		return s
	}
	s.dir = filepath.Join(stateDir, "sessions")
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		slog.Warn("create session store directory; persistence disabled", "dir", s.dir, "error", err)
		return &sessionStore{idleTimeout: idleTimeout}
	}
	return s
}

func (s *sessionStore) enabled() bool { return s != nil && s.dir != "" }

func (s *sessionStore) pathFor(externalID string) string {
	sum := sha256.Sum256([]byte(externalID))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}

// loadRecord returns nil (and no error) when the record is absent.
func (s *sessionStore) loadRecord(externalID string) (*sessionRecord, error) {
	if !s.enabled() {
		return nil, nil
	}
	data, err := os.ReadFile(s.pathFor(externalID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec sessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse session record: %w", err)
	}
	if rec.Version != sessionRecordVersion {
		return nil, fmt.Errorf("unsupported session record version %d", rec.Version)
	}
	if rec.ExternalID != externalID {
		slog.Warn("session record external id mismatch", "expected", externalID, "got", rec.ExternalID)
		return nil, nil
	}
	return &rec, nil
}

// storeRecord atomically writes the record, refreshing UpdatedAt. It creates
// the record identified by rec.ExternalID.
func (s *sessionStore) storeRecord(rec *sessionRecord) error {
	if !s.enabled() {
		return nil
	}
	if rec.ExternalID == "" {
		return errors.New("store session record: empty external id")
	}
	rec.Version = sessionRecordVersion
	rec.UpdatedAt = time.Now()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	final := s.pathFor(rec.ExternalID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *sessionStore) deleteRecord(externalID string) {
	if !s.enabled() {
		return
	}
	if err := os.Remove(s.pathFor(externalID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("delete session record", "external_id", externalID, "error", err)
	}
}

// touchLastUsed refreshes LastUsedAt on an existing record. Missing records are
// tolerated (a fresh session that never completed a turn has no mapping yet).
func (s *sessionStore) touchLastUsed(externalID string, lastUsed time.Time) error {
	if !s.enabled() {
		return nil
	}
	rec, err := s.loadRecord(externalID)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	rec.LastUsedAt = lastUsed
	return s.storeRecord(rec)
}

// pruneExpired removes records idle longer than idleTimeout and sweeps stray
// .tmp files. It scans on demand (startup) and is best-effort.
func (s *sessionStore) pruneExpired(now time.Time) {
	if !s.enabled() {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read session store directory", "dir", s.dir, "error", err)
		}
		return
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(s.dir, name)
		if strings.HasSuffix(name, ".tmp") {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("remove stale tmp", "path", path, "error", err)
			}
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			slog.Warn("read session record during prune", "path", path, "error", err)
			continue
		}
		var rec sessionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			slog.Warn("skip corrupt session record", "path", path, "error", err)
			continue
		}
		if now.Sub(rec.LastUsedAt) > s.idleTimeout {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("remove expired session record", "path", path, "error", err)
			}
		}
	}
}
