package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

func TestToolVersionMemoizesProbes(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "probes")
	tool := filepath.Join(dir, "scip-fixture")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  echo probe >> "$TLD_VERSION_COUNTER"
  echo fixture-1.2.3
  exit 0
fi
exit 1
`
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TLD_VERSION_COUNTER", counter)
	ctx := context.Background()
	for range 3 {
		if got := toolVersion(ctx, tool, []string{"--version"}); got != "fixture-1.2.3" {
			t.Fatalf("toolVersion = %q, want fixture-1.2.3", got)
		}
	}
	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if probes := strings.Count(string(raw), "\n"); probes != 1 {
		t.Fatalf("probed %d times, want 1", probes)
	}
}

func TestSymbolInputsInvalidatesSwappedSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projects := []*pb.Project{{Root: ".", Language: "go", ConfigPath: "go.mod"}}
	sources := map[string]*graph.Source{
		"a.go": {Language: "go", Hash: "hash-a"},
		"b.go": {Language: "go", Hash: "hash-b"},
	}
	before, err := symbolInputs(root, projects, sources, "config")
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		same, err := symbolInputs(root, projects, sources, "config")
		if err != nil {
			t.Fatal(err)
		}
		if same["go|."] != before["go|."] {
			t.Fatal("fingerprint depends on map iteration order")
		}
	}
	sources["a.go"].Hash, sources["b.go"].Hash = sources["b.go"].Hash, sources["a.go"].Hash
	after, err := symbolInputs(root, projects, sources, "config")
	if err != nil {
		t.Fatal(err)
	}
	if before["go|."] == after["go|."] {
		t.Fatal("swapping source contents must invalidate the project artifact")
	}
}
