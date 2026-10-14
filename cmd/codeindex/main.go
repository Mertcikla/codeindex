// Command codeindex indexes a codebase and writes its facts to a JSON store.
//
// It is the standalone face of the codeindex module: one command, no server,
// no database. The pipeline and store interface are the same ones hosts embed,
// so output captured here is a faithful baseline for parity checks.
//
// Usage:
//
//	codeindex index [flags] <path>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pb "buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go/codeindex/v1"
	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/digest"
	"github.com/mertcikla/codeindex/graph"
	"github.com/mertcikla/codeindex/indexer"
	"github.com/mertcikla/codeindex/internal/jsonstore"
	"github.com/mertcikla/codeindex/parity"
)

// golden mirrors the file format captured by golden runs: the parity report
// plus the id-stable digest of the full projection.
type golden struct {
	Report parity.Report `json:"report"`
	Digest string        `json:"digest"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "codeindex:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// The optional "index" subcommand token is accepted for readability
	// ("codeindex index <path>") but not required ("codeindex <path>").
	if len(args) > 0 && args[0] == "index" {
		args = args[1:]
	}
	args = reorderArgs(args)
	fs := flag.NewFlagSet("codeindex", flag.ContinueOnError)
	var (
		outDir     = fs.String("out", "codeindex-out", "directory the JSON store writes to")
		configPath = fs.String("config", "", "optional config yaml (defaults plus CODEINDEX_* env overlay)")
		excludes   multiFlag
		showReport = fs.Bool("report", false, "print the parity report as JSON")
		showDigest = fs.Bool("digest", false, "print the id-stable digest of the extraction")
		goldenDir  = fs.String("golden-dir", "", "write or byte-compare parity goldens in this directory")
		progress   = fs.Bool("progress", false, "print pipeline stage progress to stderr")
	)
	fs.Var(&excludes, "exclude", "repository-relative path to exclude (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: codeindex index [flags] <path>")
	}
	target := fs.Arg(0)

	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", target, err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", target)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	st, err := jsonstore.Open(*outDir)
	if err != nil {
		return err
	}
	defer st.Close()

	repositoryID := graph.RepositoryID(abs)
	pipeline := indexer.Pipeline{Config: cfg, RepositoryID: repositoryID}
	req := &pb.IndexRequest{Directory: abs, Exclude: excludes}

	var progressFunc indexer.ProgressFunc
	if *progress {
		progressFunc = func(p indexer.Progress) {
			if p.Detail != "" {
				fmt.Fprintf(os.Stderr, "%s: %s\n", p.Stage, p.Detail)
				return
			}
			fmt.Fprintf(os.Stderr, "%s: %d/%d\n", p.Stage, p.Current, p.Total)
		}
	}

	snap, g, err := pipeline.Build(context.Background(), req, progressFunc)
	if err != nil {
		return fmt.Errorf("index %s: %w", abs, err)
	}
	if err := st.Publish(context.Background(), abs, snap, g); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}

	repositoryID = snap.GetRepositoryId()
	facts, edges, err := readBack(context.Background(), st, snap.GetId())
	if err != nil {
		return err
	}
	report := parity.Summarize(snap, g)
	digestValue := digest.Snapshot(snap, g.SortedFacts(), g.SortedEdgeFacts())

	fmt.Printf("snapshot %s\nrepository %s\nsources %d\nprojects %d\nfacts %d\nedges %d\noutput %s\n",
		snap.GetId(), snap.GetRepositoryId(), report.Sources, report.Projects, len(facts), len(edges), filepath.Join(*outDir, repositoryID, snap.GetId()))
	if report.Warnings > 0 {
		fmt.Printf("warnings %d\n", report.Warnings)
		for _, w := range snap.GetWarnings() {
			fmt.Printf("  %s\n", w)
		}
	}

	if *showReport {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
	}
	if *showDigest {
		fmt.Printf("digest %s\n", digestValue)
	}

	if *goldenDir != "" {
		return verifyGolden(*goldenDir, filepath.Base(abs), golden{Report: report, Digest: digestValue})
	}
	return nil
}

// readBack loads the published snapshot through the store, proving the store
// round-trips the graph the pipeline built.
func readBack(ctx context.Context, st *jsonstore.Store, snapshotID string) ([]*pb.CodeFact, []*pb.EdgeFact, error) {
	facts, err := st.Facts(ctx, snapshotID, pb.FactKind_FACT_KIND_UNSPECIFIED, "", "", 0)
	if err != nil {
		return nil, nil, fmt.Errorf("read back facts: %w", err)
	}
	edges, err := st.EdgeFacts(ctx, snapshotID, pb.EdgeKind_EDGE_KIND_UNSPECIFIED, "", "", 0)
	if err != nil {
		return nil, nil, fmt.Errorf("read back edges: %w", err)
	}
	reloaded, err := st.LoadGraph(ctx, snapshotID)
	if err != nil {
		return nil, nil, fmt.Errorf("reload graph: %w", err)
	}
	if len(reloaded.Facts) != len(facts) || len(reloaded.EdgeFacts) != len(edges) {
		return nil, nil, fmt.Errorf("reloaded graph mismatch: %d/%d facts, %d/%d edges",
			len(reloaded.Facts), len(facts), len(reloaded.EdgeFacts), len(edges))
	}
	if len(reloaded.Sources) == 0 && len(facts) > 0 {
		return nil, nil, errors.New("reloaded graph has no sources")
	}
	return facts, edges, nil
}

// verifyGolden writes the golden when none exists yet and otherwise compares
// it byte-for-byte with a freshly computed one.
func verifyGolden(dir, name string, current golden) error {
	path := filepath.Join(dir, name+".json")
	encoded, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	existing, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			return err
		}
		fmt.Printf("golden %s written\n", path)
		return nil
	}
	if string(existing) != string(encoded) {
		return fmt.Errorf("parity mismatch for %s: %s does not match the fresh extraction", name, path)
	}
	fmt.Printf("golden %s matches\n", path)
	return nil
}

// reorderArgs moves positional arguments after the flags so both
// "codeindex index --out dir path" and "codeindex index path --out dir" work;
// the flag package otherwise stops parsing at the first positional.
func reorderArgs(args []string) []string {
	valueFlags := map[string]bool{
		"--out": true, "-out": true,
		"--config": true, "-config": true,
		"--golden-dir": true, "-golden-dir": true,
		"--exclude": true, "-exclude": true,
	}
	flags := make([]string, 0, len(args))
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) > 1 && arg[0] == '-' {
			flags = append(flags, arg)
			name := arg
			if eq := strings.Index(arg, "="); eq >= 0 {
				name = arg[:eq]
			}
			if !strings.Contains(arg, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return append(flags, positional...)
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return fmt.Sprint(*m) }

func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}
