package main

import (
	"context"
	"fmt"
	"slices"

	"time"

	"github.com/urfave/cli/v3"
)

func apiAddrFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "api-addr",
		Value:   "127.0.0.1:8723",
		Usage:   "API listen address",
		Sources: cli.EnvVars("TRAE_API_ADDR"),
		Local:   true,
	}
}

func authAddrFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "auth-addr",
		Value:   "127.0.0.1:8765",
		Usage:   "Auth loopback listen address",
		Sources: cli.EnvVars("TRAE_API_AUTH_ADDR"),
		Local:   true,
	}
}

func apiTokenFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "api-token",
		Usage:   "Gateway access token (prefer TRAE_API_TOKEN environment variable)",
		Sources: cli.EnvVars("TRAE_API_TOKEN"),
		Local:   true,
	}
}

func commandConfig(cmd *cli.Command) config {
	c := defaultConfig()
	flags := cmd.LocalFlagNames()
	if slices.Contains(flags, "api-addr") {
		c.Addr = cmd.String("api-addr")
	}
	if slices.Contains(flags, "auth-addr") {
		c.AuthAddr = cmd.String("auth-addr")
	}
	if slices.Contains(flags, "api-token") {
		c.APIToken = cmd.String("api-token")
	}
	return c
}

func newCommand() *cli.Command {
	noArgs := func(_ context.Context, cmd *cli.Command) error {
		if cmd.NArg() != 0 {
			return fmt.Errorf("unexpected arguments: %v", cmd.Args().Slice())
		}
		return nil
	}
	return &cli.Command{
		Name:         "trae-api",
		Usage:        "OpenAI-compatible Trae CN gateway and local authorization",
		Version:      version,
		Flags:        []cli.Flag{apiAddrFlag(), authAddrFlag(), apiTokenFlag()},
		ArgValidator: noArgs,
		Action:       func(ctx context.Context, cmd *cli.Command) error { return runCombined(ctx, commandConfig(cmd)) },
		Commands: []*cli.Command{
			{
				Name:   "api",
				Usage:  "Start only the API gateway",
				Flags:  []cli.Flag{apiAddrFlag(), apiTokenFlag()},
				Action: func(ctx context.Context, cmd *cli.Command) error { return runAPIOnly(ctx, commandConfig(cmd)) },
			},
			{
				Name:  "login",
				Usage: "Log in and keep Auth renewal in the foreground, or reuse active Auth",
				Flags: []cli.Flag{authAddrFlag()},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					c := commandConfig(cmd)
					return runLogin(ctx, c, cmd.Writer, openOfficialBrowser, 500*time.Millisecond, 10*time.Minute)
				},
			},
		},
	}
}
