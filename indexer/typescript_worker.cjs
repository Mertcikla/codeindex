// Runs with the exact compiler and FileIndexer shipped by scip-typescript 0.4.0.
// stdout is a JSON-lines protocol; diagnostics must never corrupt that channel.
const fs = require('fs'), path = require('path'), readline = require('readline');
const dir = process.argv[1], root = process.argv[2];
const ts = require('module').createRequire(path.join(dir, 'main.js'))('typescript');
const { FileIndexer } = require(path.join(dir, 'FileIndexer'));
const { Input } = require(path.join(dir, 'Input'));
const { Packages } = require(path.join(dir, 'Packages'));
const { scip } = require(path.join(dir, 'scip'));
process.chdir(root);
console.log = (...args) => console.error(...args);
const states = [], visited = new Set(), sources = new Map(), overlays = new Map();
function config(file) {
  file = path.resolve(file);
  if (visited.has(file)) return;
  visited.add(file);
  const read = ts.readConfigFile(file, ts.sys.readFile);
  if (read.error) throw Error('Cannot read ' + file);
  const cfg = ts.parseJsonConfigFileContent(read.config, ts.sys, path.dirname(file));
  if (cfg.errors.some(e => e.code !== 18003)) throw Error('Invalid ' + file);
  for (const ref of cfg.projectReferences || []) config(fs.statSync(ref.path).isDirectory() ? path.join(ref.path, 'tsconfig.json') : ref.path);
  if (cfg.fileNames.length) states.push({ cfg, file });
}
for (const name of fs.readdirSync(root).filter(n => /^tsconfig.*\.json$/.test(n)).sort()) config(name);
if (!states.length) throw Error('No configured TypeScript project');
function update(texts, active = states, allowedChanges = null) {
  for (const [rel, text] of Object.entries(texts)) overlays.set(path.resolve(root, rel), text);
  for (const state of active) {
    const options = { ...state.cfg.options, noEmit: false, declaration: true, emitDeclarationOnly: true };
    const host = ts.createCompilerHost(options), read = host.readFile;
    host.readFile = file => overlays.has(file) ? overlays.get(file) : read(file);
    const get = host.getSourceFile;
    host.getSourceFile = (file, version, error, fresh) => {
      const key = file + '|' + JSON.stringify(version), text = host.readFile(file), previous = sources.get(key);
      if (previous && previous.text === text) return previous;
      if (previous && allowedChanges && !allowedChanges.has(file)) throw Error('Dependency input changed: ' + file);
      const source = get(file, version, error, fresh);
      if (source) sources.set(key, source);
      return source;
    };
    state.program = ts.createProgram({ rootNames: state.cfg.fileNames, options, host, oldProgram: state.program });
    state.checker = state.program.getTypeChecker();
  }
}
function signature(state, file) {
  const source = state.program.getSourceFile(file);
  if (!source || !state.cfg.fileNames.includes(file)) return null;
  let result = '';
  const emitted = state.program.emit(source, (name, text) => { if (name.endsWith('.d.ts')) result += text; }, undefined, true);
  if (emitted.emitSkipped || !result) throw Error('Cannot establish declaration signature for ' + file);
  return result;
}
function diagnostics(state, file) {
  const source = state.program.getSourceFile(file);
  return state.program.getSemanticDiagnostics(source)
    .filter(d => d.category === ts.DiagnosticCategory.Error)
    .map(d => d.code + ":" + ts.flattenDiagnosticMessageText(d.messageText, " "))
    .sort().join("\n");
}
function document(state, file, previousDiagnostics) {
  const source = state.program.getSourceFile(file);
  if (state.program.getSyntacticDiagnostics(source).length || diagnostics(state,file) !== previousDiagnostics) throw Error('TypeScript diagnostics: ' + file);
  const doc = new scip.Document({ relative_path: path.relative(root, file), occurrences: [] });
  new FileIndexer(state.checker, { cwd: root, projectRoot: root, maxFileByteSizeNumber: 1048576 }, new Input(file, source.getText()), doc, new Map(), new Map(), new Packages(root), source).index();
  return doc;
}
update({});
// Warm the checker while the CLI's initial full SCIP baseline runs.
for (const state of states) state.program.getSemanticDiagnostics();
process.stdout.write(JSON.stringify({ ready: true }) + '\n');
readline.createInterface({ input: process.stdin }).on('line', line => {
  try {
    const request = JSON.parse(line), files = Object.keys(request.after).map(f => path.resolve(root, f));
    const active = states.filter(state => files.some(file => state.cfg.fileNames.includes(file)));
    const allowed = new Set(files);
    update(request.before, active, allowed);
    const signatures = active.map(state => files.map(file => signature(state, file)));
    const previousDiagnostics = active.map((state, i) => files.map((file, j) =>
      signatures[i][j] === null ? null : diagnostics(state, file)));
    update(request.after, active, allowed);
    const docs = [];
    active.forEach((state, i) => files.forEach((file, j) => {
      const before = signatures[i][j];
      if (before === null) return;
      if (signature(state, file) !== before) throw Error('Declaration API changed: ' + file);
      docs.push(document(state, file, previousDiagnostics[i][j]));
    }));
    if (!docs.length) throw Error('Changed files not covered by compiler configurations');
    const index = new scip.Index({ metadata: new scip.Metadata({ tool_info: new scip.ToolInfo({ version: '0.4.0' }) }), documents: docs });
    process.stdout.write(JSON.stringify({ data: Buffer.from(index.serializeBinary()).toString('base64') }) + '\n');
  } catch (error) { process.stdout.write(JSON.stringify({ error: String(error) }) + '\n'); }
});
