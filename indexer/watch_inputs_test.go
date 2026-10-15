package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/gitstate"
	"github.com/mertcikla/codeindex/graph"
)

func TestWatchInputVerificationDetectsTimestampPreservingDrift(t *testing.T) {
	for _, change := range []string{"revert", "excluded file"} {
		t.Run(change, func(t *testing.T) {
			cfg := stubGoIndexer(t)
			root := t.TempDir()
			ctx := context.Background()
			write := func(path, text string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, path), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			original := "package p\nfunc A() int {return 1}\n"
			write("go.mod", "module example.com/drift\n\ngo 1.26\n")
			write("a.go", original)
			write("README.md", "before")
			for _, args := range [][]string{{"init", "-b", "main"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base"}} {
				if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			request := &pb.IndexRequest{Directory: root, Exclude: []string{"README.md"}}
			baseSnap, baseGraph, err := (Pipeline{Config: cfg}).Build(ctx, request, nil)
			if err != nil {
				t.Fatal(err)
			}
			write("a.go", "package p\nfunc A() int {return 2}\n")
			state, err := gitstate.CaptureQuick(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			var manifests gitstate.ManifestCache
			pipeline := Pipeline{Config: cfg, WatchState: &state, ManifestCache: &manifests}
			edited := false
			retry := false
			_, g, _, err := pipeline.BuildIncremental(ctx, request, func(p Progress) {
				if p.Stage != "verify" {
					return
				}
				if p.Detail != "" {
					retry = true
					return
				}
				if edited {
					return
				}
				edited = true
				path, text := "a.go", original
				if change == "excluded file" {
					path, text = "README.md", "after!"
				}
				info, statErr := os.Stat(filepath.Join(root, path))
				if statErr != nil {
					t.Fatal(statErr)
				}
				write(path, text)
				if err := os.Chtimes(filepath.Join(root, path), info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}, baseFor(baseSnap, baseGraph))
			if err != nil {
				t.Fatal(err)
			}
			if !edited || !retry {
				t.Fatal("did not reject the drifting watch attempt")
			}
			if change == "revert" && g.Sources["a.go"].Hash != graph.Hash([]byte(original)) {
				t.Fatal("revert was not captured")
			}
			if change == "excluded file" {
				manifest, manifestErr := gitstate.Manifest(ctx, root)
				if manifestErr != nil {
					t.Fatal(manifestErr)
				}
				if len(manifests) == 0 {
					t.Fatal("missing rebuilt manifest cache")
				}
				for _, f := range manifest.Files {
					if manifests[f.Path].Hash != f.Hash {
						t.Fatalf("stale manifest hash for %s", f.Path)
					}
				}
			}
		})
	}
}
