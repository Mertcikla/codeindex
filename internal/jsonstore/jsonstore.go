// Package jsonstore persists codeindex snapshots as canonical JSON files under
// a target directory. It backs the standalone codeindex CLI: every snapshot
// gets one directory of JSON documents (snapshot, facts, edges, sources and
// project artifacts), ordered deterministically so repeated indexing of the
// same content produces the same bytes. It also serves as the reference
// implementation of the store interface for hosts.
package jsonstore

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/mertcikla/codeindex/identity"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
)

// Store is a store.Store backed by a directory of JSON files.
type Store struct {
	dir  string
	lock *os.File
	mu   sync.Mutex
}

// Open creates (if needed) and returns a store rooted at dir.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("jsonstore: directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("jsonstore: create %s: %w", dir, err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("jsonstore: output is locked: %w", err)
	}
	if err := resetLegacyIndex(dir); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &Store{dir: dir, lock: lock}, nil
}

// Close implements store.Store; file-backed storage holds no resources.
func (s *Store) Close() error { return s.lock.Close() }

// Publish writes the snapshot as the repository's latest.
func (s *Store) Publish(ctx context.Context, root string, snap *pb.Snapshot, g *graph.Graph) error {
	if err := s.write(ctx, root, snap, g); err != nil {
		return err
	}
	return s.writeLatest(snap.GetRepositoryId(), snap.GetId())
}

// PublishHistorical writes the snapshot without advancing the latest pointer.
func (s *Store) PublishHistorical(ctx context.Context, root string, snap *pb.Snapshot, g *graph.Graph) error {
	return s.write(ctx, root, snap, g)
}

func (s *Store) write(ctx context.Context, root string, snap *pb.Snapshot, g *graph.Graph) error {
	if snap == nil {
		return fmt.Errorf("jsonstore: snapshot is required")
	}
	if g != nil && temporaryID(snap.Id) {
		resolved, err := identity.Apply(ctx, s, root, "", "", false, uuid.Nil)
		if err != nil {
			return err
		}
		snap.RepositoryId = resolved.ID
		normalizeReuse(g, snap.RepositoryId)
		first, err := s.AllocateIDs(ctx, graphIDCount(g))
		if err != nil {
			return err
		}
		assignGraphIDs(snap, g, first)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.dir, snap.GetRepositoryId(), snap.GetId())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("jsonstore: create %s: %w", dir, err)
	}

	facts := make([]*pb.CodeFact, 0, len(g.Facts))
	for _, f := range g.Facts {
		facts = append(facts, f)
	}
	sortFacts(facts)

	edges := make([]*pb.EdgeFact, 0, len(g.EdgeFacts))
	for _, e := range g.EdgeFacts {
		edges = append(edges, e)
	}
	sortEdges(edges)

	sources := make([]sourceDoc, 0, len(g.Sources))
	for _, src := range g.Sources {
		if src == nil {
			continue
		}
		sources = append(sources, sourceDoc{
			Path: src.Path, Language: src.Language, Hash: src.Hash,
			InputBlob: src.InputBlob, SyntaxCache: src.SyntaxCache,
			FileCache: src.FileCache, Dirty: src.Dirty, Text: src.Text,
		})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })

	artifacts := make([]artifactDoc, 0, len(g.ProjectArtifacts))
	for key, artifact := range g.ProjectArtifacts {
		artifacts = append(artifacts, artifactDoc{Key: key, Fingerprint: artifact.Fingerprint, Data: artifact.Data})
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Key < artifacts[j].Key })

	for _, doc := range []struct {
		name  string
		value any
	}{
		{"snapshot.json", snap},
		{"facts.json", facts},
		{"edges.json", edges},
		{"sources.json", sources},
		{"artifacts.json", artifacts},
	} {
		if err := writeJSON(filepath.Join(dir, doc.name), doc.value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeLatest(repositoryID, snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	repoDir := filepath.Join(s.dir, repositoryID)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return fmt.Errorf("jsonstore: create %s: %w", repoDir, err)
	}
	return os.WriteFile(filepath.Join(repoDir, "latest"), []byte(snapshotID+"\n"), 0o644)
}

// Snapshot reads a snapshot by id.
func (s *Store) Snapshot(_ context.Context, id string) (*pb.Snapshot, error) {
	if temporaryID(id) {
		return nil, fmt.Errorf("jsonstore: snapshot %s not found", id)
	}
	matches, err := filepath.Glob(filepath.Join(s.dir, "*", id, "snapshot.json"))
	if err != nil {
		return nil, fmt.Errorf("jsonstore: glob: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("jsonstore: snapshot %s not found", id)
	}
	snap := &pb.Snapshot{}
	if err := readJSON(matches[0], snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// Snapshots lists a repository's snapshots, oldest first.
func (s *Store) Snapshots(_ context.Context, repositoryID string) ([]*pb.Snapshot, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, repositoryID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("jsonstore: read %s: %w", repositoryID, err)
	}
	var snaps []*pb.Snapshot
	for _, entry := range entries {
		if !entry.IsDir() || temporaryID(entry.Name()) {
			continue
		}
		snap := &pb.Snapshot{}
		if err := readJSON(filepath.Join(s.dir, repositoryID, entry.Name(), "snapshot.json"), snap); err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].GetCreatedUnix() != snaps[j].GetCreatedUnix() {
			return snaps[i].GetCreatedUnix() < snaps[j].GetCreatedUnix()
		}
		return snaps[i].GetId() < snaps[j].GetId()
	})
	return snaps, nil
}

// LoadGraph rebuilds a snapshot's graph from its JSON documents.
func (s *Store) LoadGraph(_ context.Context, snapshotID string) (*graph.Graph, error) {
	snapDir, err := s.snapshotDir(snapshotID)
	if err != nil {
		return nil, err
	}
	snap := &pb.Snapshot{}
	if err := readJSON(filepath.Join(snapDir, "snapshot.json"), snap); err != nil {
		return nil, err
	}

	var sources []sourceDoc
	if err := readJSON(filepath.Join(snapDir, "sources.json"), &sources); err != nil {
		return nil, err
	}
	var artifacts []artifactDoc
	if err := readJSON(filepath.Join(snapDir, "artifacts.json"), &artifacts); err != nil {
		return nil, err
	}

	g := graph.NewGraph(snap.GetRepositoryId(), snap.GetId())
	for _, src := range sources {
		g.Sources[src.Path] = &graph.Source{
			Path: src.Path, Language: src.Language, Hash: src.Hash,
			InputBlob: src.InputBlob, SyntaxCache: src.SyntaxCache,
			FileCache: src.FileCache, Dirty: src.Dirty, Text: src.Text,
		}
	}
	for _, artifact := range artifacts {
		g.ProjectArtifacts[artifact.Key] = graph.ProjectArtifact{Fingerprint: artifact.Fingerprint, Data: artifact.Data}
	}

	facts, err := s.Facts(context.Background(), snapshotID, pb.FactKind_FACT_KIND_UNSPECIFIED, "", "", 0)
	if err != nil {
		return nil, err
	}
	for _, f := range facts {
		g.Facts[f.GetId()] = f
	}
	edges, err := s.EdgeFacts(context.Background(), snapshotID, pb.EdgeKind_EDGE_KIND_UNSPECIFIED, "", "", 0)
	if err != nil {
		return nil, err
	}
	for _, e := range edges {
		g.EdgeFacts[e.GetId()] = e
	}
	return g, nil
}

// Facts reads a snapshot's facts with the same filtering contract as the host
// stores: kind FACT_KIND_UNSPECIFIED matches every kind, pathPrefix filters on
// the anchor path, after skips ids up to and including the cursor, and limit
// <= 0 is unlimited.
func (s *Store) Facts(_ context.Context, snapshotID string, kind pb.FactKind, pathPrefix, after string, limit int) ([]*pb.CodeFact, error) {
	facts, err := s.readFacts(snapshotID)
	if err != nil {
		return nil, err
	}
	sort.Slice(facts, func(i, j int) bool { return idLess(facts[i].Id, facts[j].Id) })
	var out []*pb.CodeFact
	for _, f := range facts {
		if kind != pb.FactKind_FACT_KIND_UNSPECIFIED && f.GetKind() != kind {
			continue
		}
		if pathPrefix != "" && !strings.HasPrefix(f.GetAnchor().GetPath(), pathPrefix) {
			continue
		}
		if after != "" && !idLess(after, f.GetId()) {
			continue
		}
		out = append(out, f)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// EdgeFacts reads a snapshot's edges with the same filtering contract as the
// host stores: kind EDGE_KIND_UNSPECIFIED matches every kind, logicalKey
// filters exactly, after skips ids up to and including the cursor, and limit
// <= 0 is unlimited.
func (s *Store) EdgeFacts(_ context.Context, snapshotID string, kind pb.EdgeKind, logicalKey, after string, limit int) ([]*pb.EdgeFact, error) {
	edges, err := s.readEdges(snapshotID)
	if err != nil {
		return nil, err
	}
	sort.Slice(edges, func(i, j int) bool { return idLess(edges[i].Id, edges[j].Id) })
	var out []*pb.EdgeFact
	for _, e := range edges {
		if kind != pb.EdgeKind_EDGE_KIND_UNSPECIFIED && e.GetKind() != kind {
			continue
		}
		if logicalKey != "" && e.GetLogicalKey() != logicalKey {
			continue
		}
		if after != "" && !idLess(after, e.GetId()) {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// RepositoryExists reports whether the repository has a snapshot directory.
func (s *Store) RepositoryExists(_ context.Context, id string) (bool, error) {
	info, err := os.Stat(filepath.Join(s.dir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

// RepositoryByRemoteKey resolves registered checkouts across standalone runs.
func (s *Store) RepositoryByRemoteKey(ctx context.Context, key string) (string, bool, error) {
	return s.findRepository(func(r repositoryDoc) bool { return key != "" && r.RemoteKey == key })
}
func (s *Store) EnsureRepositoryIdentity(_ context.Context, id, root, remoteURL, remoteKey string, managed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "repository.json"), repositoryDoc{ID: id, Root: root, RemoteURL: remoteURL, RemoteKey: remoteKey, Managed: managed})
}

func (s *Store) snapshotDir(snapshotID string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*", snapshotID))
	if err != nil {
		return "", fmt.Errorf("jsonstore: glob: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("jsonstore: snapshot %s not found", snapshotID)
	}
	return matches[0], nil
}

func (s *Store) readFacts(snapshotID string) ([]*pb.CodeFact, error) {
	snapDir, err := s.snapshotDir(snapshotID)
	if err != nil {
		return nil, err
	}
	var facts []*pb.CodeFact
	if err := readJSON(filepath.Join(snapDir, "facts.json"), &facts); err != nil {
		return nil, err
	}
	sortFacts(facts)
	return facts, nil
}

func (s *Store) readEdges(snapshotID string) ([]*pb.EdgeFact, error) {
	snapDir, err := s.snapshotDir(snapshotID)
	if err != nil {
		return nil, err
	}
	var edges []*pb.EdgeFact
	if err := readJSON(filepath.Join(snapDir, "edges.json"), &edges); err != nil {
		return nil, err
	}
	sortEdges(edges)
	return edges, nil
}

// sourceDoc mirrors graph.Source for JSON encoding, including the source text
// so a graph can be reloaded without the checkout.
type sourceDoc struct {
	Path        string `json:"path"`
	Language    string `json:"language,omitempty"`
	Hash        string `json:"hash"`
	InputBlob   string `json:"input_blob,omitempty"`
	SyntaxCache string `json:"syntax_cache,omitempty"`
	FileCache   string `json:"file_cache,omitempty"`
	Dirty       bool   `json:"dirty,omitempty"`
	Text        []byte `json:"text,omitempty"`
}

// artifactDoc mirrors graph.ProjectArtifact for JSON encoding.
type artifactDoc struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Data        []byte `json:"data,omitempty"`
}

func sortFacts(facts []*pb.CodeFact) {
	sort.SliceStable(facts, func(i, j int) bool { return factLess(facts[i], facts[j]) })
}

func sortEdges(edges []*pb.EdgeFact) {
	sort.SliceStable(edges, func(i, j int) bool { return edgeLess(edges[i], edges[j]) })
}

// factLess orders facts by anchor path, kind, name and byte range, then by the
// remaining identifiers, so the encoded documents are byte-stable regardless
// of the graph's map iteration order.
func factLess(a, b *pb.CodeFact) bool {
	ap, bp := a.GetAnchor().GetPath(), b.GetAnchor().GetPath()
	if ap != bp {
		return ap < bp
	}
	if a.GetKind() != b.GetKind() {
		return a.GetKind() < b.GetKind()
	}
	if an, bn := a.GetName(), b.GetName(); an != bn {
		return an < bn
	}
	if s, bs := a.GetAnchor().GetStartByte(), b.GetAnchor().GetStartByte(); s != bs {
		return s < bs
	}
	if e, be := a.GetAnchor().GetEndByte(), b.GetAnchor().GetEndByte(); e != be {
		return e < be
	}
	return a.GetId() < b.GetId()
}

// edgeLess orders edges the same way.
func edgeLess(a, b *pb.EdgeFact) bool {
	ap, bp := a.GetAnchor().GetPath(), b.GetAnchor().GetPath()
	if ap != bp {
		return ap < bp
	}
	if a.GetKind() != b.GetKind() {
		return a.GetKind() < b.GetKind()
	}
	if at, bt := a.GetTargetSymbolKey(), b.GetTargetSymbolKey(); at != bt {
		return at < bt
	}
	if s, bs := a.GetAnchor().GetStartByte(), b.GetAnchor().GetStartByte(); s != bs {
		return s < bs
	}
	if e, be := a.GetAnchor().GetEndByte(), b.GetAnchor().GetEndByte(); e != be {
		return e < be
	}
	return a.GetId() < b.GetId()
}

func writeJSON(path string, doc any) error {
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("jsonstore: marshal %s: %w", path, err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		return fmt.Errorf("jsonstore: write %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, doc any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("jsonstore: read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, doc); err != nil {
		return fmt.Errorf("jsonstore: decode %s: %w", path, err)
	}
	return nil
}

func idLess(a, b string) bool {
	ai, ae := strconv.ParseInt(a, 10, 64)
	bi, be := strconv.ParseInt(b, 10, 64)
	if ae == nil && be == nil {
		return ai < bi
	}
	return a < b
}
