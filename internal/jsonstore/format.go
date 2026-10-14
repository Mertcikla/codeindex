package jsonstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
)

const indexFormat = "2"

// resetLegacyIndex runs under the output lock. Only recognized snapshot
// directories are disposable; unrelated files and repository metadata stay put.
func resetLegacyIndex(dir string) error {
	marker := filepath.Join(dir, "format")
	raw, err := os.ReadFile(marker)
	if err == nil && strings.TrimSpace(string(raw)) == indexFormat {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && strings.TrimSpace(string(raw)) != "1" {
		return fmt.Errorf("jsonstore: unsupported index format %q", strings.TrimSpace(string(raw)))
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*", "*", "snapshot.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		var snap pb.Snapshot
		if err := readJSON(path, &snap); err != nil {
			return err
		}
		snapshotDir := filepath.Dir(path)
		if snap.Id != "" && snap.Id == filepath.Base(snapshotDir) {
			if err := os.RemoveAll(snapshotDir); err != nil {
				return err
			}
		}
	}
	return os.WriteFile(marker, []byte(indexFormat+"\n"), 0o600)
}
