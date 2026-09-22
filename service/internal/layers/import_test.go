package layers

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const moduleInternal = "github.com/doipster/asset_cracker/service/internal/"

func TestImportDirection(t *testing.T) {
	allowed := map[string][]string{
		"store":             {},
		"pyfloat":           {},
		"config":            {},
		"health":            {},
		"coinbase":          {},
		"candles":           {"coinbase", "store"},
		"kalshi":            {"store"},
		"broker":            {"kalshi"},
		"engine":            {"broker", "kalshi", "pyfloat"},
		"analysis":          {"store"},
		"runner":            {"broker", "coinbase", "engine", "kalshi", "store"},
		"readsurface":       {"store"},
		"web":               {"analysis", "candles", "coinbase", "kalshi", "runner", "store"},
		"app":               {"analysis", "candles", "coinbase", "config", "engine", "health", "kalshi", "runner", "store", "web"},
		"legacy/kalshi15m":  {"pyfloat"},
		"legacy/kalshi15m2": {"pyfloat"},
		"legacy/runner":     {"coinbase", "kalshi", "legacy/kalshi15m", "legacy/kalshi15m2", "store"},
	}
	internalRoot := filepath.Clean(filepath.Join(findServiceRoot(t), "internal"))
	for pkg, allow := range allowed {
		dir := filepath.Join(internalRoot, filepath.FromSlash(pkg))
		allowSet := map[string]bool{}
		for _, a := range allow {
			allowSet[moduleInternal+a] = true
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("%s: %v", pkg, err)
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src := filepath.Join(dir, name)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, src, nil, parser.ImportsOnly)
			if err != nil {
				t.Errorf("%s: %v", src, err)
				continue
			}
			rel := filepath.Join("internal", filepath.FromSlash(pkg), name)
			for _, imp := range f.Imports {
				ip := strings.Trim(imp.Path.Value, `"`)
				if !strings.HasPrefix(ip, moduleInternal) {
					continue
				}
				if strings.Contains(ip, "/legacy") && !strings.HasPrefix(pkg, "legacy/") {
					t.Errorf("%s imports archived code %s", rel, ip)
					continue
				}
				if !allowSet[ip] {
					t.Errorf("%s imports %s, which is not down-stack of %s", rel, ip, pkg)
				}
			}
		}
	}

	// The live binary's main package is wiring only: app, config, the read surface, store.
	cmd := filepath.Join(findServiceRoot(t), "cmd", "assetcracker")
	cmdAllow := map[string]bool{
		moduleInternal + "app":         true,
		moduleInternal + "config":      true,
		moduleInternal + "readsurface": true,
		moduleInternal + "store":       true,
	}
	filepath.WalkDir(cmd, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			return nil
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(ip, moduleInternal) {
				continue
			}
			if !cmdAllow[ip] {
				t.Errorf("cmd/assetcracker imports %s; live main is wiring only", ip)
			}
		}
		return nil
	})
}

func findServiceRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
