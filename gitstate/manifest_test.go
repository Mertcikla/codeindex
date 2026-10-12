package gitstate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestManifestIncludesFilesWithoutIndexableSources(t *testing.T) {
	root := repo(t)
	put(t, root, "unit_test.go", "package a\n")
	put(t, root, "unsupported.custom", "hello")
	put(t, root, "binary.bin", "\x00\xff")
	put(t, root, "large.bin", strings.Repeat("x", (2<<20)+1))
	put(t, root, "strange\nname.txt", "newline")
	put(t, root, "ignored.go", "ignored")
	if err := os.Symlink("unsupported.custom", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	first, err := Manifest(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Complete {
		t.Fatal("manifest is incomplete")
	}
	found := map[string]bool{}
	for _, file := range first.Files {
		found[file.Path] = true
		if file.Hash == "" {
			t.Fatal("empty content identity")
		}
		if file.Path == "link" && file.Mode != "120000" {
			t.Fatal("symlink was followed")
		}
	}
	for _, path := range []string{"unit_test.go", "unsupported.custom", "binary.bin", "large.bin", "strange\nname.txt", "link"} {
		if !found[path] {
			t.Fatalf("missing %s", path)
		}
	}
	if found["ignored.go"] {
		t.Fatal("ignored untracked file included")
	}
	second, err := Manifest(context.Background(), root)
	if err != nil || !proto.Equal(first, second) {
		t.Fatalf("unstable manifest: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "a.go"), 0755); err != nil {
		t.Fatal(err)
	}
	third, err := Manifest(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if proto.Equal(first, third) {
		t.Fatal("mode-only change was missed")
	}
	put(t, root, "unit_test.go", "package b\n")
	fourth, err := Manifest(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if proto.Equal(third, fourth) {
		t.Fatal("unindexed content change was missed")
	}
}

func TestManifestCachedReusesUnchangedHashes(t *testing.T) {
	root := repo(t)
	put(t, root, "a.go", "package a\n")
	ctx := context.Background()
	first, cache, err := ManifestCached(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cache) == 0 {
		t.Fatal("cache not populated")
	}
	second, cache, err := ManifestCached(ctx, root, cache)
	if err != nil || !proto.Equal(first, second) {
		t.Fatalf("cached manifest differs: %v", err)
	}
	put(t, root, "a.go", "package b\n")
	third, _, err := ManifestCached(ctx, root, cache)
	if err != nil {
		t.Fatal(err)
	}
	if proto.Equal(first, third) {
		t.Fatal("content change was missed")
	}
	if err := os.Chmod(filepath.Join(root, "a.go"), 0755); err != nil {
		t.Fatal(err)
	}
	fourth, _, err := ManifestCached(ctx, root, cache)
	if err != nil {
		t.Fatal(err)
	}
	if proto.Equal(third, fourth) {
		t.Fatal("mode-only change was missed")
	}
}

func TestManifestPinsUninitializedGitlink(t *testing.T) {
	root := repo(t)
	revision := fixtureGit(t, root, "rev-parse", "HEAD")
	fixtureGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+revision+",module")
	if err := os.Mkdir(filepath.Join(root, "module"), 0700); err != nil {
		t.Fatal(err)
	}
	put(t, root, "a.go", "package changed\n")
	fixtureGit(t, root, "add", "a.go")
	fixtureGit(t, root, "commit", "-m", "advance parent")
	manifest, err := Manifest(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		if file.Path == "module" {
			if file.Mode != "160000" || file.Hash != revision {
				t.Fatalf("gitlink captured parent HEAD: %+v", file)
			}
			return
		}
	}
	t.Fatal("gitlink missing")
}
