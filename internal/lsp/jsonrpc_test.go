package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &rpcMessage{
		ID:     json.RawMessage("7"),
		Method: "textDocument/definition",
		Params: json.RawMessage(`{"position":{"line":1,"character":2}}`),
	}
	if err := writeFrame(&buf, in); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	raw := buf.String()
	if !strings.HasPrefix(raw, "Content-Length: ") || !strings.Contains(raw, "\r\n\r\n") {
		t.Fatalf("bad framing: %q", raw)
	}

	out, err := readFrame(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if out.JSONRPC != "2.0" || string(out.ID) != "7" || out.Method != in.Method {
		t.Errorf("round trip mismatch: %+v", out)
	}
	if string(out.Params) != string(in.Params) {
		t.Errorf("params mismatch: %s", out.Params)
	}
}

func TestReadFrameToleratesExtraHeaders(t *testing.T) {
	payload := `{"jsonrpc":"2.0","id":1,"result":null}`
	raw := "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(payload)) + "\r\n\r\n" + payload
	msg, err := readFrame(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !msg.isResponse() {
		t.Errorf("expected response, got %+v", msg)
	}
}

func TestReadFrameMissingContentLength(t *testing.T) {
	raw := "Content-Type: application/json\r\n\r\n{}"
	if _, err := readFrame(bufio.NewReader(strings.NewReader(raw))); err == nil {
		t.Fatal("expected error for missing Content-Length")
	}
}

func TestReadFrameOversized(t *testing.T) {
	raw := "Content-Length: 999999999999\r\n\r\n"
	if _, err := readFrame(bufio.NewReader(strings.NewReader(raw))); err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestDecodeLocations(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
		uri  string
	}{
		{"null", "null", 0, ""},
		{"empty array", "[]", 0, ""},
		{"single location", `{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}`, 1, "file:///a.go"},
		{"location array", `[{"uri":"file:///b.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]`, 1, "file:///b.go"},
		{"location links", `[{"targetUri":"file:///c.go","targetSelectionRange":{"start":{"line":3,"character":4},"end":{"line":3,"character":9}},"targetRange":{"start":{"line":2,"character":0},"end":{"line":5,"character":1}}}]`, 1, "file:///c.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			locs, err := decodeLocations(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatalf("decodeLocations: %v", err)
			}
			if len(locs) != tt.want {
				t.Fatalf("got %d locations, want %d", len(locs), tt.want)
			}
			if tt.want > 0 && locs[0].URI != tt.uri {
				t.Errorf("uri = %q, want %q", locs[0].URI, tt.uri)
			}
		})
	}

	t.Run("location link selection range wins", func(t *testing.T) {
		raw := `[{"targetUri":"file:///c.go","targetSelectionRange":{"start":{"line":3,"character":4},"end":{"line":3,"character":9}},"targetRange":{"start":{"line":2,"character":0},"end":{"line":5,"character":1}}}]`
		locs, err := decodeLocations(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("decodeLocations: %v", err)
		}
		if locs[0].Range.Start.Line != 3 {
			t.Errorf("expected selection range start line 3, got %d", locs[0].Range.Start.Line)
		}
	})
}

func TestURIRoundTrip(t *testing.T) {
	path := "/home/user/src/héllo dir/main.go"
	uri := URIFromPath(path)
	if !strings.HasPrefix(uri, "file:///") {
		t.Fatalf("bad uri: %q", uri)
	}
	got, ok := PathFromURI(uri)
	if !ok || got != path {
		t.Errorf("PathFromURI(%q) = %q, %v; want %q", uri, got, ok, path)
	}
	if _, ok := PathFromURI("untitled:Untitled-1"); ok {
		t.Error("non-file URI should not resolve")
	}
}
