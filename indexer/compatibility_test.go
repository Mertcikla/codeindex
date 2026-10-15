package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/config"
)

func TestCheckSnapshot(t *testing.T) {
	if !errors.Is(CheckSnapshot(nil), ErrIncompatibleSnapshot) {
		t.Fatal("nil snapshot must be incompatible")
	}
	if err := CheckSnapshot(&pb.Snapshot{IngestionStatus: "complete", ToolVersions: SnapshotVersions()}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"", "staging", "failed"} {
		if !errors.Is(CheckSnapshot(&pb.Snapshot{IngestionStatus: status, ToolVersions: SnapshotVersions()}), ErrIncompatibleSnapshot) {
			t.Fatalf("status %q must be incompatible", status)
		}
	}
	for key, version := range SnapshotVersions() {
		n, _ := strconv.Atoi(version)
		for _, value := range []string{"", "invalid", "0" + version, strconv.Itoa(n - 1), strconv.Itoa(n + 1)} {
			t.Run(key+"/"+value, func(t *testing.T) {
				versions := SnapshotVersions()
				if value == "" {
					delete(versions, key)
				} else {
					versions[key] = value
				}
				err := CheckSnapshot(&pb.Snapshot{IngestionStatus: "complete", ToolVersions: versions})
				if !errors.Is(err, ErrIncompatibleSnapshot) {
					t.Fatalf("invalid stamp accepted: %v", err)
				}
			})
		}
	}
}

func TestBuildStampsSnapshotsAndDoesNotReuseUnversionedBase(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte("package sample\nfunc Run() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline := Pipeline{Config: config.Default()}
	req := &pb.IndexRequest{Directory: root}
	snap, g, err := pipeline.Build(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSnapshot(snap); err != nil {
		t.Fatalf("new snapshot lacks current stamps: %v", err)
	}
	_, _, reused, err := pipeline.BuildIncremental(context.Background(), req, nil, baseFor(snap, g))
	if err != nil || !reused {
		t.Fatalf("current snapshot should be reused: reused=%v err=%v", reused, err)
	}
	delete(snap.ToolVersions, "relationshipVersion")
	next, _, reused, err := pipeline.BuildIncremental(context.Background(), req, nil, baseFor(snap, g))
	if err != nil || reused || next.Id == snap.Id {
		t.Fatalf("unversioned snapshot reused: reused=%v err=%v", reused, err)
	}
	if err := CheckSnapshot(next); err != nil {
		t.Fatal(err)
	}
}
