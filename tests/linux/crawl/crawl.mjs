// The Studio crawl: the real Studio against the real daemon, headless Chromium, one worker.
//
//   node crawl.mjs /path/crawl-config.json
//
// tests/linux/studio-crawl.sh starts the node with the Studio build under test, makes the fixtures (seed.mjs) and runs
// this. Two passes:
//
//   route pass   every page of the build's routes-manifest.json with the fixtures of routes.json, one browser context per
//                project and a guard that aborts every call that could change state. Per page it records each response
//                (a stub answer, a 4xx or a 5xx), page errors, console errors, failed requests and the texts that mean a
//                dead end, and it clicks what is safe to click.
//   probe pass   probes.mjs: user flows on throwaway projects and branches, judged by the state of the server and not by
//                a toast.
//
// The run fails (exit 1) on a stub answer that tests/linux/crawl/allowlist.json does not list, a 5xx or a 4xx the list does
// not give a reason for, a page error, a console error outside noise.json, a call the mutation guard aborted while a page
// loaded (a read the guard does not know), a failed probe, a page with a parameter and no fixture, and an allow-list entry
// (reach "crawl") that the run never saw, so the list only shrinks. Output in
// <out>: crawl-report.json, summary.md (also appended to $GITHUB_STEP_SUMMARY), calls.jsonl, screenshots/.
//
// Config fields: domain, proxy {host, port}, owner {email, password}, owner_pat, org, org2, out, routes_manifest,
// chrome (optional executable), functions, idle_pause (see studio-crawl.sh).
import { appendFileSync, mkdirSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { launch, newPage, openContext, pageText, safeClicks, settle, shot, signIn, Tracker, waitText } from './browser.mjs'
import {
  Allowlist, baseUrl, domFindings, expandRoutes, failures, pagesOf, readJSON, stateVisits, summaryMarkdown,
} from './lib.mjs'
import { Node } from './node.mjs'
import { runProbes } from './probes.mjs'

const HERE = dirname(fileURLToPath(import.meta.url))
const cfgPath = process.argv[2]
if (!cfgPath) {
  console.error('usage: node crawl.mjs <crawl-config.json>')
  process.exit(2)
}
const cfg = readJSON(cfgPath)
const OUT = cfg.out
const studio = baseUrl(cfg, 'studio')
const t0 = Date.now()
const log = (m) => console.error(`${String(Math.round((Date.now() - t0) / 1000)).padStart(5)}s crawl: ${m}`)

mkdirSync(join(OUT, 'screenshots'), { recursive: true })
const callsFile = join(OUT, 'calls.jsonl')
rmSync(callsFile, { force: true })
writeFileSync(callsFile, '')

const fx = readJSON(join(OUT, 'fixtures.json'))
// routes.json, allowlist.json and noise.json beside this file, unless the config names others (the stand-in test does)
const rules = readJSON(cfg.rules ?? join(HERE, 'routes.json'))
const allow = new Allowlist(readJSON(cfg.allowlist ?? join(HERE, 'allowlist.json')))
const noise = readJSON(cfg.noise ?? join(HERE, 'noise.json'))
const manifest = readJSON(cfg.routes_manifest)
const node = new Node(cfg)
const tracker = new Tracker({ cfg, allow, noise, callsFile })

// ---- what to visit ------------------------------------------------------------------------------
const anonymous = new Set(rules.anonymous ?? [])
const pages = pagesOf(manifest)
const plan = expandRoutes(pages, rules, fx)
const states = stateVisits(rules, fx)
const visits = [...plan.visits.filter((v) => !anonymous.has(v.page)), ...states.visits]
const missingStates = states.missing.filter((k) => !(k === 'idle' && fx.unavailable?.idle))
log(`${pages.length} pages in the build -> ${plan.visits.length} visits, ${plan.skipped.length} skipped, ${plan.noFixture.length} without a fixture; ${states.visits.length} state visits`)

function expectations(v) {
  const norm = v.url.replace(/^\/project\/[a-z]{20}/, '/project/[ref]')
  return (rules.expect ?? []).filter(
    (e) =>
      (e.route === v.page || e.route === norm) &&
      (!e.project || fx.projects[e.project] === v.params?.ref) &&
      Object.entries(e.match ?? {}).every(([k, want]) => v.params?.[k] === (want === '@org' ? cfg.org : want)),
  )
}

const results = []
let seq = 0

async function visit(page, v, { anonymousVisit = false } = {}) {
  const label = v.url
  const rec = tracker.begin(label)
  const res = { url: v.url, page: v.page, project: v.project, informational: v.informational, findings: rec.findings, links: rec.links }
  const started = Date.now()
  try {
    const resp = await page.goto(`${studio}${v.url}`, { waitUntil: 'domcontentloaded', timeout: 60_000 })
    await settle(page)
    if (!anonymousVisit && new URL(page.url()).pathname.startsWith('/sign-in') && !v.url.startsWith('/sign-in')) {
      tracker.add({ kind: 'signed-out', allowed: false, detail: 'the page sent the signed-in user to /sign-in' })
    }
    res.status = resp?.status() ?? 0
    let text = await pageText(page)
    for (const e of expectations(v)) {
      if (!(await waitText(page, e.text, 15_000))) tracker.add({ kind: 'expect', allowed: false, detail: `the page does not show "${e.text}": ${e.why}` })
    }
    rec.phase = 'click' // from here a call the guard aborts may be the change that a click started
    res.clicks = await safeClicks(page, rec)
    await settle(page, { quiet: 800, max: 6_000 })
    for (const e of allow.pendingDom(v.page)) await waitText(page, e.text, 15_000)
    text = `${text}\n${await pageText(page)}`
    for (const f of domFindings(v.page, text, allow)) tracker.add(f)
    res.screenshot = await shot(page, join(OUT, 'screenshots'), `${String(++seq).padStart(3, '0')}-${v.url}`)
    tracker.finish()
  } catch (e) {
    tracker.add({ kind: 'navigation', allowed: false, detail: String(e?.message ?? e).split('\n')[0].slice(0, 300) })
    res.screenshot = await shot(page, join(OUT, 'screenshots'), `${String(++seq).padStart(3, '0')}-failed-${v.url}`)
  }
  Object.assign(res, {
    calls: rec.calls, stubs: rec.stubs, n4xx: rec.n4xx, n5xx: rec.n5xx, pageErrors: rec.pageErrors, noise: rec.noise, ms: Date.now() - started,
  })
  results.push(res)
  const bad = res.findings.filter((f) => !f.allowed).length
  log(`${bad ? 'FAIL' : 'ok  '} ${v.url}${v.informational ? ' (info)' : ''}${bad ? ` ${bad} finding(s)` : ''}`)
}

const browser = await launch(cfg)
let report
try {
  // ---- route pass: one context per project ----------------------------------------------------------
  const groups = new Map()
  for (const v of visits) {
    const k = v.informational ? '~info' : (v.project ?? '')
    if (!groups.has(k)) groups.set(k, [])
    groups.get(k).push(v)
  }
  const order = ['', ...[...groups.keys()].filter((k) => k !== '' && k !== '~info'), '~info'].filter((k) => groups.has(k))
  // A group that cannot even sign in (or a browser that dies) is a failure of the run, not the end of it: the other groups
  // and the probes still run, and the report says what happened.
  const crashed = []
  for (const k of order) {
    let ctx
    try {
      ctx = await openContext(browser, cfg, tracker, { guard: true })
      const page = await newPage(ctx, tracker)
      const rec = tracker.begin(`sign-in ${k || 'account'}`)
      await signIn(page, cfg)
      tracker.finish()
      results.push({ url: `(sign-in for ${k || 'the account pages'})`, page: '(sign-in)', project: null, informational: null, findings: rec.findings, links: [], calls: rec.calls, stubs: rec.stubs, n4xx: rec.n4xx, n5xx: rec.n5xx, pageErrors: rec.pageErrors, noise: rec.noise, ms: 0 })
      for (const v of groups.get(k)) await visit(page, v)
    } catch (e) {
      const msg = String(e?.message ?? e).split('\n')[0].slice(0, 300)
      log(`FAIL the group '${k || 'account'}' stopped: ${msg}`)
      crashed.push({ where: `route pass, group '${k || 'account'}'`, kind: 'crash', detail: msg })
    } finally {
      await ctx?.close().catch(() => {})
    }
  }

  // ---- the pages that need no session -----------------------------------------------------------------
  {
    const ctx = await openContext(browser, cfg, tracker, { guard: true })
    const page = await newPage(ctx, tracker)
    for (const p of rules.anonymous ?? []) {
      if (pages.includes(p)) await visit(page, { page: p, url: p, project: null, params: {}, informational: null }, { anonymousVisit: true })
    }
    await ctx.close()
  }

  // ---- probe pass ----------------------------------------------------------------------------------------
  log('probes')
  const probes = cfg.probes === false ? [] : await runProbes({ browser, cfg, fx, node, tracker, allow, log, outDir: OUT })

  // ---- report ---------------------------------------------------------------------------------------------
  const noFixture = [
    ...plan.noFixture,
    ...missingStates.map((k) => ({ route: `state fixture ${k}`, params: [k] })),
  ]
  report = {
    version: 1,
    started: new Date(t0).toISOString(),
    finished: new Date().toISOString(),
    seconds: Math.round((Date.now() - t0) / 1000),
    totals: { ...tracker.totals, pages: results.length },
    pages: results,
    probes,
    skipped: plan.skipped,
    noFixture,
    unavailable: fx.unavailable ?? {},
    externalHosts: [...tracker.externals.keys()].sort(),
    externals: [...tracker.externals.entries()].sort(([a], [b]) => (a < b ? -1 : 1)).map(([host, pages]) => ({
      host, pages: [...pages].slice(0, 25), documented: (noise.externalHosts ?? []).find((x) => x.host === host)?.reason ?? null,
    })),
    mutationAttempts: tracker.mutationAttempts,
    allowlist: { unseen: allow.unseen(), seenStatic: allow.seenStatic() },
  }
  report.crashed = crashed
  tracker.finish()
  report.failures = failures(report)
} finally {
  await browser.close().catch(() => {})
}

writeFileSync(join(OUT, 'crawl-report.json'), `${JSON.stringify(report, null, 1)}\n`)
const md = summaryMarkdown(report)
writeFileSync(join(OUT, 'summary.md'), md)
if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, `${md}\n`)
if (report.failures.length > 0) {
  console.error(`\n${report.failures.length} failure(s):`)
  for (const f of report.failures.slice(0, 60)) console.error(`  ${f.where}: ${f.kind} ${f.op ? `${f.op} ` : ''}${f.detail ?? f.text ?? ''}`)
  if (report.failures.length > 60) console.error(`  ... and ${report.failures.length - 60} more in ${join(OUT, 'crawl-report.json')}`)
  process.exit(1)
}
log(`passed: ${report.pages.length} page visits, ${report.probes.length} probes`)
