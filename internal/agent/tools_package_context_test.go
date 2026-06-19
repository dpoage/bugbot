package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakePackageContextLookup returns a fake onLookup function backed by
// a simple map. Packages in the map return found=true; absent ones return
// found=false. errPkg (if non-empty) forces a lookup error for that package.
func fakePackageContextLookup(summaries map[string]string, errPkg string) func(string) (string, bool, error) {
	return func(pkg string) (string, bool, error) {
		if pkg == errPkg {
			return "", false, errors.New("store error")
		}
		s, ok := summaries[pkg]
		return s, ok, nil
	}
}

func TestPackageContextTool_Hit(t *testing.T) {
	tool := NewPackageContextTool(fakePackageContextLookup(map[string]string{
		"internal/store": "Package store manages SQLite state.",
	}, ""))

	raw, _ := json.Marshal(map[string]string{"pkg": "internal/store"})
	result, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Package store manages SQLite state." {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestPackageContextTool_Miss(t *testing.T) {
	tool := NewPackageContextTool(fakePackageContextLookup(map[string]string{}, ""))

	raw, _ := json.Marshal(map[string]string{"pkg": "internal/store"})
	result, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "no cached summary for internal/store" {
		t.Fatalf("unexpected miss message: %q", result)
	}
}

func TestPackageContextTool_LookupError(t *testing.T) {
	tool := NewPackageContextTool(fakePackageContextLookup(map[string]string{}, "internal/store"))

	raw, _ := json.Marshal(map[string]string{"pkg": "internal/store"})
	_, err := tool.Run(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "store error") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPackageContextTool_EmptyPkg(t *testing.T) {
	tool := NewPackageContextTool(fakePackageContextLookup(map[string]string{}, ""))

	raw, _ := json.Marshal(map[string]string{"pkg": ""})
	_, err := tool.Run(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for empty pkg")
	}
}

func TestPackageContextTool_BadArgs(t *testing.T) {
	tool := NewPackageContextTool(fakePackageContextLookup(map[string]string{}, ""))

	_, err := tool.Run(context.Background(), json.RawMessage(`not-json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestPackageContextTool_FilePathDerivation(t *testing.T) {
	// A file path like "internal/store/cartographer.go" should be resolved
	// to "internal/store" before lookup.
	called := ""
	tool := NewPackageContextTool(func(pkg string) (string, bool, error) {
		called = pkg
		return "store summary", true, nil
	})

	raw, _ := json.Marshal(map[string]string{"pkg": "internal/store/cartographer.go"})
	result, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != "internal/store" {
		t.Fatalf("expected lookup for \"internal/store\", got %q", called)
	}
	if result != "store summary" {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestPackageContextTool_RepoRootRejection(t *testing.T) {
	tool := NewPackageContextTool(fakePackageContextLookup(map[string]string{}, ""))

	// A file in the repo root resolves dir to "." which should be rejected.
	raw, _ := json.Marshal(map[string]string{"pkg": "main.go"})
	_, err := tool.Run(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for repo-root file")
	}
	if !strings.Contains(err.Error(), "repo root") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPackageContextTool_RepoRootDirRejection(t *testing.T) {
	tool := NewPackageContextTool(fakePackageContextLookup(map[string]string{}, ""))

	// A bare "." should also be rejected.
	raw, _ := json.Marshal(map[string]string{"pkg": "."})
	_, err := tool.Run(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for dot pkg")
	}
}

// -- PackageGraphTool tests --

func fakeGraphQuery(graph map[string][]string) func(pkg, direction string) ([]string, []string, error) {
	// graph maps pkgDir -> importerPkgDirs (matching cartography.importers)
	return func(pkg, direction string) ([]string, []string, error) {
		var importerList, importList []string
		if direction == "importers" || direction == "both" {
			importerList = append(importerList, graph[pkg]...)
		}
		if direction == "imports" || direction == "both" {
			// Invert: find packages Y where pkg ∈ graph[Y]
			for candidate, imps := range graph {
				for _, imp := range imps {
					if imp == pkg {
						importList = append(importList, candidate)
						break
					}
				}
			}
		}
		return importerList, importList, nil
	}
}

func TestPackageGraphTool_Importers(t *testing.T) {
	graph := map[string][]string{
		"internal/store": {"internal/funnel", "internal/cli"},
	}
	tool := NewPackageGraphTool(fakeGraphQuery(graph))

	raw, _ := json.Marshal(map[string]string{"pkg": "internal/store", "direction": "importers"})
	result, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "internal/funnel") || !strings.Contains(result, "internal/cli") {
		t.Fatalf("expected importers in result, got: %q", result)
	}
	if strings.Contains(result, "imports (packages") {
		t.Fatalf("direction=importers should not include imports section, got: %q", result)
	}
}

func TestPackageGraphTool_Imports(t *testing.T) {
	// internal/funnel imports internal/store (i.e., internal/store's importers include internal/funnel)
	graph := map[string][]string{
		"internal/store": {"internal/funnel"},
	}
	tool := NewPackageGraphTool(fakeGraphQuery(graph))

	raw, _ := json.Marshal(map[string]string{"pkg": "internal/funnel", "direction": "imports"})
	result, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "internal/store") {
		t.Fatalf("expected internal/store in imports result, got: %q", result)
	}
	if strings.Contains(result, "importers (packages") {
		t.Fatalf("direction=imports should not include importers section, got: %q", result)
	}
}

func TestPackageGraphTool_Both(t *testing.T) {
	graph := map[string][]string{
		"internal/store":  {"internal/funnel"},
		"internal/funnel": {"internal/cli"},
	}
	tool := NewPackageGraphTool(fakeGraphQuery(graph))

	// internal/funnel: importers=[internal/cli], imports=[internal/store]
	raw, _ := json.Marshal(map[string]string{"pkg": "internal/funnel"})
	result, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "importers") || !strings.Contains(result, "imports") {
		t.Fatalf("both sections expected, got: %q", result)
	}
}

func TestPackageGraphTool_UnknownPkg(t *testing.T) {
	tool := NewPackageGraphTool(fakeGraphQuery(map[string][]string{}))

	raw, _ := json.Marshal(map[string]string{"pkg": "internal/nonexistent"})
	result, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("unknown pkg should return empty result, not error: %v", err)
	}
	if strings.Contains(result, "ERROR") {
		t.Fatalf("unexpected error in result: %q", result)
	}
	// Should contain "(none)" for empty lists.
	if !strings.Contains(result, "(none)") {
		t.Fatalf("expected (none) for unknown pkg, got: %q", result)
	}
}

func TestPackageGraphTool_BadArgs(t *testing.T) {
	tool := NewPackageGraphTool(fakeGraphQuery(map[string][]string{}))

	_, err := tool.Run(context.Background(), json.RawMessage(`bad`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestPackageGraphTool_EmptyPkg(t *testing.T) {
	tool := NewPackageGraphTool(fakeGraphQuery(map[string][]string{}))

	raw, _ := json.Marshal(map[string]string{"pkg": ""})
	_, err := tool.Run(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for empty pkg")
	}
}

func TestPackageGraphTool_BadDirection(t *testing.T) {
	tool := NewPackageGraphTool(fakeGraphQuery(map[string][]string{}))

	raw, _ := json.Marshal(map[string]interface{}{"pkg": "internal/store", "direction": "sideways"})
	_, err := tool.Run(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for invalid direction")
	}
}

func TestPackageGraphTool_FilePathDerivation(t *testing.T) {
	called := ""
	tool := NewPackageGraphTool(func(pkg, direction string) ([]string, []string, error) {
		called = pkg
		return nil, nil, nil
	})

	raw, _ := json.Marshal(map[string]string{"pkg": "internal/store/cartographer.go"})
	_, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != "internal/store" {
		t.Fatalf("expected lookup for \"internal/store\", got %q", called)
	}
}

func TestPackageGraphTool_RepoRootRejection(t *testing.T) {
	tool := NewPackageGraphTool(fakeGraphQuery(map[string][]string{}))

	raw, _ := json.Marshal(map[string]string{"pkg": "main.go"})
	_, err := tool.Run(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for repo-root file")
	}
}

func TestPackageContextTool_Def(t *testing.T) {
	tool := NewPackageContextTool(fakePackageContextLookup(map[string]string{}, ""))
	def := tool.Def()
	if def.Name != "get_package_context" {
		t.Fatalf("unexpected tool name: %q", def.Name)
	}
	if def.Parameters == nil {
		t.Fatal("Parameters must not be nil")
	}
}

func TestPackageGraphTool_Def(t *testing.T) {
	tool := NewPackageGraphTool(fakeGraphQuery(map[string][]string{}))
	def := tool.Def()
	if def.Name != "package_graph" {
		t.Fatalf("unexpected tool name: %q", def.Name)
	}
	if def.Parameters == nil {
		t.Fatal("Parameters must not be nil")
	}
}
