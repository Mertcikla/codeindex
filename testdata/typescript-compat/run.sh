#!/bin/sh
set -eu

npm install -g --no-audit --no-fund "typescript-language-server@$LSP_VERSION" "typescript@$TS_VERSION"
if [ "${SCIP_VERSION:-}" != "" ]; then
    npm install -g --no-audit --no-fund "@sourcegraph/scip-typescript@$SCIP_VERSION"
fi

node --version
npm list -g --depth=0
/tests/tools.test -test.v -test.run 'Test(TypeScriptSDK|TypeScriptLanguageServerIntegration)$'
/tests/lsp.test -test.v -test.run 'TestTypeScriptClientCompatibility$'

if [ "${SCIP_VERSION:-}" != "" ] && [ "$CODEINDEX_TEST_TS_HEALTH" = pass ]; then
    # A workspace on TS 7 must still use SCIP's JS SDK for semantic parity.
    CODEINDEX_TEST_TS_LSP=1 CODEINDEX_TEST_TS_WORKSPACE_SDK="$(npm root -g)/typescript" \
        /tests/indexer.test -test.v -test.timeout=5m -test.run '^TestTypeScriptLSPSemanticParity$'
fi
