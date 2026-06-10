package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// rpcMessage is a JSON-RPC 2.0 message: request, notification, or response.
// ID is kept raw so we can echo server request IDs verbatim (the spec allows
// both numbers and strings) and key our pending-call table on the exact bytes.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// methodNotFound is the JSON-RPC error code for an unhandled method.
const methodNotFound = -32601

// isRequest reports whether m is a request (has both method and id), as opposed
// to a notification (method, no id) or a response (id, no method).
func (m *rpcMessage) isRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// isResponse reports whether m is a response to one of our requests.
func (m *rpcMessage) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// maxFrameBytes bounds a single LSP frame so a misbehaving server cannot make
// us allocate unboundedly. Real responses for our queries are tiny; 32MB leaves
// generous headroom for huge reference lists.
const maxFrameBytes = 32 << 20

// writeFrame encodes msg and writes it with LSP base-protocol framing
// (Content-Length header, CRLF CRLF, then the JSON payload). The caller must
// serialize concurrent writers.
func writeFrame(w io.Writer, msg *rpcMessage) error {
	msg.JSONRPC = "2.0"
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("lsp: marshal frame: %w", err)
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// maxHeaderBytes bounds the total header section of one frame. Real frames
// carry at most two short headers; a server streaming header lines without the
// blank terminator must not make us read forever.
const maxHeaderBytes = 16 << 10

// readFrame reads one framed message: headers until a blank line (only
// Content-Length matters; Content-Type is tolerated and ignored), then exactly
// Content-Length bytes of JSON.
func readFrame(r *bufio.Reader) (*rpcMessage, error) {
	length := -1
	headerBytes := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if headerBytes += len(line); headerBytes > maxHeaderBytes {
			return nil, fmt.Errorf("lsp: frame headers exceed %d byte limit", maxHeaderBytes)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("lsp: malformed header line %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q", strings.TrimSpace(value))
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("lsp: frame missing Content-Length header")
	}
	if length > maxFrameBytes {
		return nil, fmt.Errorf("lsp: frame of %d bytes exceeds %d byte limit", length, maxFrameBytes)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	var msg rpcMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil, fmt.Errorf("lsp: invalid JSON frame: %w", err)
	}
	return &msg, nil
}
