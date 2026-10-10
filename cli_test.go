package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTRAE003LoginCommandParsing(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		code   int
		stop   bool
		output string
	}{
		{[]string{"login", "--help"}, 0, true, "trae-api login"},
		{[]string{"unknown"}, 2, true, ""},
		{[]string{"login", "extra"}, 2, true, ""},
		{[]string{"login"}, 0, false, ""},
	} {
		out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
		code, stop := parseFlags(tc.args, out, errOut)
		if code != tc.code || stop != tc.stop || !strings.Contains(out.String(), tc.output) {
			t.Fatalf("args=%v code=%d stop=%v output=%s", tc.args, code, stop, out)
		}
	}
}
