package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseFlags(t *testing.T) {
	const testVersion = "v9.9.9-test"
	oldVersion := version
	version = testVersion
	defer func() { version = oldVersion }()

	tests := []struct {
		name       string
		args       []string
		wantStop   bool
		wantCode   int
		wantStdout string // substring expected in stdout
		wantStderr string // substring expected in stderr
	}{
		{
			"no args continues",
			nil,
			false,
			0,
			"",
			"",
		},
		{
			"unknown command",
			[]string{"whatever"},
			true,
			2,
			"",
			"unknown command",
		},
		{
			"long version",
			[]string{"--version"},
			true,
			0,
			"trae-api " + testVersion,
			"",
		},
		{
			"short version",
			[]string{"-version"},
			true,
			0,
			"trae-api " + testVersion,
			"",
		},
		{
			"long help",
			[]string{"--help"},
			true,
			0,
			"TRAE_API_ADDR",
			"",
		},
		{
			"short help",
			[]string{"-h"},
			true,
			0,
			"TRAE_API_ADDR",
			"",
		},
		{
			"unknown flag",
			[]string{"--wat"},
			true,
			2,
			"TRAE_API_ADDR",
			"flag provided but not defined: -wat",
		},
		{
			"flag with value",
			[]string{"-x=1"},
			true,
			2,
			"TRAE_API_ADDR",
			"flag provided but not defined: -x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			code, stop := parseFlags(tt.args, stdout, stderr)
			if code != tt.wantCode || stop != tt.wantStop {
				t.Fatalf("parseFlags(%q) = (%d, %v), want (%d, %v)", tt.args, code, stop, tt.wantCode, tt.wantStop)
			}
			if got := stdout.String(); !strings.Contains(got, tt.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", got, tt.wantStdout)
			}
			if got := stderr.String(); !strings.Contains(got, tt.wantStderr) {
				t.Errorf("stderr = %q, want substring %q", got, tt.wantStderr)
			}
		})
	}
}
