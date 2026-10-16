package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const credentialVersion = 1

// apiCredential is intentionally a separate, narrow decoding target. API code
// must never deserialize the refresh token or device signing key.
type apiCredential struct {
	Version   int       `json:"version"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	UserID    string    `json:"user_id"`
	ClientID  string    `json:"client_id"`
	DeviceID  string    `json:"device_id"`
	MachineID string    `json:"machine_id"`
}

type storedCredential struct {
	credential
	Version          int    `json:"version"`
	DevicePrivateKey string `json:"device_private_key"`
}

// The stable writer-lock file also identifies its active Auth listener. It is
// not an API credential; login reads it only to verify an already running Auth.
type authInstance struct {
	Version int    `json:"version"`
	Address string `json:"address"`
	Key     string `json:"key"`
}

// credentialStore owns the sole writer lock for its entire lifetime. Never
// remove the sidecar: replacing its inode would allow two separate lock owners.
type credentialStore struct {
	mu         sync.Mutex
	path       string
	lock       *os.File
	createTemp func(string, string) (*os.File, error)
	syncDir    func(*os.File) error
}

func credentialPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return "", fmt.Errorf("determine user config directory: %w", coalesceError(err))
	}
	return filepath.Join(dir, "trae-api", "auth.json"), nil
}
func coalesceError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("empty config directory")
}

func checkedCredentialDir(dir string, create bool) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) && create {
		if err = os.MkdirAll(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create credential directory: %w", err)
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("credential directory unavailable: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("credential directory is not a private regular directory (0700)")
	}
	return nil
}

func openCredentialStore() (*credentialStore, error) {
	path, err := credentialPath()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err = checkedCredentialDir(dir, true); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open credential writer lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		lock.Close()
		return nil, errors.New("unsafe credential writer lock")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("credential writer already active: %w", err)
	}
	s := &credentialStore{
		path: path,
		lock: lock,
	}
	if _, err = s.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *credentialStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	fd := int(s.lock.Fd())
	err := syscall.Flock(fd, syscall.LOCK_UN)
	closeErr := s.lock.Close()
	s.lock = nil
	if err != nil {
		return err
	}
	return closeErr
}

func (s *credentialStore) PublishAuthInstance(addr string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil, errors.New("credential store closed")
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	data, err := json.Marshal(authInstance{
		Version: 1,
		Address: addr,
		Key:     hex.EncodeToString(key[:]),
	})
	if err != nil {
		return nil, err
	}
	if err := s.lock.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := s.lock.WriteAt(data, 0); err != nil {
		return nil, err
	}
	if err := s.lock.Sync(); err != nil {
		return nil, err
	}
	return key[:], nil
}

func readAuthInstance() (authInstance, []byte, error) {
	path, err := credentialPath()
	if err != nil {
		return authInstance{}, nil, err
	}
	data, err := readCredentialFile(path + ".lock")
	if err != nil {
		return authInstance{}, nil, err
	}
	if len(data) > 512 {
		return authInstance{}, nil, errors.New("invalid Auth instance marker")
	}
	var marker authInstance
	if err := decodeCredential(data, &marker, true); err != nil {
		return authInstance{}, nil, err
	}
	key, err := hex.DecodeString(marker.Key)
	host, port, addressErr := net.SplitHostPort(marker.Address)
	if err != nil || len(key) != 32 || marker.Version != 1 || addressErr != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || port == "0" {
		return authInstance{}, nil, errors.New("invalid Auth instance marker")
	}
	return marker, key, nil
}

// readCredentialFile checks file metadata on the opened descriptor, so rename
// cannot turn a validated pathname into a different snapshot during decoding.
func readCredentialFile(path string) ([]byte, error) {
	if err := checkedCredentialDir(filepath.Dir(path), false); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("unsafe credential file type or mode")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("credential file too large")
	}
	return data, nil
}

func decodeCredential(data []byte, out any, strict bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid credential JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("trailing credential JSON")
	}
	return nil
}

func validateAPI(c apiCredential) error {
	if c.Version != credentialVersion {
		return fmt.Errorf("unsupported credential version %d", c.Version)
	}
	if c.Token == "" || c.UserID == "" || c.ClientID == "" || c.DeviceID == "" || c.MachineID == "" || c.ExpiresAt.IsZero() {
		return errors.New("incomplete credential")
	}
	return nil
}

// readAPICredential reads a fresh snapshot on every call; it never refreshes.
func readAPICredential() (apiCredential, error) {
	path, err := credentialPath()
	if err != nil {
		return apiCredential{}, err
	}
	data, err := readCredentialFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return apiCredential{}, errLoginRequired
	}
	if err != nil {
		return apiCredential{}, fmt.Errorf("credential unavailable: %w", err)
	}
	var c apiCredential
	if err = decodeCredential(data, &c, false); err != nil {
		return apiCredential{}, err
	}
	if err = validateAPI(c); err != nil {
		return apiCredential{}, err
	}
	if !time.Now().Before(c.ExpiresAt) {
		return apiCredential{}, errLoginRequired
	}
	return c, nil
}

// Load is Auth-only and reads the full state, including refresh and signing key.
func (s *credentialStore) Load() (credential, error) {
	data, err := readCredentialFile(s.path)
	if err != nil {
		return credential{}, err
	}
	var disk storedCredential
	if err = decodeCredential(data, &disk, true); err != nil {
		return credential{}, err
	}
	if err = validateAPI(apiCredential{
		Version:   disk.Version,
		Token:     disk.Token,
		ExpiresAt: disk.ExpiresAt,
		UserID:    disk.UserID,
		ClientID:  disk.ClientID,
		DeviceID:  disk.DeviceID,
		MachineID: disk.MachineID,
	}); err != nil {
		return credential{}, err
	}
	if disk.RefreshToken == "" || disk.RefreshExpiresAt.IsZero() || disk.DevicePrivateKey == "" {
		return credential{}, errors.New("incomplete Auth credential")
	}
	der, err := base64.StdEncoding.DecodeString(disk.DevicePrivateKey)
	if err != nil {
		return credential{}, errors.New("invalid device private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return credential{}, errors.New("invalid device private key")
	}
	private, ok := key.(*ecdsa.PrivateKey)
	if !ok || private.Curve != elliptic.P256() {
		return credential{}, errors.New("device private key must be P-256")
	}
	disk.DeviceKey = private
	return disk.credential, nil
}

// Save validates both the new state and any existing file before replacement.
// A failed write before rename leaves the previous snapshot untouched.
func (s *credentialStore) Save(c credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return errors.New("credential store closed")
	}
	if err := validateAPI(apiCredential{
		Version:   credentialVersion,
		Token:     c.Token,
		ExpiresAt: c.ExpiresAt,
		UserID:    c.UserID,
		ClientID:  c.ClientID,
		DeviceID:  c.DeviceID,
		MachineID: c.MachineID,
	}); err != nil {
		return err
	}
	if c.RefreshToken == "" || c.RefreshExpiresAt.IsZero() || c.DeviceKey == nil || c.DeviceKey.Curve != elliptic.P256() {
		return errors.New("incomplete Auth credential")
	}
	der, err := x509.MarshalPKCS8PrivateKey(c.DeviceKey)
	if err != nil {
		return errors.New("encode device private key failed")
	}
	disk := storedCredential{
		credential:       c,
		Version:          credentialVersion,
		DevicePrivateKey: base64.StdEncoding.EncodeToString(der),
	}
	data, err := json.Marshal(disk)
	if err != nil {
		return errors.New("encode credential failed")
	}
	dir := filepath.Dir(s.path)
	if err = checkedCredentialDir(dir, false); err != nil {
		return err
	}
	if _, err = s.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("refusing to replace invalid credential: %w", err)
	}
	createTemp := s.createTemp
	if createTemp == nil {
		createTemp = os.CreateTemp
	}
	temp, err := createTemp(dir, ".auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), s.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("credential published but directory sync unavailable: %w", err)
	}
	defer d.Close()
	syncDir := s.syncDir
	if syncDir == nil {
		syncDir = (*os.File).Sync
	}
	if err = syncDir(d); err != nil {
		return fmt.Errorf("credential published but directory sync failed: %w", err)
	}
	return nil
}

// QuarantineRejected removes only the rejected current snapshot from the API's
// fixed path, without deleting it or overwriting another user's file. This is
// the fail-closed fallback if publishing its expired replacement fails.
func (s *credentialStore) QuarantineRejected(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return errors.New("credential store closed")
	}
	dir := filepath.Dir(s.path)
	if err := checkedCredentialDir(dir, false); err != nil {
		return err
	}
	current, err := s.Load()
	if err != nil {
		return err
	}
	if current.Token != token {
		return errors.New("credential changed during rejection")
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	destination := s.path + ".rejected-" + hex.EncodeToString(suffix[:])
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("credential quarantine destination exists")
		}
		return err
	}
	if err := os.Rename(s.path, destination); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("rejected credential quarantined but directory sync unavailable: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("rejected credential quarantined but directory sync failed: %w", err)
	}
	return nil
}
