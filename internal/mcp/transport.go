package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"
)

// httpTransport speaks JSON-RPC over HTTP POST (one request/response per call).
type httpTransport struct {
	url    string
	client *http.Client
	mu     sync.Mutex
	id     int
}

func newHTTPTransport(url string) *httpTransport {
	return &httpTransport{url: url, client: &http.Client{Timeout: 30 * time.Second}}
}

func (t *httpTransport) call(method string, params any) (json.RawMessage, error) {
	t.mu.Lock()
	t.id++
	id := t.id
	t.mu.Unlock()

	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mcp http %d: %s", resp.StatusCode, string(raw))
	}
	return decodeRPC(raw, id)
}

func (t *httpTransport) close() error { return nil }

// stdioTransport launches an MCP server as a subprocess and exchanges
// newline-delimited JSON-RPC over its stdin/stdout.
type stdioTransport struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	id     int
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func newStdioTransport(command string, args []string) (*stdioTransport, error) {
	cmd := exec.Command(command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &stdioTransport{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

func (t *stdioTransport) call(method string, params any) (json.RawMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.id++
	id := t.id

	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if _, err := t.stdin.Write(append(body, '\n')); err != nil {
		return nil, err
	}
	// Read lines until we find the response with the matching id (skip any
	// notifications the server may interleave).
	for {
		line, err := t.stdout.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		res, matchErr := decodeRPC(line, id)
		if matchErr == errIDMismatch {
			continue
		}
		return res, matchErr
	}
}

func (t *stdioTransport) close() error {
	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	return nil
}

// errIDMismatch signals a JSON-RPC message that isn't the response we await.
var errIDMismatch = fmt.Errorf("rpc id mismatch")

// decodeRPC parses a JSON-RPC response, enforcing the expected id and
// surfacing RPC-level errors.
func decodeRPC(raw []byte, wantID int) (json.RawMessage, error) {
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode rpc: %w", err)
	}
	if resp.ID != wantID {
		return nil, errIDMismatch
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}
