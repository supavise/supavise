// The logic of the Studio crawl that needs no browser and no network: which routes to visit with which
// fixtures, how a response, a console line or a page text is judged against the allow-list, which calls
// the mutation guard lets through, and the report. crawl.mjs drives Chromium with it; selftest.mjs tests it
// (`node --test tests/linux/crawl/selftest.mjs`, no variables).
import { readFileSync } from 'node:fs'

export const readJSON = (p) => JSON.parse(readFileSync(p, 'utf8'))

// ---- routes ------------------------------------------------------------------------------------
const PARAM = /\[\[?(\.\.\.)?([A-Za-z0-9_]+)\]?\]/g

// routeParams('/project/[ref]/functions/[functionSlug]') -> [{name:'ref'}, {name:'functionSlug'}]
export function routeParams(route) {
  return [...route.matchAll(PARAM)].map((m) => ({ name: m[2], catchAll: !!m[1] }))
}

// fillRoute substitutes the values; a catch-all value may hold slashes and may be empty.
export function fillRoute(route, values) {
  return route
    .replace(PARAM, (_m, dots, name) => {
      const v = values[name]
      if (v === undefined) throw new Error(`no value for [${name}] in ${route}`)
      return dots ? v : encodeURIComponent(v)
    })
    .replace(/\/{2,}/g, '/')
    .replace(/(.)\/$/, '$1')
}

// glob: `*` matches any run of characters (slashes included), the rest is literal.
export function globRegExp(glob) {
  return new RegExp(`^${glob.split('*').map((s) => s.replace(/[.+?^${}()|[\]\\]/g, '\\$&')).join('.*')}$`)
}

// pagesOf reads Next's routes-manifest.json: every page the build serves.
export function pagesOf(manifest) {
  const pages = new Set()
  for (const r of [...(manifest.staticRoutes ?? []), ...(manifest.dynamicRoutes ?? [])]) {
    if (r.page && !/^\/(?:_error|_not-found|404|500)$/.test(r.page) && !r.page.startsWith('/api/')) pages.add(r.page)
  }
  return [...pages].sort()
}

// expandRoutes decides what to visit.
//   rules.skip     [{route (glob), reason}]            not visited
//   rules.hidden   [{route (glob), feature}]           visited in the informational pass
//   rules.params   [{route (glob), param, from}]       where a parameter's values come from (first match wins)
//   rules.defaults {param: collection}                 the same for every route
//   rules.catchAll {route: [value...]}                 values of an optional catch-all
//   rules.refs     {route (glob): [fixture key...]}    which projects a [ref] route is visited with (default rules.defaultRefs)
// fixtures: { orgs: [slug], projects: {key: ref}, byRef: {ref: {collection: [value]}}, global: {collection: [value]} }
// A collection is looked up under the visit's project (byRef) first, then in global. A route none of whose
// parameters has a value is `noFixture`, unless it is skipped. Returns { visits, skipped, noFixture }.
export function expandRoutes(pages, rules, fixtures) {
  const skip = (rules.skip ?? []).map((s) => ({ ...s, re: globRegExp(s.route) }))
  const hidden = (rules.hidden ?? []).map((s) => ({ ...s, re: globRegExp(s.route) }))
  const paramRules = (rules.params ?? []).map((s) => ({ ...s, re: globRegExp(s.route) }))
  const refRules = Object.entries(rules.refs ?? {}).map(([g, keys]) => ({ re: globRegExp(g), keys }))
  const visits = []
  const skipped = []
  const noFixture = []

  const collection = (page, name) => paramRules.find((r) => r.param === name && r.re.test(page))?.from ?? rules.defaults?.[name] ?? null
  const valuesOf = (coll, ref) => fixtures.byRef?.[ref]?.[coll] ?? fixtures.global?.[coll] ?? []

  for (const page of pages) {
    const sk = skip.find((s) => s.re.test(page))
    if (sk) {
      skipped.push({ route: page, reason: sk.reason })
      continue
    }
    const informational = hidden.find((h) => h.re.test(page))?.feature
    const params = routeParams(page)
    const hasRef = params.some((p) => p.name === 'ref')
    const refKeys = hasRef ? (refRules.find((r) => r.re.test(page))?.keys ?? rules.defaultRefs ?? Object.keys(fixtures.projects ?? {})) : [null]
    const custom = (rules.urls ?? []).find((u) => u.route === page)
    if (custom) {
      for (const key of refKeys) {
        const ref = fixtures.projects?.[key]
        if (!ref) continue
        for (const t of custom.urls) visits.push({ page, url: t.replace('{ref}', ref), project: key, params: { ref }, informational: informational ?? null })
      }
      continue
    }
    const found = []
    for (const key of refKeys) {
      const ref = key === null ? null : fixtures.projects?.[key]
      if (hasRef && !ref) continue
      // the cartesian product of the other parameters' values under this project
      let combos = [{ ...(hasRef ? { ref } : {}) }]
      for (const p of params) {
        if (p.name === 'ref') continue
        let values
        if (p.name === 'slug') values = fixtures.orgs ?? []
        else if (p.catchAll) values = rules.catchAll?.[page] ?? ['']
        else {
          const coll = collection(page, p.name)
          values = coll ? valuesOf(coll, ref) : []
        }
        combos = combos.flatMap((c) => values.map((v) => ({ ...c, [p.name]: v })))
      }
      for (const c of combos) found.push({ key, values: c })
    }
    if (found.length === 0) {
      noFixture.push({ route: page, params: params.map((p) => p.name) })
      continue
    }
    for (const f of found) {
      visits.push({ page, url: fillRoute(page, f.values), project: f.key, params: f.values, informational: informational ?? null })
    }
  }
  return { visits, skipped, noFixture }
}

// stateVisits: the project home and the Branches page (rules.states.routes) of each branch state the fixtures
// made (rules.states.projects: keys of fixtures.projects). A state that the fixtures lack is a failure: the
// fixture is the point of the visit.
export function stateVisits(rules, fixtures) {
  const visits = []
  const missing = []
  for (const key of rules.states?.projects ?? []) {
    const ref = fixtures.projects?.[key]
    if (!ref) {
      missing.push(key)
      continue
    }
    for (const r of rules.states.routes) visits.push({ page: r, url: fillRoute(r, { ref }), project: key, params: { ref }, informational: null })
  }
  return { visits, missing }
}

// ---- the allow-list ----------------------------------------------------------------------------
// opRegExp('/platform/projects/{ref}/status') matches a concrete path; {x} is one path segment.
export function opRegExp(path) {
  return new RegExp(`^${path.split(/\{[^}]*\}/).map((s) => s.replace(/[.+?^${}()|[\]\\*]/g, '\\$&')).join('[^/]+')}$`)
}

function parseOp(op) {
  const i = op.indexOf(' ')
  return { method: op.slice(0, i), path: op.slice(i + 1), re: opRegExp(op.slice(i + 1)) }
}

export class Allowlist {
  constructor(doc) {
    const wrap = (e, extra = {}) => ({ ...e, ...(e.op ? parseOp(e.op) : {}), ...extra, seen: 0 })
    this.stubs = (doc.stubs ?? []).map((e) => wrap(e))
    this.statuses = (doc.statuses ?? []).map((e) => wrap(e))
    this.dom = (doc.dom ?? []).map((e) => ({ ...e, routeRe: globRegExp(e.route), seen: 0 }))
  }

  static methodMatches(entry, method) {
    return entry.method === '*' || entry.method === method || (method === 'HEAD' && entry.method === 'GET')
  }

  // stub: the entry that allows a stub-answered call, or null. Marks it seen.
  stub(method, path) {
    const e = this.stubs.find((x) => Allowlist.methodMatches(x, method) && x.re.test(path))
    if (e) e.seen++
    return e ?? null
  }

  status(method, path, status) {
    const e = this.statuses.find((x) => x.status === status && Allowlist.methodMatches(x, method) && x.re.test(path))
    if (e) e.seen++
    return e ?? null
  }

  domText(route, text) {
    const e = this.dom.find((x) => x.routeRe.test(route) && x.text === text)
    if (e) e.seen++
    return e ?? null
  }

  // pendingDom: the dom entries of reach "crawl" that the run has not seen yet and that this route may show. A visit waits for
  // their text before it judges the page, because a card that queries logs retries for a few seconds before it prints its error,
  // so whether the text is on the page at the end of the settle is a matter of timing, and an entry that the run must see would
  // fail it for nothing.
  pendingDom(route) {
    return this.dom.filter((e) => e.reach === 'crawl' && e.seen === 0 && e.routeRe.test(route))
  }

  // unseen: the entries the run must have hit and did not (reach "crawl"); each makes the run fail.
  unseen() {
    const pick = (list) => list.filter((e) => e.reach === 'crawl' && e.seen === 0)
    return {
      stubs: pick(this.stubs).map((e) => e.op),
      statuses: pick(this.statuses).map((e) => `${e.op} ${e.status}`),
      dom: pick(this.dom).map((e) => `${e.route} "${e.text}"`),
    }
  }

  // seenStatic: entries marked "static" that the run did hit; they can become "crawl".
  seenStatic() {
    const pick = (list) => list.filter((e) => e.reach === 'static' && e.seen > 0)
    return {
      stubs: pick(this.stubs).map((e) => e.op),
      statuses: pick(this.statuses).map((e) => `${e.op} ${e.status}`),
      dom: pick(this.dom).map((e) => `${e.route} "${e.text}"`),
    }
  }
}

// ---- judging what a page did ----------------------------------------------------------------------
// Hosts: the dashboard, the Management API, and the project hosts (<ref>.api.<domain>). A port is ignored.
// baseUrl is the origin of a node name ("studio", "api" or "<ref>.api"): the port is written when the proxy is not on 80.
export function baseUrl(cfg, sub) {
  const port = cfg.proxy?.port
  return `http://${sub}.${cfg.domain}${port && port !== 80 ? `:${port}` : ''}`
}

export function hostKind(hostWithPort, domain) {
  const host = hostWithPort.replace(/:\d+$/, '')
  if (host === `studio.${domain}`) return 'studio'
  if (host === `api.${domain}`) return 'api'
  if (host.endsWith(`.api.${domain}`)) return 'project'
  return 'external'
}

const API_PATH = /^\/(?:platform|v1|v2)\//

// judgeResponse: the findings of one response. A finding is {kind, op, detail, allowed}. `stub` is the
// X-Supavise-Stub header. Only the Management API is held to the stub rule; every host is held to the
// 5xx and 4xx rules; a 4xx or a 5xx is allowed by a "statuses" entry.
export function judgeResponse({ method, host, path, status, stub }, domain, allow) {
  const kind = hostKind(host, domain)
  if (kind === 'external') return []
  const out = []
  if (kind === 'api' && API_PATH.test(path) && stub) {
    const e = allow.stub(method, path)
    out.push({ kind: 'stub', op: `${method} ${path}`, allowed: !!e, detail: e ? e.reason : 'answered by a stub that is not in the allow-list' })
  }
  if (status >= 500) {
    // an honest 5xx of a handler (a 503 while a project comes up) is listed under "statuses" like a 4xx; any other fails
    const e = allow.status(method, path, status)
    out.push({ kind: '5xx', op: `${method} ${path}`, status, allowed: !!e, detail: e ? e.reason : `${status}` })
  } else if (status >= 400) {
    const e = allow.status(method, path, status)
    out.push({ kind: '4xx', op: `${method} ${path}`, status, allowed: !!e, detail: e ? e.reason : `${status} that is not in the allow-list` })
  }
  return out
}

// A console line, page error or failed request is noise when a noise.json entry matches it. An entry is
//   pattern      a regular expression over the text of the line
//   reason       why the line does not count (required)
//   onPage       a regular expression over the pathname of the page when the line appeared (default: any page)
//   blockedHost  a third party host, or a list of them, that the crawl blocked: the line is a consequence of the block,
//                so it is noise only in a run that blocked one of them (a node with internet does not show it)
//   with         a regular expression over a console line that is noise itself: the entry counts only together with such
//                a line on the same record, one for one (pairedLeftover decides at the end of the record)
// ctx: { pathname, blocked } where blocked is the Set of the hosts the crawl has blocked so far.
export function noiseMatch(list, text, ctx = {}) {
  return (list ?? []).find((n) => noiseApplies(n, ctx) && new RegExp(n.pattern).test(text)) ?? null
}

function noiseApplies(n, { pathname, blocked } = {}) {
  if (n.onPage && !(typeof pathname === 'string' && new RegExp(n.onPage).test(pathname))) return false
  if (n.blockedHost) {
    const hosts = [].concat(n.blockedHost)
    if (!hosts.some((h) => blocked?.has(h))) return false
  }
  return true
}

// pairedLeftover: the deferred lines ({entry, text}) that found no console noise line to go with. Each console line pairs
// with one deferred line.
export function pairedLeftover(pending, consoleNoise) {
  const free = [...consoleNoise]
  const left = []
  for (const p of pending) {
    const re = new RegExp(p.entry.with)
    const i = free.findIndex((t) => re.test(t))
    if (i < 0) left.push(p)
    else free.splice(i, 1)
  }
  return left
}

// The page texts that mean a dead end. text is a substring; the oracles of the lane spec.
export const DOM_ORACLES = [
  'Failed to', 'Error fetching', 'Something went wrong', 'Unknown interface', '[object Object]', 'NaN',
  'OPENAI_API_KEY', 'Sorry! An unexpected error occurred', 'Application error',
]

// domFindings returns the oracle texts found in the page and whether the allow-list covers each.
export function domFindings(route, pageText, allow, extra = []) {
  const out = []
  for (const t of [...DOM_ORACLES, ...extra]) {
    const hit = t === 'NaN' ? /(?:^|[^A-Za-z0-9_])NaN(?:$|[^A-Za-z0-9_])/.test(pageText) : pageText.includes(t)
    if (!hit) continue
    const e = allow.domText(route, t)
    out.push({ kind: 'dom', text: t, allowed: !!e, detail: e ? e.reason : `the page shows "${t}"` })
  }
  return out
}

// ---- the mutation guard --------------------------------------------------------------------------
const MUTATING_SQL = /\b(?:insert|update|delete|drop|alter|create|truncate|grant|revoke|call|copy|vacuum|reindex|notify|set|reset|do)\b/i

const DOLLAR_TAG = /\$(?:[A-Za-z_][A-Za-z_0-9]*)?\$/y

// splitSQL cuts a script into its statements at the semicolons that are outside quotes, comments and dollar-quoted bodies.
// A statement is { code, bodies }: `code` has its comments dropped, its strings written '', its quoted identifiers "" and
// its dollar-quoted bodies $$; `bodies` holds the text of those bodies. Returns null when a quote or a comment stays open.
export function splitSQL(sql) {
  const s = String(sql ?? '')
  const out = []
  let code = ''
  let bodies = []
  const flush = () => {
    if (code.trim() !== '' || bodies.length > 0) out.push({ code: code.trim(), bodies })
    code = ''
    bodies = []
  }
  const quoted = (i, q) => {
    // the end of a '...' or "..." that starts at i; a doubled quote is part of it
    for (let j = i + 1; j < s.length; j++) {
      if (s[j] !== q) continue
      if (s[j + 1] === q) j++
      else return j
    }
    return -1
  }
  for (let i = 0; i < s.length; ) {
    const c = s[i]
    if (c === '-' && s[i + 1] === '-') {
      const j = s.indexOf('\n', i)
      code += ' '
      i = j < 0 ? s.length : j
    } else if (c === '/' && s[i + 1] === '*') {
      let depth = 1
      let j = i + 2
      while (j < s.length && depth > 0) {
        if (s[j] === '/' && s[j + 1] === '*') (depth++, (j += 2))
        else if (s[j] === '*' && s[j + 1] === '/') (depth--, (j += 2))
        else j++
      }
      if (depth > 0) return null
      code += ' '
      i = j
    } else if (c === "'" || c === '"') {
      const j = quoted(i, c)
      if (j < 0) return null
      code += c + c
      i = j + 1
    } else if (c === '$' && !/[\w$]/.test(s[i - 1] ?? ' ')) {
      DOLLAR_TAG.lastIndex = i
      const m = DOLLAR_TAG.exec(s)
      if (!m) {
        code += c
        i++
        continue
      }
      const j = s.indexOf(m[0], i + m[0].length)
      if (j < 0) return null
      bodies.push(s.slice(i + m[0].length, j))
      code += '$$'
      i = j + m[0].length
    } else if (c === ';') {
      flush()
      i++
    } else {
      code += c
      i++
    }
  }
  flush()
  return out
}

// What the dashboard sends before or around a read, none of which changes what the database holds:
//   set [session|local] search_path | statement_timeout | lock_timeout ...   (the reports, the advisors, the queue metrics)
//   the helpers it makes in pg_temp, which go with the connection: a function (the row count estimate, the index
//   advisor's explain) and an enum type, whose body or text holds no word that changes state
//   a do block that only reads a table and keeps the result with set_config (the migrations list)
const SET_SAFE = /^set\s+(?:(?:session|local)\s+)?(?:search_path|statement_timeout|lock_timeout)\s*(?:=|to)\s*[\w\s'",.]+$/i
const TEMP_FUNCTION = /^create\s+(?:or\s+replace\s+)?function\s+pg_temp\./i
const TEMP_TYPE_DROP = /^drop\s+type\s+if\s+exists\s+pg_temp\.\w+(?:\s+cascade)?$/i
const TEMP_TYPE = /^create\s+type\s+pg_temp\.\w+\s+as\s+enum\s*\((?:\s*''\s*,?)*\s*\)$/i
const DO_BLOCK = /^do\s+(?:language\s+\w+\s+)?\$\$(?:\s+language\s+\w+)?$/i

// plpgsqlReadsOnly: the body of a function or a do block holds no word that changes state, no nested body, and (a do block
// runs at once) no dynamic SQL and no perform other than set_config.
function plpgsqlReadsOnly(body, { immediate }) {
  const parts = splitSQL(body)
  if (parts === null || parts.some((p) => p.bodies.length > 0)) return false
  const code = parts.map((p) => p.code).join(';')
  if (MUTATING_SQL.test(code)) return false
  if (/\bperform\b(?!\s+(?:pg_catalog\.)?set_config\s*\()/i.test(code)) return false
  return !(immediate && /\bexecute\b/i.test(code))
}

function readsOnly(code) {
  return /^\(*\s*(?:select|with|explain|show|table|values)\b/i.test(code) && !MUTATING_SQL.test(code.replace(/\bexplain\s*\(?\s*analyze\b/gi, 'explain')) && !/\binto\b/i.test(code)
}

// isReadOnlySQL: every statement of the script is a read (select, with, explain, show, table, values: no word that changes
// state, no select into) or one of the statements above. A false negative aborts a harmless query and records it as a
// mutation attempt; a false positive would let a write through, so the check errs on the first side.
export function isReadOnlySQL(sql) {
  const stmts = splitSQL(sql)
  if (!stmts || stmts.length === 0) return false
  return stmts.every(({ code, bodies }) => {
    if (bodies.length === 0) return SET_SAFE.test(code) || TEMP_TYPE_DROP.test(code) || TEMP_TYPE.test(code) || readsOnly(code)
    if (DO_BLOCK.test(code)) return bodies.length === 1 && plpgsqlReadsOnly(bodies[0], { immediate: true })
    if (TEMP_FUNCTION.test(code)) {
      return !MUTATING_SQL.test(code.replace(/^create\s+(?:or\s+replace\s+)?function/i, '')) && bodies.every((b) => plpgsqlReadsOnly(b, { immediate: false }))
    }
    return readsOnly(code)
  })
}

// isReadOnlyGraphQL: a query (or the shorthand {...}) and no mutation or subscription, whatever the strings and comments say.
export function isReadOnlyGraphQL(doc) {
  if (typeof doc !== 'string') return false
  const code = doc.replace(/"""[\s\S]*?"""|"(?:[^"\\\n]|\\.)*"/g, '""').replace(/#[^\n]*/g, ' ')
  return /\S/.test(code) && !/\b(?:mutation|subscription)\b/.test(code)
}

// sqlHead: the first n characters of a query on one line, to show what the guard aborted.
export const sqlHead = (sql, n = 120) => String(sql ?? '').replace(/\s+/g, ' ').trim().slice(0, n)

// READ_POSTS are the POST routes that only read: path regexps.
const READ_POSTS = [
  /^\/platform\/projects\/[^/]+\/api-keys\/temporary$/,
  /^\/platform\/projects\/[^/]+\/analytics\/endpoints\/[^/]+$/,
  /^\/v1\/projects\/[^/]+\/analytics\/endpoints\/[^/]+$/,
  /^\/platform\/telemetry\//,
  /^\/platform\/storage\/[^/]+\/buckets\/[^/]+\/objects\/(?:list|list-v2|sign|sign-multi|public-url)$/,
  /^\/v1\/projects\/[^/]+\/network-bans\/retrieve$/,
  /^\/platform\/organizations\/[^/]+\/billing\/[a-z/-]*preview$/,
  /^\/v2\/projects\/[^/]+\/advisors\/run$/,
  /^\/platform\/auth\/[^/]+\/validate\/spam$/,
  /^\/platform\/projects\/[^/]+\/health$/,
  // the organization's available Postgres versions, asked by the new-project form
  /^\/platform\/organizations\/[^/]+\/available-versions$/,
  // the sign-in form posts the login event after every sign-in; the crawl signs in once per context
  /^\/platform\/profile\/audit-login$/,
]

const PG_META_QUERY = /^\/platform\/pg-meta\/[^/]+\/query$/
const GRAPHQL = /^\/platform\/projects\/[^/]+\/api\/graphql$/

function queryOf(postData) {
  try {
    const q = JSON.parse(postData ?? '{}')?.query
    return typeof q === 'string' ? q : null
  } catch {
    return null
  }
}

// guardVerdict: 'allow' for a call that cannot change state, otherwise 'abort'. postData is the request body.
export function guardVerdict(method, path, postData) {
  if (method === 'GET' || method === 'HEAD' || method === 'OPTIONS') return 'allow'
  if (method === 'POST') {
    if (PG_META_QUERY.test(path)) return isReadOnlySQL(queryOf(postData)) ? 'allow' : 'abort'
    if (GRAPHQL.test(path)) return isReadOnlyGraphQL(queryOf(postData)) ? 'allow' : 'abort'
    if (READ_POSTS.some((re) => re.test(path))) return 'allow'
  }
  return 'abort'
}

// guardNote: what to record with an aborted call, so that a read the guard does not know can be taught to it: the head of the
// SQL or GraphQL text. Other bodies are not recorded (they can hold secrets).
export function guardNote(method, path, postData) {
  if (method !== 'POST' || !(PG_META_QUERY.test(path) || GRAPHQL.test(path))) return ''
  return sqlHead(queryOf(postData))
}

// ---- safe clicks --------------------------------------------------------------------------------
export const DENY_CLICK = /\b(?:delete|remove|drop|reset|revoke|pause|restart|transfer|upgrade|sign\s*out|log\s*out|disable|empty|rotate|regenerate|unlink|merge|deploy|confirm|cancel subscription|terminate|destroy|wipe|clear)\b/i

// safeToClick: a label that is not on the deny list. An empty label is allowed only for a tab or a menu trigger,
// which the caller decides.
export function safeToClick(label) {
  return !DENY_CLICK.test(label ?? '')
}

// ---- the report --------------------------------------------------------------------------------
// A finding fails the run unless it is allowed or the page is informational.
export function failures(report) {
  const out = []
  for (const p of report.pages) {
    if (p.informational) continue
    for (const f of p.findings) if (!f.allowed) out.push({ where: p.url, ...f })
  }
  for (const pr of report.probes) {
    if (pr.ok === false) out.push({ where: `probe ${pr.name}`, kind: 'probe', detail: pr.detail })
    // what the pages of a probe did is judged like the route pass: a stub or an error nobody listed fails the run
    for (const f of pr.findings ?? []) if (!f.allowed) out.push({ where: `probe ${pr.name}`, ...f })
  }
  for (const c of report.crashed ?? []) out.push(c)
  for (const n of report.noFixture ?? []) out.push({ where: n.route, kind: 'no-fixture', detail: `no fixture for [${n.params.join('], [')}]` })
  const un = report.allowlist?.unseen ?? { stubs: [], statuses: [], dom: [] }
  for (const [what, list] of Object.entries(un)) for (const e of list) out.push({ where: `allow-list ${what}`, kind: 'unseen', detail: `${e} was never seen in this run: remove the entry` })
  return out
}

const esc = (s) => String(s).replace(/\|/g, '\\|').replace(/\n/g, ' ')

// summaryMarkdown: the table for $GITHUB_STEP_SUMMARY.
export function summaryMarkdown(report) {
  const fails = failures(report)
  const L = []
  L.push(`## Studio crawl: ${fails.length === 0 ? 'passed' : `${fails.length} failure(s)`}`)
  L.push('')
  L.push(`${report.pages.length} page visits, ${report.probes.length} probes, ${report.totals.calls} API calls, ${report.totals.stubs} stub answers, ${report.totals.mutationAttempts} aborted mutation attempts.`)
  if (fails.length) {
    L.push('', '### Failures', '', '| Where | Kind | Detail |', '|---|---|---|')
    for (const f of fails.slice(0, 200)) L.push(`| ${esc(f.where)} | ${esc(f.kind)} | ${esc(f.op ? `${f.op}: ` : '')}${esc(f.detail ?? f.text ?? '')} |`)
    if (fails.length > 200) L.push(`| | | and ${fails.length - 200} more in crawl-report.json |`)
  }
  L.push('', '### Probes', '', '| Probe | Result | Seconds | Detail |', '|---|---|---|---|')
  for (const p of report.probes) L.push(`| ${esc(p.name)} | ${p.ok === true ? 'ok' : p.ok === null ? 'skipped' : 'FAILED'} | ${(p.ms / 1000).toFixed(0)} | ${esc(p.detail ?? '')} |`)
  L.push('', '### Pages', '', '| Route | Project | Calls | Stubs | 4xx | 5xx | Page errors | Findings |', '|---|---|---|---|---|---|---|---|')
  for (const p of report.pages) {
    const bad = p.findings.filter((f) => !f.allowed).length
    L.push(`| ${esc(p.url)}${p.informational ? ' (info)' : ''} | ${esc(p.project ?? '')} | ${p.calls} | ${p.stubs} | ${p.n4xx} | ${p.n5xx} | ${p.pageErrors} | ${bad === 0 ? '' : bad} |`)
  }
  if (report.externals?.length) {
    L.push('', '### Third parties the crawl blocked', '', '| Host | Asked by | Documented in noise.json |', '|---|---|---|')
    for (const x of report.externals) L.push(`| ${esc(x.host)} | ${esc(x.pages.slice(0, 3).join(', '))}${x.pages.length > 3 ? ` and ${x.pages.length - 3} more` : ''} | ${x.documented ? 'yes' : '**no**'} |`)
  }
  const seen = report.allowlist?.seenStatic
  if (seen && (seen.stubs.length || seen.statuses.length || seen.dom.length)) {
    L.push('', '### Allow-list entries marked static that this run hit (set their reach to "crawl")', '')
    for (const e of [...seen.stubs, ...seen.statuses, ...seen.dom]) L.push(`- ${e}`)
  }
  L.push('')
  return L.join('\n')
}
