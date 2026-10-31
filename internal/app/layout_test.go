package app_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/sarpaydemir/dost"

type goPackage struct {
	ImportPath string
	Doc        string
	GoFiles    []string
	Imports    []string
	Deps       []string
}

// listPackages returns every package in the module that has non-test files, with its direct and transitive
// non-test dependencies.
func listPackages(t *testing.T) []goPackage {
	t.Helper()
	out, err := exec.Command("go", "list", "-json=ImportPath,Doc,GoFiles,Imports,Deps", module+"/...").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	var pkgs []goPackage
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var p goPackage
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		if len(p.GoFiles) > 0 {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs
}

// leafPackages may use only the standard library.
func leafPackages() []string { return local("internal/textfmt", "internal/clock") }

// local qualifies module-relative package paths.
func local(paths ...string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = module + "/" + p
	}
	return out
}

func within(path string, prefixes ...string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func TestPackagesAreDocumented(t *testing.T) {
	for _, p := range listPackages(t) {
		if p.Doc == "" {
			t.Errorf("%s has no package comment", p.ImportPath)
		}
	}
}

func TestStandardLibraryOnlyPackages(t *testing.T) {
	for _, p := range listPackages(t) {
		if !within(p.ImportPath, leafPackages()...) {
			continue
		}
		for _, d := range p.Deps {
			if first, _, _ := strings.Cut(d, "/"); strings.Contains(first, ".") {
				t.Errorf("%s depends on %s; it may use only the standard library", p.ImportPath, d)
			}
		}
	}
}

func TestDependencyDirection(t *testing.T) {
	foundation := local("internal/config", "internal/logging", "internal/prompt")
	clients := local("internal/mattermost", "internal/llm", "internal/integrations", "internal/store")
	domain := local("internal/routing", "internal/memory", "internal/kb", "internal/files", "internal/tools", "internal/automation")
	components := local("internal/gateway", "internal/dispatch", "internal/pipeline")
	above := func(layers ...[]string) []string { return append(concat(layers...), local("internal/app")...) }

	rules := []struct {
		from, except, forbidden []string
		direct                  bool // check direct imports only, not transitive dependencies
	}{
		{from: foundation, forbidden: above(clients, domain, components)},
		{from: clients, forbidden: above(domain, components)},
		{from: domain, forbidden: above(components)},
		{from: local("internal/gateway"), forbidden: local("internal/dispatch", "internal/pipeline")},
		{from: local("internal/dispatch"), forbidden: local("internal/gateway", "internal/pipeline")},
		{from: local("internal/pipeline"), forbidden: local("internal/gateway", "internal/dispatch")},
		{from: local("internal"), forbidden: local("internal/app")},
		{from: []string{module}, except: local("internal/testkit"), forbidden: local("internal/testkit")},
		{from: []string{module}, except: local("internal/store", "internal/testkit"), forbidden: []string{"github.com/jackc/pgx"}, direct: true},
	}

	pkgs := listPackages(t)
	layered := concat(leafPackages(), foundation, clients, domain, components)
	for _, prefix := range layered {
		if !hasPackage(pkgs, prefix) {
			t.Errorf("rule refers to %s, which matches no package", prefix)
		}
	}
	for _, p := range pkgs {
		if !within(p.ImportPath, append(layered, local("cmd/dost", "internal/app", "internal/testkit")...)...) {
			t.Errorf("%s belongs to no layer; add it to a layer in this test", p.ImportPath)
		}
	}
	for _, r := range rules {
		for _, p := range pkgs {
			if !within(p.ImportPath, r.from...) || within(p.ImportPath, r.except...) {
				continue
			}
			deps := p.Deps
			if r.direct {
				deps = p.Imports
			}
			for _, d := range deps {
				if within(d, r.forbidden...) {
					t.Errorf("%s must not depend on %s", p.ImportPath, d)
				}
			}
		}
	}
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func hasPackage(pkgs []goPackage, prefix string) bool {
	for _, p := range pkgs {
		if within(p.ImportPath, prefix) {
			return true
		}
	}
	return false
}
