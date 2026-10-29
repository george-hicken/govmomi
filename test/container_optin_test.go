// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// containerCalls start or probe containers; a test file that makes one must opt in.
var containerCalls = map[string]bool{"HasDocker": true, "HasContainerBridge": true, "RunContainer": true}

// optInGuards make a file's container use opt-in: RequireContainerOptIn and the package
// helpers that must call it.
var optInGuards = map[string]bool{"RequireContainerOptIn": true, "requireContainerBridge": true}

func callName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// usesContainers reports whether n calls a container helper or execs docker or podman.
func usesContainers(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		name := callName(c)
		if containerCalls[name] {
			found = true
		}
		if name == "Command" || name == "CommandContext" {
			for _, a := range c.Args[:min(2, len(c.Args))] {
				if l, ok := a.(*ast.BasicLit); ok && l.Kind == token.STRING {
					s, _ := strconv.Unquote(l.Value)
					found = found || s == "docker" || s == "podman"
				}
			}
		}
		return !found
	})
	return found
}

func callsAny(n ast.Node, names map[string]bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && names[callName(c)] {
			found = true
		}
		return !found
	})
	return found
}

// optInViolations lists why a test file's container use is not opt-in: an unguarded file, a
// Test that uses containers itself without a guard, a guard that does not call
// RequireContainerOptIn, or an Example that runs (has an Output comment) and uses containers,
// since an Example cannot skip.
func optInViolations(f *ast.File) []string {
	var v []string
	if usesContainers(f) && !callsAny(f, optInGuards) {
		v = append(v, "uses containers without test.RequireContainerOptIn")
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fn.Name.Name
		if optInGuards[name] && name != "RequireContainerOptIn" && !callsAny(fn.Body, map[string]bool{"RequireContainerOptIn": true}) {
			v = append(v, "guard "+name+" does not call RequireContainerOptIn")
		}
		if strings.HasPrefix(name, "Test") && usesContainers(fn.Body) && !callsAny(fn.Body, optInGuards) {
			v = append(v, "test "+name+" uses containers without test.RequireContainerOptIn")
		}
		if strings.HasPrefix(name, "Example") && usesContainers(fn.Body) && hasOutput(f, fn) {
			v = append(v, "Example "+name+" uses containers and has an Output comment, so it runs and cannot skip")
		}
	}
	return v
}

// hasOutput reports whether fn, an Example, has an Output comment, which makes go test run it.
func hasOutput(f *ast.File, fn *ast.FuncDecl) bool {
	for _, g := range f.Comments {
		if g.Pos() > fn.Body.Lbrace && g.End() < fn.Body.Rbrace+1 && strings.HasPrefix(strings.TrimSpace(g.Text()), "Output:") {
			return true
		}
	}
	return false
}

func parseSrc(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x_test.go", "package p\n\n"+src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestContainerOptInPolicyControls proves the policy fails an unguarded exec, a hollow guard
// and a running container Example, and passes their guarded forms.
func TestContainerOptInPolicyControls(t *testing.T) {
	for src, want := range map[string]int{
		`func TestX(t *testing.T) { exec.Command("docker", "ps").Run() }`:                                                                                   2,
		`func TestX(t *testing.T) { exec.CommandContext(ctx, "podman", "ps").Run() }`:                                                                       2,
		`func TestX(t *testing.T) { if !test.HasDocker() { t.Skip() } }`:                                                                                    2,
		"func TestY(t *testing.T) { test.RequireContainerOptIn(t) }\nfunc TestX(t *testing.T) { exec.Command(\"docker\").Run() }":                           1,
		`func requireContainerBridge(t *testing.T) {}`:                                                                                                      1,
		"func ExampleX() {\n\tsimulator.RunContainer(ctx, c, vm, \"nginx\")\n\t// Output: x\n}\nfunc TestX(t *testing.T) { test.RequireContainerOptIn(t) }": 1,
		`func TestX(t *testing.T) { test.RequireContainerOptIn(t); exec.Command("docker", "ps").Run() }`:                                                    0,
		"func ExampleX() {\n\tsimulator.RunContainer(ctx, c, vm, \"nginx\")\n}\nfunc TestX(t *testing.T) { test.RequireContainerOptIn(t); ExampleX() }":     0,
	} {
		if got := optInViolations(parseSrc(t, src)); len(got) != want {
			t.Errorf("%q: got violations %q, want %d", src, got, want)
		}
	}
}

// TestContainerOptInPolicy fails each test file in the module whose container use is not opt-in.
func TestContainerOptInPolicy(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	guarded := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "vendor" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, v := range optInViolations(f) {
			t.Errorf("%s: %s", rel, v)
		}
		if callsAny(f, map[string]bool{"RequireContainerOptIn": true}) {
			guarded++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if guarded == 0 {
		t.Error("the walk found no guarded container test: it is not scanning the module")
	}
}
