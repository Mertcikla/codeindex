package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mertcikla/codeindex/config"
	"github.com/mertcikla/codeindex/lsp"
)

// TypeScriptSDK resolves the SDK loaded from an npm executable's real entry
// point. Let Node resolve dependencies so nested, hoisted and pnpm installations
// follow the same rules as the producer. Do not skip a nearer incompatible SDK:
// using a different compiler can change standard-library symbol identities.
func TypeScriptSDK(ctx context.Context, executable string) (string, error) {
	exe, err := ResolveName(executable)
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	const resolve = `
try {
  const req = require('module').createRequire(process.argv[1]);
  const pkg = req.resolve('typescript/package.json');
  const sdk = require('path').join(require('path').dirname(pkg), 'lib', 'tsserver.js');
  if (!require('fs').statSync(sdk).isFile()) throw Error('Not a tsserver.js file: ' + sdk);
  process.stdout.write(JSON.stringify(sdk));
} catch (e) {
  process.stderr.write(e.message);
  process.exitCode = 1;
}`
	probe, stop := context.WithTimeout(ctx, versionTimeout)
	defer stop()
	out, err := Run(probe, "node", []string{"-e", resolve, exe}, "")
	if err != nil {
		return "", fmt.Errorf("cannot resolve TypeScript tsserver.js from %s (TypeScript 7 installations may omit it): %w: %s", exe, err, strings.TrimSpace(string(out)))
	}
	var sdk string
	if err := json.Unmarshal(out, &sdk); err != nil {
		return "", fmt.Errorf("invalid TypeScript SDK resolution from %s: %w", exe, err)
	}
	if info, err := os.Stat(sdk); err != nil || info.IsDir() {
		return "", fmt.Errorf("TypeScript SDK from %s has no tsserver.js at %s", exe, sdk)
	}
	return sdk, nil
}

func checkTypeScriptHealth(ctx context.Context, executable, root string, cfg config.Config) error {
	// Match watch mode's compiler whenever SCIP is installed. Global TypeScript
	// may be 7.x without the JS tsserver bridge. A mismatched SDK is not safe.
	var sdk string
	if scip, err := ResolveName(cfg.Tools.SCIPTypeScript); err == nil {
		sdk, err = TypeScriptSDK(ctx, scip)
		if err != nil {
			return fmt.Errorf("scip-typescript SDK: %w; reinstall the configured SCIP tool with its bundled TypeScript SDK", err)
		}
	}
	client, err := lsp.StartTypeScript(ctx, executable, root, sdk)
	if err != nil {
		return err
	}
	defer client.Close()
	health := filepath.Join(root, "__codeindex_health__.ts")
	text := []byte("function health(value: number): number { return value; }\nhealth(1);\n")
	if err := os.WriteFile(health, text, 0600); err != nil {
		return err
	}
	if err := client.Sync(health, text); err != nil {
		return err
	}
	if err := client.CheckTypeScriptDocument(ctx, health); err != nil {
		return err
	}
	locations, err := client.Definition(ctx, health, lsp.Position{Line: 1, Character: 1})
	if err != nil {
		return err
	}
	for _, loc := range locations {
		if loc.URI == lsp.URI(health) && loc.Range.Start.Line == 0 && loc.Range.Start.Character == 9 {
			return nil
		}
	}
	return fmt.Errorf("TypeScript language server returned no matching health definition")
}
