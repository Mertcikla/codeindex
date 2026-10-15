package indexer

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	scip "github.com/scip-code/scip/bindings/go/scip"
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
	previous, err := readSCIPDocuments(ctx, g, project, old)
	if err != nil {
		return err
	}
	current, err := readSCIPDocuments(ctx, g, project, fresh)
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
	rewrite, err := projectVersionRewriter(previous, current, "")
	if err != nil {
		return err
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
	return writeSCIPDocuments(ctx, artifact, "0.2.7", previous)
}
