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
content always produces the same bytes per snapshot. Snapshot, fact and edge ids
embed build time and therefore differ between runs; use `digest` or the parity
report for cross-run comparison.

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
