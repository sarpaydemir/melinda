package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// loggerMethods are the slog.Logger methods whose string arguments name attributes or groups.
var loggerMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "With": true, "WithGroup": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
}

// TestLogCallsDoNotUseMaskedAttributeNames keeps non-secret values from being logged as [REDACTED]: no log call in
// the module names an attribute or group with a masked name.
func TestLogCallsDoNotUseMaskedAttributeNames(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root: %v", err)
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !logs(call) {
				return true
			}
			for _, arg := range call.Args {
				if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if name, _ := strconv.Unquote(lit.Value); sensitive(name) {
						t.Errorf("%s: %q is a masked attribute name", fset.Position(lit.Pos()), name)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// logs reports whether call is to a slog function or a logger method.
func logs(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "slog" {
		return true
	}
	return loggerMethods[sel.Sel.Name]
}
