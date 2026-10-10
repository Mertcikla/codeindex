package identity

import (
	"context"
	"errors"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/google/uuid"
	"github.com/mertcikla/codeindex/graph"
)

// fakeStore is an in-memory store.Store holding registered repository
// identities, exercising the identity resolution paths without a database. It
// mirrors the host stores' tenant scoping: every row is namespaced by the
// organisation carried on the context.
type fakeStore struct {
	repos    map[string]map[string]storedRepository
	byRemote map[string]map[string]string
}

type storedRepository struct {
	root, remoteURL, remoteKey string
	managed                    bool
}

var errRemoteKeyTaken = errors.New("remote key already registered")

func newFakeStore() *fakeStore {
	return &fakeStore{repos: map[string]map[string]storedRepository{}, byRemote: map[string]map[string]string{}}
}

type orgKey struct{}

func withOrg(ctx context.Context, org uuid.UUID) context.Context {
	return context.WithValue(ctx, orgKey{}, org)
}

func orgOf(ctx context.Context) string {
	if org, ok := ctx.Value(orgKey{}).(uuid.UUID); ok && org != uuid.Nil {
		return org.String()
	}
	return "nil"
}

func (s *fakeStore) Close() error { return nil }

func (s *fakeStore) Publish(context.Context, string, *pb.Snapshot, *graph.Graph) error { return nil }
func (s *fakeStore) PublishHistorical(context.Context, string, *pb.Snapshot, *graph.Graph) error {
	return nil
}
func (s *fakeStore) Snapshot(context.Context, string) (*pb.Snapshot, error) { return nil, nil }
func (s *fakeStore) Snapshots(context.Context, string) ([]*pb.Snapshot, error) {
	return nil, nil
}
func (s *fakeStore) LoadGraph(context.Context, string) (*graph.Graph, error) { return nil, nil }
func (s *fakeStore) Facts(context.Context, string, pb.FactKind, string, string, int) ([]*pb.CodeFact, error) {
	return nil, nil
}
func (s *fakeStore) EdgeFacts(context.Context, string, pb.EdgeKind, string, string, int) ([]*pb.EdgeFact, error) {
	return nil, nil
}

func (s *fakeStore) RepositoryExists(ctx context.Context, id string) (bool, error) {
	_, ok := s.repos[orgOf(ctx)][id]
	return ok, nil
}

func (s *fakeStore) RepositoryByRemoteKey(ctx context.Context, key string) (string, bool, error) {
	id, ok := s.byRemote[orgOf(ctx)][key]
	return id, ok, nil
}

func (s *fakeStore) EnsureRepositoryIdentity(ctx context.Context, id, root, remoteURL, remoteKey string, managed bool) error {
	org := orgOf(ctx)
	if s.repos[org] == nil {
		s.repos[org] = map[string]storedRepository{}
	}
	if s.byRemote[org] == nil {
		s.byRemote[org] = map[string]string{}
	}
	if remoteKey != "" {
		if existing, ok := s.byRemote[org][remoteKey]; ok && existing != id {
			// Mirrors the store's unique (org, remote_key) index.
			return errRemoteKeyTaken
		}
		s.byRemote[org][remoteKey] = id
	}
	s.repos[org][id] = storedRepository{root: root, remoteURL: remoteURL, remoteKey: remoteKey, managed: managed}
	return nil
}

func TestApplyReusesRemoteIdentityAcrossCheckouts(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	first, err := Apply(ctx, st, "/Users/alice/proj", "", "https://github.com/Owner/Repo.git", false, uuid.Nil)
	if err != nil {
		t.Fatalf("apply first: %v", err)
	}
	if first.RemoteKey != "github.com/owner/repo" {
		t.Fatalf("remote key = %q", first.RemoteKey)
	}
	second, err := Apply(ctx, st, "/home/bob/proj", "", "git@github.com:Owner/Repo.git", false, uuid.Nil)
	if err != nil {
		t.Fatalf("apply second: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second id = %q, want %q", second.ID, first.ID)
	}
}

func TestResolveFallsBackToPathWithoutRemote(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	resolved, err := Resolve(ctx, st, "/tmp/standalone", "", "", uuid.Nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.RemoteKey != "" {
		t.Fatalf("remote key = %q, want empty", resolved.RemoteKey)
	}
	if resolved.ID == "" {
		t.Fatal("expected a path-derived id")
	}
}

func TestResolveHonorsExplicitRegisteredID(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()

	if err := st.EnsureRepositoryIdentity(ctx, "pinned", "/somewhere", "", "", false); err != nil {
		t.Fatalf("ensure pinned: %v", err)
	}
	resolved, err := Resolve(ctx, st, "/tmp/standalone", "pinned", "", uuid.Nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ID != "pinned" {
		t.Fatalf("id = %q, want pinned", resolved.ID)
	}
}

func TestApplyIsolatesSameCheckoutAcrossOrganisations(t *testing.T) {
	st := newFakeStore()
	orgA, orgB := uuid.New(), uuid.New()
	const root = "/shared/checkout"
	const remoteURL = "https://github.com/owner/repo"
	first, err := Apply(withOrg(context.Background(), orgA), st, root, "", remoteURL, false, orgA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Apply(withOrg(context.Background(), orgB), st, root, "", remoteURL, false, orgB)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("organisations share a repository identity")
	}
	for _, tc := range []struct {
		org uuid.UUID
		id  string
	}{{orgA, first.ID}, {orgB, second.ID}} {
		again, err := Apply(withOrg(context.Background(), tc.org), st, root, "", remoteURL, false, tc.org)
		if err != nil || again.ID != tc.id {
			t.Fatalf("repeat identity = %q, err = %v, want %q", again.ID, err, tc.id)
		}
	}
}

func TestResolvePreservesExistingPathIdentity(t *testing.T) {
	st := newFakeStore()
	org := uuid.New()
	ctx := withOrg(context.Background(), org)
	const root = "/shared/legacy-checkout"
	id := graph.RepositoryID(root)
	if err := st.EnsureRepositoryIdentity(ctx, id, root, "", "", false); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(ctx, st, root, "", "", org)
	if err != nil || resolved.ID != id {
		t.Fatalf("identity = %q, err = %v, want existing %q", resolved.ID, err, id)
	}
	namespaced, err := Resolve(ctx, st, "/local/checkout", "", "", org)
	if err != nil || namespaced.ID != graph.ID("organisation", org.String(), graph.RepositoryID("/local/checkout")) {
		t.Fatalf("local identity not namespaced: %q, err = %v", namespaced.ID, err)
	}
	plain, err := Resolve(context.Background(), st, "/local/checkout", "", "", uuid.Nil)
	if err != nil || plain.ID != graph.RepositoryID("/local/checkout") {
		t.Fatalf("local identity changed: %+v, err = %v", plain, err)
	}
}
