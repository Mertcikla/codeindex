package parser

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mertcikla/codeindex/graph"
)

func TestScanCacheMatchesFreshAcrossChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sources := map[string]*graph.Source{}
	cache := &ScanCache{}
	put := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		sources[path] = &graph.Source{Path: path, Text: []byte(text), Hash: graph.Hash([]byte(text))}
	}
	put("compose.yml", "services:\n  api:\n    image: app\n  db:\n    image: postgres:16\n")
	put("main.go", "package main\nfunc main() {}\n")
	for _, edit := range []func(){func() {}, func() {}, func() { put("compose.yml", "services:\n  cache:\n    image: redis:7\n") }, func() { put("main.go", "package main\nfunc main() { println(1) }\n") }, func() {
		delete(sources, "compose.yml")
		if err := os.Remove(filepath.Join(root, "compose.yml")); err != nil {
			t.Fatal(err)
		}
	}} {
		edit()
		got, err := ScanCached(ctx, root, sources, cache)
		if err != nil {
			t.Fatal(err)
		}
		want, err := Scan(ctx, root, sources)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cached facts differ: %+v vs %+v", got, want)
		}
	}
	if _, ok := cache.Files["compose.yml"]; ok {
		t.Fatal("removed source remained cached")
	}
}
