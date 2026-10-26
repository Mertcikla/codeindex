#!/bin/sh
# Run from any directory. Each row gets a fresh npm installation and filesystem.
set -eu
cd "$(dirname "$0")/.."
image=codeindex-typescript-compat
docker build -f testdata/typescript-compat/Dockerfile -t "$image" .

while read -r lsp ts scip health; do
    [ "$scip" != none ] || scip=
    echo "Testing language-server=$lsp typescript=$ts scip=${scip:-none} health=$health"
    docker run --rm \
        -e "LSP_VERSION=$lsp" -e "TS_VERSION=$ts" -e "SCIP_VERSION=$scip" \
        -e "CODEINDEX_TEST_TS_HEALTH=$health" "$image"
done <<'MATRIX'
4.3.4 5.6.3 none fail
4.4.1 4.9.5 none pass
4.4.1 5.6.3 none pass
5.1.3 5.9.3 none pass
5.1.3 6.0.3 none pass
6.0.2 6.0.3 none pass
5.1.3 7.0.2 none fail
6.0.2 7.0.2 none fail
4.4.1 7.0.2 0.4.0 pass
5.1.3 7.0.2 0.4.0 pass
6.0.2 7.0.2 0.5.2 pass
MATRIX
