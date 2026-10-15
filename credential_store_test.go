package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolatedCredentialPath(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	p, err := credentialPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, os.Getenv("HOME")) {
		t.Fatalf("path escaped synthetic HOME: %q", p)
	}
	return p
}

func syntheticCredential(t *testing.T, token string) credential {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return credential{
		Token:            token,
		RefreshToken:     "refresh-secret",
		UserID:           "user",
		ClientID:         "client",
		DeviceID:         "device",
		MachineID:        "machine",
		DeviceKey:        key,
		ExpiresAt:        time.Now().Add(time.Hour).UTC(),
		RefreshExpiresAt: time.Now().Add(24 * time.Hour).UTC(),
	}
}

func TestCredentialStoreRoundTripAndPermissions(t *testing.T) {
	path := isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	want := syntheticCredential(t, "access-secret")
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm() != 0700 || file.Mode().Perm() != 0600 {
		t.Fatalf("modes: dir %o file %o", dir.Mode().Perm(), file.Mode().Perm())
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != want.Token || got.RefreshToken != want.RefreshToken || got.UserID != want.UserID || got.DeviceID != want.DeviceID || got.DeviceKey == nil || got.DeviceKey.D.Cmp(want.DeviceKey.D) != 0 {
		t.Fatalf("roundtrip mismatch")
	}
	api, err := readAPICredential()
	if err != nil {
		t.Fatal(err)
	}
	if api.Token != want.Token || api.UserID != want.UserID || api.ClientID != want.ClientID || api.DeviceID != want.DeviceID || api.MachineID != want.MachineID {
		t.Fatal("API snapshot mismatch")
	}
	b, err := json.Marshal(api)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "refresh") || strings.Contains(string(b), "private") {
		t.Fatal("API snapshot contains Auth secrets")
	}
}

func TestCredentialStoreRejectsInvalidAndPreservesExisting(t *testing.T) {
	path := isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	good := syntheticCredential(t, "original")
	if err := store.Save(good); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(credential{
		Token: "incomplete",
	}); err == nil {
		t.Fatal("accepted incomplete credential")
	}
	api, err := readAPICredential()
	if err != nil || api.Token != good.Token {
		t.Fatalf("old snapshot lost: %v", err)
	}
	for _, data := range []string{"{", `{"version":999,"token":"x"}`, `{"version":1,"token":""}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readAPICredential(); err == nil {
			t.Fatalf("API accepted %q", data)
		}
		if _, err := store.Load(); err == nil {
			t.Fatalf("Auth accepted %q", data)
		}
		if err := store.Save(good); err == nil {
			t.Fatalf("overwrote invalid existing %q", data)
		}
	}
}

func TestCredentialStoreMissingExpiredAndUnsafeFiles(t *testing.T) {
	path := isolatedCredentialPath(t)
	if _, err := readAPICredential(); !errors.Is(err, errLoginRequired) {
		t.Fatalf("missing: %v", err)
	}
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	good := syntheticCredential(t, "access")
	if err := store.Save(good); err != nil {
		t.Fatal(err)
	}
	good.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.Save(good); err != nil {
		t.Fatal(err)
	}
	if _, err := readAPICredential(); !errors.Is(err, errLoginRequired) {
		t.Fatalf("expired: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readAPICredential(); err == nil {
		t.Fatal("read unsafe mode")
	}
	if err := store.Save(good); err == nil {
		t.Fatal("wrote over unsafe mode")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := readAPICredential(); err == nil {
		t.Fatal("read symlink")
	}
	if err := store.Save(good); err == nil {
		t.Fatal("wrote over symlink")
	}
}

func TestCredentialStoreRejectsUnsafeDirectoryAndNonregularFile(t *testing.T) {
	path := isolatedCredentialPath(t)
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(path)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := openCredentialStore(); err == nil {
		t.Fatal("accepted symlink app directory")
	}
	if _, err := readAPICredential(); err == nil {
		t.Fatal("read through symlink app directory")
	}
	if err := os.Remove(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := readAPICredential(); err == nil {
		t.Fatal("read nonregular file")
	}
	if err := store.Save(syntheticCredential(t, "new")); err == nil {
		t.Fatal("replaced nonregular file")
	}
}

func TestCredentialStoreRejectsMalformedPrivateKey(t *testing.T) {
	path := isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(syntheticCredential(t, "access")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	state["device_private_key"] = "invalid"
	b, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("loaded malformed key")
	}
	if err := store.Save(syntheticCredential(t, "replacement")); err == nil {
		t.Fatal("replaced malformed key")
	}
}

func TestCredentialStoreExclusiveLock(t *testing.T) {
	isolatedCredentialPath(t)
	first, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	if second, err := openCredentialStore(); err == nil {
		second.Close()
		t.Fatal("second writer acquired lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := openCredentialStore()
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	defer next.Close()
}

// Child uses the inherited synthetic HOME and attempts to acquire the lock.
func TestCredentialStoreLockChild(t *testing.T) {
	if os.Getenv("TRAE_STORE_LOCK_CHILD") != "1" {
		return
	}
	store, err := openCredentialStore()
	if err == nil {
		store.Close()
		t.Fatal("child acquired parent writer lock")
	}
}

func TestCredentialStoreCrossProcessLock(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCredentialStoreLockChild$")
	cmd.Env = append(os.Environ(), "TRAE_STORE_LOCK_CHILD=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v %s", err, output)
	}
}

func TestCredentialStoreAtomicReplacementReaders(t *testing.T) {
	isolatedCredentialPath(t)
	store, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := syntheticCredential(t, "first")
	b := syntheticCredential(t, "second")
	if err := store.Save(a); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			c := a
			if i%2 == 0 {
				c = b
			}
			if e := store.Save(c); e != nil {
				select {
				case errs <- e:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 300; i++ {
		c, e := readAPICredential()
		if e != nil || (c.Token != "first" && c.Token != "second") {
			t.Fatalf("partial snapshot: %v %+v", e, c)
		}
	}
	wg.Wait()
	select {
	case e := <-errs:
		t.Fatal(e)
	default:
	}
}

func TestCredentialRejectedQuarantineHidesAPIWithoutDeletingPrivateFile(t *testing.T) {
	path := isolatedCredentialPath(t)
	s, err := openCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := syntheticCredential(t, "rejected")
	if err := s.Save(old); err != nil {
		t.Fatal(err)
	}
	if err := s.QuarantineRejected(old.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := readAPICredential(); !errors.Is(err, errLoginRequired) {
		t.Fatalf("API still accepted rejected access: %v", err)
	}
	if _, err := s.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected snapshot still published: %v", err)
	}
	backups, err := filepath.Glob(path + ".rejected-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("private backup missing: %d %v", len(backups), err)
	}
	info, err := os.Lstat(backups[0])
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe backup: %v %v", info, err)
	}
	if err := s.Save(syntheticCredential(t, "new-account")); err != nil {
		t.Fatalf("cannot log in again after rejection: %v", err)
	}
}
