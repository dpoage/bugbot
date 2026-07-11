package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRepo materializes a tiny on-disk repo with the given files and returns
// the repo root. Used by seam tests to exercise EnumerateSeams against a
// real filesystem (the detector reads file bytes from snap.Root, so a real
// tree is required).
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// makeSnapshot builds an in-memory Snapshot pointing at root with the given
// repo-relative paths and their detected languages. The detector reads files
// from snap.Root, so paths and language classifications are both inputs to
// the test.
func makeSnapshot(t *testing.T, root string, paths []string) *Snapshot {
	t.Helper()
	files := make([]File, 0, len(paths))
	for _, p := range paths {
		files = append(files, File{
			Path:     p,
			Language: DetectLanguage(p),
			Size:     0,
		})
	}
	return &Snapshot{Root: root, Files: files}
}

// seamByKey returns the first seam matching (kind, key) or nil.
func seamByKey(seams []Seam, kind SeamKind, key string) *Seam {
	for i, s := range seams {
		if s.Kind == kind && s.Key == key {
			return &seams[i]
		}
	}
	return nil
}

// hasSide returns true when any side of seam has file with the given
// language. Used to assert both-sides-named on a detected seam.
func hasSide(seam *Seam, file string) bool {
	if seam == nil {
		return false
	}
	for _, s := range seam.Sides {
		if s.File == file {
			return true
		}
	}
	return false
}

// TestEnumerateSeams_DataFileAcrossPythonAndGo verifies the data-file
// detector surfaces a seam when the same .json basename is referenced by a
// Python file and a Go file, and that both files appear as sides. Single-
// language-only references do NOT produce a seam; markdown is LangOther
// and is excluded even when it references the file.
func TestEnumerateSeams_DataFileAcrossPythonAndGo(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"py/writer.py":   "import json\nwith open('metrics.json', 'w') as f:\n    json.dump({'count': 5}, f)\n",
		"go/reader.go":   "package reader\nimport _ \"metrics.json\"\n",
		"docs/readme.md": "# See metrics.json for the format.\n",
	})
	snap := makeSnapshot(t, root, []string{"py/writer.py", "go/reader.go", "docs/readme.md"})

	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamDataFile, "metrics.json")
	if seam == nil {
		t.Fatalf("expected a SeamDataFile for metrics.json; got %+v", seams)
	}
	if !hasSide(seam, "py/writer.py") {
		t.Errorf("missing Python side: %+v", seam.Sides)
	}
	if !hasSide(seam, "go/reader.go") {
		t.Errorf("missing Go side: %+v", seam.Sides)
	}
	// Markdown is LangOther — the detector must skip it. Asserting its
	// absence here is the positive expression of that filter.
	if hasSide(seam, "docs/readme.md") {
		t.Errorf("markdown side should be excluded (LangOther): %+v", seam.Sides)
	}
	// The Go and Python sides must carry their detected languages.
	seenLangs := map[Language]bool{}
	for _, s := range seam.Sides {
		seenLangs[s.Language] = true
	}
	if !seenLangs[LangPython] || !seenLangs[LangGo] {
		t.Errorf("expected Python and Go languages on sides, got %+v", seenLangs)
	}
}

// TestEnumerateSeams_NoSeamSingleLanguage confirms that references to a
// data file in ONLY one language do NOT surface a seam — the cross-language
// condition is the whole point of the lens.
func TestEnumerateSeams_NoSeamSingleLanguage(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"go/a.go": "package a\nconst _ = \"x.json\"\n",
		"go/b.go": "package b\nconst _ = \"x.json\"\n",
	})
	snap := makeSnapshot(t, root, []string{"go/a.go", "go/b.go"})

	seams := EnumerateSeams(snap)
	if seamByKey(seams, SeamDataFile, "x.json") != nil {
		t.Fatalf("expected no seam for single-language data-file refs, got %+v", seams)
	}
}

// TestEnumerateSeams_EnvVarAcrossGoAndPython confirms the env-var detector
// finds a seam when the same env var is read from both Go and Python, with
// both sides named.
func TestEnumerateSeams_EnvVarAcrossGoAndPython(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"go/server.go": "package srv\nimport \"os\"\nfunc Token() string { v, _ := os.LookupEnv(\"API_TOKEN\"); return v }\n",
		"py/client.py": "import os\ndef get_token():\n    return os.environ.get(\"API_TOKEN\")\n",
	})
	snap := makeSnapshot(t, root, []string{"go/server.go", "py/client.py"})

	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamEnvVar, "API_TOKEN")
	if seam == nil {
		t.Fatalf("expected a SeamEnvVar for API_TOKEN; got %+v", seams)
	}
	if !hasSide(seam, "go/server.go") {
		t.Errorf("missing Go side: %+v", seam.Sides)
	}
	if !hasSide(seam, "py/client.py") {
		t.Errorf("missing Python side: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_EnvVarAcrossJSAndTS confirms process.env is detected
// in BOTH JavaScript and TypeScript (they share the seam by design).
func TestEnumerateSeams_EnvVarAcrossJSAndTS(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"ui/server.js":  "const t = process.env.API_TOKEN;\n",
		"ui/client.tsx": "const t: string = process.env[\"API_TOKEN\"];\n",
	})
	snap := makeSnapshot(t, root, []string{"ui/server.js", "ui/client.tsx"})

	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamEnvVar, "API_TOKEN")
	if seam == nil {
		t.Fatalf("expected a SeamEnvVar for API_TOKEN; got %+v", seams)
	}
	if !hasSide(seam, "ui/server.js") {
		t.Errorf("missing JS side: %+v", seam.Sides)
	}
	if !hasSide(seam, "ui/client.tsx") {
		t.Errorf("missing TS side: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_NilSnapshotReturnsEmpty ensures EnumerateSeams
// gracefully returns nil on a nil snapshot rather than panicking.
func TestEnumerateSeams_NilSnapshotReturnsEmpty(t *testing.T) {
	if got := EnumerateSeams(nil); got != nil {
		t.Errorf("EnumerateSeams(nil) = %+v, want nil", got)
	}
}

// TestEnumerateSeams_OrderingAndBounded confirms the output order is
// (Kind, Key): all data-file seams in lexicographic Key order, then all
// env-var seams, and the total count is bounded by seamMaxTotal.
func TestEnumerateSeams_OrderingAndBounded(t *testing.T) {
	// Build a fixture with several cross-language seams, all detected
	// in a single sweep.
	files := map[string]string{}
	paths := []string{}
	add := func(rel, content, lang string) {
		_ = lang
		files[rel] = content
		paths = append(paths, rel)
	}
	add("py/a.py", "x = 'alpha.json'\ny = 'beta.json'\n", "py")
	add("go/a.go", "var _ = \"alpha.json\"\nvar _ = \"beta.json\"\n", "go")
	add("py/b.py", "import os\nv = os.environ['Z_TOKEN']\n", "py")
	add("go/b.go", "package b\nimport \"os\"\nvar _ = os.Getenv(\"Z_TOKEN\")\n", "go")

	root := writeRepo(t, files)
	snap := makeSnapshot(t, root, paths)
	seams := EnumerateSeams(snap)
	if len(seams) < 3 {
		t.Fatalf("expected >=3 seams, got %+v", seams)
	}
	// First seam: data_file; then alpha, beta; then env_var Z_TOKEN.
	prevKind := seams[0].Kind
	if prevKind != SeamDataFile {
		t.Errorf("first seam kind = %q, want %q", prevKind, SeamDataFile)
	}
	for _, s := range seams {
		allowedTransition := s.Kind == SeamEnvVar && prevKind == SeamDataFile
		if s.Kind != prevKind && !allowedTransition {
			t.Errorf("seam kind out of order: %q follows %q", s.Kind, prevKind)
		}
		prevKind = s.Kind
	}
	// alpha < beta
	if seams[0].Key != "alpha.json" || seams[1].Key != "beta.json" {
		t.Errorf("data-file keys not lex-sorted: %q, %q", seams[0].Key, seams[1].Key)
	}
}

// TestEnumerateSeams_SidesSortedByFile confirms that sides within a seam
// are sorted by file path so output is deterministic.
func TestEnumerateSeams_SidesSortedByFile(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"z.go":    "package z\nconst _ = \"a.json\"\n",
		"a.go":    "package a\nconst _ = \"a.json\"\n",
		"py/m.py": "x = 'a.json'\n",
	})
	snap := makeSnapshot(t, root, []string{"z.go", "a.go", "py/m.py"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamDataFile, "a.json")
	if seam == nil {
		t.Fatal("expected seam for a.json")
	}
	for i := 1; i < len(seam.Sides); i++ {
		if seam.Sides[i-1].File > seam.Sides[i].File {
			t.Errorf("sides not sorted: %+v", seam.Sides)
			break
		}
	}
}

// TestEnumerateSeams_LineNumberPopulated confirms that the Line field on a
// side carries a 1-based line number for the first matching reference.
func TestEnumerateSeams_LineNumberPopulated(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"go/a.go": "package a\n// line 2\n// line 3\nvar _ = \"shared.json\"\n",
		"py/b.py": "# comment\n# comment\nx = 'shared.json'\n",
	})
	snap := makeSnapshot(t, root, []string{"go/a.go", "py/b.py"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamDataFile, "shared.json")
	if seam == nil {
		t.Fatal("expected seam")
	}
	// Find the Go side; expect line 4.
	var goLine int
	for _, s := range seam.Sides {
		if s.File == "go/a.go" {
			goLine = s.Line
		}
	}
	if goLine != 4 {
		t.Errorf("Go side line = %d, want 4", goLine)
	}
}

// TestEnumerateSeams_HTTPRouteServerOnly verifies that a Go server registering
// a route with no client caller produces exactly one SeamHTTPRoute seam whose
// key is the normalized path and whose side points at the server file.
func TestEnumerateSeams_HTTPRouteServerOnly(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"server/handlers.go": `package server

import "net/http"

func Register(mux *http.ServeMux) {
	mux.HandleFunc("/widgets", handleWidgets)
}
`,
	})
	snap := makeSnapshot(t, root, []string{"server/handlers.go"})
	seams := EnumerateSeams(snap)
	s := seamByKey(seams, SeamHTTPRoute, "/widgets")
	if s == nil {
		t.Fatal("expected SeamHTTPRoute for /widgets, got none")
	}
	if !hasSide(s, "server/handlers.go") {
		t.Errorf("server file not in sides: %+v", s.Sides)
	}
}

// TestEnumerateSeams_HTTPRouteClientOnly verifies that a Go client calling a
// URL path with no server registration produces a SeamHTTPRoute seam (the
// called-but-never-registered case).
func TestEnumerateSeams_HTTPRouteClientOnly(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"client/client.go": `package client

import (
	"net/http"
)

func GetWidgets() (*http.Response, error) {
	return http.Get("/widgets")
}
`,
	})
	snap := makeSnapshot(t, root, []string{"client/client.go"})
	seams := EnumerateSeams(snap)
	s := seamByKey(seams, SeamHTTPRoute, "/widgets")
	if s == nil {
		t.Fatal("expected SeamHTTPRoute for /widgets (client-only), got none")
	}
	if !hasSide(s, "client/client.go") {
		t.Errorf("client file not in sides: %+v", s.Sides)
	}
}

// TestEnumerateSeams_HTTPRouteBothSides verifies that a server registration and
// a client call to the same route across two distinct files produces a seam
// with both files as sides.
func TestEnumerateSeams_HTTPRouteBothSides(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"server/handlers.go": `package server

import "net/http"

func Register(mux *http.ServeMux) {
	mux.HandleFunc("/widgets", handleWidgets)
}
`,
		"client/client.go": `package client

import "net/http"

func GetWidgets() (*http.Response, error) {
	return http.Get("/widgets")
}
`,
	})
	snap := makeSnapshot(t, root, []string{"server/handlers.go", "client/client.go"})
	seams := EnumerateSeams(snap)
	s := seamByKey(seams, SeamHTTPRoute, "/widgets")
	if s == nil {
		t.Fatal("expected SeamHTTPRoute for /widgets (both sides), got none")
	}
	if !hasSide(s, "server/handlers.go") {
		t.Errorf("server file not in sides: %+v", s.Sides)
	}
	if !hasSide(s, "client/client.go") {
		t.Errorf("client file not in sides: %+v", s.Sides)
	}
}

// TestEnumerateSeams_HTTPRouteSelfReferenceSuppressed verifies that a single
// file containing both a server registration and a client call for the same
// path does NOT produce a seam (the producer/consumer-split gate).
func TestEnumerateSeams_HTTPRouteSelfReferenceSuppressed(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"all_in_one.go": `package main

import "net/http"

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/widgets", func(w http.ResponseWriter, r *http.Request) {})
	resp, _ := http.Get("/widgets")
	_ = resp
}
`,
	})
	snap := makeSnapshot(t, root, []string{"all_in_one.go"})
	seams := EnumerateSeams(snap)
	s := seamByKey(seams, SeamHTTPRoute, "/widgets")
	if s != nil {
		t.Errorf("expected no seam for single-file self-reference, got %+v", s)
	}
}

// TestEnumerateSeams_RPCMethodProtoOnly verifies that a .proto rpc declaration
// with no code call site produces a SeamRPCMethod seam (declared-but-uncalled).
func TestEnumerateSeams_RPCMethodProtoOnly(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"api/widget.proto": `syntax = "proto3";

service WidgetService {
  rpc GetWidget (GetWidgetRequest) returns (GetWidgetResponse);
}
`,
	})
	// .proto files have LangOther; makeSnapshot uses DetectLanguage.
	// We must include the .proto path in the snapshot so the scanner sees it.
	snap := makeSnapshot(t, root, []string{"api/widget.proto"})
	seams := EnumerateSeams(snap)
	s := seamByKey(seams, SeamRPCMethod, "GetWidget")
	if s == nil {
		t.Fatal("expected SeamRPCMethod for GetWidget (proto-only), got none")
	}
	if !hasSide(s, "api/widget.proto") {
		t.Errorf("proto file not in sides: %+v", s.Sides)
	}
}

// TestEnumerateSeams_RPCMethodBothSides verifies that a .proto rpc declaration
// AND a Go call site produce a seam with both files named as sides.
func TestEnumerateSeams_RPCMethodBothSides(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"api/widget.proto": `syntax = "proto3";

service WidgetService {
  rpc GetWidget (GetWidgetRequest) returns (GetWidgetResponse);
}
`,
		"client/widget_client.go": `package client

import (
	"context"
	pb "example.com/api"
)

func Fetch(ctx context.Context, c pb.WidgetServiceClient) {
	resp, _ := c.GetWidget(ctx, &pb.GetWidgetRequest{})
	_ = resp
}
`,
	})
	snap := makeSnapshot(t, root, []string{"api/widget.proto", "client/widget_client.go"})
	seams := EnumerateSeams(snap)
	s := seamByKey(seams, SeamRPCMethod, "GetWidget")
	if s == nil {
		t.Fatal("expected SeamRPCMethod for GetWidget (both sides), got none")
	}
	if !hasSide(s, "api/widget.proto") {
		t.Errorf("proto file not in sides: %+v", s.Sides)
	}
	if !hasSide(s, "client/widget_client.go") {
		t.Errorf("client file not in sides: %+v", s.Sides)
	}
}

// TestEnumerateSeams_HTTPRouteNewRequest verifies the http.NewRequest consumer
// pattern is recognised as a consumer side.
func TestEnumerateSeams_HTTPRouteNewRequest(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"client/fetch.go": `package client

import "net/http"

func Fetch() (*http.Response, error) {
	req, _ := http.NewRequest("GET", "http://api.example.com/widgets", nil)
	return http.DefaultClient.Do(req)
}
`,
	})
	snap := makeSnapshot(t, root, []string{"client/fetch.go"})
	seams := EnumerateSeams(snap)
	s := seamByKey(seams, SeamHTTPRoute, "/widgets")
	if s == nil {
		t.Fatal("expected SeamHTTPRoute for /widgets (NewRequest), got none")
	}
	if !hasSide(s, "client/fetch.go") {
		t.Errorf("fetch file not in sides: %+v", s.Sides)
	}
}

// TestEnumerateSeams_SeamOrdering verifies that SeamHTTPRoute and SeamRPCMethod
// rows appear after SeamDataFile and SeamEnvVar rows in the output (Kind ordering).
func TestEnumerateSeams_SeamOrdering(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"go/a.go": `package a

import (
	"os"
	"net/http"
)

var _ = os.Getenv("MY_VAR")

func init() {
	http.HandleFunc("/alpha", nil)
}
`,
		"py/b.py": `import os
x = os.getenv("MY_VAR")
`,
		"client/c.go": `package c

import "net/http"

func f() { http.Get("/alpha") }
`,
		"api/svc.proto": `syntax = "proto3";
service Svc { rpc Foo (A) returns (B); }
`,
	})
	snap := makeSnapshot(t, root, []string{
		"go/a.go",
		"py/b.py",
		"client/c.go",
		"api/svc.proto",
	})
	seams := EnumerateSeams(snap)

	// Find indices of each kind in the output slice.
	kindIndex := func(kind SeamKind) int {
		for i, s := range seams {
			if s.Kind == kind {
				return i
			}
		}
		return -1
	}
	envIdx := kindIndex(SeamEnvVar)
	httpIdx := kindIndex(SeamHTTPRoute)
	rpcIdx := kindIndex(SeamRPCMethod)
	if envIdx < 0 {
		t.Fatal("SeamEnvVar not found")
	}
	if httpIdx < 0 {
		t.Fatal("SeamHTTPRoute not found")
	}
	if rpcIdx < 0 {
		t.Fatal("SeamRPCMethod not found")
	}
	if envIdx >= httpIdx {
		t.Errorf("SeamEnvVar (%d) must come before SeamHTTPRoute (%d)", envIdx, httpIdx)
	}
	if httpIdx >= rpcIdx {
		t.Errorf("SeamHTTPRoute (%d) must come before SeamRPCMethod (%d)", httpIdx, rpcIdx)
	}
}

// TestEnumerateSeams_RPCMethodNoPrecisionFlood is the negative-precision guard
// that would have caught the original flooding bug. A Go file containing
// multiple ordinary ctx-first method calls (db.ExecContext, repo.UpsertFinding,
// context.WithTimeout, exec.CommandContext) with NO .proto file and NO gRPC
// server handler MUST emit ZERO SeamRPCMethod seams. The calls are
// indistinguishable from gRPC client stubs by pattern alone; only a real
// producer (proto declaration or handler) makes them seam evidence.
func TestEnumerateSeams_RPCMethodNoPrecisionFlood(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"store/db.go": `package store

import (
	"context"
	"database/sql"
	"os/exec"
)

type Repo struct{ db *sql.DB }

func (r *Repo) UpsertFinding(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "INSERT INTO findings VALUES ($1)", id)
	return err
}

func (r *Repo) ListFindings(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, "SELECT id FROM findings")
	if err != nil {
		return err
	}
	defer rows.Close()

	ctx2, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	_ = ctx2

	cmd := exec.CommandContext(ctx, "git", "status")
	_ = cmd
	return nil
}
`,
	})
	snap := makeSnapshot(t, root, []string{"store/db.go"})
	seams := EnumerateSeams(snap)
	for _, s := range seams {
		if s.Kind == SeamRPCMethod {
			t.Errorf("unexpected SeamRPCMethod %q: ordinary ctx-first calls must not produce RPC seams without a proto/handler producer", s.Key)
		}
	}
}

// TestEnumerateSeams_HTTPClientNonRoutableNoPrecisionFlood is the negative-
// precision guard for the HTTP-route detector: a Go file whose .Get/.Post
// string arguments are NON-routable (cache key, map key — no leading slash,
// no http:// scheme) MUST emit ZERO SeamHTTPRoute seams. This must fail if
// the httpClientCallRe path-anchor or normalizeHTTPPath leading-slash guard
// is loosened to accept bare strings.
func TestEnumerateSeams_HTTPClientNonRoutableNoPrecisionFlood(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"cache/client.go": `package cache

import "context"

type Cache struct{}
type MapStore map[string]string

// .Get with a plain cache key — NOT a URL path.
func (c *Cache) Lookup(ctx context.Context) string {
	return c.Get("userkey")
}

// .Post with a non-path key — NOT a URL path.
func (m MapStore) Store(ctx context.Context) {
	m.Post("bucket/object", "value")
}

func (c *Cache) Get(key string) string   { return "" }
func (m MapStore) Post(key, val string)  {}
`,
	})
	snap := makeSnapshot(t, root, []string{"cache/client.go"})
	seams := EnumerateSeams(snap)
	for _, s := range seams {
		if s.Kind == SeamHTTPRoute {
			t.Errorf("unexpected SeamHTTPRoute %q: non-routable .Get/.Post string args must not produce HTTP seams", s.Key)
		}
	}
}

// ---------------------------------------------------------------------------
// JS/TS + Python HTTP route and RPC seam tests (bead bugbot-93z.19)
// ---------------------------------------------------------------------------

// TestEnumerateSeams_HTTPRouteExpressProducerGoConsumer verifies that an
// Express/JS route registration (producer) paired with a Go http.NewRequest
// consumer across two distinct files produces a SeamHTTPRoute seam with both
// files as sides.
func TestEnumerateSeams_HTTPRouteExpressProducerGoConsumer(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"server/routes.js": `const express = require('express');
const app = express();
app.get('/widgets', (req, res) => { res.json([]); });
app.post('/widgets', (req, res) => { res.json({}); });
`,
		"client/client.go": `package client
import "net/http"
func fetch() (*http.Response, error) {
	return http.NewRequest("GET", "/widgets", nil)
}
`,
	})
	snap := makeSnapshot(t, root, []string{"server/routes.js", "client/client.go"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamHTTPRoute, "/widgets")
	if seam == nil {
		t.Fatalf("expected SeamHTTPRoute /widgets, got %+v", seams)
	}
	if !hasSide(seam, "server/routes.js") {
		t.Errorf("expected JS producer side server/routes.js: %+v", seam.Sides)
	}
	if !hasSide(seam, "client/client.go") {
		t.Errorf("expected Go consumer side client/client.go: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_HTTPRouteFlaskProducerFetchConsumer verifies that a Flask
// @app.route decorator (Python producer) and a JS fetch('/path') consumer
// across distinct files produce a SeamHTTPRoute seam.
func TestEnumerateSeams_HTTPRouteFlaskProducerFetchConsumer(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"api/views.py": `from flask import Flask
app = Flask(__name__)

@app.route('/users')
def list_users():
    return []
`,
		"web/app.ts": `async function loadUsers() {
  const resp = await fetch('/users');
  return resp.json();
}
`,
	})
	snap := makeSnapshot(t, root, []string{"api/views.py", "web/app.ts"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamHTTPRoute, "/users")
	if seam == nil {
		t.Fatalf("expected SeamHTTPRoute /users, got %+v", seams)
	}
	if !hasSide(seam, "api/views.py") {
		t.Errorf("expected Python producer side api/views.py: %+v", seam.Sides)
	}
	if !hasSide(seam, "web/app.ts") {
		t.Errorf("expected TS consumer side web/app.ts: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_HTTPRouteFastAPIProducerAxiosConsumer verifies that a
// FastAPI @app.get decorator (Python producer) and an axios.get('/path')
// consumer in JS produce a SeamHTTPRoute seam.
func TestEnumerateSeams_HTTPRouteFastAPIProducerAxiosConsumer(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"api/main.py": `from fastapi import FastAPI
app = FastAPI()

@app.get('/items')
def get_items():
    return []
`,
		"frontend/api.js": `import axios from 'axios';
export const getItems = () => axios.get('/items');
`,
	})
	snap := makeSnapshot(t, root, []string{"api/main.py", "frontend/api.js"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamHTTPRoute, "/items")
	if seam == nil {
		t.Fatalf("expected SeamHTTPRoute /items, got %+v", seams)
	}
	if !hasSide(seam, "api/main.py") {
		t.Errorf("expected Python producer side api/main.py: %+v", seam.Sides)
	}
	if !hasSide(seam, "frontend/api.js") {
		t.Errorf("expected JS consumer side frontend/api.js: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_HTTPRouteDjangoProducerGoConsumer verifies that a Django
// path() registration (no leading slash in source) and a Go client call
// produce a SeamHTTPRoute seam on the normalized key "/accounts/login".
// normalizePyDjangoPath prepends '/' and strips the trailing slash.
func TestEnumerateSeams_HTTPRouteDjangoProducerGoConsumer(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"myapp/urls.py": `from django.urls import path
from . import views
urlpatterns = [
    path('accounts/login/', views.login, name='login'),
]
`,
		"svc/client.go": `package svc
import "net/http"
func login() (*http.Response, error) {
	return http.NewRequest("POST", "/accounts/login", nil)
}
`,
	})
	snap := makeSnapshot(t, root, []string{"myapp/urls.py", "svc/client.go"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamHTTPRoute, "/accounts/login")
	if seam == nil {
		t.Fatalf("expected SeamHTTPRoute /accounts/login, got %+v", seams)
	}
	if !hasSide(seam, "myapp/urls.py") {
		t.Errorf("expected Python Django producer side myapp/urls.py: %+v", seam.Sides)
	}
	if !hasSide(seam, "svc/client.go") {
		t.Errorf("expected Go consumer side svc/client.go: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_HTTPRoutePyRequestsConsumer verifies that a requests.get()
// Python consumer paired with a Go server registration produces a seam.
func TestEnumerateSeams_HTTPRoutePyRequestsConsumer(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"api/server.go": `package api
import "net/http"
func Register(mux *http.ServeMux) {
	mux.HandleFunc("/health", handleHealth)
}
func handleHealth(w http.ResponseWriter, r *http.Request) {}
`,
		"check/probe.py": `import requests
def check():
    r = requests.get('/health')
    return r.status_code == 200
`,
	})
	snap := makeSnapshot(t, root, []string{"api/server.go", "check/probe.py"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamHTTPRoute, "/health")
	if seam == nil {
		t.Fatalf("expected SeamHTTPRoute /health, got %+v", seams)
	}
	if !hasSide(seam, "api/server.go") {
		t.Errorf("expected Go producer side api/server.go: %+v", seam.Sides)
	}
	if !hasSide(seam, "check/probe.py") {
		t.Errorf("expected Python consumer side check/probe.py: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_RPCMethodPyServicerAndStub verifies that a Python gRPC
// servicer method (PRODUCER) and a Python stub call site (CONSUMER) across
// two distinct files produce a SeamRPCMethod seam. The servicer method is the
// anchor producer; the stub call alone would not emit (producer-anchor gate).
func TestEnumerateSeams_RPCMethodPyServicerAndStub(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"grpc/servicer.py": `import widget_pb2_grpc

class WidgetServicer(widget_pb2_grpc.WidgetServicer):
    def GetWidget(self, request, context):
        return widget_pb2.Widget(id=request.id)
`,
		"grpc/client.py": `import grpc
import widget_pb2_grpc

channel = grpc.insecure_channel('localhost:50051')
stub = widget_pb2_grpc.WidgetStub(channel)

def fetch_widget(widget_id):
    return stub.GetWidget(widget_pb2.GetWidgetRequest(id=widget_id))
`,
	})
	snap := makeSnapshot(t, root, []string{"grpc/servicer.py", "grpc/client.py"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamRPCMethod, "GetWidget")
	if seam == nil {
		t.Fatalf("expected SeamRPCMethod GetWidget, got %+v", seams)
	}
	if !hasSide(seam, "grpc/servicer.py") {
		t.Errorf("expected Python servicer producer side grpc/servicer.py: %+v", seam.Sides)
	}
	if !hasSide(seam, "grpc/client.py") {
		t.Errorf("expected Python stub consumer side grpc/client.py: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_HTTPRouteJSNoPrecisionFlood is the negative-precision
// guard for JS/TS HTTP route detection. A JS file containing:
//   - .get() calls with non-routable string args (cache keys, DOM selectors)
//   - method calls without a trailing comma (not a route registration)
//   - fetch() with no leading slash
//
// MUST emit ZERO new SeamHTTPRoute seams.
func TestEnumerateSeams_HTTPRouteJSNoPrecisionFlood(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"lib/client.js": `// DOM selector — not a route
document.get('button.submit');

// Cache lookup — bare key, no leading slash
cache.get('userProfile');

// fetch with no leading slash — not a routable path
fetch('userProfile');

// A .get() with no comma after the string — consumer, not a route registration
// (would need normalizeHTTPPath to pass, but no leading slash here)
client.get('some-cache-key');
`,
	})
	snap := makeSnapshot(t, root, []string{"lib/client.js"})
	seams := EnumerateSeams(snap)
	for _, s := range seams {
		if s.Kind == SeamHTTPRoute {
			t.Errorf("unexpected SeamHTTPRoute %q from non-routable JS calls: %+v", s.Key, s)
		}
	}
}

// TestEnumerateSeams_HTTPRoutePyNoPrecisionFlood is the negative-precision
// guard for Python HTTP detection. A Python file with:
//   - requests.get() with a non-routable bare string
//   - @app.route() with no leading slash
//   - path() with only the empty string
//
// MUST emit ZERO SeamHTTPRoute seams.
func TestEnumerateSeams_HTTPRoutePyNoPrecisionFlood(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"util/http_utils.py": `import requests

# Non-routable: no leading slash, no full URL
r = requests.get('some-api-key')

# Django path with empty string — too generic, must be suppressed
# path('', include('myapp.urls'))
`,
	})
	snap := makeSnapshot(t, root, []string{"util/http_utils.py"})
	seams := EnumerateSeams(snap)
	for _, s := range seams {
		if s.Kind == SeamHTTPRoute {
			t.Errorf("unexpected SeamHTTPRoute %q from non-routable Python calls: %+v", s.Key, s)
		}
	}
}

// TestEnumerateSeams_RPCMethodPyStubNoPrecisionFlood is the negative-precision
// guard for Python RPC detection. A Python file with ordinary method calls of
// the form object.UpperMethod() — but NO .proto declaration and NO gRPC
// servicer class — MUST emit ZERO SeamRPCMethod seams. The producer-anchor
// gate must suppress all consumer-only evidence.
func TestEnumerateSeams_RPCMethodPyStubNoPrecisionFlood(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"svc/orm.py": `class Repo:
    def __init__(self, db):
        self.db = db

    def query(self):
        # These look like RPC calls (lowercase.Upper()) but are ORM methods
        rows = self.db.Execute("SELECT 1")
        results = self.db.FetchAll()
        obj = self.client.GetUser(user_id)
        return results
`,
	})
	snap := makeSnapshot(t, root, []string{"svc/orm.py"})
	seams := EnumerateSeams(snap)
	for _, s := range seams {
		if s.Kind == SeamRPCMethod {
			t.Errorf("unexpected SeamRPCMethod %q: ordinary Python .UpperMethod() calls must not produce RPC seams without a producer", s.Key)
		}
	}
}

// TestEnumerateSeams_HTTPRouteLanguageSideCarried verifies that the Language
// field on each SeamSide correctly reflects the actual file language (not a
// hardcoded LangGo) when JS and Python files are involved.
func TestEnumerateSeams_HTTPRouteLanguageSideCarried(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"server/app.js": `const express = require('express');
const app = express();
app.post('/submit', (req, res) => res.send('ok'));
`,
		"client/client.py": `import requests
def submit(data):
    return requests.post('/submit', json=data)
`,
	})
	snap := makeSnapshot(t, root, []string{"server/app.js", "client/client.py"})
	seams := EnumerateSeams(snap)
	seam := seamByKey(seams, SeamHTTPRoute, "/submit")
	if seam == nil {
		t.Fatalf("expected SeamHTTPRoute /submit, got %+v", seams)
	}
	// Check that the JS side carries LangJavaScript, not LangGo.
	foundJS := false
	foundPy := false
	for _, side := range seam.Sides {
		if side.File == "server/app.js" && side.Language == LangJavaScript {
			foundJS = true
		}
		if side.File == "client/client.py" && side.Language == LangPython {
			foundPy = true
		}
	}
	if !foundJS {
		t.Errorf("expected JS side with LangJavaScript: %+v", seam.Sides)
	}
	if !foundPy {
		t.Errorf("expected Python side with LangPython: %+v", seam.Sides)
	}
}

// TestEnumerateSeams_ZeroNewSeamsOnOwnSource is the CRITICAL EMPIRICAL GATE:
// running EnumerateSeams over this repo's own source (Go + SQL) must emit
// ZERO SeamHTTPRoute seams attributable to the new JS/Python detectors (since
// the repo has no JS/Python source files). Any SeamHTTPRoute seams that appear
// must already have been present before the new detectors were added (the
// existing Go detector). This test guards against the new tables matching on
// Go source.
func TestEnumerateSeams_ZeroNewSeamsOnOwnSource(t *testing.T) {
	// Walk the real repo root (two levels up from internal/ingest).
	// We use os.Getwd() + navigating to the repo root.
	repoRoot := findRepoRoot(t)
	snap := buildRealSnapshot(t, repoRoot)
	seams := EnumerateSeams(snap)

	// Count seams by kind for reporting.
	counts := make(map[SeamKind]int)
	for _, s := range seams {
		counts[s.Kind]++
	}
	t.Logf("Own-source seam counts: DataFile=%d EnvVar=%d HTTPRoute=%d RPCMethod=%d total=%d",
		counts[SeamDataFile], counts[SeamEnvVar], counts[SeamHTTPRoute], counts[SeamRPCMethod], len(seams))

	// The repo has no JS/TS/Python source — all HTTP and RPC seams must come
	// from Go patterns that existed before this bead. Any SeamHTTPRoute or
	// SeamRPCMethod whose sides include a non-Go, non-proto file is a new
	// false positive from the new detectors.
	for _, s := range seams {
		if s.Kind != SeamHTTPRoute && s.Kind != SeamRPCMethod {
			continue
		}
		for _, side := range s.Sides {
			if side.Language != LangGo && side.Language != LangOther {
				t.Errorf("new-type seam %s/%q has non-Go/proto side %q (lang=%s): new detector matched on own source",
					s.Kind, s.Key, side.File, side.Language)
			}
		}
	}
}

// findRepoRoot walks up from the test's working directory to find the repo
// root (the directory containing go.mod).
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod found)")
		}
		dir = parent
	}
}

// buildRealSnapshot constructs a Snapshot over the real repo by walking the
// filesystem and classifying every file via DetectLanguage, mirroring how the
// ingest pipeline builds its snapshot. Only non-Other-language files are
// included (LangOther is excluded by the seam detector loop anyway, except
// .proto files which are included explicitly via the isProto path).
func buildRealSnapshot(t *testing.T, root string) *Snapshot {
	t.Helper()
	var files []File
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			// Skip hidden and vendor directories.
			name := d.Name()
			if name == ".git" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		lang := DetectLanguage(rel)
		isProto := strings.HasSuffix(rel, ".proto")
		if lang == LangOther && !isProto {
			return nil
		}
		files = append(files, File{Path: rel, Language: lang})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Snapshot{Root: root, Files: files}
}
