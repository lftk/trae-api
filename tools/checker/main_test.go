package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inspectFixture(t *testing.T, source string) []finding {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.go")
	if err := os.WriteFile(path, []byte("package fixture\n"+source), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := inspectSources([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStructLayoutChecker(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		wantRule string
	}{
		{
			name:     "multiple names in a definition",
			source:   "type request struct {\n A, B string\n}\n",
			wantRule: "declare one struct field per line",
		},
		{
			name:     "two declaration fields sharing a line",
			source:   "type request struct {\n A string; B int\n}\n",
			wantRule: "multiple struct fields on one line",
		},
		{
			name:     "multiline anonymous declaration ends beside next field",
			source:   "type outer struct {\n A struct {\n X int\n }; B int\n}\n",
			wantRule: "multiple struct fields on one line",
		},
		{
			name:     "inline named literal",
			source:   "type request struct {\n A string\n}\nvar _ = request{A: \"x\"}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:     "nested implicit literal in slice",
			source:   "var _ = []struct {\n A string\n}{{A: \"x\"}}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:     "nested implicit map value",
			source:   "type request struct {\n A string\n}\nvar _ = map[string]request{\"x\": {A: \"y\"}}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:     "nested struct field literal",
			source:   "type inner struct {\n A string\n}\ntype outer struct {\n Inner inner\n}\nvar _ = outer{\n Inner: inner{A: \"x\"},\n}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:     "multiline nested literal ends beside next field",
			source:   "type inner struct {\n X int\n}\ntype outer struct {\n A inner\n B int\n}\nvar _ = outer{\n A: inner{\n X: 1,\n }, B: 2,\n}\n",
			wantRule: "multiple struct literal fields on one line",
		},
		{
			name:     "external positional struct literal",
			source:   "import \"image\"\nvar _ = image.Point{1, 2}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:     "pointer slice element with omitted type",
			source:   "type inner struct {\n X int\n}\nvar _ = []*inner{{X: 1}}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:     "pointer map value with omitted type",
			source:   "type inner struct {\n X int\n}\nvar _ = map[string]*inner{\"k\": {X: 1}}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:     "generic struct with omitted type",
			source:   "type box[T any] struct {\n Value T\n}\nvar _ = []box[int]{{Value: 1}}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:   "local map shadows struct name",
			source: "type data struct {\n A int\n B int\n}\nfunc f() {\n type data map[string]int\n _ = data{\"a\": 1, \"b\": 2}\n}\n",
		},
		{
			name:     "local struct shadows map name",
			source:   "type data map[string]int\nfunc f() {\n type data struct {\n A int\n B int\n}\n _ = data{A: 1, B: 2}\n}\n",
			wantRule: "nonempty struct literal must use separate lines",
		},
		{
			name:   "external map alias with identifier keys",
			source: "import \"net/url\"\nvar A, B string\nvar _ = url.Values{A: {\"x\"}, B: {\"y\"}}\n",
		},
		{
			name:   "empty struct, map and slice are not violations",
			source: "type empty struct{}\nvar _ = empty{}\nvar _ = map[string]any{\"x\": 1, \"y\": 2}\nvar _ = []int{1, 2}\n",
		},
		{
			name:   "properly expanded definition and literal",
			source: "type request struct {\n A string\n B int\n}\nvar _ = request{\n A: \"x\",\n B: 2,\n}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := inspectFixture(t, tc.source)
			if tc.wantRule == "" {
				if len(findings) != 0 {
					t.Fatalf("unexpected violations: %+v", findings)
				}
				return
			}
			for _, issue := range findings {
				if strings.Contains(issue.rule, tc.wantRule) {
					return
				}
			}
			t.Fatalf("missing %q in %+v", tc.wantRule, findings)
		})
	}
}

func TestCheckerSeparatesExternalTestPackage(t *testing.T) {
	dir := t.TempDir()
	files := []struct {
		name string
		body string
	}{
		{
			name: "demo.go",
			body: "package demo\ntype item struct {\n ID string\n}\n",
		},
		{
			name: "demo_test.go",
			body: "package demo_test\ntype item struct {\n ID int\n}\n",
		},
	}
	var paths []string
	for _, file := range files {
		path := filepath.Join(dir, file.name)
		if err := os.WriteFile(path, []byte(file.body), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	findings, err := inspectSourcesWithExports(paths, nil, true)
	if err != nil || len(findings) != 0 {
		t.Fatalf("separate packages should type-check without findings: %v %+v", err, findings)
	}
}

func TestCheckerSkipsInactiveBuildTag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/checkerfixture\n\ngo 1.25\n"), 0600); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, "active.go")
	inactive := filepath.Join(dir, "inactive.go")
	tagged := filepath.Join(dir, "tagged.go")
	if err := os.WriteFile(active, []byte("package fixture\ntype item struct {\n ID int\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inactive, []byte("//go:build never\n\npackage fixture\ntype item struct {\n ID string\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tagged, []byte("//go:build custom\n\npackage fixture\ntype tagged struct { A int; B int }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, err := sourcePaths(dir)
	if err != nil || len(paths) != 1 || paths[0] != active {
		t.Fatalf("build-tag filtering failed: paths=%v err=%v", paths, err)
	}
	t.Setenv("GOFLAGS", "-tags=custom")
	paths, err = sourcePaths(dir)
	if err != nil || len(paths) != 2 || paths[0] != active || paths[1] != tagged {
		t.Fatalf("GOFLAGS tag filtering failed: paths=%v err=%v", paths, err)
	}
	findings, err := inspectSourcesWithExports(paths, nil, true)
	if err != nil || len(findings) == 0 {
		t.Fatalf("tagged struct violation was missed: findings=%v err=%v", findings, err)
	}
}
