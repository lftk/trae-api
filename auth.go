package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errLoginRequired = errors.New("Trae login required; run trae-api login")

type credential struct {
	Token            string            `json:"token"`
	RefreshToken     string            `json:"refresh_token"`
	UserID           string            `json:"user_id"`
	ClientID         string            `json:"client_id"`
	ExpiresAt        time.Time         `json:"expires_at"`
	RefreshExpiresAt time.Time         `json:"refresh_expires_at"`
	DeviceID         string            `json:"device_id"`
	MachineID        string            `json:"machine_id"`
	DeviceKey        *ecdsa.PrivateKey `json:"-"`
}
type loginAttempt struct {
	expires    time.Time
	device     string
	machine    string
	key        *ecdsa.PrivateKey
	generation uint64
}
type loginResult struct {
	Status  string
	Expires time.Time
}
type authManager struct {
	mu           sync.Mutex
	cfg          config
	client       *http.Client
	cred         credential
	generation   uint64
	refreshing   chan struct{}
	refreshErr   error
	pending      map[string]loginAttempt
	callbackURL  string
	latestLogin  string
	loginResults map[string]loginResult
	refreshedAt  time.Time
}

func newHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}
func newAuthManager(c config) (*authManager, error) {
	return &authManager{
		cfg:          c,
		client:       newHTTPClient(30 * time.Second),
		pending:      map[string]loginAttempt{},
		loginResults: map[string]loginResult{},
	}, nil
}
func (a *authManager) snapshot() (credential, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cred, a.generation
}
func (a *authManager) validGeneration(g uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.generation == g && a.cred.Token != ""
}
func (a *authManager) get(ctx context.Context) (credential, uint64, error) {
	for {
		a.mu.Lock()
		if a.cred.Token == "" || a.refreshErr != nil {
			a.mu.Unlock()
			return credential{}, 0, errLoginRequired
		}
		if a.cred.ExpiresAt.IsZero() || time.Until(a.cred.ExpiresAt) > 2*time.Minute || (time.Until(a.cred.ExpiresAt) > 0 && time.Since(a.refreshedAt) < time.Minute) {
			c, g := a.cred, a.generation
			a.mu.Unlock()
			return c, g, nil
		}
		if a.refreshing != nil {
			done := a.refreshing
			a.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return credential{}, 0, ctx.Err()
			}
		}
		old, g := a.cred, a.generation
		done := make(chan struct{})
		a.refreshing = done
		a.mu.Unlock()
		fresh, e := a.exchange(ctx, old)
		a.mu.Lock()
		changed := a.generation != g
		usable := false
		if !changed {
			if e == nil {
				a.cred = fresh
				a.refreshedAt = time.Now()
			} else if ctx.Err() == nil {
				if errors.Is(e, errLoginRequired) {
					a.refreshErr = e
				} else if !old.ExpiresAt.IsZero() && time.Now().Before(old.ExpiresAt) {
					// Proactive refresh is best effort. Back off briefly without
					// poisoning a token that the upstream has not rejected.
					a.refreshedAt = time.Now()
					usable = true
				}
			}
		}
		a.refreshing = nil
		close(done)
		a.mu.Unlock()
		if changed {
			continue
		}
		if e != nil {
			if ctx.Err() != nil {
				return credential{}, 0, ctx.Err()
			}
			if usable {
				return old, g, nil
			}
			return credential{}, 0, e
		}
	}
}
func (a *authManager) invalidate(g uint64, rejectedToken string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.generation == g && a.cred.Token == rejectedToken {
		a.refreshErr = errLoginRequired
	}
}
func (a *authManager) logout() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cred = credential{}
	a.generation++
	a.refreshErr = nil
	a.pending = map[string]loginAttempt{}
	a.latestLogin = ""
	return nil
}
func (a *authManager) beginLogin() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.callbackURL == "" {
		return "", fmt.Errorf("authorization callback listener is unavailable")
	}
	now := time.Now()
	for k, p := range a.pending {
		if now.After(p.expires) {
			delete(a.pending, k)
		}
	}
	for id, result := range a.loginResults {
		if now.After(result.Expires) {
			delete(a.loginResults, id)
			continue
		}
		if result.Status == "pending" {
			result.Status = "superseded"
			a.loginResults[id] = result
		}
	}
	if len(a.loginResults) >= 128 {
		var oldest string
		var expiry time.Time
		for id, result := range a.loginResults {
			if oldest == "" || result.Expires.Before(expiry) {
				oldest = id
				expiry = result.Expires
			}
		}
		delete(a.loginResults, oldest)
	}
	// A new attempt replaces old attempts; only the latest authorization may win.
	a.pending = map[string]loginAttempt{}
	state := newID("")
	key, err := newDeviceKey()
	if err != nil {
		return "", fmt.Errorf("device key generation failed")
	}
	device, err := newDeviceID()
	if err != nil {
		return "", fmt.Errorf("device ID generation failed")
	}
	machine := make([]byte, 16)
	if _, err = rand.Read(machine); err != nil {
		return "", fmt.Errorf("machine ID generation failed")
	}
	p := loginAttempt{
		expires:    now.Add(10 * time.Minute),
		device:     device,
		machine:    fmt.Sprintf("%x", machine),
		key:        key,
		generation: a.generation,
	}
	a.pending[state] = p
	a.latestLogin = state
	a.loginResults[state] = loginResult{
		Status:  "pending",
		Expires: p.expires,
	}
	u, _ := url.Parse(a.cfg.AuthURL)
	q := u.Query()
	values := map[string]string{
		"login_version":     "1",
		"auth_from":         "solo",
		"login_channel":     "native_ide",
		"plugin_version":    "2.3.24254",
		"auth_type":         "local",
		"client_id":         a.cfg.ClientID,
		"redirect":          "0",
		"login_trace_id":    state,
		"auth_callback_url": a.callbackURL,
		"machine_id":        p.machine,
		"device_id":         p.device,
		"x_device_id":       p.device,
		"x_machine_id":      p.machine,
		"x_device_brand":    "ASUS TUF Gaming A15 FA507RM_FA507RM",
		"x_device_type":     "windows",
		"x_os_version":      "Windows 10 Pro",
		"x_env":             "",
		"x_app_version":     a.cfg.IDEVersion,
		"x_app_type":        "stable",
		"hide_saas_login":   "true",
	}
	for k, v := range values {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
func (a *authManager) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fail := func(status int, text string) {
		w.WriteHeader(status)
		fmt.Fprint(w, "<!doctype html><meta charset=utf-8><title>Trae 登录</title><p>"+text+"</p>")
	}
	if len(r.URL.RawQuery) > 65536 {
		fail(400, "授权回调过大。")
		return
	}
	q := r.URL.Query()
	state := q.Get("loginTraceID")
	if state == "" {
		state = q.Get("login_trace_id")
	}
	a.mu.Lock()
	p, ok := a.pending[state]
	result := a.loginResults[state]
	ok = ok && result.Status == "pending" && time.Now().Before(result.Expires)
	if ok {
		delete(a.pending, state)
	}
	a.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		fail(400, "授权已失效或已使用，请重新发起登录。")
		return
	}
	defer a.finishLogin(state)
	jwt := objectValue(q.Get("userJwt"))
	info := objectValue(q.Get("userInfo"))
	region := firstString(info, "Region", "region")
	if region == "" {
		region = q.Get("region")
	}
	if region != "" && !strings.EqualFold(region, "CN") {
		fail(400, "仅支持国内版 Trae CN 账号。")
		return
	}
	c := credential{
		Token:        firstString(jwt, "Token", "token"),
		RefreshToken: firstString(jwt, "RefreshToken", "refreshToken"),
		UserID:       firstString(info, "UserID", "UserId", "userId", "userID"),
		ClientID:     firstString(jwt, "ClientID", "clientId"),
		DeviceID:     p.device,
		MachineID:    p.machine,
		DeviceKey:    p.key,
	}
	if c.ClientID == "" {
		c.ClientID = q.Get("clientID")
	}
	if c.ClientID == "" {
		c.ClientID = a.cfg.ClientID
	}
	if c.RefreshToken == "" {
		c.RefreshToken = q.Get("refreshToken")
	}
	if c.RefreshToken == "" {
		c.RefreshToken = q.Get("data")
	}
	if c.UserID == "" {
		c.UserID = q.Get("userId")
	}
	c.ExpiresAt = parseExpiry(jwt["TokenExpireAt"])
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = parseExpiry(jwt["tokenExpireAt"])
	}
	c.RefreshExpiresAt = parseExpiry(jwt["RefreshExpireAt"])
	if c.RefreshExpiresAt.IsZero() {
		c.RefreshExpiresAt = parseExpiry(jwt["refreshExpireAt"])
	}
	if c.RefreshExpiresAt.IsZero() {
		c.RefreshExpiresAt = parseExpiry(q.Get("refreshExpireAt"))
	}
	// Ignore callback-provided hosts: credentials only go to the configured CN auth origin.
	if c.Token == "" {
		var e error
		c, e = a.exchange(r.Context(), c)
		if e != nil {
			fail(400, "未收到有效凭证，令牌兑换失败，请重新登录。")
			return
		}
	}
	if c.UserID == "" {
		claims := jwtClaims(c.Token)
		c.UserID = firstString(claims, "user_id", "userId", "UserID", "sub")
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = parseExpiry(jwtClaims(c.Token)["exp"])
	}
	if c.Token == "" || c.UserID == "" {
		fail(400, "授权缺少账号信息，请重新登录。")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result = a.loginResults[state]
	if p.generation != a.generation || a.latestLogin != state || result.Status != "pending" || !time.Now().Before(result.Expires) {
		fail(400, "登录状态已改变，请重新登录。")
		return
	}
	a.cred = c
	a.generation++
	a.refreshErr = nil
	result = a.loginResults[state]
	result.Status = "completed"
	a.loginResults[state] = result
	fmt.Fprint(w, "<!doctype html><html lang=zh-CN><meta charset=utf-8><title>Trae 登录成功</title><p>登录成功，可以关闭此页面并返回 trae-api。</p></html>")
}
func newDeviceID() (string, error) {
	var out strings.Builder
	for i := 0; i < 19; i++ {
		base := int64(10)
		if i == 0 {
			base = 9
		}
		n, err := rand.Int(rand.Reader, big.NewInt(base))
		if err != nil {
			return "", err
		}
		d := n.Int64()
		if i == 0 {
			d++
		}
		out.WriteByte(byte('0' + d))
	}
	return out.String(), nil
}
func newDeviceKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

const deviceExchangePath = "/trae/api/v3/oauth/ExchangeToken"

func (a *authManager) exchange(ctx context.Context, c credential) (credential, error) {
	if c.RefreshToken == "" || (!c.RefreshExpiresAt.IsZero() && time.Now().After(c.RefreshExpiresAt)) {
		return c, errLoginRequired
	}
	if c.DeviceKey != nil || c.DeviceID != "" || c.MachineID != "" {
		if c.DeviceKey == nil || c.DeviceID == "" || c.MachineID == "" {
			return c, errLoginRequired
		}
		return a.boundExchange(ctx, c)
	}
	return a.legacyExchange(ctx, c)
}
func (a *authManager) boundExchange(ctx context.Context, c credential) (credential, error) {
	pub, err := x509.MarshalPKIXPublicKey(&c.DeviceKey.PublicKey)
	if err != nil {
		return c, fmt.Errorf("device public key encoding failed")
	}
	clients := []struct {
		id       string
		platform string
		version  string
	}{{
		"en1oxy7wnw8j9n",
		"SOLO_PC",
		"0.1.64",
	}, {
		"ono9krqynydwx5",
		"IDE_PC",
		"3.3.99",
	}}
	var last error
	for _, as := range clients {
		nonce := make([]byte, 16)
		if _, err = rand.Read(nonce); err != nil {
			return c, fmt.Errorf("device nonce generation failed")
		}
		at := time.Now().Unix()
		hexNonce := fmt.Sprintf("%x", nonce)
		text := fmt.Sprintf("POST\n%s\n%s\n%s\n%d\n%s", deviceExchangePath, as.id, c.RefreshToken, at, hexNonce)
		digest := sha256.Sum256([]byte(text))
		sig, e := ecdsa.SignASN1(rand.Reader, c.DeviceKey, digest[:])
		if e != nil {
			return c, fmt.Errorf("device signing failed")
		}
		body := map[string]any{"ClientID": as.id, "ClientSecret": "", "RefreshToken": c.RefreshToken, "IDEVersion": as.version,
			"DeviceProof": map[string]any{
				"Signature": base64.StdEncoding.EncodeToString(sig),
				"Timestamp": at,
				"Nonce":     hexNonce,
			},
			"DeviceInfo": map[string]any{"DeviceID": c.DeviceID, "MachineID": c.MachineID, "PlatformCode": as.platform, "DeviceType": "PC", "DeviceName": "", "DeviceModel": "", "ClientVersion": as.version, "DevicePublicKey": string(pem.EncodeToMemory(&pem.Block{
				Type:  "PUBLIC KEY",
				Bytes: pub,
			})), "DeviceBrand": "ASUS TUF Gaming A15 FA507RM_FA507RM", "DeviceCPU": "AMD", "OSInfo": "Windows", "OSVersion": "10.0.22631"}}
		req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.cfg.AuthBaseURL, "/")+deviceExchangePath, bytes.NewBufferString(jsonText(body)))
		if e != nil {
			return c, e
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		resp, e := a.client.Do(req)
		if e != nil {
			if ctx.Err() != nil {
				return c, ctx.Err()
			}
			// The server may already have spent the refresh token before the
			// connection failed. Never send it again to another client.
			return c, fmt.Errorf("device exchange connection failed")
		}
		fresh, parseErr := decodeExchange(resp, c)
		if parseErr == nil {
			return fresh, nil
		}
		last = parseErr
		if resp.StatusCode != 401 && resp.StatusCode != 403 {
			return c, parseErr
		}
	}
	return c, last
}
func (a *authManager) legacyExchange(ctx context.Context, c credential) (credential, error) {
	payload := map[string]string{
		"ClientID":     c.ClientID,
		"RefreshToken": c.RefreshToken,
		"ClientSecret": "-",
		"UserID":       c.UserID,
	}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.cfg.AuthBaseURL, "/")+"/cloudide/api/v3/trae/oauth/ExchangeToken", bytes.NewBufferString(jsonText(payload)))
	if e != nil {
		return c, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, e := a.client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return c, ctx.Err()
		}
		return c, fmt.Errorf("token exchange connection failed")
	}
	return decodeExchange(resp, c)
}
func decodeExchange(resp *http.Response, c credential) (credential, error) {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return c, errLoginRequired
	}
	if resp.StatusCode != 200 {
		return c, fmt.Errorf("token exchange returned HTTP %d", resp.StatusCode)
	}
	var root map[string]any
	if e := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root); e != nil {
		return c, fmt.Errorf("invalid token exchange response")
	}
	data := root
	if m, ok := root["Result"].(map[string]any); ok {
		data = m
	} else if m, ok := root["result"].(map[string]any); ok {
		data = m
	}
	token := firstString(data, "Token", "token", "AccessToken", "accessToken")
	if token == "" {
		return c, errLoginRequired
	}
	c.Token = token
	if v := firstString(data, "RefreshToken", "refreshToken"); v != "" {
		c.RefreshToken = v
	}
	if v := firstString(data, "ClientID", "clientId"); v != "" {
		c.ClientID = v
	}
	c.ExpiresAt = parseExpiry(data["TokenExpireAt"])
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = parseExpiry(data["tokenExpireAt"])
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = parseExpiry(jwtClaims(token)["exp"])
	}
	refreshExpiry := parseExpiry(data["RefreshExpireAt"])
	if refreshExpiry.IsZero() {
		refreshExpiry = parseExpiry(data["refreshExpireAt"])
	}
	if !refreshExpiry.IsZero() {
		c.RefreshExpiresAt = refreshExpiry
	}
	info := objectValue(data["UserInfo"])
	if len(info) == 0 {
		info = objectValue(data["userInfo"])
	}
	if region := firstString(info, "Region", "region"); region != "" && !strings.EqualFold(region, "CN") {
		return c, fmt.Errorf("only CN accounts are supported")
	}
	for _, source := range []map[string]any{info, data} {
		if v := firstString(source, "UserID", "userId"); v != "" {
			if c.UserID != "" && c.UserID != v {
				return c, errLoginRequired
			}
			c.UserID = v
		}
	}
	return c, nil
}
func objectValue(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if s, ok := v.(string); ok {
		var m map[string]any
		_ = json.Unmarshal([]byte(s), &m)
		return m
	}
	return nil
}
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return ""
}
func parseExpiry(v any) time.Time {
	s := fmt.Sprint(v)
	if n, e := strconv.ParseFloat(s, 64); e == nil && n > 0 {
		if n > 1e12 {
			n /= 1000
		}
		return time.Unix(int64(n), 0)
	}
	for _, format := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, e := time.Parse(format, s); e == nil {
			return t
		}
	}
	return time.Time{}
}
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
func (s *server) startAuthListener() (*http.Server, error) {
	listener, e := net.Listen("tcp", s.cfg.AuthAddr)
	if e != nil {
		return nil, e
	}
	s.auth.mu.Lock()
	s.auth.callbackURL = "http://" + listener.Addr().String() + "/authorize"
	s.auth.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		// Browser authorization is a cross-site top-level redirect. Do not apply
		// the management API's same-origin policy to this listener.
		if !localManagementHost(r.Host) {
			writeError(w, http.StatusForbidden, "non-local authorization callback Host denied")
			return
		}
		s.auth.callback(w, r)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    128 << 10,
	}
	go func() { _ = srv.Serve(listener) }()
	return srv, nil
}

// A rejected token may have no usable expiry claim. Only an HTTP authentication
// rejection (before any model output) permits a refresh and one new attempt.
func (a *authManager) refreshRejected(ctx context.Context, token string, g uint64) (credential, uint64, error) {
	a.mu.Lock()
	if a.generation != g {
		a.mu.Unlock()
		return credential{}, 0, errLoginRequired
	}
	if a.cred.Token == token {
		a.cred.ExpiresAt = time.Now().Add(-time.Second)
	}
	a.mu.Unlock()
	c, current, err := a.get(ctx)
	if err != nil {
		return credential{}, 0, err
	}
	// Refresh must never transfer an in-flight request to a newly logged-in account.
	if current != g || !a.validGeneration(g) {
		return credential{}, 0, errLoginRequired
	}
	return c, current, nil
}

func (a *authManager) finishLogin(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result, ok := a.loginResults[id]
	if ok && result.Status == "pending" {
		result.Status = "failed"
		a.loginResults[id] = result
	}
}
func (s *server) startLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) {
		writeError(w, 403, "cross-origin management request denied")
		return
	}
	s.auth.mu.Lock()
	logged := s.auth.cred.Token != "" && s.auth.refreshErr == nil && (s.auth.cred.ExpiresAt.IsZero() || time.Now().Before(s.auth.cred.ExpiresAt))
	s.auth.mu.Unlock()
	if logged {
		writeJSON(w, 200, map[string]string{"status": "already_logged_in"})
		return
	}
	link, err := s.auth.beginLogin()
	if err != nil {
		writeError(w, 503, "authorization callback listener is unavailable")
		return
	}
	u, _ := url.Parse(link)
	writeJSON(w, 200, map[string]string{
		"status":     "pending",
		"url":        link,
		"attempt_id": u.Query().Get("login_trace_id"),
	})
}
func (s *server) cancelLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) {
		writeError(w, 403, "cross-origin management request denied")
		return
	}
	body, _, err := decodeBody(w, r)
	if err != nil {
		writeError(w, 400, "invalid cancellation request")
		return
	}
	id := firstString(objectValue(string(body)), "attempt_id")
	s.auth.mu.Lock()
	result, ok := s.auth.loginResults[id]
	if ok && result.Status == "pending" {
		result.Status = "cancelled"
		s.auth.loginResults[id] = result
		delete(s.auth.pending, id)
		if s.auth.latestLogin == id {
			s.auth.latestLogin = ""
		}
	}
	s.auth.mu.Unlock()
	if !ok {
		writeError(w, 404, "login attempt not found")
		return
	}
	writeJSON(w, 200, map[string]string{"status": result.Status})
}

func (s *server) loginStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) {
		writeError(w, 403, "cross-origin management request denied")
		return
	}
	body, _, err := decodeBody(w, r)
	if err != nil {
		writeError(w, 400, "invalid login status request")
		return
	}
	id := firstString(objectValue(string(body)), "attempt_id")
	s.auth.mu.Lock()
	result, ok := s.auth.loginResults[id]
	if ok && result.Status == "pending" && time.Now().After(result.Expires) {
		result.Status = "expired"
		s.auth.loginResults[id] = result
		delete(s.auth.pending, id)
		if s.auth.latestLogin == id {
			s.auth.latestLogin = ""
		}
	}
	s.auth.mu.Unlock()
	if !ok {
		writeError(w, 404, "login attempt not found")
		return
	}
	writeJSON(w, 200, map[string]string{"status": result.Status})
}
