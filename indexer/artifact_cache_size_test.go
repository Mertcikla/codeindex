package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	"google.golang.org/protobuf/proto"
)

// Optional measurement against artifacts and captured sources exported to an
// isolated directory. This keeps the standalone module independent of tld/SQL.
func TestArtifactCacheRealData(t *testing.T) {
	dir := os.Getenv("CODEINDEX_ARTIFACT_FIXTURE")
	if dir == "" {
		t.Skip("set CODEINDEX_ARTIFACT_FIXTURE to an isolated fixture directory")
	}
	var fixtures []struct {
		ProjectKey string
		Artifact   string
		Sources    []*graph.Source
	}
	raw, err := os.ReadFile(filepath.Join(dir, "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no artifact fixtures")
	}
	var totalRaw, totalCached int
	for _, fixture := range fixtures {
		raw, err := os.ReadFile(filepath.Join(dir, fixture.Artifact))
		if err != nil {
			t.Fatal(err)
		}
		family, root, _ := strings.Cut(fixture.ProjectKey, "|")
		project := &pb.Project{Root: root}
		sources := map[string]*graph.Source{}
		for _, source := range fixture.Sources {
			sources[source.Path] = source
		}
		original, calls := artifactTestGraph(t, sources, !isSyntaxFamily(family))
		cached, err := compactSCIPArtifact(context.Background(), original, project, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		load := func(g *graph.Graph, calls []callSite, data []byte) {
			t.Helper()
			reader, err := scipArtifactReader(data)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			table := newSymbols()
			if err := importSCIPReader(context.Background(), g, project, reader, projectHashes(project, g.Sources), false, !isSyntaxFamily(family), table); err != nil {
				t.Fatal(err)
			}
			table.apply(g)
			deriveCalls(g, calls, table)
		}
		load(original, calls, raw)
		replay, replayCalls := artifactTestGraph(t, sources, !isSyntaxFamily(family))
		load(replay, replayCalls, cached)
		if len(original.Facts) != len(replay.Facts) || len(original.EdgeFacts) != len(replay.EdgeFacts) {
			t.Fatalf("%s graph counts changed", fixture.ProjectKey)
		}
		for id, fact := range original.Facts {
			if !proto.Equal(fact, replay.Facts[id]) {
				t.Fatalf("%s fact %s changed", fixture.ProjectKey, id)
			}
		}
		for id, edge := range original.EdgeFacts {
			if !proto.Equal(edge, replay.EdgeFacts[id]) {
				t.Fatalf("%s edge %s changed", fixture.ProjectKey, id)
			}
		}
		t.Logf("%s: %d -> %d bytes (%.1f%% reduction), facts=%d edges=%d unchanged", fixture.ProjectKey, len(raw), len(cached), 100*(1-float64(len(cached))/float64(len(raw))), len(original.Facts), len(original.EdgeFacts))
		totalRaw += len(raw)
		totalCached += len(cached)
	}
	t.Logf("total: %d -> %d bytes (%.1f%% reduction)", totalRaw, totalCached, 100*(1-float64(totalCached)/float64(totalRaw)))
	if totalCached*10 >= totalRaw {
		t.Fatal("representative artifact reduction below 90%")
	}
}
