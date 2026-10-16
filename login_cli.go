package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

func loginAddress(c config) string {
	h, p, _ := net.SplitHostPort(c.AuthAddr)
	return "http://" + net.JoinHostPort(h, p)
}
func openOfficialBrowser(ctx context.Context, link string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
		args = []string{link}
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", link}
	default:
		name = "xdg-open"
		args = []string{link}
	}
	// No shell, no inherited output containing the authorization URL.
	return exec.CommandContext(ctx, name, args...).Run()
}
func loginCall(ctx context.Context, client *http.Client, c config, path string, body any, proofKey []byte) (map[string]string, error) {
	if len(proofKey) != 32 {
		return nil, errors.New("Auth proof key unavailable")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("invalid login request")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", loginAddress(c)+path, bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("invalid service address")
	}
	req.Header.Set("Content-Type", "application/json")
	var challenge [32]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return nil, errors.New("create Auth challenge failed")
	}
	req.Header.Set("X-Trae-Auth-Challenge", hex.EncodeToString(challenge[:]))

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("local Auth service unavailable")
	}
	defer resp.Body.Close()
	proof, proofErr := hex.DecodeString(resp.Header.Get("X-Trae-Auth-Proof"))
	mac := hmac.New(sha256.New, proofKey)
	mac.Write(challenge[:])
	if proofErr != nil || !hmac.Equal(proof, mac.Sum(nil)) {
		return nil, errors.New("unverified Auth response")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, errors.New("Auth service rejected the login request; check the local Auth address")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("login request failed (HTTP %d)", resp.StatusCode)
	}
	var result map[string]string
	if json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(&result) != nil {
		return nil, errors.New("invalid login response")
	}
	return result, nil
}
func localLoginTarget(c config) bool {
	host, _, err := net.SplitHostPort(c.AuthAddr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	target := net.ParseIP(host)
	return target != nil && target.IsLoopback()
}

// The active writer publishes a fresh identity in its private, locked sidecar.
// Health JSON alone is not proof that an occupied port belongs to this Auth.
var errAuthStarting = errors.New("Auth instance not ready")

func verifyRunningAuth(ctx context.Context, c config) (string, []byte, error) {
	marker, key, err := readAuthInstance()
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", errAuthStarting, err)
	}
	requestedHost, requestedPort, err := net.SplitHostPort(c.AuthAddr)
	if err != nil {
		return "", nil, err
	}
	boundHost, boundPort, _ := net.SplitHostPort(marker.Address)
	if requestedPort != boundPort || requestedHost != "localhost" && !net.ParseIP(requestedHost).Equal(net.ParseIP(boundHost)) {
		return "", nil, errors.New("active writer uses a different Auth address")
	}
	var challenge [32]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return "", nil, err
	}
	client := newHTTPClient(2 * time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+marker.Address+"/healthz", nil)
	if err != nil {
		return "", nil, errors.New("invalid Auth listener address")
	}
	req.Header.Set("X-Trae-Auth-Challenge", hex.EncodeToString(challenge[:]))
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("%w: listener unavailable", errAuthStarting)
	}
	defer resp.Body.Close()
	proof, err := hex.DecodeString(resp.Header.Get("X-Trae-Auth-Proof"))
	mac := hmac.New(sha256.New, key)
	mac.Write(challenge[:])
	if resp.StatusCode != http.StatusOK || err != nil || !hmac.Equal(proof, mac.Sum(nil)) {
		return "", nil, errors.New("occupied port is not the verified Auth service")
	}
	return marker.Address, key, nil
}

func waitForRunningAuth(ctx context.Context, c config) (string, []byte, error) {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		addr, key, err := verifyRunningAuth(ctx, c)
		if err == nil || !errors.Is(err, errAuthStarting) {
			return addr, key, err
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-deadline.C:
			return "", nil, fmt.Errorf("Auth service did not become ready: %w", err)
		case <-ticker.C:
		}
	}
}

func runLogin(parent context.Context, c config, out io.Writer, open func(context.Context, string) error, poll, timeout time.Duration) (resultErr error) {
	if !localLoginTarget(c) {
		return errors.New("login can only connect to a service on this computer")
	}
	auth, err := newAuthService(c, c.AuthAddr)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			boundAddr, proofKey, verifyErr := waitForRunningAuth(parent, c)
			if verifyErr != nil {
				return fmt.Errorf("Auth writer active but service not verified: %w", verifyErr)
			}
			flowConfig := c
			flowConfig.AuthAddr = boundAddr
			return runLoginFlow(parent, flowConfig, out, open, poll, timeout, proofKey)
		}
		return fmt.Errorf("start local Auth: %w", err)
	}
	serviceCtx, stop := context.WithCancel(parent)
	done := make(chan struct{})
	var serviceErr error
	go func() {
		serviceErr = auth.Run(serviceCtx)
		close(done)
		stop()
	}()
	defer func() {
		stop()
		<-done
		if serviceErr != nil && !errors.Is(resultErr, serviceErr) {
			resultErr = errors.Join(resultErr, serviceErr)
		}
	}()
	ready := time.NewTimer(5 * time.Second)
	defer ready.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	var bound string
	for {
		auth.manager.mu.Lock()
		bound = auth.manager.callbackURL
		auth.manager.mu.Unlock()
		if bound != "" {
			break
		}
		select {
		case <-done:
			if errors.Is(serviceErr, syscall.EADDRINUSE) {
				return fmt.Errorf("Auth address occupied by another program: %w", serviceErr)
			}
			return fmt.Errorf("Auth listener failed: %w", serviceErr)
		case <-parent.Done():
			return loginError(parent.Err())
		case <-ready.C:
			return errors.New("Auth listener did not become ready")
		case <-ticker.C:
		}
	}
	flowConfig := c
	callbackURL, err := url.Parse(bound)
	if err != nil || callbackURL.Host == "" {
		return errors.New("Auth callback address unavailable")
	}
	flowConfig.AuthAddr = callbackURL.Host
	if err := runLoginFlow(serviceCtx, flowConfig, out, open, poll, timeout, auth.instanceKey); err != nil {
		return err
	}
	fmt.Fprintln(out, "Auth remains running in this foreground login process for token renewal; Ctrl+C stops it.")
	select {
	case <-parent.Done():
		return nil
	case <-done:
		if serviceErr == nil {
			return errors.New("Auth stopped unexpectedly")
		}
		return serviceErr
	}
}

func runLoginFlow(parent context.Context, c config, out io.Writer, open func(context.Context, string) error, poll, timeout time.Duration, proofKey []byte) error {
	if len(proofKey) != 32 {
		return errors.New("Auth proof key unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	client := newHTTPClient(5 * time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer client.CloseIdleConnections()
	result, err := loginCall(ctx, client, c, "/api/auth/login", map[string]string{}, proofKey)
	if err != nil {
		return loginError(err)
	}
	if result["status"] == "already_logged_in" {
		fmt.Fprintln(out, "Already logged in.")
		return nil
	}
	id, link := result["attempt_id"], result["url"]
	if result["status"] != "pending" || len(id) != 32 {
		return errors.New("invalid login attempt")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = loginCall(cleanup, client, c, "/api/auth/login/cancel", map[string]string{"attempt_id": id}, proofKey)
	}()
	u, e := url.Parse(link)
	official, _ := url.Parse(c.AuthURL)
	if e != nil || u.Scheme != official.Scheme || u.Host != official.Host || u.Path != official.Path || u.User != nil || u.Fragment != "" || u.Query().Get("login_trace_id") != id {
		return errors.New("invalid official authorization link")
	}
	// Bound an opener that may block; timeout/cancellation still cancels the attempt.
	openCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	err = open(openCtx, link)
	stop()
	if err != nil {
		if ctx.Err() != nil {
			return loginError(ctx.Err())
		}
		fmt.Fprintln(out, "Could not open the browser. Open this official authorization link:")
		fmt.Fprintln(out, link)
	} else {
		fmt.Fprintln(out, "Browser authorization opened. Waiting for login; Ctrl+C cancels.")
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return loginError(ctx.Err())
		case <-ticker.C:
			state, e := loginCall(ctx, client, c, "/api/auth/login/status", map[string]string{"attempt_id": id}, proofKey)
			if e != nil {
				return loginError(e)
			}
			switch state["status"] {
			case "completed":
				fmt.Fprintln(out, "Login successful. Auth saved credentials in the user config directory.")
				return nil
			case "durability_uncertain":
				return errors.New("credential is usable now, but disk durability is unconfirmed; repeat login before restarting Auth")
			case "pending":
			case "cancelled":
				return errors.New("login cancelled")
			case "expired":
				return errors.New("login timed out; run trae-api login again")
			case "superseded":
				return errors.New("login replaced by another attempt")
			case "failed":
				return errors.New("browser authorization failed; run trae-api login again")
			default:
				return errors.New("invalid login status")
			}
		}
	}
}
func loginError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("login cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("login timed out; run trae-api login again")
	}
	return err
}
