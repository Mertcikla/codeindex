// Package store defines the persistence contract between the codeindex engine
// and the systems that host it.
//
// The interface is deliberately thin: it covers publishing a built snapshot,
// reading it back, resolving repository identity, and the graph reload that
// incremental indexing needs. A host implements its own store (a relational
// database for tld) and hands it to the engine; the codeindex module ships an
// internal JSON implementation for standalone runs.
package store

import (
	"context"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

// Store persists codeindex snapshots and answers identity and read queries
// against them.
type Store interface {
	// Close releases the store's resources.
	Close() error

	// Publish writes a snapshot and its graph as the repository's latest.
	Publish(ctx context.Context, root string, snap *pb.Snapshot, g *graph.Graph) error

	// PublishHistorical writes a snapshot without advancing the repository's
	// latest pointer, used for backfilled commit snapshots.
	PublishHistorical(ctx context.Context, root string, snap *pb.Snapshot, g *graph.Graph) error

	// Snapshot returns the snapshot with the given id.
	Snapshot(ctx context.Context, id string) (*pb.Snapshot, error)

	// Snapshots returns every snapshot of a repository.
	Snapshots(ctx context.Context, repositoryID string) ([]*pb.Snapshot, error)

	// LoadGraph rebuilds the graph (sources, facts and edges) of a snapshot.
	LoadGraph(ctx context.Context, snapshotID string) (*graph.Graph, error)

	// Facts returns facts of a snapshot, optionally filtered by kind and path
	// prefix. kind FACT_KIND_UNSPECIFIED matches every kind; limit <= 0 means
	// unlimited.
	Facts(ctx context.Context, snapshotID string, kind pb.FactKind, pathPrefix, after string, limit int) ([]*pb.CodeFact, error)

	// EdgeFacts returns edges of a snapshot, optionally filtered by kind and
	// logical key. kind EDGE_KIND_UNSPECIFIED matches every kind; limit <= 0
	// means unlimited.
	EdgeFacts(ctx context.Context, snapshotID string, kind pb.EdgeKind, logicalKey, after string, limit int) ([]*pb.EdgeFact, error)

	// RepositoryExists reports whether a repository id is registered.
	RepositoryExists(ctx context.Context, id string) (bool, error)

	// RepositoryByRemoteKey returns the repository id registered for a
	// normalized remote key.
	RepositoryByRemoteKey(ctx context.Context, key string) (string, bool, error)

	// EnsureRepositoryIdentity registers a repository identity, adopting an
	// existing row when the remote key was registered first.
	EnsureRepositoryIdentity(ctx context.Context, id, root, remoteURL, remoteKey string, managed bool) error
}
