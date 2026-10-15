# codeindex

`codeindex` extracts facts from a codebase: declarations, references, calls,
implementations, and infrastructure/config surfaces. It is the standalone
engine that was extracted from [tld](https://github.com/Mertcikla/tld) — the pipeline, protobuf fact model, parser
stack, and a thin persistence contract.

It runs by itself:

```sh
go run ./cmd/codeindex index --out /tmp/ci-out /path/to/repo
```

## Layout

| Path | Contents |
| --- | --- |
| `cmd/codeindex` | thin CLI: index a path, write the JSON store, print/verify goldens |
| `indexer` | the pipeline: discover, tree-sitter, scip, relationships, infra |
| `graph` | the fact model: sources, facts, edges, logical keys, stable ids |
| `symbols` | SCIP symbol normalization and snapshot package-version restoration |
| `parser` | infra/config scanners (Dockerfile, compose, k8s, helm, terraform, openapi, proto) |
| `tools` | external indexer toolchain resolution (`scip-go`, `scip-typescript`, …) |
| `config` | configuration defaults plus `CODEINDEX_*` env overlay |
| `gitstate` | git revision and checkout helpers |
| `watch` | filesystem change detection with debounce |
| `remote` | remote reference parsing, managed clones, canonical remote keys |
| `identity` | stable repository identity across checkouts and machines |
| `ingest` | snapshot preparation shared by CLIs and servers |
| `parity` | deterministic report of one build (counts by kind/path) |
| `community` | Louvain grouping over the file dependency graph |
| `digest` | id-stable sha256 of a full extraction, for byte-parity checks |
| `store` | the persistence interface hosts implement |
| `internal/jsonstore` | file-backed store: canonical JSON documents per snapshot |

## The store contract

`store.Store` (`store/store.go`) is everything the engine needs from a host:
publish a snapshot, read it back, resolve repository identity. Hosts implement
it — tld implements it on bun/SQLite/Postgres — and the module ships
`internal/jsonstore` for standalone runs:

```
<out>/<repository-id>/<snapshot-id>/snapshot.json
<out>/<repository-id>/<snapshot-id>/facts.json
<out>/<repository-id>/<snapshot-id>/edges.json
<out>/<repository-id>/<snapshot-id>/sources.json
<out>/<repository-id>/<snapshot-id>/artifacts.json
<out>/<repository-id>/latest
```

Documents are canonically ordered (sorted slices, stable encoders), so the same
content always produces the same bytes per snapshot. Published repository, snapshot, fact and edge IDs are allocated 64-bit integers
represented as decimal strings. Extraction-local hashes are translated at publication;
fact logical keys and content hashes remain unchanged. Edge logical keys are
64-character lowercase SHA-256 digests of length-delimited kind, source logical
key, target logical key (or unresolved symbol), and optional qualifier. Public
export qualifiers are included before hashing; entity IDs and observation sites
are excluded. The JSON store records its index
format, silently clears legacy snapshots, and persists its sequence under a process
lock that is released on exit. IDs differ between runs; use `digest` or the parity
report for cross-run comparison.

Index format 3 clears format-1/2 snapshot directories while retaining repository
metadata and unrelated files. Reindex to populate the new edge keys. The
standalone store retains JSON anchors and evidence on facts. Edges emit only
relationship identity, endpoints, unresolved symbols and weights; observation
sites are used internally for deduplication and are not persisted. tld stores
the same relationship fields without an edge provenance column.

Project artifacts are private import caches, not copies of the original SCIP
output. `importProject` streams the original index into a filtered cache and
compresses it with zlib under a versioned `SCIP\0` header. It keeps only captured,
non-test documents; non-local occurrences with valid ranges; and the symbol
metadata, signatures and implementation/type-definition relationships used by
the importer. Embedded source text, external symbols, diagnostics, hover
overrides and unused fields are omitted. Caller-provided artifacts remain
untouched. Cached and fresh imports produce the same facts and edges.

Older raw caches remain readable and are compacted when replayed. Stores treat
`ProjectArtifact.Data` as opaque bytes; no database or public API schema change
is required. Existing content-addressed database rows are not rewritten by this
producer change. If the importer needs additional fields in future, bump the
cache format and relationship version to invalidate insufficient caches.

## Watch semantic layers (Go and TypeScript)

Hosts can create `indexer.NewWatchSession(ctx, cfg, root)` before the initial
SCIP build and attach it to `ingest.Engine.Semantics`. The session warms gopls
and typescript-language-server in parallel, lives for the checkout's watch
lifetime, and is used only for the working tree. Close it when watching stops.
Historical checkouts always use
SCIP; no editor overlay is shared with a historical build.

The Go adapter replaces changed function-body occurrences through LSP definition
queries, synchronizing the entire edit batch before querying and checking pull
diagnostics. It retains the baseline's declaration identities, documentation,
signatures and implementation relationships, and remaps shifted source ranges.
External calls retain their SCIP package/descriptor keys. Output uses the same
reduced SCIP cache as a regular build: unused SCIP metadata is omitted rather
than recreated through LSP.

This first gate supports body-only changes in a single ordinary Go module with
scip-go 0.2.7. API, import, documentation, module/build configuration and file
membership changes, unsupported external declarations, workspaces, replacements,
or unhealthy servers fall back to SCIP. An explicit `GOWORK=off` disables
ancestor workspace detection. Fallbacks invalidate LSP overlays before reuse.
The existing manifest/drift verification and immutable dirty/commit snapshot
publication apply to both paths.

Configure the server with `tools.gopls` or `CODEINDEX_GOPLS`. `tools.All` and
`tools.ForFamilies` include gopls; its availability requires an LSP handshake,
not merely a successful version banner. tld exposes this in doctor and its
repository tool checklist, including probe errors.

Run real-tool graph parity with:

```sh
CODEINDEX_TEST_LSP=1 go test ./indexer -run TestGoLSP -v
CODEINDEX_LSP_REPOS=/path/to/go-repo:/path/to/another-go-repo \
  go test ./indexer -run TestGoLSPRepositoryParity -v
```

The repository test copies its inputs, adds references in two source files, and
compares every persisted fact/edge field after removing snapshot-specific IDs.
Gorilla mux v1.8.1, pflag v1.0.10, Cobra v1.10.2, bun v1.2.18 and Gorilla
WebSocket v1.5.3 pass this gate. In one local two-file run, bun's 1,510 facts /
8,606 edges took 0.90 s through LSP versus 2.22 s through fresh SCIP; WebSocket
took 0.46 s versus 0.95 s. These are full pipeline times, not server-only query
times, and are not performance guarantees. Declaration, membership and build
changes still use SCIP.

The TypeScript adapter supports body edits in functions, methods and arrows
with explicit return annotations, including TSX, against scip-typescript 0.4.0.
It uses the TypeScript SDK bundled with that SCIP installation to keep compiler
and standard-library declaration identities consistent. Definition queries map
back to captured SCIP identities, including aliases, constructors and merged
declarations. External identities are learned from baseline definition locations;
an unseen external target uses SCIP. Synchronous tsserver diagnostics establish
that queries apply to the current edit batch.

Inferred APIs, constructors, object/type identities inside changed bodies,
imports, configuration and file membership changes, ambiguous configurations,
or unavailable servers/SDKs use SCIP. This conservative boundary preserves all
consumed tld facts and edges; it does not attempt to synthesize unused SCIP
metadata. Both paths publish the same reduced artifact and immutable dirty/commit
snapshots. Rust and Python remain the next languages, in that order.

Configure `tools.typescript_language_server` or
`CODEINDEX_TYPESCRIPT_LANGUAGE_SERVER` (tld also accepts
`TLD_INDEX_TYPESCRIPT_LANGUAGE_SERVER`). Doctor and the repository UI require a
successful initialize handshake and tsserver-bridge capability, alongside the
version probe. Install with `npm install -g typescript-language-server typescript`.

```sh
CODEINDEX_TEST_TS_LSP=1 go test ./indexer -run TestTypeScriptLSP -v
CODEINDEX_TS_LSP_REPOS=/path/to/typescript-repo:/path/to/another-repo \
  go test ./indexer -run TestTypeScriptLSPRepositoryParity -v
```

The real-tool fixture checks complete graph equality across TS/TSX, Unicode,
aliases, overloads, constructors, external calls, reverts, multiple edited files
and safe fallbacks. Representative one/two-file changes in points-on-curve and
path-data-parser also pass complete graph equality. Extraction version 10 fixes
scip-typescript's implicit UTF-16 source positions and stabilizes call selection
for merged declarations; older extraction baselines are recaptured.

The five artifacts captured from the tld database reduced from 66,271,827 to
3,616,709 bytes (94.5%), with identical graph facts and edges on replay. Run
`TestArtifactCacheRealData` with `CODEINDEX_ARTIFACT_FIXTURE` pointing to an
isolated fixture directory to repeat the measurement; normal tests use tool-free
fixtures and cover legacy caches, corruption and cross-project rebinding.

## Byte-parity goldens

`testdata/parity/<repo>.json` holds a parity report plus a `digest` fingerprint
of one extraction, both free of volatile identifiers. The CLI writes a golden
when missing and byte-compares it when present:

```sh
go run ./cmd/codeindex index --golden-dir testdata/parity /path/to/repo
```

These goldens were captured before the extraction from tld and are the
regression gate for that migration: any drift in the pipeline shows up as a
byte mismatch, not a silently different count. Goldens are toolchain-pinned —
`scip-*` versions change what gets extracted.

The historical `go-gin` and `ts-prisma` digests use readable edge keys and must
be recaptured for format 3 from their original checkouts and pinned toolchains;
their source revisions are not recorded in these files. Do not replace them
with hashes from unrelated revisions. `TestSnapshotCompactEdgeKeyGolden` pins
the new digest format with a deterministic fixture independent of external tools.

Note: the `signature` and `documentation` fields are excluded from the digest.
The tree-sitter and SCIP producers render both differently for facts they both
observe, and the winner depends on processing order; the digest pins identity
(kind, logical key, anchors, symbol keys) and content (body hashes).

## Development

```sh
go build ./...
go test ./...
task parity -- /path/to/repo   # byte-compare against the goldens
```

The module depends on the generated protobuf types at
`buf.build/gen/go/tldiagramcom/diagram/protocolbuffers/go` — the same package
tld uses, so fact types are shared across the module boundary.

### Public package exports

Indexing extracts `exports` from `package.json` and `jsr.json` as file reference
edges with `package-exports` evidence. Each witness records the public subpath,
condition chain, declared target and exact manifest span. Conditions that resolve
to the same file share an edge within that entry point; distinct subpaths and
conditional targets retain separate identities. Changed targets and entry points
therefore participate in snapshot relationship diffs.

Targets must be repository-relative captured JavaScript/TypeScript files. An
explicit `sourceMappingURL` can resolve a captured build output to its source when
the captured version-3 map identifies exactly one captured source. The map itself
is retained as evidence. Without a map, a captured build output remains the exact
export target; missing outputs are not guessed back to `src/`. Inline/remote or
ambiguous maps, wildcard exports and fallback arrays are currently skipped.
This does not run a build or fetch additional files. Relationship configuration
versioning invalidates previously cached extraction on the next index.
