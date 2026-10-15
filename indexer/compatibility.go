package indexer

import (
	"errors"
	"fmt"
	"strconv"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
)

// ErrIncompatibleSnapshot identifies snapshots that must be recaptured before use.
var ErrIncompatibleSnapshot = errors.New("snapshot is incompatible; delete it and capture a new snapshot")

// SnapshotVersions returns the extraction versions stamped on new snapshots.
// Keep these separate from external tool versions: compatibility must also work
// on hosts without the repository or its indexers installed.
func SnapshotVersions() map[string]string {
	return map[string]string{
		"relationshipVersion": strconv.Itoa(relationshipVersion),
		"syntaxCacheVersion":  strconv.Itoa(syntaxCacheVersion),
		"scipArtifactVersion": strconv.Itoa(int(scipArtifactVersion)),
	}
}

// CheckSnapshot requires a complete snapshot with exactly the current versions.
// Missing, malformed, older, and newer stamps all fail closed.
func CheckSnapshot(snap *pb.Snapshot) error {
	if snap == nil || snap.GetIngestionStatus() != "complete" {
		return fmt.Errorf("%w: indexing is incomplete", ErrIncompatibleSnapshot)
	}
	versions := SnapshotVersions()
	for _, key := range []string{"relationshipVersion", "syntaxCacheVersion", "scipArtifactVersion"} {
		if got := snap.GetToolVersions()[key]; got != versions[key] {
			return fmt.Errorf("%w: %s is %q, expected %s", ErrIncompatibleSnapshot, key, got, versions[key])
		}
	}
	return nil
}
