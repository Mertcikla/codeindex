package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestIgnoredPaths(t *testing.T) {
	d := &Detector{opts: Options{Root: "/repo", Exclude: []string{"generated"}}}
	ignored := []string{
		".git/objects/ab/cdef", ".git/logs/HEAD", ".tld/elements.yaml",
		"node_modules/pkg/index.js", "dist/app.js", "generated/api.go",
		"schema.scip", "notes.txt~", ".#lock",
	}
	for _, rel := range ignored {
		if !d.ignored(rel) {
			t.Errorf("expected %q to be ignored", rel)
		}
	}
	interesting := []string{".git/index", ".git/HEAD", ".git/refs/heads/main", "src/main.go", "README.md"}
	for _, rel := range interesting {
		if d.ignored(rel) {
			t.Errorf("expected %q to matter", rel)
		}
	}
}

func TestPollingFallbackReturnsChangedSignature(t *testing.T) {
	d := &Detector{opts: Options{Root: t.TempDir(), PollInterval: 5 * time.Millisecond}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	current := "one"
	capture := func(context.Context) (string, error) {
		calls++
		if calls >= 2 {
			return "two", nil
		}
		return "one", nil
	}
	got, err := d.Next(ctx, current, capture)
	if err != nil {
		t.Fatal(err)
	}
	if got != "two" {
		t.Fatalf("signature = %q, want two", got)
	}
}

func TestPollingFallbackStopsOnContext(t *testing.T) {
	d := &Detector{opts: Options{Root: t.TempDir(), PollInterval: 5 * time.Millisecond}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Next(ctx, "same", func(context.Context) (string, error) { return "same", nil }); err == nil {
		t.Fatal("expected context error")
	}
}

func TestSettleReportsDebouncing(t *testing.T) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	var states []string
	d := &Detector{watcher: w, opts: Options{Debounce: time.Millisecond, MaxWait: time.Second, OnState: func(state string) { states = append(states, state) }}}
	signature, err := d.settle(context.Background(), func(context.Context) (string, error) { return "changed", nil })
	if err != nil || signature != "changed" || len(states) != 1 || states[0] != "debouncing" {
		t.Fatalf("settle: %s %v states %v", signature, err, states)
	}
}

// A save before watches are registered produces no event. Both detector modes
// must capture it immediately instead of waiting for their polling interval.
func TestInitialCaptureIncludesPreStartupSave(t *testing.T) {
	for _, polling := range []bool{false, true} {
		t.Run(fmt.Sprintf("polling=%v", polling), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "main.go")
			if err := os.WriteFile(path, []byte("saved before startup"), 0600); err != nil {
				t.Fatal(err)
			}
			d := NewDetector(Options{Root: root, PollInterval: time.Hour})
			defer func() { _ = d.Close() }()
			if polling {
				d.usingFS = false
			} else if !d.usingFS {
				t.Fatal("filesystem watches unavailable")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := d.Next(ctx, "", func(context.Context) (string, error) {
				raw, err := os.ReadFile(path)
				return string(raw), err
			})
			if err != nil || got != "saved before startup" {
				t.Fatalf("initial capture = %q, %v", got, err)
			}
		})
	}
}

func TestInitialCapturePreservesSubsequentEvents(t *testing.T) {
	root := t.TempDir()
	d := NewDetector(Options{Root: root, Debounce: time.Millisecond, PollInterval: time.Hour})
	defer func() { _ = d.Close() }()
	if !d.usingFS {
		t.Fatal("filesystem watches unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := d.Next(ctx, "", func(context.Context) (string, error) {
		// This event arrives during the initial capture/indexing window.
		return "initial", os.WriteFile(filepath.Join(root, "main.go"), []byte("later save"), 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Next(ctx, "initial", func(context.Context) (string, error) { return "later", nil })
	if err != nil || got != "later" {
		t.Fatalf("follow-up capture = %q, %v", got, err)
	}
}

func TestInitialCaptureErrorAndCancellation(t *testing.T) {
	failure := errors.New("capture failed")
	d := &Detector{}
	if _, err := d.Next(context.Background(), "", func(context.Context) (string, error) {
		return "", failure
	}); !errors.Is(err, failure) {
		t.Fatalf("capture error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Next(ctx, "", func(context.Context) (string, error) {
		t.Fatal("capture called after cancellation")
		return "", nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}
