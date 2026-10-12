// The browser side of the Studio crawl: Chromium with the node's made-up host names mapped to the proxy, a context
// that blocks third parties and (in the route pass) every call that could change state, a tracker that turns what a
// page does into findings, and the sign-in. crawl.mjs (the route pass) and probes.mjs (the probes) share it.
import { appendFileSync, mkdirSync } from 'node:fs'
import { join } from 'node:path'

import { chromium } from 'playwright-core'

import { baseUrl, domFindings, guardNote, guardVerdict, hostKind, judgeResponse, noiseMatch, pairedLeftover, safeToClick } from './lib.mjs'

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

export async function launch(cfg) {
  const { domain } = cfg
  const studio = baseUrl(cfg, 'studio')
  const api = baseUrl(cfg, 'api')
  return chromium.launch({
    headless: true,
    ...(cfg.chrome ? { executablePath: cfg.chrome } : {}),
    args: [
      // The node's names are made up (supavise.test): one rule maps them all to the proxy on this machine.
      `--host-resolver-rules=MAP studio.${domain} ${cfg.proxy?.host ?? '127.0.0.1'}, MAP api.${domain} ${cfg.proxy?.host ?? '127.0.0.1'}, MAP *.api.${domain} ${cfg.proxy?.host ?? '127.0.0.1'}`,
      // The node serves plain HTTP: no HTTPS upgrade, and the private-network checks that block a page of a made-up
      // host from reaching loopback are off (the same flags as tests/e2e/readme-shots/shots.mjs).
      '--disable-features=HttpsUpgrades,HttpsFirstBalancedModeAutoEnable,BlockInsecurePrivateNetworkRequests,PrivateNetworkAccessSendPreflights,PrivateNetworkAccessRespectPreflightResults,PrivateNetworkAccessForNavigations,PrivateNetworkAccessForWorkers,LocalNetworkAccessChecks',
      `--unsafely-treat-insecure-origin-as-secure=${studio},${api}`,
      '--font-render-hinting=none',
    ],
  })
}

// stripUpgrade drops `upgrade-insecure-requests` from a CSP: Studio's policy would turn every sub-resource of this
// plain-HTTP node into an https request.
function stripUpgrade(headers) {
  const h = { ...headers }
  for (const k of ['content-security-policy', 'content-security-policy-report-only']) {
    if (h[k]) h[k] = h[k].split(';').filter((d) => d.trim() !== 'upgrade-insecure-requests').join(';')
  }
  return h
}

// A Tracker collects what pages do: one record per label, findings judged against the allow-list and the noise list.
export class Tracker {
  constructor({ cfg, allow, noise, callsFile }) {
    this.cfg = cfg
    this.allow = allow
    this.noise = noise
    this.callsFile = callsFile
    this.mutationAttempts = []
    this.externals = new Map() // host -> the labels of the records that asked for it
    this.totals = { calls: 0, stubs: 0, mutationAttempts: 0 }
    this.ours = new WeakSet() // requests this crawl aborted on purpose
    this.rec = this.fresh('')
  }

  // phase: 'load' while the page loads and settles (Studio only reads then), 'click' once the safe clicks begin (a click can
  // start a change, which the guard aborts). consoleNoise and paired are for noise entries that count only together with a
  // console line (see noiseMatch); abortLogs is how many "Failed to fetch" console lines the guard's aborts can explain.
  fresh(label) {
    return { label, calls: 0, stubs: 0, n4xx: 0, n5xx: 0, pageErrors: 0, findings: [], noise: 0, links: [], phase: 'load', consoleNoise: [], paired: [], abortLogs: 0 }
  }

  begin(label) {
    this.finish()
    this.rec = this.fresh(label)
    return this.rec
  }

  // finish settles the noise entries that count only with a console line: the ones that found none are findings.
  finish() {
    const rec = this.rec
    if (rec.paired.length === 0) return
    const left = pairedLeftover(rec.paired, rec.consoleNoise)
    rec.noise += rec.paired.length - left.length
    for (const p of left) this.add({ kind: 'pageerror', allowed: false, detail: p.text.slice(0, 300) })
    rec.paired = []
  }

  // noteExternal: a third party the crawl blocked, and which record asked for it.
  noteExternal(host) {
    if (!this.externals.has(host)) this.externals.set(host, new Set())
    this.externals.get(host).add(this.rec.label)
  }

  // noteAbort: the guard aborted a call. It is a finding when it happens while the page loads: Studio reads then, so the call
  // is a read the guard does not know (teach it to lib.mjs), not a change. The aborted fetch makes Studio log "Failed to fetch"
  // once, which is the guard's doing and not Studio's.
  noteAbort(req, pathname) {
    this.ours.add(req)
    this.totals.mutationAttempts++
    const entry = { page: this.rec.label, method: req.method(), path: pathname }
    const query = guardNote(req.method(), pathname, req.postData())
    if (query) entry.query = query
    this.mutationAttempts.push(entry)
    this.rec.abortLogs++
    if (this.rec.phase === 'load') {
      this.add({
        kind: 'guard', op: `${entry.method} ${pathname}`, allowed: false,
        detail: `the mutation guard aborted this call while the page loaded, so it is a read the guard does not know (lib.mjs)${query ? `: ${query}` : ''}`,
      })
    }
  }

  add(f) {
    if (!this.rec.findings.some((x) => x.kind === f.kind && x.op === f.op && x.detail === f.detail && x.text === f.text)) this.rec.findings.push(f)
  }

  // attach wires one page's events to the tracker.
  attach(page) {
    const domain = this.cfg.domain
    page.on('dialog', (d) => d.dismiss().catch(() => {})) // a dialog is never confirmed
    page.__lastRequest = Date.now()
    page.on('request', () => {
      page.__lastRequest = Date.now()
    })
    page.on('response', (r) => {
      const req = r.request()
      let u
      try {
        u = new URL(r.url())
      } catch {
        return
      }
      const kind = hostKind(u.host, domain)
      if (kind === 'external') return
      const stub = r.headers()['x-supavise-stub'] === 'true'
      const call = { method: req.method(), host: u.host, path: u.pathname, status: r.status(), stub }
      if (kind === 'api' || kind === 'project' || u.pathname.startsWith('/api/')) {
        this.rec.calls++
        this.totals.calls++
        if (stub) {
          this.rec.stubs++
          this.totals.stubs++
        }
        if (this.callsFile) appendFileSync(this.callsFile, `${JSON.stringify({ page: this.rec.label, ...call })}\n`)
      }
      for (const f of judgeResponse(call, domain, this.allow)) {
        if (f.kind === '4xx') this.rec.n4xx++
        if (f.kind === '5xx') this.rec.n5xx++
        this.add(f)
      }
    })
    page.on('requestfailed', (req) => {
      if (this.ours.has(req)) return
      let u
      try {
        u = new URL(req.url())
      } catch {
        return
      }
      if (hostKind(u.host, domain) === 'external') return
      const text = `${req.failure()?.errorText ?? 'failed'} ${req.method()} ${u.pathname}`
      if (noiseMatch(this.noise.requestfailed, text)) return void this.rec.noise++
      this.add({ kind: 'requestfailed', op: `${req.method()} ${u.pathname}`, allowed: false, detail: req.failure()?.errorText ?? 'failed' })
    })
    // where the page is when a line appears, for the noise entries that name a page
    const here = () => {
      try {
        return { pathname: new URL(page.url()).pathname, blocked: new Set(this.externals.keys()) }
      } catch {
        return { blocked: new Set(this.externals.keys()) }
      }
    }
    page.on('pageerror', (e) => {
      const text = String(e?.message ?? e)
      this.rec.pageErrors++
      const n = noiseMatch(this.noise.pageerror, text, here())
      if (n?.with) return void this.rec.paired.push({ entry: n, text })
      if (n) return void this.rec.noise++
      this.add({ kind: 'pageerror', allowed: false, detail: text.slice(0, 300) })
    })
    page.on('console', (m) => {
      if (m.type() !== 'error') return
      const text = m.text()
      // The browser logs every failed or refused load as "Failed to load resource: ..." as well. The response and
      // requestfailed events judge those (with the allow-list), so the console line only repeats them.
      if (text.startsWith('Failed to load resource:')) return
      const loc = m.location()?.url
      if (loc) {
        try {
          if (hostKind(new URL(loc).host, domain) === 'external') return // a third party the crawl blocks
        } catch {
          /* not a URL */
        }
      }
      if (noiseMatch(this.noise.console, text, here())) {
        this.rec.consoleNoise.push(text)
        return void this.rec.noise++
      }
      // Studio's fetch wrapper logs this for every request that is aborted: one line for each call the guard aborted
      if (/^TypeError: Failed to fetch\b/.test(text) && this.rec.abortLogs > 0) {
        this.rec.abortLogs--
        return void this.rec.noise++
      }
      this.add({ kind: 'console.error', allowed: false, detail: text.slice(0, 300) })
    })
  }
}

// openContext: a new context for one signed-in user. guard=true aborts every call that could change state.
export async function openContext(browser, cfg, tracker, { guard = false } = {}) {
  const domain = cfg.domain
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 900 }, locale: 'en-US', timezoneId: 'UTC', colorScheme: 'light', serviceWorkers: 'block',
  })
  ctx.setDefaultTimeout(30_000)
  await ctx.route('**/*', async (route) => {
    const req = route.request()
    let u
    try {
      u = new URL(req.url())
    } catch {
      return route.abort()
    }
    const kind = hostKind(u.host, domain)
    if (kind === 'external') {
      tracker.noteExternal(u.host)
      tracker.ours.add(req)
      return route.abort('blockedbyclient')
    }
    if (guard && kind === 'api' && /^\/(?:platform|v1|v2)\//.test(u.pathname)) {
      if (guardVerdict(req.method(), u.pathname, req.postData()) === 'abort') {
        tracker.noteAbort(req, u.pathname)
        return route.abort('blockedbyclient')
      }
    }
    if (kind === 'studio' && req.resourceType() === 'document') {
      // route.fetch runs in Node, which does not know the made-up names Chromium maps itself: fetch from the proxy by its
      // address with the Host header of the page, and hand a redirect to the browser as it is.
      const px = cfg.proxy ?? { host: '127.0.0.1', port: 80 }
      const res = await route.fetch({ url: `http://${px.host}:${px.port}${u.pathname}${u.search}`, headers: { ...req.headers(), host: u.host }, maxRedirects: 0 })
      return route.fulfill({ response: res, headers: stripUpgrade(res.headers()) })
    }
    return route.continue()
  })
  await ctx.addInitScript(() => {
    // Toasts leave the DOM after a few seconds; keep their text for the checks.
    window.__toasts = []
    const start = () => {
      const seen = new WeakSet()
      new MutationObserver(() => {
        document.querySelectorAll('[data-sonner-toast]').forEach((n) => {
          if (!seen.has(n)) {
            seen.add(n)
            window.__toasts.push(n.textContent || '')
          }
        })
      }).observe(document.body, { childList: true, subtree: true })
    }
    document.readyState === 'loading' ? document.addEventListener('DOMContentLoaded', start) : start()
  })
  return ctx
}

export async function newPage(ctx, tracker) {
  const page = await ctx.newPage()
  tracker.attach(page)
  return page
}

// signIn fills the e-mail form of /sign-in. The form re-renders once while React hydrates and clears what was typed,
// so it fills until the values stick.
export async function signIn(page, cfg, who = cfg.owner) {
  const studio = baseUrl(cfg, 'studio')
  await page.goto(`${studio}/sign-in`, { waitUntil: 'domcontentloaded' })
  const email = page.getByPlaceholder('you@example.com')
  const password = page.locator('input[type="password"]').first()
  for (let i = 0; i < 10; i++) {
    await email.fill(who.email)
    await password.fill(who.password)
    await page.waitForTimeout(400)
    if ((await email.inputValue()) === who.email && (await password.inputValue()) === who.password) break
  }
  await page.getByRole('button', { name: /^sign in$/i }).first().click()
  await page.waitForURL((u) => !u.pathname.startsWith('/sign-in'), { timeout: 60_000 })
}

// settle waits for the page to go quiet: the load event, then no new request for `quiet` ms (Studio polls the health of
// the project every few seconds and keeps sockets open, so network idle is never reached), with a ceiling of `max` ms.
export async function settle(page, { quiet = 1500, max = 12_000 } = {}) {
  const start = Date.now()
  await page.waitForLoadState('load', { timeout: max }).catch(() => {})
  while (Date.now() - start < max) {
    if (Date.now() - (page.__lastRequest ?? 0) >= quiet) break
    await page.waitForTimeout(200)
  }
}

export const pageText = (page) => page.evaluate(() => document.body?.innerText ?? '')

// waitText waits until the page shows the text; false on timeout.
export async function waitText(page, text, timeout = 20_000) {
  try {
    await page.waitForFunction((t) => (document.body?.innerText ?? '').includes(t), text, { timeout })
    return true
  } catch {
    return false
  }
}

// safeClicks clicks tabs, summaries, menu triggers and links that stay on the page, unless the label is on the deny
// list, and never confirms what opens: a dialog, a menu or a popover is closed with Escape. It lists the other links of
// the page in rec.links, which is where the route pass looks for pages it does not know.
export async function safeClicks(page, rec, budget = 6) {
  const found = await page.evaluate(() => {
    const vis = (el) => {
      const r = el.getBoundingClientRect()
      const s = getComputedStyle(el)
      return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none'
    }
    const label = (el) => (el.getAttribute('aria-label') || el.innerText || el.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 60)
    const out = []
    const pick = (sel, kind) =>
      [...document.querySelectorAll(sel)].forEach((el, i) => {
        if (vis(el)) out.push({ kind, sel, i, label: label(el), href: el.getAttribute('href') || '' })
      })
    pick('[role="tab"]', 'tab')
    pick('summary', 'summary')
    pick('button[aria-haspopup="menu"]', 'menu')
    pick('a[href^="/"]', 'link')
    return out
  })
  const here = new URL(page.url())
  let clicked = 0
  for (const t of found) {
    if (t.kind === 'link') {
      const u = new URL(t.href, here)
      if (u.pathname !== here.pathname) {
        if (!rec.links.includes(u.pathname)) rec.links.push(u.pathname)
        continue
      }
    }
    if (clicked >= budget) break
    if (!safeToClick(t.label)) continue
    const loc = page.locator(t.sel).nth(t.i)
    try {
      if (!(await loc.isVisible())) continue
      await loc.click({ timeout: 3000, noWaitAfter: true })
      clicked++
      await page.waitForTimeout(350)
      if (new URL(page.url()).pathname !== here.pathname) {
        await page.goBack({ waitUntil: 'domcontentloaded' }).catch(() => {})
        await page.waitForTimeout(500)
      }
      if ((await page.locator('[role="dialog"], [role="menu"], [role="listbox"]').count()) > 0) await page.keyboard.press('Escape')
    } catch {
      /* a click that cannot happen is not a finding */
    }
  }
  return clicked
}

export async function shot(page, dir, name) {
  mkdirSync(dir, { recursive: true })
  const file = `${name.replace(/[^A-Za-z0-9._-]+/g, '_').slice(0, 120)}.jpg`
  await page.screenshot({ path: join(dir, file), type: 'jpeg', quality: 55 }).catch(() => {})
  return file
}

export { domFindings }
