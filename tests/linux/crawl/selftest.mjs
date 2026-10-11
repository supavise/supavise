// The logic of lib.mjs and the shape of the files next to it: `node --test tests/linux/crawl/selftest.mjs`.
// It needs no variables, no browser and no node: run it after changing anything in this directory.
import assert from 'node:assert/strict'
import { execFileSync, spawn } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import http from 'node:http'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  Allowlist, DOM_ORACLES, domFindings, expandRoutes, failures, fillRoute, globRegExp, guardVerdict, hostKind, isReadOnlySQL,
  judgeResponse, noiseMatch, opRegExp, pagesOf, readJSON, routeParams, safeToClick, stateVisits, summaryMarkdown,
} from './lib.mjs'
import { Node, redact, totp, waitFor } from './node.mjs'
import { REF, startStandin } from './standin.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))

test('route parameters and filling', () => {
  assert.deepEqual(routeParams('/project/[ref]/functions/[functionSlug]/code'), [{ name: 'ref', catchAll: false }, { name: 'functionSlug', catchAll: false }])
  assert.deepEqual(routeParams('/org/_/[[...routeSlug]]'), [{ name: 'routeSlug', catchAll: true }])
  assert.equal(fillRoute('/project/[ref]/editor/[id]', { ref: 'abc', id: '12' }), '/project/abc/editor/12')
  assert.equal(fillRoute('/org/_/[[...routeSlug]]', { routeSlug: '' }), '/org/_')
  assert.equal(fillRoute('/org/_/[[...routeSlug]]', { routeSlug: 'general/x' }), '/org/_/general/x')
  assert.throws(() => fillRoute('/a/[x]', {}), /no value for \[x\]/)
  assert.ok(globRegExp('/integrations/vercel/*').test('/integrations/vercel/[slug]/marketplace/choose-project'))
  assert.ok(!globRegExp('/org/[slug]/billing').test('/org/[slug]/billing/x'))
})

test('pages of a routes manifest', () => {
  const m = { staticRoutes: [{ page: '/organizations' }, { page: '/_error' }, { page: '/api/x' }], dynamicRoutes: [{ page: '/project/[ref]' }, { page: '/organizations' }] }
  assert.deepEqual(pagesOf(m), ['/organizations', '/project/[ref]'])
})

test('expanding routes with fixtures', () => {
  const fixtures = {
    orgs: ['default', 'second'],
    projects: { A: 'aaaaaaaaaaaaaaaaaaaa', B: 'bbbbbbbbbbbbbbbbbbbb' },
    byRef: { aaaaaaaaaaaaaaaaaaaa: { tables: ['7'], functions: ['hello'] }, bbbbbbbbbbbbbbbbbbbb: { functions: ['hello'] } },
    global: {},
  }
  const rules = {
    defaults: { functionSlug: 'functions' },
    params: [{ route: '/project/[ref]/editor/[id]', param: 'id', from: 'tables' }],
    skip: [{ route: '/stripe-*', reason: 'hosted' }],
    hidden: [{ route: '/org/[slug]/billing', feature: 'billing:all' }],
    refs: { '/project/[ref]/merge': ['B'] },
    catchAll: { '/org/_/[[...routeSlug]]': ['', 'general'] },
  }
  const pages = ['/organizations', '/org/[slug]/billing', '/org/_/[[...routeSlug]]', '/project/[ref]', '/project/[ref]/editor/[id]', '/project/[ref]/functions/[functionSlug]', '/project/[ref]/merge', '/stripe-atlas', '/project/[ref]/storage/files/buckets/[bucketId]']
  const r = expandRoutes(pages, rules, fixtures)
  const urls = r.visits.map((v) => v.url)
  assert.deepEqual(urls.filter((u) => u.startsWith('/org/')), ['/org/default/billing', '/org/second/billing', '/org/_', '/org/_/general'])
  assert.equal(r.visits.find((v) => v.url === '/org/default/billing').informational, 'billing:all')
  assert.ok(urls.includes('/project/aaaaaaaaaaaaaaaaaaaa/editor/7'))
  assert.ok(!urls.some((u) => u.startsWith('/project/bbbbbbbbbbbbbbbbbbbb/editor')), 'B has no tables: no visit, and not a failure as A has one')
  assert.deepEqual(urls.filter((u) => u.endsWith('/merge')), ['/project/bbbbbbbbbbbbbbbbbbbb/merge'])
  assert.equal(urls.filter((u) => u.includes('/functions/hello')).length, 2)
  assert.deepEqual(r.skipped.map((s) => s.route), ['/stripe-atlas'])
  assert.deepEqual(r.noFixture.map((n) => n.route), ['/project/[ref]/storage/files/buckets/[bucketId]'])
})

test('custom URLs and state visits', () => {
  const fixtures = { orgs: ['default'], projects: { A: 'aaaaaaaaaaaaaaaaaaaa', B: 'bbbbbbbbbbbbbbbbbbbb', paused: 'pppppppppppppppppppp' }, byRef: {}, global: {} }
  const rules = { defaultRefs: ['A', 'B'], urls: [{ route: '/project/[ref]/integrations/[id]', urls: ['/project/{ref}/integrations/cron'] }], states: { routes: ['/project/[ref]'], projects: ['paused', 'idle'] } }
  const r = expandRoutes(['/project/[ref]/integrations/[id]'], rules, fixtures)
  assert.deepEqual(r.visits.map((v) => v.url), ['/project/aaaaaaaaaaaaaaaaaaaa/integrations/cron', '/project/bbbbbbbbbbbbbbbbbbbb/integrations/cron'])
  const s = stateVisits(rules, fixtures)
  assert.deepEqual(s.visits.map((v) => v.url), ['/project/pppppppppppppppppppp'])
  assert.deepEqual(s.missing, ['idle'])
})

test('the allow-list matches by method, template and status, and tracks what it saw', () => {
  const allow = new Allowlist({
    stubs: [{ op: 'GET /platform/notifications', reach: 'crawl', reason: 'a' }, { op: 'GET /platform/projects/{ref}/api/rest', reach: 'static', reason: 'b' }],
    statuses: [{ op: 'GET /v1/projects/{ref}/ssl-enforcement', status: 400, reach: 'crawl', reason: 'c' }, { op: '* /auth/v1/user', status: 401, reach: 'static', reason: 'd' }],
    dom: [{ route: '/project/[ref]/logs/*', text: 'Error fetching', reach: 'crawl', reason: 'e' }],
  })
  assert.ok(allow.stub('GET', '/platform/notifications'))
  assert.equal(allow.stub('POST', '/platform/notifications'), null)
  assert.ok(allow.stub('HEAD', '/platform/projects/abcdefghijklmnopqrst/api/rest'), 'a HEAD is answered by the GET route')
  assert.equal(allow.stub('GET', '/platform/projects/a/b/api/rest'), null, 'a segment does not cross a slash')
  assert.ok(allow.status('GET', '/v1/projects/abcdefghijklmnopqrst/ssl-enforcement', 400))
  assert.equal(allow.status('GET', '/v1/projects/abcdefghijklmnopqrst/ssl-enforcement', 404), null)
  assert.ok(allow.status('PATCH', '/auth/v1/user', 401), 'a star method matches any')
  assert.deepEqual(allow.unseen(), { stubs: [], statuses: [], dom: ['/project/[ref]/logs/* "Error fetching"'] })
  assert.deepEqual(allow.seenStatic().stubs, ['GET /platform/projects/{ref}/api/rest'])
  assert.ok(allow.domText('/project/[ref]/logs/edge-logs', 'Error fetching'))
  assert.deepEqual(allow.unseen().dom, [])
})

test('judging a response', () => {
  const allow = new Allowlist({
    stubs: [{ op: 'GET /platform/notifications', reach: 'crawl', reason: 'known stub' }],
    statuses: [{ op: 'GET /v1/projects/{ref}/ssl-enforcement', status: 400, reach: 'crawl', reason: 'refusal' }],
  })
  const d = 'supavise.test'
  assert.equal(hostKind('api.supavise.test', d), 'api')
  assert.equal(hostKind('abc.api.supavise.test', d), 'project')
  assert.equal(hostKind('studio.supavise.test', d), 'studio')
  assert.equal(hostKind('cdn.example.com', d), 'external')
  const j = (r) => judgeResponse({ host: 'api.supavise.test', method: 'GET', stub: false, status: 200, ...r }, d, allow)
  assert.deepEqual(j({ path: '/platform/projects' }), [])
  assert.equal(j({ path: '/platform/notifications', stub: true })[0].allowed, true)
  assert.equal(j({ path: '/platform/other', stub: true })[0].allowed, false)
  assert.equal(j({ path: '/v1/projects/abcdefghijklmnopqrst/ssl-enforcement', status: 400 })[0].allowed, true)
  assert.equal(j({ path: '/v1/projects/abcdefghijklmnopqrst/health', status: 404 })[0].allowed, false)
  assert.equal(j({ path: '/platform/projects', status: 500 })[0].kind, '5xx')
  assert.deepEqual(judgeResponse({ host: 'cdn.example.com', method: 'GET', path: '/x', status: 500, stub: false }, d, allow), [])
  // a stub header on a project host is not the Management API's business
  assert.deepEqual(judgeResponse({ host: 'a.api.supavise.test', method: 'GET', path: '/rest/v1/', status: 200, stub: true }, d, allow), [])
})

test('console noise', () => {
  const noise = [{ pattern: 'usercentrics', reason: 'x' }, { pattern: '^Minified React error #418', reason: 'y' }]
  assert.ok(noiseMatch(noise, 'Failed to load https://app.usercentrics.eu/x'))
  assert.ok(noiseMatch(noise, 'Minified React error #418; visit'))
  assert.equal(noiseMatch(noise, 'TypeError: x is not a function'), null)
})

test('page text oracles', () => {
  const allow = new Allowlist({ dom: [{ route: '/project/[ref]/logs/*', text: 'Error fetching', reach: 'crawl', reason: 'no log store' }] })
  assert.deepEqual(domFindings('/x', 'All good. Rows: 12', allow), [])
  const f = domFindings('/project/[ref]/logs/edge-logs', 'Error fetching logs. Failed to load. NaN%', allow)
  assert.deepEqual(f.map((x) => [x.text, x.allowed]), [['Failed to', false], ['Error fetching', true], ['NaN', false]])
  assert.deepEqual(domFindings('/x', 'Nancy and banana', allow), [], 'NaN is a word of its own')
  assert.ok(DOM_ORACLES.includes('OPENAI_API_KEY'))
})

test('read-only SQL', () => {
  for (const q of ['select 1', ' SELECT * from t', 'with a as (select 1) select * from a', 'explain select 1', 'show server_version', '/* c */ select 1 -- x', "select 'delete me' as s", 'table foo'])
    assert.equal(isReadOnlySQL(q), true, q)
  for (const q of ['delete from t', 'insert into t values (1)', 'select 1; drop table t', 'with d as (delete from t returning *) select * from d', 'create table x()', 'select set_config(\'a\',\'b\',false); set a=1', 'explain analyze delete from t', 'grant all on t to x', '', null])
    assert.equal(isReadOnlySQL(q), false, String(q))
})

test('the mutation guard', () => {
  assert.equal(guardVerdict('GET', '/platform/projects', null), 'allow')
  assert.equal(guardVerdict('HEAD', '/platform/projects/x/api/rest', null), 'allow')
  assert.equal(guardVerdict('DELETE', '/platform/projects/x', null), 'abort')
  assert.equal(guardVerdict('PATCH', '/platform/projects/x', '{}'), 'abort')
  assert.equal(guardVerdict('POST', '/platform/pg-meta/x/query', JSON.stringify({ query: 'select 1' })), 'allow')
  assert.equal(guardVerdict('POST', '/platform/pg-meta/x/query', JSON.stringify({ query: 'drop table t' })), 'abort')
  assert.equal(guardVerdict('POST', '/platform/pg-meta/x/query', 'not json'), 'abort')
  assert.equal(guardVerdict('POST', '/platform/projects/x/api-keys/temporary', '{}'), 'allow')
  assert.equal(guardVerdict('POST', '/platform/projects/x/analytics/endpoints/logs.all', '{}'), 'allow')
  assert.equal(guardVerdict('POST', '/platform/telemetry/page', '{}'), 'allow')
  assert.equal(guardVerdict('POST', '/platform/storage/x/buckets/b/objects/list', '{}'), 'allow')
  assert.equal(guardVerdict('POST', '/platform/storage/x/buckets/b/objects/move', '{}'), 'abort')
  assert.equal(guardVerdict('POST', '/platform/profile/audit-login', '{}'), 'allow')
  assert.equal(guardVerdict('POST', '/v1/projects/x/branches', '{}'), 'abort')
})

test('the click deny list', () => {
  for (const l of ['Delete project', 'Remove member', 'Drop table', 'Reset password', 'Revoke', 'Pause project', 'Restart', 'Transfer project', 'Upgrade', 'Sign out', 'Disable', 'Empty bucket', 'Rotate key', 'Regenerate', 'Unlink', 'Merge branch', 'Deploy'])
    assert.equal(safeToClick(l), false, l)
  for (const l of ['Settings', 'Providers', 'Policies', 'Overview', 'Templates', ''])
    assert.equal(safeToClick(l), true, l)
})

test('failures and the summary', () => {
  const report = {
    pages: [
      { url: '/a', project: 'A', calls: 3, stubs: 1, n4xx: 0, n5xx: 0, pageErrors: 0, informational: null, findings: [{ kind: 'stub', op: 'GET /x', allowed: true }] },
      { url: '/b', project: 'A', calls: 1, stubs: 0, n4xx: 0, n5xx: 1, pageErrors: 0, informational: null, findings: [{ kind: '5xx', op: 'GET /y', allowed: false, detail: '500' }] },
      { url: '/c', project: null, calls: 1, stubs: 0, n4xx: 0, n5xx: 1, pageErrors: 0, informational: 'billing:all', findings: [{ kind: '5xx', op: 'GET /z', allowed: false }] },
    ],
    probes: [{ name: 'p1', ok: true, ms: 1000, findings: [{ kind: '4xx', op: 'GET /x', allowed: false, detail: '404' }] }, { name: 'p2', ok: false, ms: 2000, detail: 'nope' }, { name: 'p3', ok: null, ms: 0, detail: 'skipped' }],
    noFixture: [{ route: '/project/[ref]/x/[id]', params: ['id'] }],
    crashed: [{ where: "route pass, group 'A'", kind: 'crash', detail: 'sign-in timed out' }],
    allowlist: { unseen: { stubs: ['GET /platform/q'], statuses: [], dom: [] }, seenStatic: { stubs: ['GET /platform/w'], statuses: [], dom: [] } },
    totals: { calls: 5, stubs: 1, mutationAttempts: 0 },
  }
  const f = failures(report)
  assert.deepEqual(f.map((x) => x.kind), ['5xx', '4xx', 'probe', 'crash', 'no-fixture', 'unseen'])
  const md = summaryMarkdown(report)
  assert.match(md, /6 failure\(s\)/)
  assert.match(md, /\| p2 \| FAILED \|/)
  assert.match(md, /GET \/platform\/w/)
})

// ---- the files next to lib.mjs ----------------------------------------------------------------
test('allowlist.json, noise.json and routes.json are well formed', () => {
  const allow = new Allowlist(readJSON(join(HERE, 'allowlist.json')))
  assert.ok(allow.stubs.length > 100)
  for (const e of [...allow.stubs, ...allow.statuses]) {
    assert.match(e.op, /^(?:\*|GET|POST|PUT|PATCH|DELETE|HEAD) \//)
    assert.ok(opRegExp(e.path).test(e.path.replace(/\{[^}]*\}/g, 'abcdefghijklmnopqrst')), e.op)
    assert.ok(['crawl', 'static'].includes(e.reach), e.op)
    assert.ok(e.reason.split(/\s+/).length >= 3, e.op)
  }
  const noise = readJSON(join(HERE, 'noise.json'))
  for (const k of ['console', 'pageerror', 'requestfailed']) {
    assert.ok(Array.isArray(noise[k]), k)
    for (const n of noise[k]) {
      new RegExp(n.pattern)
      assert.ok(n.reason, `${k} ${n.pattern}`)
    }
  }
  const rules = readJSON(join(HERE, 'routes.json'))
  for (const s of [...rules.skip, ...rules.hidden]) globRegExp(s.route)
  for (const s of rules.skip) assert.ok(s.reason, s.route)
  for (const p of rules.params) assert.ok(p.param && p.from, p.route)
  assert.ok(existsSync(join(HERE, 'crawl.mjs')))
})

// ---- node.mjs -----------------------------------------------------------------------------------
test('TOTP matches the vectors of RFC 6238', () => {
  const secret = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ' // the ASCII key 12345678901234567890
  assert.equal(totp(secret, 59_000), '287082')
  assert.equal(totp(secret, 1111111109_000), '081804')
  assert.equal(totp(secret, 1234567890_000), '005924')
})

test('the node client sends the Host header and the right bearer, and cuts secrets from errors', async () => {
  const seen = []
  const server = http.createServer((req, res) => {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      seen.push({ method: req.method, url: req.url, host: req.headers.host, auth: req.headers.authorization, body })
      if (req.url.startsWith('/auth/v1/token')) return res.end(JSON.stringify({ access_token: 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln' }))
      if (req.url === '/v1/boom') return res.writeHead(500).end('{"message":"sbp_redactme0123456789abcdef failed"}')
      res.end('{"ok":true}')
    })
  })
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  try {
    const n = new Node({ domain: 'supavise.test', proxy: { host: '127.0.0.1', port: server.address().port }, owner: { email: 'o@example.com', password: 'p' }, owner_pat: 'sbp_pat' })
    assert.deepEqual((await n.api('GET', '/v1/projects')).json, { ok: true })
    assert.equal(seen[0].host, 'api.supavise.test')
    assert.equal(seen[0].auth, 'Bearer sbp_pat')
    await n.api('GET', '/platform/profile')
    assert.match(seen.at(-1).auth, /^Bearer eyJ/)
    assert.equal(seen.at(-2).url, '/auth/v1/token?grant_type=password')
    await assert.rejects(n.api('GET', '/v1/boom'), (e) => /500/.test(e.message) && !e.message.includes('0123456789abcdef'))
    assert.equal((await n.api('GET', '/v1/boom', undefined, { expect: [500] })).status, 500)
    assert.equal(n.hostOf('abc'), 'abc.api.supavise.test')
  } finally {
    server.close()
  }
  assert.ok(!redact('sb_secret_abcdefgh12345678').includes('12345678'))
})

test('waitFor returns the value, and says what it waited for on a timeout', async () => {
  let n = 0
  assert.equal(await waitFor('three', () => ++n >= 3 && 'done', { timeout: 2000, every: 5 }), 'done')
  await assert.rejects(waitFor('never', () => false, { timeout: 30, every: 5 }), /waiting for never/)
})

// ---- crawl.mjs against the stand-in ---------------------------------------------------------------------
// Needs a Chrome: CRAWL_STANDIN_CHROME=/path/to/chrome node --test tests/linux/crawl/selftest.mjs
const chrome = process.env.CRAWL_STANDIN_CHROME

async function crawlStandin(pages, { allowlist, extraRules = {} }) {
  const domain = 'supavise.test'
  const st = await startStandin({ domain })
  const dir = mkdtempSync(join(tmpdir(), 'crawl-standin-'))
  const w = (name, v) => {
    writeFileSync(join(dir, name), JSON.stringify(v))
    return join(dir, name)
  }
  const cfg = {
    domain, proxy: { host: '127.0.0.1', port: st.port }, owner: { email: 'o@example.com', password: 'pw' }, owner_pat: 'x', org: 'default', org2: 'second',
    out: join(dir, 'out'), chrome, probes: false,
    routes_manifest: w('routes-manifest.json', { staticRoutes: pages.filter((p) => !p.includes('[')).map((page) => ({ page })), dynamicRoutes: pages.filter((p) => p.includes('[')).map((page) => ({ page })) }),
    rules: w('routes.json', {
      defaultRefs: ['A'], anonymous: ['/sign-in'],
      expect: [{ route: '/project/[ref]', project: 'A', text: 'Project home', why: 'the toy page says so' }, { route: '/org/[slug]', text: 'crawl-a', match: { slug: '@org' }, why: 'the toy page says so' }],
      ...extraRules,
    }),
    allowlist: w('allowlist.json', allowlist),
    noise: w('noise.json', { console: [{ pattern: 'usercentrics', reason: 'noise' }], pageerror: [], requestfailed: [{ pattern: 'net::ERR_ABORTED', reason: 'navigation' }] }),
  }
  w('config.json', cfg)
  mkdirSync(cfg.out, { recursive: true })
  w('out/fixtures.json', { orgs: ['default'], projects: { A: REF }, byRef: {}, global: {}, unavailable: {} })
  try {
    const child = spawn(process.execPath, [join(HERE, 'crawl.mjs'), join(dir, 'config.json')], { stdio: ['ignore', 'ignore', 'pipe'] })
    let stderr = ''
    child.stderr.on('data', (d) => (stderr += d))
    const code = await new Promise((r) => child.on('exit', r))
    if (!existsSync(join(cfg.out, 'crawl-report.json'))) throw new Error(`crawl.mjs ended with ${code} and no report:\n${stderr.slice(-3000)}`)
    return { code, stderr, calls: st.calls, report: JSON.parse(readFileSync(join(cfg.out, 'crawl-report.json'), 'utf8')) }
  } finally {
    await st.close()
  }
}

test('crawl.mjs finds what it should in a dirty stand-in', { skip: !chrome && 'set CRAWL_STANDIN_CHROME to a Chrome executable', timeout: 280_000 }, async () => {
  const pages = ['/organizations', '/org/[slug]', '/project/[ref]', '/project/[ref]/tabs', '/project/[ref]/stubbed', '/project/[ref]/boom', '/project/[ref]/missing', '/project/[ref]/bad',
    '/project/[ref]/mutate', '/project/[ref]/console', '/project/[ref]/throws', '/project/[ref]/external', '/sign-in']
  const r = await crawlStandin(pages, {
    allowlist: {
      stubs: [{ op: 'GET /platform/notifications', reach: 'crawl', reason: 'a known stub' }, { op: 'GET /platform/never', reach: 'crawl', reason: 'a stub nobody calls' }],
      statuses: [{ op: 'GET /platform/missing', status: 404, reach: 'crawl', reason: 'an honest refusal' }], dom: [],
    },
  })
  assert.equal(r.code, 1, r.stderr)
  const f = r.report.failures
  const has = (kind, re) => f.some((x) => x.kind === kind && re.test(`${x.where} ${x.op ?? ''} ${x.detail ?? ''} ${x.text ?? ''}`))
  assert.ok(has('stub', /\/platform\/stubbed/), 'a stub that is not listed')
  assert.ok(!has('stub', /notifications/), 'a listed stub is not a failure')
  assert.ok(has('5xx', /\/platform\/boom/), 'a 5xx')
  assert.ok(!has('4xx', /missing/), 'a listed refusal is not a failure')
  assert.ok(has('dom', /Failed to/), 'a dead-end text')
  assert.ok(has('console.error', /something broke/), 'a console error')
  assert.ok(!has('console.error', /usercentrics/), 'noise is not a failure')
  assert.ok(!has('console.error', /ERR_BLOCKED/), 'a blocked third party is not a failure')
  assert.ok(has('pageerror', /page exploded/), 'a page error')
  assert.ok(has('unseen', /\/platform\/never/), 'an allow-list entry that was never seen')
  assert.ok(!f.some((x) => x.kind === 'expect' || x.kind === 'signed-out' || x.kind === 'navigation'), `unexpected: ${JSON.stringify(f.filter((x) => ['expect', 'signed-out', 'navigation'].includes(x.kind)))}`)
  assert.ok(r.report.externalHosts.includes('cdn.example.invalid'))
  assert.deepEqual(r.report.mutationAttempts.map((m) => `${m.method} ${m.path}`), [`DELETE /platform/projects/${REF}`])
  assert.ok(r.calls.some((c) => c.path === '/platform/clicked-tab'), 'a tab was clicked')
  assert.ok(!r.calls.some((c) => c.path === '/platform/clicked-delete'), 'a tab on the deny list was not clicked')
  assert.ok(r.calls.some((c) => c.path === '/platform/pg-meta/x/query'), 'a read-only POST went through the guard')
  assert.equal(r.report.pages.filter((p) => p.url.includes(REF)).length, 10)
})

test('crawl.mjs passes a clean stand-in', { skip: !chrome && 'set CRAWL_STANDIN_CHROME to a Chrome executable', timeout: 200_000 }, async () => {
  const r = await crawlStandin(['/organizations', '/org/[slug]', '/project/[ref]', '/project/[ref]/external', '/sign-in'], {
    allowlist: { stubs: [{ op: 'GET /platform/notifications', reach: 'crawl', reason: 'a known stub' }], statuses: [], dom: [] },
  })
  assert.equal(r.code, 0, r.stderr)
  assert.deepEqual(r.report.failures, [])
  assert.equal(r.report.allowlist.unseen.stubs.length, 0)
})

// ---- changes.sh ---------------------------------------------------------------------------------------------
test('changes.sh tells which parts a push touched, and runs on main whatever it touched', () => {
  const dir = mkdtempSync(join(tmpdir(), 'crawl-changes-'))
  const git = (...a) => execFileSync('git', ['-C', dir, '-c', 'user.name=t', '-c', 'user.email=t@example.com', ...a], { encoding: 'utf8' }).trim()
  const put = (f, c) => {
    const p = join(dir, f)
    mkdirSync(join(p, '..'), { recursive: true })
    writeFileSync(p, c)
    git('add', f)
  }
  git('init', '-q', '-b', 'main')
  put('README.md', 'a')
  git('commit', '-q', '-m', 'base')
  git('update-ref', 'refs/remotes/origin/main', 'HEAD')
  const run = (env) => {
    const out = execFileSync('bash', [join(HERE, 'changes.sh'), 'studio=^(studio/|internal/versions/)', 'api=^internal/(api|proxy)/'], { cwd: dir, env: { ...process.env, GITHUB_EVENT_NAME: 'push', ...env }, encoding: 'utf8' })
    return Object.fromEntries(out.trim().split('\n').map((l) => l.split('=')))
  }
  // the first push of a branch: against the merge base with main
  git('checkout', '-q', '-b', 'ws/x')
  put('internal/api/x.go', 'package api')
  git('commit', '-q', '-m', 'api')
  assert.deepEqual(run({ GITHUB_REF: 'refs/heads/ws/x', EVENT_BEFORE: '0'.repeat(40) }), { studio: 'false', api: 'true', always: 'false', run: 'true' })
  // a later push: the files since the old commit
  const before = git('rev-parse', 'HEAD')
  put('docs/a.md', 'x')
  git('commit', '-q', '-m', 'docs')
  assert.deepEqual(run({ GITHUB_REF: 'refs/heads/ws/x', EVENT_BEFORE: before }), { studio: 'false', api: 'false', always: 'false', run: 'false' })
  put('studio/PATCHSET', '4')
  git('commit', '-q', '-m', 'studio')
  assert.deepEqual(run({ GITHUB_REF: 'refs/heads/ws/x', EVENT_BEFORE: before }), { studio: 'true', api: 'false', always: 'false', run: 'true' })
  // main, a manual run and a schedule always run (the release gate needs a run on the tagged commit)
  assert.deepEqual(run({ GITHUB_REF: 'refs/heads/main', EVENT_BEFORE: git('rev-parse', 'HEAD') }), { studio: 'false', api: 'false', always: 'true', run: 'true' })
  assert.equal(run({ GITHUB_EVENT_NAME: 'schedule', GITHUB_REF: 'refs/heads/main' }).run, 'true')
  assert.equal(run({ GITHUB_EVENT_NAME: 'workflow_dispatch', GITHUB_REF: 'refs/heads/ws/x', EVENT_BEFORE: '' }).always, 'true')
})
