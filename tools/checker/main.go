// Command checker checks the repository's Go struct layout without formatting code.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

type finding struct {
	file string
	line int
	rule string
}

type inspector struct {
	fileset  *token.FileSet
	types    map[string]ast.Expr
	typeInfo *types.Info
	seen     map[*ast.CompositeLit]bool
	findings []finding
}

func (i *inspector) report(pos token.Pos, rule string) {
	position := i.fileset.Position(pos)
	i.findings = append(i.findings, finding{
		file: position.Filename,
		line: position.Line,
		rule: rule,
	})
}

func (i *inspector) underlying(expr ast.Expr) ast.Expr {
	for n := 0; n < 10; n++ {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
			continue
		case *ast.IndexExpr:
			expr = t.X
			continue
		case *ast.IndexListExpr:
			expr = t.X
			continue
		}
		name, ok := expr.(*ast.Ident)
		if !ok {
			return expr
		}
		resolved, found := i.types[name.Name]
		if !found {
			return expr
		}
		expr = resolved
	}
	return expr
}

func (i *inspector) structFields(expr ast.Expr) *ast.FieldList {
	if t, ok := i.underlying(expr).(*ast.StructType); ok {
		return t.Fields
	}
	return nil
}

func (i *inspector) checkDefinition(t *ast.StructType) {
	fields := t.Fields.List
	if len(fields) == 0 {
		return
	}
	if i.fileset.Position(t.Struct).Line == i.fileset.Position(fields[0].Pos()).Line ||
		i.fileset.Position(fields[len(fields)-1].End()).Line == i.fileset.Position(t.Fields.Closing).Line {
		i.report(t.Struct, "nonempty struct definition must span field lines")
	}
	lastEndLine := 0
	for _, field := range fields {
		line := i.fileset.Position(field.Pos()).Line
		if len(field.Names) > 1 {
			i.report(field.Pos(), "declare one struct field per line")
		}
		if line == lastEndLine {
			i.report(field.Pos(), "multiple struct fields on one line")
		}
		lastEndLine = i.fileset.Position(field.End()).Line
	}
}

func (i *inspector) checkLiteral(lit *ast.CompositeLit, expected ast.Expr) {
	if i.seen[lit] {
		return
	}
	i.seen[lit] = true
	typ := expected
	if lit.Type != nil {
		typ = lit.Type
	}
	underlying := i.underlying(typ)
	fields := i.structFields(typ)
	isStruct := fields != nil
	knownType := false
	if i.typeInfo != nil {
		if value, ok := i.typeInfo.Types[lit]; ok && value.Type != nil {
			knownType = true
			typ := value.Type
			if ptr, ok := typ.(*types.Pointer); ok {
				typ = ptr.Elem()
			}
			_, isStruct = typ.Underlying().(*types.Struct)
		}
	}
	if !knownType && !isStruct {
		// External SDK types cannot be resolved with parser alone. An exported
		// selector with keyed fields is a struct; map/slice syntax is classified
		// by its explicit AST type and never by a comma regex.
		if _, ok := underlying.(*ast.SelectorExpr); ok && len(lit.Elts) > 0 {
			keyed := true
			for _, element := range lit.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					keyed = false
					break
				}
				name, ok := pair.Key.(*ast.Ident)
				if !ok || !ast.IsExported(name.Name) {
					keyed = false
					break
				}
			}
			isStruct = keyed
		}
	}
	if isStruct && len(lit.Elts) > 0 {
		first := i.fileset.Position(lit.Elts[0].Pos()).Line
		last := i.fileset.Position(lit.Elts[len(lit.Elts)-1].End()).Line
		if i.fileset.Position(lit.Lbrace).Line == first || i.fileset.Position(lit.Rbrace).Line == last {
			i.report(lit.Lbrace, "nonempty struct literal must use separate lines")
		}
		previousEnd := 0
		for _, element := range lit.Elts {
			line := i.fileset.Position(element.Pos()).Line
			if previousEnd == line {
				i.report(element.Pos(), "multiple struct literal fields on one line")
			}
			previousEnd = i.fileset.Position(element.End()).Line
		}
	}
	for index, element := range lit.Elts {
		value := element
		var childType ast.Expr
		switch t := underlying.(type) {
		case *ast.ArrayType:
			childType = t.Elt
		case *ast.MapType:
			childType = t.Value
		case *ast.StructType:
			if index < len(t.Fields.List) {
				childType = t.Fields.List[index].Type
			}
		}
		if pair, ok := element.(*ast.KeyValueExpr); ok {
			value = pair.Value
			if fields != nil {
				if key, ok := pair.Key.(*ast.Ident); ok {
					for _, field := range fields.List {
						for _, name := range field.Names {
							if name.Name == key.Name {
								childType = field.Type
							}
						}
					}
				}
			}
		}
		if child, ok := value.(*ast.CompositeLit); ok {
			i.checkLiteral(child, childType)
		}
	}
}

func inspectSources(paths []string) ([]finding, error) {
	return inspectSourcesWithExports(paths, nil, false)
}

func inspectSourcesWithExports(paths []string, exports map[string]string, strict bool) ([]finding, error) {
	i := &inspector{
		fileset: token.NewFileSet(),
		types:   make(map[string]ast.Expr),
		seen:    make(map[*ast.CompositeLit]bool),
	}
	groups := make(map[string][]*ast.File)
	for _, path := range paths {
		file, err := parser.ParseFile(i.fileset, path, nil, parser.AllErrors)
		if err != nil {
			return nil, err
		}
		dir := filepath.Dir(path)
		key := dir + string(filepath.Separator) + file.Name.Name
		groups[key] = append(groups[key], file)
	}
	for key, files := range groups {
		i.types = make(map[string]ast.Expr)
		i.typeInfo = &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
		}
		config := &types.Config{
			Importer: importer.ForCompiler(i.fileset, "gc", func(path string) (io.ReadCloser, error) {
				if export, ok := exports[path]; ok {
					return os.Open(export)
				}
				return nil, fmt.Errorf("missing Go export for %s", path)
			}),
		}
		if exports == nil {
			config.Importer = importer.Default()
		}
		var firstError error
		config.Error = func(err error) {
			if firstError == nil {
				firstError = err
			}
		}
		_, _ = config.Check(key, i.fileset, files, i.typeInfo)
		if strict && firstError != nil {
			return nil, fmt.Errorf("type-check %s: %w", key, firstError)
		}
		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				if spec, ok := node.(*ast.TypeSpec); ok {
					i.types[spec.Name.Name] = spec.Type
				}
				return true
			})
		}
		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.StructType:
					i.checkDefinition(n)
				case *ast.CompositeLit:
					i.checkLiteral(n, n.Type)
				}
				return true
			})
		}
	}
	sort.Slice(i.findings, func(a, b int) bool {
		left, right := i.findings[a], i.findings[b]
		if left.file != right.file {
			return left.file < right.file
		}
		return left.line < right.line
	})
	return i.findings, nil
}

func packageExports(root string) (map[string]string, error) {
	cmd := exec.Command("go", "list", "-test", "-deps", "-export", "-json", "./...")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list exports: %w", err)
	}
	var pkg struct {
		ImportPath string
		Export     string
	}
	exports := make(map[string]string)
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		if err := decoder.Decode(&pkg); err != nil {
			return nil, err
		}
		if pkg.Export != "" {
			exports[pkg.ImportPath] = pkg.Export
		}
	}
	return exports, nil
}

func sourcePaths(root string) ([]string, error) {
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list source files: %w", err)
	}
	seen := make(map[string]bool)
	var paths []string
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var pkg struct {
			Dir          string
			GoFiles      []string
			TestGoFiles  []string
			XTestGoFiles []string
		}
		if err := decoder.Decode(&pkg); err != nil {
			return nil, err
		}
		for _, names := range [][]string{pkg.GoFiles, pkg.TestGoFiles, pkg.XTestGoFiles} {
			for _, name := range names {
				path := filepath.Join(pkg.Dir, name)
				if !seen[path] {
					seen[path] = true
					paths = append(paths, path)
				}
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	paths, err := sourcePaths(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	exports, err := packageExports(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	findings, err := inspectSourcesWithExports(paths, exports, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, f := range findings {
		name := f.file
		if relative, err := filepath.Rel(absRoot, name); err == nil && filepath.IsLocal(relative) {
			name = relative
		}
		fmt.Printf("%s:%d: %s\n", name, f.line, f.rule)
	}
	fmt.Printf("checked %d Go files; %d struct-layout violations\n", len(paths), len(findings))
	if len(findings) > 0 {
		os.Exit(1)
	}
}
