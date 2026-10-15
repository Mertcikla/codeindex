package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/gitstate"
	"github.com/mertcikla/codeindex/graph"
)

// Reuse discovery only for edits to already indexed Go files under an unchanged
// complete inventory. Configuration, mode, membership, revision and other
// language changes use normal discovery. The full inventory is verified again
// after indexing, so newly arriving edits cannot be silently published as clean.
func (p Pipeline) watchDiscovery(ctx context.Context, root string, req *pb.IndexRequest, base *IncrementalBase, cache gitstate.ManifestCache) ([]*pb.Project, map[string]*graph.Source, *pb.SnapshotManifest, gitstate.ManifestCache, bool, error) {
	miss := func() ([]*pb.Project, map[string]*graph.Source, *pb.SnapshotManifest, gitstate.ManifestCache, bool, error) {
		return nil, nil, nil, cache, false, nil
	}
	if p.WatchState == nil || base == nil || base.unpublished || base.Snapshot.ConfigHash != ConfigurationHash(p.Config, req) || !base.Snapshot.GetManifest().GetComplete() || base.Snapshot.GitRevision != p.WatchState.Revision || base.Snapshot.GitBranch != p.WatchState.Branch {
		return miss()
	}
	manifest, next, err := gitstate.ManifestCachedGit(ctx, root, cache)
	if err != nil {
		return nil, nil, nil, cache, false, err
	}
	if !manifest.Complete || len(manifest.Files) != len(base.Snapshot.Manifest.Files) {
		return miss()
	}
	old := map[string]*pb.SourceFile{}
	for _, file := range base.Snapshot.Manifest.Files {
		old[file.Path] = file
	}
	for _, file := range manifest.Files {
		previous := old[file.Path]
		if previous == nil || previous.Mode != file.Mode || file.Mode == "160000" {
			return miss()
		}
		if hash, dirty := p.WatchState.ContentHashes[file.Path]; dirty && hash != file.Hash {
			return miss()
		}
		if previous.Hash == file.Hash {
			continue
		}
		src := base.Graph.Sources[file.Path]
		if src == nil || src.Language != "go" || strings.HasSuffix(file.Path, "_test.go") {
			return miss()
		}
	}
	sources := make(map[string]*graph.Source, len(base.Graph.Sources))
	hashes := map[string]string{}
	for _, file := range manifest.Files {
		hashes[file.Path] = file.Hash
	}
	for path, old := range base.Graph.Sources {
		src := &graph.Source{Path: path, Language: old.Language, Text: old.Text, Hash: old.Hash, InputBlob: old.InputBlob, Dirty: p.WatchState.Dirty[path], SyntaxCache: old.SyntaxCache, FileCache: old.FileCache}
		if hashes[path] != old.Hash {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil {
				return miss()
			}
			if !utf8.Valid(raw) || len(raw) > 2<<20 || graph.Hash(raw) != hashes[path] {
				return miss()
			}
			src.Text, src.Hash = raw, hashes[path]
		}
		sources[path] = src
	}
	return base.Snapshot.Projects, sources, manifest, next, true, nil
}
