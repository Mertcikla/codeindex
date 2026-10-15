package indexer

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

func artifactCacheFixture(t *testing.T) (*scip.Index, map[string]*graph.Source) {
	t.Helper()
	const code = "package sample\nfunc Target() {}\nfunc Caller() { Target() }\n"
	src := &graph.Source{Path: "nested/a.go", Language: "go", Text: []byte(code), Hash: graph.Hash([]byte(code))}
	target, caller := "scip-go gomod fixture 1 Target().", "scip-go gomod fixture 1 Caller()."
	junk := strings.Repeat("unused hover and diagnostics", 1000)
	doc := &scip.Document{RelativePath: "a.go", Text: code, Language: "go", PositionEncoding: scip.PositionEncoding_UTF16CodeUnitOffsetFromLineStart,
		Symbols: []*scip.SymbolInformation{
			{Symbol: target, Kind: scip.SymbolInformation_Function, DisplayName: "Target", Documentation: []string{"target docs"}, EnclosingSymbol: junk,
				SignatureDocumentation: &scip.Signature{Text: "func Target()", Language: junk, Occurrences: []*scip.Occurrence{{Symbol: junk}}}},
			{Symbol: caller, Kind: scip.SymbolInformation_Function, DisplayName: "Caller", Documentation: []string{"caller docs"}, Relationships: []*scip.Relationship{
				{Symbol: target, IsImplementation: true, IsTypeDefinition: true, IsReference: true}, {Symbol: junk, IsReference: true},
			}},
			{Symbol: "local 1", Documentation: []string{junk}},
		},
		Occurrences: []*scip.Occurrence{
			{Symbol: target, SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{99, 0, 1}, TypedRange: &scip.Occurrence_SingleLineRange{SingleLineRange: &scip.SingleLineRange{Line: 1, StartCharacter: 5, EndCharacter: 11}},
				TypedEnclosingRange:   &scip.Occurrence_SingleLineEnclosingRange{SingleLineEnclosingRange: &scip.SingleLineRange{Line: 1, StartCharacter: 0, EndCharacter: 16}},
				OverrideDocumentation: []string{junk}, SyntaxKind: scip.SyntaxKind_IdentifierBuiltin, Diagnostics: []*scip.Diagnostic{{Message: junk}}},
			{Symbol: caller, SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{2, 5, 11}, EnclosingRange: []int32{2, 0, 26}},
			{Symbol: target, Range: []int32{2, 16, 22}, OverrideDocumentation: []string{junk}},
			{Symbol: "local 1", Range: []int32{-1, -1, -1}, OverrideDocumentation: []string{junk}},
			{Symbol: target, Range: []int32{-1, -1, -1}},
		},
	}
	doc.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	return &scip.Index{
		Metadata: &scip.Metadata{ToolInfo: &scip.ToolInfo{Name: "fixture", Version: "1", Arguments: []string{junk}}, ProjectRoot: junk},
		Documents: []*scip.Document{doc,
			{RelativePath: "a_test.go", Text: junk}, {RelativePath: "generated.go", Text: junk},
			{RelativePath: "../outside.go", Text: junk}, {RelativePath: "/absolute.go", Text: junk},
		},
		ExternalSymbols: []*scip.SymbolInformation{{Symbol: junk, Documentation: []string{junk}}},
	}, map[string]*graph.Source{src.Path: src}
}

func artifactTestGraph(t *testing.T, sources map[string]*graph.Source, scipBacked bool) (*graph.Graph, []callSite) {
	t.Helper()
	g := graph.NewGraph("repo", "snapshot")
	var calls []callSite
	for path, original := range sources {
		src := &graph.Source{Path: path, Language: original.Language, Text: original.Text, Hash: original.Hash}
		g.Sources[path] = src
		if !scipBacked && isSyntaxFamily(languageFamily(src.Language)) {
			sites, err := syntaxFacts(context.Background(), g, src)
			if err != nil {
				t.Fatal(err)
			}
			calls = append(calls, sites...)
		}
	}
	addFileFacts(g)
	return g, calls
}

func TestArtifactCacheRetainsImporterData(t *testing.T) {
	idx, sources := artifactCacheFixture(t)
	original := proto.Clone(idx)
	raw := marshalIndex(t, idx)
	g, _ := artifactTestGraph(t, sources, true)
	project := &pb.Project{Root: "nested", Language: "go"}
	data, err := compactSCIPArtifact(context.Background(), g, project, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(idx, original) {
		t.Fatal("compaction mutated original index")
	}
	reader, err := scipArtifactReader(data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var retained scip.Index
	if err := proto.Unmarshal(decoded, &retained); err != nil {
		t.Fatal(err)
	}
	if len(retained.Documents) != 1 || len(retained.ExternalSymbols) != 0 || retained.Metadata.GetToolInfo().GetVersion() != "1" || retained.Metadata.GetProjectRoot() != "" {
		t.Fatalf("unexpected cached structure: %v", &retained)
	}
	doc := retained.Documents[0]
	if doc.Text != "" || doc.Language != "" || len(doc.Occurrences) != 3 || len(doc.Symbols) != 2 || len(doc.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("cache retained ignored document data")
	}
	for _, o := range doc.Occurrences {
		if len(o.OverrideDocumentation) != 0 || len(o.Diagnostics) != 0 || o.SyntaxKind != 0 {
			t.Fatal("cache retained ignored occurrence fields")
		}
	}
	if doc.Occurrences[0].GetSingleLineRange() == nil || doc.Occurrences[0].GetSingleLineEnclosingRange() == nil || len(doc.Occurrences[1].EnclosingRange) == 0 {
		t.Fatal("cache lost typed/legacy ranges")
	}
	if len(doc.Symbols[1].Relationships) != 1 || !doc.Symbols[1].Relationships[0].IsImplementation || !doc.Symbols[1].Relationships[0].IsTypeDefinition {
		t.Fatal("cache lost used relationships or retained unused ones")
	}
	if doc.Symbols[0].GetSignatureDocumentation().GetText() != "func Target()" || doc.Symbols[0].GetSignatureDocumentation().GetLanguage() != "" || len(doc.Symbols[0].GetSignatureDocumentation().GetOccurrences()) != 0 {
		t.Fatal("cache lost signature text or retained unused signature fields")
	}
	if len(data) >= len(decoded) || len(data)*10 >= len(raw) {
		t.Fatalf("expected filtering and compression: raw=%d filtered=%d cached=%d", len(raw), len(decoded), len(data))
	}
}

func TestImportProjectReplaysCompactAndLegacyArtifacts(t *testing.T) {
	idx, sources := artifactCacheFixture(t)
	raw := marshalIndex(t, idx)
	path := filepath.Join(t.TempDir(), "original.scip")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, refreshed := range []bool{false, true} {
		for _, scipBacked := range []bool{false, true} {
			g, calls := artifactTestGraph(t, sources, scipBacked)
			plan := &projectPlan{project: &pb.Project{Root: "nested", Language: "go"}, projectKey: "go|nested", fingerprint: "fixture", artifact: path, toolName: "fixture", toolVersion: "1", scipBacked: scipBacked, preExisting: true, goLSP: refreshed}
			table := newSymbols()
			snap := &pb.Snapshot{ToolVersions: map[string]string{}}
			if err := (Pipeline{}).importProject(context.Background(), g, snap, table, plan); err != nil {
				t.Fatal(err)
			}
			table.apply(g)
			deriveCalls(g, calls, table)
			artifact := g.ProjectArtifacts[plan.projectKey]
			if !bytes.HasPrefix(artifact.Data, []byte(scipArtifactMagic)) || artifact.Fingerprint != plan.fingerprint {
				t.Fatal("importProject persisted raw artifact or changed fingerprint")
			}
			if len(g.EdgeFacts) == 0 {
				t.Fatal("fixture did not exercise relationships")
			}
			for _, cached := range [][]byte{raw, artifact.Data} {
				replay, calls := artifactTestGraph(t, sources, scipBacked)
				cachedPlan := *plan
				cachedPlan.cached = &graph.ProjectArtifact{Fingerprint: plan.fingerprint, Data: cached}
				table := newSymbols()
				if err := (Pipeline{}).importProject(context.Background(), replay, &pb.Snapshot{ToolVersions: map[string]string{}}, table, &cachedPlan); err != nil {
					t.Fatal(err)
				}
				table.apply(replay)
				deriveCalls(replay, calls, table)
				if !bytes.HasPrefix(replay.ProjectArtifacts[plan.projectKey].Data, []byte(scipArtifactMagic)) {
					t.Fatal("legacy cache was forwarded without compaction")
				}
				if !reflect.DeepEqual(normalizeGraph(t, g), normalizeGraph(t, replay)) {
					t.Fatalf("cached replay changed graph (scipBacked=%v, compact=%v)", scipBacked, bytes.HasPrefix(cached, []byte(scipArtifactMagic)))
				}
			}
		}
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, raw) {
		t.Fatal("caller-provided artifact was modified")
	}
}

func TestArtifactCacheRejectsCorruption(t *testing.T) {
	idx, sources := artifactCacheFixture(t)
	g, _ := artifactTestGraph(t, sources, true)
	project := &pb.Project{Root: "nested"}
	data, err := compactSCIPArtifact(context.Background(), g, project, bytes.NewReader(marshalIndex(t, idx)))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{
		[]byte(scipArtifactMagic), append([]byte(scipArtifactMagic), 2),
		append(append([]byte(scipArtifactMagic), scipArtifactVersion), []byte("garbage")...),
		data[:len(data)-1], append(append([]byte(nil), data[:len(data)-1]...), data[len(data)-1]^0xff),
	} {
		reader, err := scipArtifactReader(bad)
		if err == nil {
			_, err = io.ReadAll(reader)
			_ = reader.Close()
		}
		if err == nil {
			t.Fatalf("accepted corrupt cache %x", bad)
		}
	}
	idx.Documents[0].Text = "different source"
	if _, err := compactSCIPArtifact(context.Background(), g, project, bytes.NewReader(marshalIndex(t, idx))); err == nil {
		t.Fatal("compaction hid embedded source mismatch")
	}
	if _, err := compactSCIPArtifact(context.Background(), g, project, bytes.NewReader([]byte{0xff})); err == nil {
		t.Fatal("accepted malformed raw artifact")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := compactSCIPArtifact(ctx, g, project, bytes.NewReader(marshalIndex(t, idx))); err == nil {
		t.Fatal("cache construction ignored cancellation")
	}
}

func TestCachedArtifactRebindsCrossProjectCalls(t *testing.T) {
	callerText := "package caller\nfunc Run() { Target() }\n"
	targetText := "package target\nfunc Target() {}\n"
	from, target := "scip-go gomod caller 1 Run().", "scip-go gomod target 1 Target()."
	callerIndex := &scip.Index{Metadata: &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: "1"}}, Documents: []*scip.Document{{
		RelativePath: "a.go", Text: callerText,
		Symbols: []*scip.SymbolInformation{{Symbol: from, Kind: scip.SymbolInformation_Function}},
		Occurrences: []*scip.Occurrence{
			{Symbol: from, Range: []int32{1, 5, 8}, SymbolRoles: int32(scip.SymbolRole_Definition)},
			{Symbol: target, Range: []int32{1, 13, 19}},
		},
	}}, ExternalSymbols: []*scip.SymbolInformation{{Symbol: target, Kind: scip.SymbolInformation_Function}}}
	targetIndex := &scip.Index{Metadata: &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: "1"}}, Documents: []*scip.Document{{
		RelativePath: "b.go", Text: targetText,
		Symbols:     []*scip.SymbolInformation{{Symbol: target, Kind: scip.SymbolInformation_Function}},
		Occurrences: []*scip.Occurrence{{Symbol: target, Range: []int32{1, 5, 11}, SymbolRoles: int32(scip.SymbolRole_Definition)}},
	}}}
	sources := map[string]*graph.Source{}
	for path, text := range map[string]string{"caller/a.go": callerText, "target/b.go": targetText} {
		sources[path] = &graph.Source{Path: path, Language: "go", Text: []byte(text), Hash: graph.Hash([]byte(text))}
	}
	callerProject := &pb.Project{Root: "caller"}
	g, _ := artifactTestGraph(t, sources, false)
	raw := marshalIndex(t, callerIndex)
	compact, err := compactSCIPArtifact(context.Background(), g, callerProject, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, targetPresent := range []bool{false, true} {
		var graphs []*graph.Graph
		for _, data := range [][]byte{raw, compact} {
			g, calls := artifactTestGraph(t, sources, false)
			table := newSymbols()
			reader, err := scipArtifactReader(data)
			if err != nil {
				t.Fatal(err)
			}
			err = importSCIPReader(context.Background(), g, callerProject, reader, projectHashes(callerProject, g.Sources), false, false, table)
			_ = reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			if targetPresent {
				if err := importSCIPReader(context.Background(), g, &pb.Project{Root: "target"}, bytes.NewReader(marshalIndex(t, targetIndex)), nil, false, false, table); err != nil {
					t.Fatal(err)
				}
			}
			table.apply(g)
			deriveCalls(g, calls, table)
			found := false
			for _, e := range g.EdgeFacts {
				if e.Kind == pb.EdgeKind_EDGE_KIND_CALLS {
					found = true
					if (e.ToFactId != "") != targetPresent {
						t.Fatal("cached caller did not rebind to current target project")
					}
				}
			}
			if !found {
				t.Fatal("caller occurrence was discarded")
			}
			graphs = append(graphs, g)
		}
		if !reflect.DeepEqual(normalizeGraph(t, graphs[0]), normalizeGraph(t, graphs[1])) {
			t.Fatal("compaction changed cross-project rebinding")
		}
	}
}
