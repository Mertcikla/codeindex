package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadFrame(t *testing.T) {
	for _, frame := range []string{"\r\n", "Content-Length: -1\r\n\r\n", "Content-Length: 999999999\r\n\r\n", strings.Repeat("x", 9000), "Content-Length: 5\r\n\r\n{}"} {
		if _, err := readFrame(bufio.NewReader(strings.NewReader(frame))); err == nil {
			t.Fatalf("accepted malformed frame %q", frame[:min(50, len(frame))])
		}
	}
	data, err := readFrame(bufio.NewReader(strings.NewReader("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\ncontent-length: 2\r\n\r\n{}")))
	if err != nil || string(data) != "{}" {
		t.Fatalf("frame: %q %v", data, err)
	}
}

func TestFileURI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "space π # %.go")
	got, err := Path(URI(path))
	if err != nil || got != path {
		t.Fatalf("URI round trip: %q %v", got, err)
	}
	if _, err := Path("https://example.com/a.go"); err == nil {
		t.Fatal("accepted non-file URI")
	}
}

func TestClientLifecycle(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip("subprocess script fixture requires a POSIX shell")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEINDEX_LSP_HELPER", "1")
	tool := filepath.Join(t.TempDir(), "fake-lsp")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=TestLSPProcess\n"
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Start(ctx, tool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.Encoding != "utf-16" {
		t.Fatalf("encoding=%s", client.Encoding)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got int
			if err := client.Call(ctx, "echo", i, &got); err != nil || got != i {
				t.Errorf("echo %d: got %d err=%v", i, got, err)
			}
		}()
	}
	wg.Wait()
	timeout, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	err = client.Call(timeout, "hang", nil, nil)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	locations, err := client.Definition(ctx, "/a.go", Position{})
	if err != nil || len(locations) != 1 || locations[0].URI != "file:///b.go" || locations[0].Range.Start.Character != 3 {
		t.Fatalf("location link: %+v %v", locations, err)
	}
	if err := client.CheckDocument(ctx, "/a.go"); err == nil {
		t.Fatal("accepted a missing diagnostic report")
	}
	if err := client.CheckTypeScriptDocument(ctx, "/valid.ts"); err != nil {
		t.Fatalf("TypeScript diagnostic barrier: %v", err)
	}
	if err := client.CheckTypeScriptDocument(ctx, "/invalid.ts"); err == nil {
		t.Fatal("accepted an erroneous TypeScript document")
	}
	err = client.Call(ctx, "crash", nil, nil)
	if err == nil {
		t.Fatal("crashed server was accepted")
	}
}

func TestRustClientReadiness(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip("subprocess fixture requires a POSIX shell")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEINDEX_LSP_HELPER", "1")
	tool := filepath.Join(t.TempDir(), "fake-rust-analyzer")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\\''")+"' -test.run=TestLSPProcess\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := StartRust(ctx, tool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	deadline, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	err = client.WaitRustReady(deadline)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("accepted workspace without status: %v", err)
	}
	for _, status := range []rustStatus{{Health: "warning", Quiescent: true, Message: "missing dependency"}, {Health: "ok", Quiescent: true}} {
		if err := client.Call(ctx, "status", status, nil); err != nil {
			t.Fatal(err)
		}
		err := client.WaitRustReady(ctx)
		if (err == nil) != (status.Health == "ok") {
			t.Fatalf("status %+v: %v", status, err)
		}
	}
	if err := client.CheckDocument(ctx, "/valid.rs"); err != nil {
		t.Fatalf("Rust pull diagnostics: %v", err)
	}
	if err := client.CheckDocument(ctx, "/invalid.rs"); err == nil {
		t.Fatal("accepted invalid Rust diagnostics")
	}
	t.Setenv("CODEINDEX_RUST_NO_DIAGNOSTICS", "1")
	if other, err := StartRust(ctx, tool, t.TempDir()); err == nil {
		other.Close()
		t.Fatal("accepted Rust server without pull diagnostics")
	} else if !strings.Contains(err.Error(), "pull diagnostics") {
		t.Fatalf("capability check: %v", err)
	}
}

func TestPyrightClientDiagnostics(t *testing.T) {
	if filepath.Separator == '\\' {
		t.Skip("subprocess fixture requires POSIX shell")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEINDEX_LSP_HELPER", "1")
	tool := filepath.Join(t.TempDir(), "fake-pyright")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\\''")+"' -test.run=TestLSPProcess\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := StartPyright(ctx, tool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.PyrightVersion() != "1.1.409" {
		t.Fatalf("Pyright version=%q", client.PyrightVersion())
	}
	if err := client.CheckPythonDocument(ctx, "/valid.py"); err != nil {
		t.Fatalf("valid document: %v", err)
	}
	for _, path := range []string{"/invalid.py", "/unknown.py"} {
		if err := client.CheckPythonDocument(ctx, path); err == nil {
			t.Fatalf("accepted unchecked/invalid %s", path)
		}
	}
}

func TestLSPProcess(t *testing.T) {
	if os.Getenv("CODEINDEX_LSP_HELPER") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		data, err := readFrame(reader)
		if err != nil {
			os.Exit(0)
		}
		var req message
		if json.Unmarshal(data, &req) != nil {
			os.Exit(1)
		}
		if req.Method == "exit" {
			os.Exit(0)
		}
		if req.Method == "crash" {
			os.Exit(1)
		}
		if req.Method == "hang" || len(req.ID) == 0 || req.Method == "" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			log := `{"jsonrpc":"2.0","method":"window/logMessage","params":{"type":3,"message":"Pyright language server 1.1.409 starting"}}`
			_, _ = fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(log), log)
			// Exercise server requests while a client request is pending.
			request := `{"jsonrpc":"2.0","id":"config","method":"workspace/configuration","params":{"items":[{}]}}`
			_, _ = fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(request), request)
			capabilities := map[string]any{"definitionProvider": true, "diagnosticProvider": map[string]any{"interFileDependencies": true}}
			if os.Getenv("CODEINDEX_RUST_NO_DIAGNOSTICS") == "1" {
				delete(capabilities, "diagnosticProvider")
			}
			result = map[string]any{"capabilities": capabilities}
		case "status":
			notification, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "experimental/serverStatus", "params": req.Params})
			_, _ = fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(notification), notification)
		case "textDocument/diagnostic":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if strings.HasSuffix(params.TextDocument.URI, ".rs") || strings.HasSuffix(params.TextDocument.URI, ".py") {
				items := []map[string]any{}
				if strings.Contains(params.TextDocument.URI, "/invalid.") {
					items = append(items, map[string]any{"severity": 1, "message": "type mismatch"})
				}
				report := map[string]any{"kind": "full", "items": items}
				if !strings.HasSuffix(params.TextDocument.URI, "/unknown.py") {
					report["resultId"] = "checked"
				}
				result = report
			}
		case "echo":
			result = req.Params
		case "textDocument/definition":
			result = []any{map[string]any{"targetUri": "file:///b.go", "targetSelectionRange": Range{Start: Position{Character: 3}}}}
		case "workspace/executeCommand":
			var params struct {
				Arguments []json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &params) != nil || len(params.Arguments) != 3 {
				os.Exit(1)
			}
			var args struct {
				File string `json:"file"`
			}
			_ = json.Unmarshal(params.Arguments[1], &args)
			var diagnostics []map[string]any
			if args.File == "/invalid.ts" {
				diagnostics = append(diagnostics, map[string]any{"category": "error", "text": "type mismatch"})
			}
			result = map[string]any{"success": true, "body": diagnostics}
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		_, _ = fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(response), response)
	}
}
