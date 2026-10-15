package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
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
func loginCall(ctx context.Context, client *http.Client, c config, path string, body any) (map[string]string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("invalid login request")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", loginAddress(c)+path, bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("invalid service address")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Auth service is not running or unavailable; start trae-api auth first")
	}
	defer resp.Body.Close()
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
func runLogin(parent context.Context, c config, out io.Writer, open func(context.Context, string) error, poll, timeout time.Duration) error {
	if !localLoginTarget(c) {
		return errors.New("login can only connect to a service on this computer")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	client := newHTTPClient(5 * time.Second)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	result, err := loginCall(ctx, client, c, "/api/auth/login", map[string]string{})
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
		_, _ = loginCall(cleanup, client, c, "/api/auth/login/cancel", map[string]string{"attempt_id": id})
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
			state, e := loginCall(ctx, client, c, "/api/auth/login/status", map[string]string{"attempt_id": id})
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
