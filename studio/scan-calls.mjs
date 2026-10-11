#!/usr/bin/env node
// Static scan of the Management API calls the pinned Studio source makes: studio/calls.json.
//
//   node studio/scan-calls.mjs --fetch            fetch the pinned commit, apply studio/patches, scan, write studio/calls.json
//   node studio/scan-calls.mjs --fetch --check    the same, but fail when studio/calls.json differs from the result
//   node studio/scan-calls.mjs --src DIR          scan a tree that holds apps/studio and packages/ (patched already)
//   node studio/scan-calls.mjs --src DIR --summary   print counts instead of writing
//
// It needs the `typescript` package pinned in studio/scan/package.json (`npm ci --prefix studio/scan`) and,
// with --fetch, git and network access to github.com/supabase/supabase. The output is deterministic: the same
// source gives the same bytes. internal/api/studio_calls_test.go joins the file with the routes the API
// server implements; the rules are in docs/reference/studio-platform-calls.md ("Method").
//
// What counts as a call. The scan parses every TypeScript and JavaScript file of apps/studio/{data,lib,components,
// hooks,state,pages,app,routes} and packages/{common,ui,ui-patterns} (tests, mocks and declaration files left out)
// and finds:
//   - get, post, put, patch, del, head and client.GET/POST/... imported from data/fetchers: the openapi-fetch
//     client, whose first argument is a path;
//   - fetchGet, fetchPost, fetchPut, fetchPatch, fetchDelete and fetchHeadWithTimeout, and fetch() with a path under
//     /platform, /v1 or /v2 (the method comes from the `method` member of the options, GET without one).
// The first argument is resolved to a path when it is a string, a template literal (a `${x}` becomes {x}), a
// conditional (both branches, with IS_PLATFORM folded to true), a sum of strings, or a const of this file or a
// member of a const object of this file. A path that cannot be resolved (a parameter, a call) goes to
// `unresolved`, and the runtime crawl (tests/linux/crawl) is the authority on it. A leading ${API_URL} is the
// /platform prefix of the platform build. Strings that look like an API path and are not the argument of a
// call go to `mentions`, so a wrapper the scan does not know shows up there and not nowhere.
import { execFileSync } from 'node:child_process'
import { createRequire } from 'node:module'
import { existsSync, mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { dirname, join, relative, resolve, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const REPO = resolve(HERE, '..')
const OUT_DEFAULT = join(HERE, 'calls.json')

const SCAN_DIRS = [
  'apps/studio/data', 'apps/studio/lib', 'apps/studio/components', 'apps/studio/hooks', 'apps/studio/state',
  'apps/studio/pages', 'apps/studio/app', 'apps/studio/routes',
  'packages/common', 'packages/ui', 'packages/ui-patterns',
]
const SKIP_DIRS = new Set(['node_modules', '.next', 'dist', '__mocks__', '__tests__', '__snapshots__', 'tests', 'test', 'e2e', 'evals', 'stories'])
const SOURCE = /\.(?:[cm]?[jt]s|[jt]sx)$/
const SKIP_FILE = /(?:\.d\.ts|\.test\.[cm]?[jt]sx?|\.spec\.[cm]?[jt]sx?|\.stories\.[jt]sx?|\.mock\.[jt]sx?)$/

const API_PATH = /^\/(?:platform|v1|v2)\/[A-Za-z{]/
const METHOD_OF = { get: 'GET', post: 'POST', put: 'PUT', patch: 'PATCH', del: 'DELETE', head: 'HEAD' }
const LEGACY_METHOD_OF = {
  fetchGet: 'GET', fetchPost: 'POST', fetchPut: 'PUT', fetchPatch: 'PATCH', fetchDelete: 'DELETE', fetchHeadWithTimeout: 'HEAD',
}
// Base-URL names and what they add: the platform build's API_URL ends in /platform, the API domain does not.
const BASE_PREFIX = { API_URL: '/platform', NEXT_PUBLIC_API_URL: '/platform', apiUrl: '/platform', NEXT_PUBLIC_API_DOMAIN: '' }

function die(msg) {
  console.error(`scan-calls: ${msg}`)
  process.exit(1)
}

// ---- arguments ------------------------------------------------------------------------------------
const args = process.argv.slice(2)
const opt = { fetch: false, check: false, summary: false, src: '', out: OUT_DEFAULT }
for (let i = 0; i < args.length; i++) {
  const a = args[i]
  if (a === '--fetch') opt.fetch = true
  else if (a === '--check') opt.check = true
  else if (a === '--summary') opt.summary = true
  else if (a === '--src') opt.src = resolve(args[++i] ?? die('--src needs a directory'))
  else if (a === '--out') opt.out = resolve(args[++i] ?? die('--out needs a file'))
  else die(`unknown argument ${a}`)
}
if (opt.fetch === !!opt.src) die('give exactly one of --fetch and --src DIR')

// ---- the pinned source ------------------------------------------------------------------------------
function pins() {
  const build = readFileSync(join(HERE, 'build.sh'), 'utf8')
  const commit = /^PINNED_COMMIT=([0-9a-f]{40})/m.exec(build)?.[1] ?? die('PINNED_COMMIT not found in studio/build.sh')
  const versions = readFileSync(join(REPO, 'internal/versions/versions.yaml'), 'utf8')
  const tag = /^studio:\n(?:[ \t]+.*\n|\n)*?[ \t]+tag:[ \t]*(\S+)/m.exec(versions)?.[1] ?? die('studio.tag not found in versions.yaml')
  const patchset = readFileSync(join(HERE, 'PATCHSET'), 'utf8').trim()
  return { commit, tag, patchset }
}

function git(cwd, ...a) {
  return execFileSync('git', a, { cwd, stdio: ['ignore', 'pipe', 'inherit'], encoding: 'utf8' })
}

// fetchSource: the sparse checkout build.sh makes (apps/studio and packages/), patched with studio/patches.
function fetchSource(pin) {
  const dir = join(HERE, '.build-cache', 'scan-src')
  const patches = readdirSync(join(HERE, 'patches')).filter((f) => f.endsWith('.patch')).sort()
  const stamp = `${pin.commit}+${patches.join(',')}+${execFileSync('git', ['hash-object', '--stdin'], {
    input: patches.map((f) => readFileSync(join(HERE, 'patches', f))).reduce((a, b) => Buffer.concat([a, b]), Buffer.alloc(0)),
    encoding: 'utf8',
  }).trim()}`
  const marker = join(dir, '.supavise-scan-fetched')
  if (existsSync(marker) && readFileSync(marker, 'utf8') === stamp) return dir
  console.error(`scan-calls: fetching supabase/supabase@${pin.commit.slice(0, 7)}`)
  execFileSync('rm', ['-rf', dir])
  mkdirSync(dir, { recursive: true })
  git(dir, 'init', '-q')
  git(dir, 'remote', 'add', 'origin', 'https://github.com/supabase/supabase.git')
  git(dir, 'config', 'user.name', 'supavise')
  git(dir, 'config', 'user.email', 'supavise@users.noreply.invalid')
  git(dir, 'sparse-checkout', 'init', '--no-cone')
  // The same set as studio/build.sh: a patch may touch any file the build checks out, and git refuses to apply one to a
  // file outside the sparse set.
  git(dir, 'sparse-checkout', 'set',
    '/*', '!/apps/', '!/packages/', '!/blocks/', '!/e2e/', '!/examples/', '!/i18n/', '!/docker/', '!/supabase/',
    '/apps/studio/', '/packages/', '/patches/', '/apps/*/package.json', '/blocks/*/package.json', '/e2e/*/package.json')
  git(dir, 'fetch', '-q', '--depth', '1', '--filter=blob:none', 'origin', pin.commit)
  git(dir, 'checkout', '-q', '--detach', 'FETCH_HEAD')
  if (git(dir, 'rev-parse', 'HEAD').trim() !== pin.commit) die(`the fetched commit is not ${pin.commit}`)
  git(dir, 'am', '--3way', ...patches.map((f) => join(HERE, 'patches', f)))
  writeFileSync(marker, stamp)
  return dir
}

// ---- TypeScript ---------------------------------------------------------------------------------
const nodeRequire = createRequire(import.meta.url)
let ts
try {
  ts = nodeRequire(nodeRequire.resolve('typescript', { paths: [join(HERE, 'scan'), HERE] }))
} catch {
  die('the typescript package is missing: run `npm ci --prefix studio/scan`')
}

// Studio's own routes (pages/api) serve the self-hosted build; the platform build answers 404 for all but a few
// (docs/reference/studio-platform-calls.md, "Studio's own /api routes"), and their handlers call a local postgres-meta.
const SKIP_PATH = /^apps\/studio\/(?:pages\/api\/|lib\/api\/self-hosted\/)/

function* walk(dir, rel) {
  let entries
  try {
    entries = readdirSync(dir, { withFileTypes: true })
  } catch {
    return
  }
  entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
  for (const e of entries) {
    const p = join(dir, e.name)
    const r = `${rel}/${e.name}`
    if (e.isDirectory()) {
      if (!SKIP_DIRS.has(e.name)) yield* walk(p, r)
    } else if (e.isFile() && SOURCE.test(e.name) && !SKIP_FILE.test(e.name) && !SKIP_PATH.test(r)) {
      yield [p, r]
    }
  }
}

const kindOf = (file) => (file.endsWith('.tsx') ? ts.ScriptKind.TSX : file.endsWith('.jsx') ? ts.ScriptKind.JSX : /\.[cm]?js$/.test(file) ? ts.ScriptKind.JS : ts.ScriptKind.TS)

function unwrap(n) {
  while (n && (ts.isParenthesizedExpression(n) || ts.isAsExpression(n) || ts.isNonNullExpression(n) || ts.isSatisfiesExpression?.(n) || ts.isTypeAssertionExpression?.(n))) n = n.expression
  return n
}

// fold: true or false for the few conditions the build fixes (IS_PLATFORM is true in the platform build); null otherwise.
function fold(n) {
  n = unwrap(n)
  if (ts.isIdentifier(n) && n.text === 'IS_PLATFORM') return true
  if (ts.isPrefixUnaryExpression(n) && n.operator === ts.SyntaxKind.ExclamationToken) {
    const v = fold(n.operand)
    return v === null ? null : !v
  }
  if (ts.isBinaryExpression(n)) {
    const l = fold(n.left)
    const r = fold(n.right)
    if (n.operatorToken.kind === ts.SyntaxKind.AmpersandAmpersandToken) {
      if (l === false || r === false) return false
      if (l === true && r === true) return true
    }
    if (n.operatorToken.kind === ts.SyntaxKind.BarBarToken) {
      if (l === true || r === true) return true
      if (l === false && r === false) return false
    }
  }
  return null
}

const MAX_VALUES = 16
function cross(a, b) {
  const out = []
  for (const x of a) for (const y of b) out.push(x + y)
  return out.length > MAX_VALUES ? out.slice(0, MAX_VALUES) : out
}

function placeholder(expr) {
  const e = unwrap(expr)
  if (ts.isIdentifier(e)) return `{${e.text}}`
  if (ts.isPropertyAccessExpression(e)) return `{${e.name.text}}`
  return '{?}'
}

// One parsed file: its import map and the call sites it holds.
class FileScope {
  constructor(sf, rel) {
    this.sf = sf
    this.rel = rel
    this.imports = new Map() // local name -> {module, name}
    this.calls = [] // every CallExpression with an identifier callee, for the call-site search
    const visit = (n) => {
      if (ts.isImportDeclaration(n) && ts.isStringLiteral(n.moduleSpecifier) && n.importClause?.namedBindings && ts.isNamedImports(n.importClause.namedBindings)) {
        for (const el of n.importClause.namedBindings.elements) {
          this.imports.set(el.name.text, { module: n.moduleSpecifier.text, name: (el.propertyName ?? el.name).text })
        }
      } else if (ts.isCallExpression(n)) {
        const callee = unwrap(n.expression)
        if (ts.isIdentifier(callee)) this.calls.push(n)
      }
      ts.forEachChild(n, visit)
    }
    visit(sf)
  }
}

// The whole scanned tree: resolves identifiers across files, enough to follow a path through a constant, a
// small function or the call sites of a function that takes it as a parameter.
class Project {
  constructor() {
    this.scopes = new Map() // rel path -> FileScope
    this.byFileName = new Map() // sf.fileName -> FileScope
    this.callIndex = new Map() // callee name -> [{scope, call}]
  }

  add(file, rel, text) {
    const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, kindOf(file))
    const scope = new FileScope(sf, rel)
    this.scopes.set(rel, scope)
    this.byFileName.set(sf.fileName, scope)
    for (const c of scope.calls) {
      const name = unwrap(c.expression).text
      const list = this.callIndex.get(name) ?? []
      list.push({ scope, call: c })
      this.callIndex.set(name, list)
    }
    return scope
  }

  scopeOf(node) {
    return this.byFileName.get(node.getSourceFile().fileName)
  }

  // moduleFile: the scanned file an import specifier names, or null.
  moduleFile(fromRel, spec) {
    let base
    if (spec.startsWith('@/')) base = `apps/studio/${spec.slice(2)}`
    else if (spec.startsWith('.')) base = join(dirname(fromRel), spec).split(sep).join('/')
    else return null
    for (const cand of [base, `${base}.ts`, `${base}.tsx`, `${base}/index.ts`, `${base}/index.tsx`, `${base}.js`]) {
      if (this.scopes.has(cand)) return this.scopes.get(cand)
    }
    return null
  }

  // lookup: what a name means at a node: {kind:'init', node} for a const/let, {kind:'fn', node} for a function,
  // {kind:'param', fn, index, key} for a parameter, or null.
  lookup(name, from) {
    for (let p = from.parent; p; p = p.parent) {
      if (ts.isBlock(p) || ts.isSourceFile(p) || ts.isModuleBlock(p)) {
        for (const st of p.statements) {
          if (ts.isVariableStatement(st)) {
            for (const d of st.declarationList.declarations) {
              if (ts.isIdentifier(d.name) && d.name.text === name && d.initializer) return { kind: 'init', node: d.initializer }
            }
          } else if (ts.isFunctionDeclaration(st) && st.name?.text === name) {
            return { kind: 'fn', node: st }
          }
        }
      }
      if (ts.isFunctionLike(p) && p.parameters) {
        for (let i = 0; i < p.parameters.length; i++) {
          const par = p.parameters[i]
          if (ts.isIdentifier(par.name) && par.name.text === name) return { kind: 'param', fn: p, index: i, key: null }
          if (ts.isObjectBindingPattern(par.name)) {
            for (const el of par.name.elements) {
              if (ts.isIdentifier(el.name) && el.name.text === name) return { kind: 'param', fn: p, index: i, key: (el.propertyName ?? el.name).text }
            }
          }
        }
      }
    }
    // an import: the top-level declaration of the same name in the module it comes from
    const scope = this.scopeOf(from)
    const imp = scope?.imports.get(name)
    if (imp) {
      const target = this.moduleFile(scope.rel, imp.module)
      if (target) {
        for (const st of target.sf.statements) {
          if (ts.isVariableStatement(st)) {
            for (const d of st.declarationList.declarations) {
              if (ts.isIdentifier(d.name) && d.name.text === imp.name && d.initializer) return { kind: 'init', node: d.initializer }
            }
          } else if (ts.isFunctionDeclaration(st) && st.name?.text === imp.name) {
            return { kind: 'fn', node: st }
          }
        }
      }
    }
    return null
  }

  // objectOf: the object literal an expression stands for (a const, a member of one, an imported const).
  objectOf(expr, depth = 0) {
    expr = unwrap(expr)
    if (depth > 6) return null
    if (ts.isObjectLiteralExpression(expr)) return expr
    if (ts.isIdentifier(expr)) {
      const d = this.lookup(expr.text, expr)
      return d?.kind === 'init' ? this.objectOf(d.node, depth + 1) : null
    }
    if (ts.isPropertyAccessExpression(expr)) {
      const base = this.objectOf(expr.expression, depth + 1)
      const prop = base?.properties.find((p) => ts.isPropertyAssignment(p) && (ts.isIdentifier(p.name) || ts.isStringLiteral(p.name)) && p.name.text === expr.name.text)
      return prop ? this.objectOf(prop.initializer, depth + 1) : null
    }
    return null
  }

  // resolve: the strings an expression can evaluate to, or null when it depends on something the source does not fix.
  resolve(n, depth = 0, seen = new Set()) {
    if (depth > 10) return null
    n = unwrap(n)
    if (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n)) return [n.text]
    if (ts.isTemplateExpression(n)) {
      let acc = [n.head.text]
      for (const span of n.templateSpans) {
        const inner = this.resolve(span.expression, depth + 1, seen)
        acc = cross(acc, inner ?? [placeholder(span.expression)])
        acc = cross(acc, [span.literal.text])
      }
      return acc
    }
    if (ts.isConditionalExpression(n)) {
      const f = fold(n.condition)
      if (f !== null) return this.resolve(f ? n.whenTrue : n.whenFalse, depth + 1, seen)
      const a = this.resolve(n.whenTrue, depth + 1, seen)
      const b = this.resolve(n.whenFalse, depth + 1, seen)
      return a && b ? [...new Set([...a, ...b])] : null
    }
    if (ts.isBinaryExpression(n)) {
      if (n.operatorToken.kind === ts.SyntaxKind.PlusToken) {
        const a = this.resolve(n.left, depth + 1, seen)
        const b = this.resolve(n.right, depth + 1, seen)
        return a && b ? cross(a, b) : null
      }
      if (n.operatorToken.kind === ts.SyntaxKind.QuestionQuestionToken || n.operatorToken.kind === ts.SyntaxKind.BarBarToken) {
        const a = this.resolve(n.left, depth + 1, seen)
        const b = this.resolve(n.right, depth + 1, seen)
        return a && b ? [...new Set([...a, ...b])] : null
      }
      return null
    }
    if (ts.isIdentifier(n)) {
      const key = `${n.getSourceFile().fileName}:${n.getStart()}:${n.text}`
      if (seen.has(key)) return null
      const d = this.lookup(n.text, n)
      if (!d) return null
      const next = new Set(seen).add(key)
      if (d.kind === 'init') return this.resolve(d.node, depth + 1, next)
      if (d.kind === 'param') return this.fromCallSites(d, depth, next)
      return null
    }
    if (ts.isPropertyAccessExpression(n) || ts.isElementAccessExpression(n)) {
      const base = this.objectOf(n.expression)
      if (!base) return null
      const key = ts.isPropertyAccessExpression(n) ? n.name.text : ts.isStringLiteral(n.argumentExpression) ? n.argumentExpression.text : null
      const out = []
      for (const p of base.properties) {
        if (!ts.isPropertyAssignment(p)) continue
        const name = ts.isIdentifier(p.name) || ts.isStringLiteral(p.name) ? p.name.text : null
        if (key !== null && name !== key) continue
        const v = this.resolve(p.initializer, depth + 1, seen)
        if (!v) return null
        out.push(...v)
      }
      return out.length ? [...new Set(out)] : null
    }
    if (ts.isCallExpression(n)) {
      // a call of a small function of this tree: what it returns, with its parameters left open
      const callee = unwrap(n.expression)
      if (!ts.isIdentifier(callee)) return null
      const d = this.lookup(callee.text, callee)
      let fn = null
      if (d?.kind === 'fn') fn = d.node
      else if (d?.kind === 'init') {
        const i = unwrap(d.node)
        if (ts.isArrowFunction(i) || ts.isFunctionExpression(i)) fn = i
      }
      if (!fn) return null
      const rets = []
      if (!fn.body) return null
      if (!ts.isBlock(fn.body)) rets.push(fn.body)
      else {
        const find = (x) => {
          if (ts.isFunctionLike(x) && x !== fn) return
          if (ts.isReturnStatement(x) && x.expression) rets.push(x.expression)
          ts.forEachChild(x, find)
        }
        find(fn.body)
      }
      const out = []
      for (const r of rets) {
        const v = this.resolve(r, depth + 1, seen)
        if (!v) return null
        out.push(...v)
      }
      return out.length ? [...new Set(out)] : null
    }
    return null
  }

  // fromCallSites: a parameter's values, taken from the arguments of every call of its function in the tree.
  fromCallSites(d, depth, seen) {
    if (depth > 6) return null
    const fn = d.fn
    let name = fn.name?.text
    if (!name && fn.parent && ts.isVariableDeclaration(fn.parent) && ts.isIdentifier(fn.parent.name)) name = fn.parent.name.text
    if (!name) return null
    const def = this.scopeOf(fn)
    const out = []
    for (const { scope, call } of this.callIndex.get(name) ?? []) {
      // only the calls that reach this function: the same file, or an import that names its module
      const imp = scope.imports.get(name)
      if (!imp) {
        if (scope !== def) continue
      } else {
        const target = this.moduleFile(scope.rel, imp.module)
        const viaPackage = !imp.module.startsWith('.') && !imp.module.startsWith('@/') && def.rel.startsWith(`packages/${imp.module}/`)
        if (target !== def && !viaPackage) continue
      }
      const arg = call.arguments[d.index]
      if (!arg) continue
      let v = null
      if (d.key === null) v = this.resolve(arg, depth + 2, seen)
      else {
        const o = unwrap(arg)
        if (!ts.isObjectLiteralExpression(o)) continue
        for (const p of o.properties) {
          if (ts.isPropertyAssignment(p) && (ts.isIdentifier(p.name) || ts.isStringLiteral(p.name)) && p.name.text === d.key) v = this.resolve(p.initializer, depth + 2, seen)
          else if (ts.isShorthandPropertyAssignment(p) && p.name.text === d.key) v = this.resolve(p.name, depth + 2, seen)
        }
      }
      if (v) out.push(...v)
    }
    return out.length ? [...new Set(out)] : null
  }
}

// toPath: a resolved string to an API path, or null when it is not one. A leading base-URL placeholder becomes its prefix.
function toPath(s) {
  let p = s
  const base = /^\{([A-Za-z_]+)\}/.exec(p)
  if (base && Object.hasOwn(BASE_PREFIX, base[1])) p = BASE_PREFIX[base[1]] + p.slice(base[0].length)
  p = p.replace(/[?#].*$/, '').replace(/\/{2,}/g, '/').replace(/\/$/, '')
  return API_PATH.test(p) ? p : null
}

// ---- scanning one file --------------------------------------------------------------------------------
function scanFile(project, scope, out) {
  const { sf, rel } = scope
  const text = sf.text
  const claimed = new Set() // nodes inside the argument of a recognized call

  const fetchers = (local) => {
    const imp = scope.imports.get(local)
    return imp && /(?:^|\/)fetchers$/.test(imp.module) ? imp.name : null
  }
  // A local alias (`const deleteRequest = del as unknown as (...) => ...`) stands for what it is assigned.
  const dealias = (id, depth = 0) => {
    if (depth > 3) return id
    const d = project.lookup(id.text, id)
    if (d?.kind !== 'init') return id
    const v = unwrap(d.node)
    return ts.isIdentifier(v) ? dealias(v, depth + 1) : id
  }
  const methodFor = (call) => {
    let callee = unwrap(call.expression)
    if (ts.isIdentifier(callee) && !scope.imports.has(callee.text)) callee = dealias(callee)
    if (ts.isIdentifier(callee)) {
      const imported = fetchers(callee.text)
      if (imported && METHOD_OF[imported]) return { method: METHOD_OF[imported], form: 'client' }
      if (imported && LEGACY_METHOD_OF[imported]) return { method: LEGACY_METHOD_OF[imported], form: 'legacy' }
      if (callee.text === 'fetch') return { method: null, form: 'fetch' }
    }
    if (ts.isPropertyAccessExpression(callee) && ts.isIdentifier(callee.expression) && fetchers(callee.expression.text) === 'client') {
      const m = callee.name.text
      if (/^(?:GET|POST|PUT|PATCH|DELETE|HEAD)$/.test(m)) return { method: m, form: 'client' }
    }
    return null
  }
  const optionMethod = (call) => {
    const o = call.arguments[1] && unwrap(call.arguments[1])
    if (!o || !ts.isObjectLiteralExpression(o)) return 'GET'
    for (const p of o.properties) {
      if (ts.isPropertyAssignment(p) && ts.isIdentifier(p.name) && p.name.text === 'method') {
        const v = project.resolve(p.initializer)
        return v && v.length === 1 ? v[0].toUpperCase() : null
      }
    }
    return 'GET'
  }
  const lineOf = (n) => sf.getLineAndCharacterOfPosition(n.getStart(sf)).line + 1
  const argText = (n) => text.slice(n.getStart(sf), n.getEnd()).replace(/\s+/g, ' ').slice(0, 120)
  const mark = (n) => {
    const stack = [n]
    while (stack.length) {
      const x = stack.pop()
      claimed.add(x)
      ts.forEachChild(x, (c) => stack.push(c))
    }
  }

  // A query module whose `enabled` is false in the platform build (`enabled: ... && !IS_PLATFORM`) never calls.
  let off = false
  const findOff = (n) => {
    if (ts.isPropertyAssignment(n) && ts.isIdentifier(n.name) && n.name.text === 'enabled' && fold(n.initializer) === false) off = true
    ts.forEachChild(n, findOff)
  }
  findOff(sf)
  if (off) {
    out.skipped.push({ file: rel, reason: 'enabled only when IS_PLATFORM is false' })
    return
  }

  const visit = (n) => {
    if (ts.isCallExpression(n)) {
      const info = methodFor(n)
      if (info && n.arguments.length > 0) {
        const arg = n.arguments[0]
        const values = project.resolve(arg)
        const paths = values ? [...new Set(values.map(toPath).filter(Boolean))] : []
        const method = info.form === 'fetch' ? optionMethod(n) : info.method
        if (paths.length > 0 && method) {
          mark(arg)
          const literal = isLiteral(arg)
          for (const p of paths) out.calls.push({ method, path: p, file: rel, form: info.form === 'fetch' ? 'fetch' : literal ? 'literal' : 'computed' })
        } else if (info.form !== 'fetch') {
          // A client call whose path is not fixed by the source: a parameter nobody passes a path to, a call.
          const external = values && values.length > 0 && values.every((v) => !/^\{[^}]*\}/.test(v) && !v.startsWith('/'))
          if (!external) {
            mark(arg)
            out.unresolved.push({ file: rel, line: lineOf(n), method: info.method, arg: argText(arg), candidates: values ? [...new Set(values)].sort() : [] })
          }
        } else if (!values && /API_URL/.test(argText(arg))) {
          out.unresolved.push({ file: rel, line: lineOf(n), method: method ?? '?', arg: argText(arg), candidates: [] })
        }
      }
    }
    ts.forEachChild(n, visit)
  }
  visit(sf)

  // Strings that look like an API path and are not inside the argument of a recognized call.
  const mention = (n) => {
    if (claimed.has(n)) return
    if (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n)) {
      const p = toPath(n.text)
      if (p && !p.includes('/:') && !ts.isImportDeclaration(n.parent) && !ts.isExportDeclaration(n.parent)) out.mentions.push({ path: p, file: rel })
      return
    }
    if (ts.isTemplateExpression(n)) {
      for (const v of project.resolve(n) ?? []) {
        const p = toPath(v)
        if (p) out.mentions.push({ path: p, file: rel })
      }
      return
    }
    ts.forEachChild(n, mention)
  }
  mention(sf)
}

function isLiteral(n) {
  n = unwrap(n)
  return ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n)
}

// ---- running ---------------------------------------------------------------------------------------
const pin = pins()
const root = opt.fetch ? fetchSource(pin) : opt.src
if (!existsSync(join(root, 'apps/studio'))) die(`${root} has no apps/studio`)

const project = new Project()
for (const d of SCAN_DIRS) {
  const dir = join(root, d)
  if (!existsSync(dir) || !statSync(dir).isDirectory()) continue
  for (const [f, rel] of walk(dir, d)) project.add(f, rel, readFileSync(f, 'utf8'))
}
const raw = { calls: [], unresolved: [], mentions: [], skipped: [] }
for (const scope of project.scopes.values()) scanFile(project, scope, raw)
const files = project.scopes.size

// Group: one entry per (method, path) with the files that make it; mentions that no call names.
const byKey = new Map()
for (const c of raw.calls) {
  const k = `${c.path} ${c.method}`
  const e = byKey.get(k) ?? { method: c.method, path: c.path, forms: new Set(), files: new Set() }
  e.forms.add(c.form)
  e.files.add(c.file)
  byKey.set(k, e)
}
const calls = [...byKey.values()]
  .sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : a.method < b.method ? -1 : a.method > b.method ? 1 : 0))
  .map((e) => ({ method: e.method, path: e.path, form: [...e.forms].sort().join('+'), files: [...e.files].sort() }))
const calledPaths = new Set(calls.map((c) => c.path))
const mentionByPath = new Map()
for (const m of raw.mentions) {
  if (calledPaths.has(m.path)) continue
  const s = mentionByPath.get(m.path) ?? new Set()
  s.add(m.file)
  mentionByPath.set(m.path, s)
}
const mentions = [...mentionByPath.entries()]
  .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
  .map(([path, s]) => ({ path, files: [...s].sort() }))
const unresolved = raw.unresolved.sort((a, b) => (a.file < b.file ? -1 : a.file > b.file ? 1 : a.line - b.line))

const doc = {
  generator: 'studio/scan-calls.mjs',
  studio: { commit: pin.commit, tag: pin.tag },
  scanned_files: files,
  calls,
  skipped: raw.skipped.sort((a, b) => (a.file < b.file ? -1 : a.file > b.file ? 1 : 0)),
  unresolved,
  mentions,
}
const json = `${JSON.stringify(doc, null, 1).replace(/\[\n\s+("[^"\n]*"(?:,\n\s+"[^"\n]*")*)\n\s+\]/g, (_m, g) => `[${g.replace(/,\n\s+/g, ', ')}]`)}\n`

if (opt.summary) {
  const paths = new Set(calls.map((c) => c.path))
  console.log(`${files} files, ${calls.length} (method, path) pairs over ${paths.size} paths, ${unresolved.length} unresolved calls, ${mentions.length} unclaimed mentions`)
} else if (opt.check) {
  const have = existsSync(opt.out) ? readFileSync(opt.out, 'utf8') : ''
  if (have !== json) {
    const tmp = `${opt.out}.new`
    writeFileSync(tmp, json)
    die(`${relative(REPO, opt.out)} is out of date: regenerate it with \`node studio/scan-calls.mjs --fetch\` and commit it (the fresh result is at ${tmp})`)
  }
  console.error(`scan-calls: ${relative(REPO, opt.out)} is current (${calls.length} pairs)`)
} else {
  writeFileSync(opt.out, json)
  console.error(`scan-calls: wrote ${relative(REPO, opt.out)}: ${calls.length} pairs, ${unresolved.length} unresolved, ${mentions.length} mentions`)
}
