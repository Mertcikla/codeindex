package jsonstore

import (
	"os"
	"path/filepath"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
)

func TestResetLegacyEdgeKeys(t *testing.T) {
	for _, version := range []string{"1", "2"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			snapshotDir := filepath.Join(dir, "1", "2")
			if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(snapshotDir, "snapshot.json"), &pb.Snapshot{Id: "2", RepositoryId: "1"}); err != nil {
				t.Fatal(err)
			}
			latestPath := filepath.Join(dir, "1", "latest")
			if err := os.WriteFile(latestPath, []byte("2\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{"format": version + "\n", "repositories.json": "retained", "unrelated": "retained"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := resetLegacyIndex(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
				t.Fatalf("legacy snapshot survived: %v", err)
			}
			if _, err := os.Stat(latestPath); !os.IsNotExist(err) {
				t.Fatalf("stale latest pointer survived: %v", err)
			}
			for _, name := range []string{"repositories.json", "unrelated"} {
				if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != "retained" {
					t.Fatalf("metadata %s changed: %s %v", name, data, err)
				}
			}
			if err := resetLegacyIndex(dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}
