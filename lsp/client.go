// Package lsp provides a bounded stdio LSP client for watch-mode semantic layers.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Client owns one process. Calls may run concurrently; notifications and requests
// share a single ordered writer. The reader never waits on a request caller.
type Client struct {
	cmd         *exec.Cmd
	in          io.WriteCloser
	cancel      context.CancelFunc
	writeMu     sync.Mutex
	mu          sync.Mutex
	next        int64
	pending     map[string]chan message
	err         error
	done        chan struct{}
	wait        chan struct{}
	Encoding    string
	opened      map[string]int
	language    string
	rustStatus  *rustStatus
	rustChanged chan struct{}
}

func URI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}
func Path(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
		return "", fmt.Errorf("unsupported file URI %q", uri)
	}
	return filepath.FromSlash(u.Path), nil
}

// Start initializes gopls and verifies the capabilities used by the Go adapter.
// The lifetime context owns the process, separately from individual query deadlines.
func Start(ctx context.Context, executable, root string) (*Client, error) {
	return start(ctx, executable, []string{"serve"}, root, "go", map[string]any{"diagnosticsTrigger": "Save"})
}

// StartTypeScript starts typescript-language-server. sdk, when set, points at
// SCIP's tsserver.js so both producers use the same compiler and standard library.
func StartTypeScript(ctx context.Context, executable, root, sdk string) (*Client, error) {
	opts := map[string]any{"disableAutomaticTypingAcquisition": true, "tsserver": map[string]any{"useSyntaxServer": "never"}}
	if sdk != "" {
		opts["tsserver"].(map[string]any)["path"] = sdk
	}
	return start(ctx, executable, []string{"--stdio"}, root, "typescript", opts)
}

func start(ctx context.Context, executable string, args []string, root, language string, options map[string]any) (*Client, error) {
	life, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(life, executable, args...)
	cmd.Dir = root
	cmd.WaitDelay = time.Second
	cleanup := configureProcess(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = in.Close()
		return nil, err
	}
	// Servers can be very chatty. Discard stderr rather than retaining an
	// unbounded log or mixing diagnostics into the JSON-RPC stream.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		_ = in.Close()
		return nil, err
	}
	c := &Client{cmd: cmd, in: in, cancel: cancel, pending: map[string]chan message{}, done: make(chan struct{}), wait: make(chan struct{}), opened: map[string]int{}, Encoding: "utf-16", language: language}
	c.rustChanged = make(chan struct{})
	go c.read(out)
	go func() { _ = cmd.Wait(); cleanup(); close(c.wait) }()
	initCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	var result struct {
		Capabilities struct {
			PositionEncoding       string          `json:"positionEncoding"`
			DiagnosticProvider     json.RawMessage `json:"diagnosticProvider"`
			DefinitionProvider     json.RawMessage `json:"definitionProvider"`
			ExecuteCommandProvider struct {
				Commands []string `json:"commands"`
			} `json:"executeCommandProvider"`
		} `json:"capabilities"`
	}
	params := map[string]any{
		"processId": os.Getpid(), "rootUri": URI(root),
		"workspaceFolders": []any{map[string]any{"uri": URI(root), "name": filepath.Base(root)}},
		"capabilities": map[string]any{
			"general":      map[string]any{"positionEncodings": []string{"utf-8", "utf-16"}},
			"textDocument": map[string]any{"definition": map[string]any{"linkSupport": true}},
		},
		"initializationOptions": options,
	}
	if language == "rust" {
		caps := params["capabilities"].(map[string]any)
		caps["experimental"] = map[string]any{"serverStatusNotification": true}
		caps["textDocument"].(map[string]any)["diagnostic"] = map[string]any{}
	}
	if err := c.Call(initCtx, "initialize", params, &result); err != nil {
		c.Close()
		return nil, err
	}
	if v := string(result.Capabilities.DefinitionProvider); v == "" || v == "null" || v == "false" {
		c.Close()
		return nil, errors.New("language server does not support definitions")
	}
	if language == "typescript" && !slices.Contains(result.Capabilities.ExecuteCommandProvider.Commands, "typescript.tsserverRequest") {
		c.Close()
		return nil, errors.New("language server does not support TypeScript synchronous queries")
	}
	if language == "rust" {
		if v := string(result.Capabilities.DiagnosticProvider); v == "" || v == "null" || v == "false" {
			c.Close()
			return nil, errors.New("rust-analyzer does not support pull diagnostics")
		}
	}
	if enc := result.Capabilities.PositionEncoding; enc != "" {
		if enc != "utf-8" && enc != "utf-16" {
			c.Close()
			return nil, fmt.Errorf("unsupported position encoding %s", enc)
		}
		c.Encoding = enc
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close attempts orderly shutdown before cancelling and reaping the process.
func (c *Client) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Call(ctx, "shutdown", nil, nil)
	_ = c.Notify("exit", nil)
	c.cancel()
	_ = c.in.Close()
	<-c.wait
}

func (c *Client) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if pipe, ok := c.in.(*os.File); ok {
		_ = pipe.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}
	_, err = fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}
func (c *Client) Notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.next++
	id := c.next
	key := strconv.FormatInt(id, 10)
	ch := make(chan message, 1)
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		_ = c.Notify("$/cancelRequest", map[string]any{"id": id})
		return ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		return err
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: LSP %d: %s", method, m.Error.Code, m.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(m.Result, result)
	}
}

const maxMessageSize = 64 << 20

func (c *Client) read(r io.Reader) {
	var readErr error
	defer func() {
		c.mu.Lock()
		c.err = fmt.Errorf("language server disconnected: %w", readErr)
		c.mu.Unlock()
		close(c.done)
	}()
	reader := bufio.NewReader(r)
	for {
		var data []byte
		data, readErr = readFrame(reader)
		if readErr != nil {
			return
		}
		var m message
		if readErr = json.Unmarshal(data, &m); readErr != nil {
			return
		}
		if m.Method != "" {
			if m.Method == "experimental/serverStatus" && c.language == "rust" {
				var status rustStatus
				if json.Unmarshal(m.Params, &status) == nil {
					c.mu.Lock()
					c.rustStatus = &status
					close(c.rustChanged)
					c.rustChanged = make(chan struct{})
					c.mu.Unlock()
				}
			}
			if len(m.ID) > 0 {
				// No dynamic registration or edits are advertised. Answer optional
				// server requests so they cannot deadlock initialization or queries.
				response := map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": nil}
				switch m.Method {
				case "window/workDoneProgress/create", "client/registerCapability", "client/unregisterCapability":
				case "workspace/configuration":
					var p struct {
						Items []json.RawMessage `json:"items"`
					}
					_ = json.Unmarshal(m.Params, &p)
					response["result"] = make([]any, len(p.Items))
				default:
					delete(response, "result")
					response["error"] = rpcError{Code: -32601, Message: "method not supported"}
				}
				if readErr = c.write(response); readErr != nil {
					return
				}
			}
			continue
		}
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
	}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	size, headers := -1, 0
	for {
		raw, err := r.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		line := string(raw)
		headers += len(line)
		if headers > 8192 {
			return nil, errors.New("LSP headers too large")
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(name, "Content-Length") {
			size, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
		}
	}
	if size < 0 || size > maxMessageSize {
		return nil, fmt.Errorf("invalid LSP content length %d", size)
	}
	data := make([]byte, size)
	_, err := io.ReadFull(r, data)
	return data, err
}

// Sync sends captured content, never a second disk read. Call serially before
// issuing queries for the same batch; all edited files must arrive first.
func (c *Client) Sync(path string, text []byte) error {
	uri := URI(path)
	version := c.opened[uri] + 1
	c.opened[uri] = version
	if version == 1 {
		language := c.language
		if language == "typescript" {
			switch strings.ToLower(filepath.Ext(path)) {
			case ".tsx":
				language = "typescriptreact"
			case ".js", ".mjs", ".cjs":
				language = "javascript"
			case ".jsx":
				language = "javascriptreact"
			}
		}
		return c.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": language, "version": version, "text": string(text)}})
	}
	return c.Notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": uri, "version": version}, "contentChanges": []any{map[string]any{"text": string(text)}}})
}

func (c *Client) Definition(ctx context.Context, path string, pos Position) ([]Location, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, "textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": URI(path)}, "position": pos}, &raw); err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > 0 && raw[0] == '{' {
		var loc Location
		err := json.Unmarshal(raw, &loc)
		return []Location{loc}, err
	}
	var entries []struct {
		Location
		TargetURI            string `json:"targetUri"`
		TargetSelectionRange Range  `json:"targetSelectionRange"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	locations := make([]Location, 0, len(entries))
	for _, e := range entries {
		if e.TargetURI != "" {
			e.URI, e.Range = e.TargetURI, e.TargetSelectionRange
		}
		locations = append(locations, e.Location)
	}
	return locations, nil
}

// CheckDocument uses pull diagnostics as a synchronization/type-check barrier.
// A missing implementation is an error: an adapter must not treat an incomplete
// or ill-typed result as a successful semantic refresh.
func (c *Client) CheckDocument(ctx context.Context, path string) error {
	var report *struct {
		Items json.RawMessage `json:"items"`
	}
	if err := c.Call(ctx, "textDocument/diagnostic", map[string]any{"textDocument": map[string]any{"uri": URI(path)}}, &report); err != nil {
		return err
	}
	if report == nil || len(report.Items) == 0 {
		return errors.New("language server returned no diagnostic report")
	}
	var items []struct {
		Severity int    `json:"severity"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(report.Items, &items); err != nil {
		return err
	}
	for _, d := range items {
		if d.Severity == 1 {
			return fmt.Errorf("%s: %s", path, d.Message)
		}
	}
	return nil
}
