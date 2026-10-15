package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestTRAE008CLILibraryHelpAndCommands(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		expected string
	}{
		{
			name:     "root help",
			args:     []string{"trae-api", "--help"},
			expected: "api",
		},
		{
			name:     "auth help",
			args:     []string{"trae-api", "auth", "--help"},
			expected: "authorization",
		},
		{
			name:     "api help",
			args:     []string{"trae-api", "api", "--help"},
			expected: "API",
		},
		{
			name:     "login help",
			args:     []string{"trae-api", "login", "--help"},
			expected: "login",
		},
		{
			name:     "version",
			args:     []string{"trae-api", "--version"},
			expected: "trae-api",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newCommand()
			var out, errOut bytes.Buffer
			app.Writer = &out
			app.ErrWriter = &errOut
			if err := app.Run(context.Background(), tc.args); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.expected) || errOut.Len() != 0 {
				t.Fatalf("output=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}
}

func TestTRAE008CLIFlagSourcesAndLibraryDispatch(t *testing.T) {
	t.Setenv("TRAE_API_ADDR", "127.0.0.1:10001")
	t.Setenv("TRAE_API_AUTH_ADDR", "127.0.0.1:10002")
	t.Setenv("TRAE_API_TOKEN", "synthetic-gateway")
	for _, tc := range []struct {
		name     string
		args     []string
		apiAddr  string
		authAddr string
		token    string
	}{
		{
			name:     "api explicit overrides environment",
			args:     []string{"trae-api", "api", "--api-addr", "127.0.0.1:10003"},
			apiAddr:  "127.0.0.1:10003",
			authAddr: "127.0.0.1:8765",
			token:    "synthetic-gateway",
		},
		{
			name:     "auth only its address",
			args:     []string{"trae-api", "auth"},
			apiAddr:  "127.0.0.1:8723",
			authAddr: "127.0.0.1:10002",
		},
		{
			name:     "combined from environment",
			args:     []string{"trae-api"},
			apiAddr:  "127.0.0.1:10001",
			authAddr: "127.0.0.1:10002",
			token:    "synthetic-gateway",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newCommand()
			var got config
			capture := func(_ context.Context, cmd *cli.Command) error {
				got = commandConfig(cmd)
				return nil
			}
			if len(tc.args) == 1 {
				app.Action = capture
			} else {
				app.Command(tc.args[1]).Action = capture
			}
			if err := app.Run(context.Background(), tc.args); err != nil {
				t.Fatal(err)
			}
			if got.Addr != tc.apiAddr || got.AuthAddr != tc.authAddr || got.APIToken != tc.token {
				t.Fatalf("resolved api=%q auth=%q token=%q", got.Addr, got.AuthAddr, got.APIToken)
			}
		})
	}
}
