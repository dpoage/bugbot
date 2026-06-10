package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestMain re-executes the test binary as a scripted fake LSP server when
// FAKE_LSP=1 is set, so manager/server tests can exercise the real subprocess
// lifecycle (spawn, handshake, crash, restart, shutdown) without any real
// language server installed.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_LSP") == "1" {
		runFakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Fake server behavior is driven by environment variables:
//
//	FAKE_LSP_CAPS            JSON capabilities object for the initialize result
//	                         (default: definition/references/implementation true).
//	FAKE_LSP_CRASH_ON        method name: os.Exit(3) upon receiving it.
//	FAKE_LSP_STALL_ON        method name: swallow the request without ever
//	                         responding (the loop keeps serving other methods),
//	                         simulating a server that is stuck indexing.
//	FAKE_LSP_CRASH_ONCE_FILE path: crash on the first query unless the file
//	                         exists (it is created before crashing), so a
//	                         restarted instance behaves normally.
//	FAKE_LSP_NO_SHUTDOWN     "1": ignore shutdown/exit so the client must kill us.
//
// Queries respond with a single fixed location whose range encodes the
// requested position (line/character echoed back), letting tests assert the
// position actually sent on the wire.
func runFakeServer() {
	br := bufio.NewReader(os.Stdin)
	w := os.Stdout

	respond := func(id json.RawMessage, result any) {
		raw, err := json.Marshal(result)
		if err != nil {
			panic(err)
		}
		if err := writeFrame(w, &rpcMessage{ID: id, Result: raw}); err != nil {
			os.Exit(1)
		}
	}

	caps := json.RawMessage(`{"definitionProvider":true,"referencesProvider":true,"implementationProvider":true}`)
	if c := os.Getenv("FAKE_LSP_CAPS"); c != "" {
		caps = json.RawMessage(c)
	}

	initialized := false
	opened := map[string]bool{}

	for {
		msg, err := readFrame(br)
		if err != nil {
			os.Exit(0) // client closed our stdin
		}

		if msg.Method == os.Getenv("FAKE_LSP_CRASH_ON") && msg.Method != "" {
			os.Exit(3)
		}
		if msg.Method == os.Getenv("FAKE_LSP_STALL_ON") && msg.Method != "" {
			continue
		}

		switch msg.Method {
		case "initialize":
			initialized = true
			respond(msg.ID, map[string]any{"capabilities": caps})
		case "initialized":
			// notification; nothing to do
		case "textDocument/didOpen":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			opened[p.TextDocument.URI] = true
		case "shutdown":
			if os.Getenv("FAKE_LSP_NO_SHUTDOWN") == "1" {
				continue
			}
			respond(msg.ID, nil)
		case "exit":
			if os.Getenv("FAKE_LSP_NO_SHUTDOWN") == "1" {
				continue
			}
			os.Exit(0)
		case "textDocument/definition", "textDocument/references", "textDocument/implementation":
			if !initialized {
				os.Exit(2)
			}
			if flag := os.Getenv("FAKE_LSP_CRASH_ONCE_FILE"); flag != "" {
				if _, err := os.Stat(flag); os.IsNotExist(err) {
					_ = os.WriteFile(flag, []byte("crashed"), 0o644)
					os.Exit(3)
				}
			}
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position Position `json:"position"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				writeErr(w, msg.ID, fmt.Sprintf("bad params: %v", err))
				continue
			}
			if !opened[p.TextDocument.URI] {
				writeErr(w, msg.ID, "document not opened before query")
				continue
			}
			// Echo the queried position back in the result range so tests can
			// assert the exact wire position.
			respond(msg.ID, []Location{{
				URI: p.TextDocument.URI,
				Range: Range{
					Start: p.Position,
					End:   Position{Line: p.Position.Line, Character: p.Position.Character + 1},
				},
			}})
		default:
			if msg.isRequest() {
				writeErr(w, msg.ID, "unsupported method "+msg.Method)
			}
		}
	}
}

func writeErr(w *os.File, id json.RawMessage, text string) {
	_ = writeFrame(w, &rpcMessage{ID: id, Error: &rpcError{Code: -32000, Message: text}})
}

// fakeServerConfig returns a ServerConfig that re-executes this test binary as
// the fake LSP server, claiming .go files, with extra env appended.
func fakeServerConfig(t *testing.T, env ...string) ServerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return ServerConfig{
		Cmd:         exe,
		Env:         append([]string{"FAKE_LSP=1"}, env...),
		LanguageIDs: map[string]string{".go": "go"},
	}
}
