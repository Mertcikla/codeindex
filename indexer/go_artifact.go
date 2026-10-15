package indexer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// mergeGoArtifact replaces whole package documents while keeping the original
// document order. API and type sets are unchanged, so the previous full scan's
// implementation relationships remain valid (a package scan cannot rediscover
// relationships to interfaces in unimported sibling packages).
func mergeGoArtifact(ctx context.Context, cached []byte, artifact string, project *pb.Project, g *graph.Graph, patterns []string) error {
	old, err := scipArtifactReader(cached)
	if err != nil {
		return err
	}
	defer func() { _ = old.Close() }()
	fresh, err := os.Open(artifact)
	if err != nil {
		return err
	}
	defer func() { _ = fresh.Close() }()
	read := func(reader io.Reader) ([]*scip.Document, error) {
		var docs []*scip.Document
		visitor := &scip.IndexVisitor{VisitDocument: func(_ context.Context, doc *scip.Document) error {
			if source := g.Sources[scipDocumentPath(project, doc.RelativePath)]; source != nil {
				docs = append(docs, doc)
			}
			return ctx.Err()
		}}
		err := visitor.ParseStreaming(ctx, reader)
		return docs, err
	}
	previous, err := read(old)
	if err != nil {
		return err
	}
	current, err := read(fresh)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, pattern := range patterns {
		wanted[strings.TrimPrefix(pattern, "./")] = true
	}
	replacements := map[string]*scip.Document{}
	for _, doc := range current {
		if !wanted[path.Dir(doc.RelativePath)] || replacements[doc.RelativePath] != nil {
			return fmt.Errorf("unexpected Go package document %s", doc.RelativePath)
		}
		replacements[doc.RelativePath] = doc
	}
	if len(replacements) == 0 {
		return fmt.Errorf("empty Go package index")
	}
	// A single module's own package version may change with HEAD/tag. Rebase
	// its cached symbols, including references, while leaving dependencies alone.
	moduleVersion := func(docs []*scip.Document) (string, string, error) {
		key, version := "", ""
		for _, doc := range docs {
			for _, info := range doc.Symbols {
				symbol, err := scip.ParseSymbol(info.Symbol)
				if err != nil || symbol.Package == nil {
					continue
				}
				v := symbol.Package.Version
				symbol.Descriptors, symbol.Package.Version = nil, ""
				k := scip.VerboseSymbolFormatter.FormatSymbol(symbol)
				if key != "" && (key != k || version != v) {
					return "", "", fmt.Errorf("ambiguous Go module version")
				}
				key, version = k, v
			}
		}
		return key, version, nil
	}
	oldKey, oldVersion, err := moduleVersion(previous)
	if err != nil {
		return err
	}
	newKey, newVersion, err := moduleVersion(current)
	if err != nil || oldKey == "" || oldKey != newKey {
		return fmt.Errorf("Go module identity changed")
	}
	rewrite := func(raw string) string {
		if oldVersion == newVersion {
			return raw
		}
		symbol, err := scip.ParseSymbol(raw)
		if err != nil || symbol.Package == nil || symbol.Package.Version != oldVersion {
			return raw
		}
		descriptors := symbol.Descriptors
		symbol.Descriptors, symbol.Package.Version = nil, ""
		if scip.VerboseSymbolFormatter.FormatSymbol(symbol) != oldKey {
			return raw
		}
		symbol.Descriptors, symbol.Package.Version = descriptors, newVersion
		return scip.VerboseSymbolFormatter.FormatSymbol(symbol)
	}
	// scip-go describes interface methods as terms when their syntax is loaded,
	// but as methods when loaded from export data. Resolve those references to
	// the unchanged full-project definitions before importing the package scan.
	definitionKey := func(raw string) string {
		symbol, err := scip.ParseSymbol(raw)
		if err != nil || len(symbol.Descriptors) == 0 {
			return raw
		}
		last := symbol.Descriptors[len(symbol.Descriptors)-1]
		if last.Suffix == scip.Descriptor_Method && last.Disambiguator == "" {
			last.Suffix = scip.Descriptor_Term
		}
		return scip.VerboseSymbolFormatter.FormatSymbol(symbol)
	}
	definitions := map[string]string{}
	for _, doc := range previous {
		for _, occurrence := range doc.Occurrences {
			if scip.SymbolRole_Definition.Matches(occurrence) {
				raw := rewrite(occurrence.Symbol)
				key := definitionKey(raw)
				if existing := definitions[key]; existing != "" && existing != raw {
					return fmt.Errorf("ambiguous Go definition %s", key)
				}
				definitions[key] = raw
			}
		}
	}
	for _, doc := range current {
		for _, occurrence := range doc.Occurrences {
			if defined := definitions[definitionKey(occurrence.Symbol)]; defined != "" {
				occurrence.Symbol = defined
			}
		}
		for _, info := range doc.Symbols {
			if defined := definitions[definitionKey(info.Symbol)]; defined != "" {
				info.Symbol = defined
			}
		}
	}
	for i, doc := range previous {
		for _, occurrence := range doc.Occurrences {
			occurrence.Symbol = rewrite(occurrence.Symbol)
		}
		relationships := map[string][]*scip.Relationship{}
		for _, info := range doc.Symbols {
			info.Symbol = rewrite(info.Symbol)
			for _, relationship := range info.Relationships {
				relationship.Symbol = rewrite(relationship.Symbol)
			}
			relationships[info.Symbol] = info.Relationships
		}
		if !wanted[path.Dir(doc.RelativePath)] {
			continue
		}
		replacement := replacements[doc.RelativePath]
		if replacement == nil {
			return fmt.Errorf("missing Go package document %s", doc.RelativePath)
		}
		for _, info := range replacement.Symbols {
			info.Relationships = relationships[info.Symbol]
		}
		previous[i] = replacement
		delete(replacements, doc.RelativePath)
	}
	if len(replacements) != 0 {
		return fmt.Errorf("Go package document membership changed")
	}
	output, err := os.Create(artifact + ".merged")
	if err != nil {
		return err
	}
	defer func() { _ = output.Close(); _ = os.Remove(output.Name()) }()
	write := func(field protowire.Number, message proto.Message) error {
		data, err := proto.Marshal(message)
		if err != nil {
			return err
		}
		header := protowire.AppendTag(nil, field, protowire.BytesType)
		header = protowire.AppendVarint(header, uint64(len(data)))
		if _, err := output.Write(header); err != nil {
			return err
		}
		_, err = output.Write(data)
		return err
	}
	if err := write(1, &scip.Metadata{ToolInfo: &scip.ToolInfo{Version: "0.2.7"}}); err != nil {
		return err
	}
	for _, doc := range previous {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := write(2, doc); err != nil {
			return err
		}
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Rename(output.Name(), artifact)
}
