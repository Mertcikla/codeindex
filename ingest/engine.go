// Package ingest shares snapshot preparation between the CLI watcher and APIs.
package ingest

import (
	"context"
	"fmt"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/gitstate"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/indexer"
	"github.com/mertcikla/codeindex/parser"
	"github.com/mertcikla/codeindex/store"
)

type Engine struct {
	Store              store.Store
	Config             config.Config
	Root, RepositoryID string
	Exclude            []string
	Progress           indexer.ProgressFunc
	PrepareCheckout    func(context.Context, string) error
	// Provenance, when set, overrides the provenance inferred from the checkout
	// for working-tree snapshots. Explicit captures publish durable saved points
	// with their own marker instead of the transient working_tree provenance.
	Provenance string
	// Manifests, when non-nil, persists working-tree file hashes across builds so
	// a watcher's repeated scans only rehash files that moved.
	Manifests *gitstate.ManifestCache
	// Semantics owns the watcher's language server for the live checkout.
	Semantics *indexer.WatchSession
	State     *gitstate.QuickState
	// Cache belongs to one serialized watch loop. Persisted snapshot identity is
	// checked before reuse, so another publisher or deletion invalidates it.
	Cache *WatchCache
}

type WatchCache struct {
	Base   *indexer.IncrementalBase
	Infra  parser.ScanCache
	Syntax indexer.SyntaxCache
}

type incrementalPublisher interface {
	PublishIncremental(context.Context, string, *pb.Snapshot, *graph.Graph, *graph.Graph) error
}

func (e Engine) Base(ctx context.Context, id string) (*indexer.IncrementalBase, error) {
	if id == "" {
		return nil, nil
	}
	snap, err := e.Store.Snapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	if indexer.CheckSnapshot(snap) != nil {
		return nil, nil
	}
	g, err := e.Store.LoadGraph(ctx, id)
	if err != nil {
		return nil, err
	}
	sources := map[string]string{}
	for _, src := range snap.Sources {
		sources[src.Path] = src.Hash
	}
	return &indexer.IncrementalBase{Snapshot: snap, Graph: g, Sources: sources}, nil
}

// snapshotRefs is an optional store capability: a repository's snapshots with
// only the metadata revision resolution needs. Preparing a target resolves
// against this listing repeatedly, so hosts whose full Snapshot listing computes
// expensive per-record statistics can offer the cheaper query.
type snapshotRefs interface {
	SnapshotRefs(ctx context.Context, repositoryID string) ([]*pb.Snapshot, error)
}

// listSnapshots prefers the lightweight listing when the store provides it.
func (e Engine) listSnapshots(ctx context.Context) ([]*pb.Snapshot, error) {
	if refs, ok := e.Store.(snapshotRefs); ok {
		return refs.SnapshotRefs(ctx, e.RepositoryID)
	}
	return e.Store.Snapshots(ctx, e.RepositoryID)
}

// Prepare prepares a target and returns its snapshot.
func (e Engine) Prepare(ctx context.Context, target *pb.Revision) (*pb.Snapshot, error) {
	snap, _, _, err := e.PrepareWithGraph(ctx, target)
	return snap, err
}

// PrepareWithGraph is Prepare plus the built graph and whether the snapshot was
// reused unchanged. Watchers use the graph to summarize a scan without loading
// the snapshot back from the store, and skip downstream comparison work when
// nothing moved.
func (e Engine) PrepareWithGraph(ctx context.Context, target *pb.Revision) (*pb.Snapshot, *graph.Graph, bool, error) {
	if target == nil {
		return nil, nil, false, fmt.Errorf("comparison target is required")
	}
	count := 0
	if target.SnapshotId != "" {
		count++
	}
	if target.GitRevision != "" {
		count++
	}
	if target.WorkingTree {
		count++
	}
	if count != 1 {
		return nil, nil, false, fmt.Errorf("select exactly one snapshot, commit, or working tree")
	}
	if target.SnapshotId != "" {
		snap, err := e.Store.Snapshot(ctx, target.SnapshotId)
		if err != nil {
			return nil, nil, false, err
		}
		if snap.RepositoryId != e.RepositoryID {
			return nil, nil, false, fmt.Errorf("snapshot belongs to another repository")
		}
		if err := indexer.CheckSnapshot(snap); err != nil {
			return nil, nil, false, err
		}
		return snap, nil, true, nil
	}
	revision, branch := target.GitRevision, target.GitBranch
	snapshots, err := e.listSnapshots(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	req := &pb.IndexRequest{Exclude: e.Exclude}
	configHash := indexer.ConfigurationHash(e.Config, req)
	find := func(sha string) *pb.Snapshot {
		for i := len(snapshots) - 1; i >= 0; i-- {
			snap := snapshots[i]
			if snap.Provenance == "commit" && snap.GitRevision == sha && snap.ConfigHash == configHash && indexer.CheckSnapshot(snap) == nil && indexer.ToolchainCompatible(ctx, e.Config, e.Root, snap, nil) {
				return snap
			}
		}
		return nil
	}
	if !target.WorkingTree {
		if snap := find(revision); snap != nil {
			return snap, nil, true, nil
		}
	}
	if !target.WorkingTree {
		revision, err = gitstate.Resolve(ctx, e.Root, revision)
		if err != nil {
			return nil, nil, false, err
		}
		if branch == "" {
			ref, _ := gitstate.Run(ctx, e.Root, "rev-parse", "--symbolic-full-name", "--verify", "--end-of-options", target.GitRevision)
			ref = strings.TrimSpace(ref)
			if strings.HasPrefix(ref, "refs/heads/") {
				branch = strings.TrimPrefix(ref, "refs/heads/")
			} else if strings.HasPrefix(ref, "refs/remotes/") {
				branch = strings.TrimPrefix(ref, "refs/remotes/")
			}
		}
		if snap := find(revision); snap != nil {
			return snap, nil, true, nil
		}
	}
	var base *indexer.IncrementalBase
	for i := len(snapshots) - 1; i >= 0; i-- {
		if snapshots[i].ConfigHash == configHash && indexer.CheckSnapshot(snapshots[i]) == nil {
			if e.Cache != nil && e.Cache.Base != nil && e.Cache.Base.Snapshot.Id == snapshots[i].Id && e.Cache.Base.Graph.RepositoryID == e.RepositoryID {
				base = e.Cache.Base
			} else {
				base, err = e.Base(ctx, snapshots[i].Id)
			}
			if err != nil {
				return nil, nil, false, err
			}
			break
		}
	}
	var snapshot *pb.Snapshot
	var preparedGraph *graph.Graph
	preparedReused := false
	build := func(directory string) error {
		if e.PrepareCheckout != nil {
			if err := e.PrepareCheckout(ctx, directory); err != nil {
				return fmt.Errorf("prepare revision %s: %w", revision, err)
			}
		}
		pipeline := indexer.Pipeline{Config: e.Config, RepositoryID: e.RepositoryID}
		if target.WorkingTree {
			pipeline.ManifestCache = e.Manifests
			pipeline.Semantics = e.Semantics
			pipeline.WatchState = e.State
			if e.Cache != nil {
				pipeline.InfraCache = &e.Cache.Infra
				pipeline.SyntaxCache = &e.Cache.Syntax
			}
		}
		input := &pb.IndexRequest{Directory: directory, Exclude: e.Exclude, Incremental: base != nil}
		snap, g, reused, err := pipeline.BuildIncremental(ctx, input, e.Progress, base)
		if err != nil {
			return err
		}
		// A provenance transition must retain a distinct immutable record:
		// commit snapshots are saved points, and an explicit working-tree
		// capture is published with the provenance the caller requested.
		provenance := ""
		if target.WorkingTree {
			provenance = e.Provenance
		} else {
			provenance = "commit"
		}
		if reused && provenance != "" && snap.Provenance != provenance {
			snap, g, err = pipeline.Build(ctx, input, e.Progress)
			if err != nil {
				return err
			}
			reused = false
		}
		if provenance != "" {
			snap.Provenance = provenance
		}
		if !target.WorkingTree {
			snap.GitRevision = revision
			snap.GitBranch = branch
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if !reused {
			if target.WorkingTree {
				if publisher, ok := e.Store.(incrementalPublisher); ok && base != nil {
					err = publisher.PublishIncremental(ctx, e.Root, snap, g, base.Graph)
				} else {
					err = e.Store.Publish(ctx, e.Root, snap, g)
				}
			} else {
				err = e.Store.PublishHistorical(ctx, e.Root, snap, g)
			}
			if err != nil {
				return err
			}
		}
		if target.WorkingTree && e.Cache != nil {
			hashes := map[string]string{}
			for path, src := range g.Sources {
				hashes[path] = src.Hash
			}
			e.Cache.Base = &indexer.IncrementalBase{Snapshot: snap, Graph: g, Sources: hashes}
		}
		snapshot, preparedGraph, preparedReused = snap, g, reused
		return nil
	}
	if target.WorkingTree {
		err = build(e.Root)
	} else {
		err = gitstate.WithCommit(ctx, e.Root, revision, build)
	}
	return snapshot, preparedGraph, preparedReused, err
}
