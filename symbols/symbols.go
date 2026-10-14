// Package symbols handles SCIP symbol metadata behind the codeindex fact model.
// Callers use facts and opaque version maps without depending on SCIP types.
package symbols

import (
	"encoding/json"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/graph"
	scip "github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

func symbolPackage(symbol *scip.Symbol) string {
	key, _ := json.Marshal([]string{symbol.Scheme, symbol.Package.Manager, symbol.Package.Name})
	return string(key)
}

// Versions returns opaque package keys and their snapshot-specific versions.
// Only packages defined by facts are included. Dependency versions remain
// identifying, and ambiguous packages (multiple versions) are omitted.
func Versions(g *graph.Graph) map[string]string {
	versions, ambiguous := map[string]string{}, map[string]bool{}
	for _, f := range g.Facts {
		symbol, err := scip.ParseSymbol(f.SymbolKey)
		if err != nil || symbol.Package == nil {
			continue
		}
		key := symbolPackage(symbol)
		if previous, ok := versions[key]; ok && previous != symbol.Package.Version {
			ambiguous[key] = true
		}
		versions[key] = symbol.Package.Version
	}
	for key := range ambiguous {
		delete(versions, key)
	}
	return versions
}

func rewriteFactSymbolVersions(f *pb.CodeFact, versions map[string]string, fingerprint bool) {
	definition, err := scip.ParseSymbol(f.SymbolKey)
	if err != nil || definition.Package == nil {
		return
	}
	key, original := symbolPackage(definition), definition.Package.Version
	version, ok := versions[key]
	if !ok || (fingerprint && version != original) {
		return
	}
	if fingerprint {
		version = "@snapshot-version"
	}
	rewrite := func(raw string) string {
		symbol, err := scip.ParseSymbol(raw)
		if err != nil || symbol.Package == nil || symbolPackage(symbol) != key || symbol.Package.Version != original {
			return raw
		}
		if version == symbol.Package.Version {
			return raw
		}
		symbol.Package.Version = version
		return scip.VerboseSymbolFormatter.FormatSymbol(symbol)
	}
	f.QualifiedName, f.SymbolKey = rewrite(f.QualifiedName), rewrite(f.SymbolKey)
	for _, evidence := range f.Evidence {
		if evidence.GetProducer() == "scip" {
			evidence.OriginalId = rewrite(evidence.OriginalId)
		}
	}
}

// ForFingerprint returns a deep copy whose defining package version is
// normalized in its symbol metadata. The original fact is left unchanged;
// source anchors, logical keys and other content are preserved.
func ForFingerprint(f *pb.CodeFact, versions map[string]string) *pb.CodeFact {
	copy := proto.Clone(f).(*pb.CodeFact)
	rewriteFactSymbolVersions(copy, versions, true)
	return copy
}

// Restore updates a fact's symbol metadata to the supplied snapshot versions.
// References to other packages or explicit other versions remain untouched.
func Restore(f *pb.CodeFact, versions map[string]string) {
	rewriteFactSymbolVersions(f, versions, false)
}
