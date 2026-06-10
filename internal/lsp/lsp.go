// Package lsp is a minimal, hand-rolled Language Server Protocol client used
// by the agent harness's code-navigation tools (find_definition,
// find_references, find_implementations).
//
// It deliberately implements only the slice of LSP these read-only tools need:
// JSON-RPC 2.0 over stdio with Content-Length framing, the
// initialize/initialized handshake, textDocument/didOpen for queried files, the
// three position queries (definition, references, implementation), and a clean
// shutdown sequence. Agents never edit files, so there is no document sync
// beyond a single didOpen per queried file with its on-disk content.
//
// We hand-roll rather than depend on a client library because golang.org/x/
// tools' LSP client is internal/ and the community client libraries are stale.
//
// Positions: LSP character offsets are UTF-16 code units, not bytes and not
// runes. [UTF16Col] and [ByteCol] convert between byte offsets and UTF-16
// offsets within a line; callers must convert in both directions.
package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// Position is an LSP text position: zero-based line and zero-based UTF-16
// code-unit offset within the line.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is an LSP text range, half-open [Start, End).
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is an LSP location: a document URI plus a range within it.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// locationLink is the LocationLink shape some servers return for definition
// queries. We only need the target document and selection range.
type locationLink struct {
	TargetURI            string `json:"targetUri"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

// decodeLocations parses the union type LSP uses for location results:
// Location | Location[] | LocationLink[] | null.
func decodeLocations(raw json.RawMessage) ([]Location, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var locs []Location
	if err := json.Unmarshal(raw, &locs); err == nil {
		// A LocationLink[] also unmarshals into []Location with empty URIs;
		// detect that and fall through to the link decoding below.
		if len(locs) == 0 || locs[0].URI != "" {
			return locs, nil
		}
	}
	var one Location
	if err := json.Unmarshal(raw, &one); err == nil && one.URI != "" {
		return []Location{one}, nil
	}
	var links []locationLink
	if err := json.Unmarshal(raw, &links); err == nil {
		out := make([]Location, 0, len(links))
		for _, l := range links {
			if l.TargetURI == "" {
				continue
			}
			out = append(out, Location{URI: l.TargetURI, Range: l.TargetSelectionRange})
		}
		return out, nil
	}
	return nil, fmt.Errorf("lsp: unrecognized location result: %s", truncateForErr(raw))
}

// truncateForErr bounds a raw payload embedded in an error message.
func truncateForErr(raw json.RawMessage) string {
	const max = 200
	s := string(raw)
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// URIFromPath converts an absolute filesystem path to a file:// URI.
func URIFromPath(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

// PathFromURI converts a file:// URI back to a filesystem path. It returns
// ok=false for non-file URIs (e.g. untitled: or jdt: schemes some servers
// emit).
func PathFromURI(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	return u.Path, true
}
