// The probe pass of the Studio crawl: user flows that the route pass cannot judge, each run in the real Studio against
// the real daemon and judged by the state of the SERVER afterwards (a project's status, a row, a branch, a config value),
// not by a toast. A probe that cannot run because the build lacks what it tests says so and is skipped (ok: null); every
// other failure fails the run.
//
// A probe works on resources of its own (the probe parent "crawl-probe", branches made for it) or on the fixtures
// whose state it exists to change (the paused, idle and scheduled branches), so no probe depends on another's leftovers.
import { join } from 'node:path'

import { newPage, openContext, pageText, settle, shot, signIn } from './browser.mjs'
import { baseUrl } from './lib.mjs'
import { createBranch, deployFunction, Failure, redact, sleep, waitBranch, waitFor } from './node.mjs'

const ok = (cond, msg) => {
  if (!cond) throw new Failure(msg)
}

class Env {
  constructor({ browser, cfg, fx, node, tracker, outDir }) {
    Object.assign(this, { browser, cfg, fx, node, tracker, outDir })
    this.studio = baseUrl(cfg, 'studio')
    this.contexts = []
    this.last = null
    this.shared = {}
  }

  // page: a signed-in page in a context of its own (no mutation guard: a probe changes things on purpose).
  async page() {
    const ctx = await openContext(this.browser, this.cfg, this.tracker, { guard: false })
    this.contexts.push(ctx)
    const page = await newPage(ctx, this.tracker)
    await signIn(page, this.cfg)
    this.last = page
    return page
  }

  async goto(page, path) {
    await page.goto(`${this.studio}${path}`, { waitUntil: 'domcontentloaded', timeout: 60_000 })
    await settle(page)
  }

  // expectText waits for text (a string or a RegExp) to show on the page. The text is what innerText returns, which is what the
  // page draws: Studio sets headings in capitals with text-transform, so a regexp for a heading takes the i flag.
  async expectText(page, text, timeout = 30_000, what = '') {
    const re = text instanceof RegExp ? text : new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
    const end = Date.now() + timeout
    let t = ''
    for (;;) {
      t = await pageText(page)
      if (re.test(t)) return t
      if (Date.now() > end) throw new Failure(`${what || 'the page'} does not show ${re} (it shows: ${t.replace(/\s+/g, ' ').slice(0, 300)})`)
      await sleep(500)
    }
  }

  async expectNoText(page, text, what = '') {
    const t = await pageText(page)
    const re = text instanceof RegExp ? text : new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
    if (re.test(t)) throw new Failure(`${what || 'the page'} shows ${re}`)
    return t
  }

  // clickRowMenu opens the "More branch actions" menu of the Branches row that holds `name`. The click is Playwright's: a Radix
  // dropdown opens on pointerdown, which element.click() in the page never sends, so the menu stayed closed and its items never showed.
  async clickRowMenu(page, name) {
    const row = page.getByText(name, { exact: true }).first().locator('xpath=ancestor::div[.//button[@aria-label="More branch actions"]][1]')
    await row.getByRole('button', { name: 'More branch actions' }).click({ timeout: 15_000 })
  }

  async branchesOf(parent) {
    return (await this.node.api('GET', `/v1/projects/${parent}/branches`)).json
  }

  async close() {
    for (const c of this.contexts) await c.close().catch(() => {})
    this.contexts = []
  }
}

const dialog = (page) => page.locator('[role="dialog"], [role="alertdialog"]').last()

// ---- the probes -----------------------------------------------------------------------------------------------
export const PROBES = [
  {
    name: 'resume-a-paused-branch',
    skip: (e) => (e.fx.projects.paused ? null : 'no paused branch fixture'),
    async run(e) {
      const page = await e.page()
      const out = []
      for (const key of ['paused', 'idle']) {
        const ref = e.fx.projects[key]
        if (!ref) continue
        ok((await e.node.status(ref)) === 'INACTIVE', `the ${key} branch is not paused before the probe`)
        await e.goto(page, `/project/${ref}`)
        await e.expectText(page, /is paused/, 30_000, `the ${key} branch`)
        await page.getByRole('button', { name: /^Resume (project|branch)$/i }).first().click()
        await dialog(page).getByRole('button', { name: /^Resume$/ }).click()
        await waitFor(`the ${key} branch to run again`, async () => (await e.node.status(ref)) === 'ACTIVE_HEALTHY', { timeout: 300_000, every: 3000 })
        await e.goto(page, `/project/${ref}`)
        await e.expectNoText(page, /is paused/, `the resumed ${key} branch`)
        out.push(key)
      }
      return `resumed: ${out.join(', ')}`
    },
  },
  {
    name: 'pause-and-resume-in-settings',
    async run(e) {
      const b = await createBranch(e.node, e.fx.projects.P, 'probe-pause')
      const page = await e.page()
      await e.goto(page, `/project/${b.ref}/settings/general`)
      await page.getByRole('button', { name: /^Pause (project|branch)$/i }).first().click()
      await dialog(page).getByRole('button', { name: /^Pause (project|branch)$/i }).click()
      await waitFor('the branch to be paused', async () => (await e.node.status(b.ref)) === 'INACTIVE', { timeout: 300_000, every: 3000 })
      await e.goto(page, `/project/${b.ref}`)
      await e.expectText(page, /is paused/, 30_000, 'the paused branch')
      await page.getByRole('button', { name: /^Resume (project|branch)$/i }).first().click()
      await dialog(page).getByRole('button', { name: /^Resume$/ }).click()
      await waitFor('the branch to run again', async () => (await e.node.status(b.ref)) === 'ACTIVE_HEALTHY', { timeout: 300_000, every: 3000 })
    },
  },
  {
    name: 'create-branch-dialog',
    async run(e) {
      const P = e.fx.projects.P
      const page = await e.page()
      await e.goto(page, `/project/${P}/branches`)
      await page.getByRole('button', { name: /^Create branch$/ }).first().click()
      const d = dialog(page)
      await d.waitFor()
      const text = await d.innerText()
      ok(/Include data/.test(text), `the Create branch dialog has no "Include data" switch: ${text.slice(0, 200)}`)
      ok(!/\$\s*\d|billed at|per hour|per month|Requires PITR|tax/i.test(text), `the Create branch dialog shows hosted billing copy: ${text.replace(/\s+/g, ' ').slice(0, 300)}`)
      ok(await d.getByRole('switch', { name: 'Include data' }).isEnabled(), 'the "Include data" switch is disabled')
      await d.getByPlaceholder('e.g. staging, dev-feature-x').fill('probe-ui')
      await d.getByRole('button', { name: /^Create branch$/ }).click()
      const b = await waitFor('the branch the dialog made to be listed', async () => (await e.branchesOf(P)).find((x) => x.name === 'probe-ui'), { timeout: 60_000, every: 2000 })
      e.shared.ui = { id: b.id, ref: b.project_ref, name: 'probe-ui' }
      // the dialog cannot say persistent; the node idle-pauses the others (awake(), node.mjs), so this one is made persistent too
      if (e.cfg.idle_pause) await e.node.api('PATCH', `/v1/branches/${b.id}`, { persistent: true })
      await waitBranch(e.node, { ...e.shared.ui, parent: P })
    },
  },
  {
    name: 'branch-home-is-not-coming-up',
    skip: (e) => (e.shared.ui ? null : 'the create-branch-dialog probe made no branch'),
    async run(e) {
      const page = await e.page()
      await e.goto(page, `/project/${e.shared.ui.ref}`)
      // a branch whose services are healthy says Healthy, not "Coming up...", even in its first minutes
      await waitFor('the project home to stop saying "Coming up..."', async () => !/Coming up\.\.\./.test(await pageText(page)), { timeout: 120_000, every: 3000 })
    },
  },
  {
    name: 'view-logs-lists-runs',
    skip: (e) => (e.shared.ui ? null : 'the create-branch-dialog probe made no branch'),
    async run(e) {
      const page = await e.page()
      await e.goto(page, `/project/${e.fx.projects.P}/branches`)
      await e.expectText(page, e.shared.ui.name, 30_000, 'the Branches page')
      const row = page.locator('div').filter({ hasText: e.shared.ui.name }).filter({ has: page.getByRole('button', { name: 'View Logs' }) }).last()
      await row.getByRole('button', { name: 'View Logs' }).first().click()
      const d = dialog(page)
      await e.expectText(page, new RegExp(`Workflow logs for ${e.shared.ui.name}`), 15_000, 'the logs dialog')
      await waitFor('a workflow run in the list', async () => (await d.locator('li button').count()) > 0, { timeout: 30_000, every: 1000 })
      await d.locator('li button').first().click()
      const logs = await waitFor('the log of the run', async () => ((await d.locator('pre').first().innerText().catch(() => '')) || '').trim(), { timeout: 30_000, every: 1000 })
      ok(logs.length > 0, 'the run has an empty log')
      return `${logs.split('\n').length} log lines`
    },
  },
  {
    name: 'merge-completes-and-closes',
    async run(e) {
      const P = e.fx.projects.P
      const b = await createBranch(e.node, P, 'probe-merge')
      // a change made the way the Table Editor makes it: through pg-meta, no migration file
      await e.node.api('POST', `/platform/pg-meta/${b.ref}/query`, { query: 'create table public.probe_merge_table (id bigint generated always as identity primary key, note text)' })
      await e.node.api('PATCH', `/v1/branches/${b.id}`, { request_review: true })
      const page = await e.page()
      await e.goto(page, `/project/${b.ref}/merge`)
      const merge = page.getByRole('button', { name: /^Merge branch$/ }).first()
      await waitFor('the Merge branch button to be enabled', async () => merge.isEnabled(), { timeout: 120_000, every: 2000 })
      await merge.click()
      await dialog(page).getByRole('button', { name: /^Merge branch$/ }).click()
      await e.expectText(page, /Workflow completed successfully/i, 300_000, 'the merge page')
      await e.expectText(page, /Close branch/, 30_000, 'the merge page after the merge')
      // the merge reached the parent
      const there = await e.node.sql(P, `select to_regclass('public.probe_merge_table') is not null as present`)
      ok(there?.[0]?.present === true, 'the table of the branch is not in the parent after the merge')
      // the diff refreshes and shows nothing left
      await e.expectText(page, /No changes detected between branches/, 60_000, 'the Database tab after the merge')
      // equal functions are not changes. The tabs are the merge page's own links (the project's sidebar has an Edge Functions and a
      // Database link too, which lead away from the merge page), and the URL says which tab the page is on.
      const tab = (name) => page.locator(`a[href*="/merge?tab=${name}"]`).first()
      await tab('edge-functions').click()
      await page.waitForURL(/[?&]tab=edge-functions/, { timeout: 30_000 })
      await e.expectText(page, /No changes detected between branches/, 60_000, 'the Edge Functions tab')
      await tab('database').click()
      await page.waitForURL(/[?&]tab=database/, { timeout: 30_000 })
      // Close branch removes the branch
      await e.expectText(page, /Close branch/, 30_000, 'the merge page')
      await page.getByRole('button', { name: /^Close branch$/ }).first().click()
      await waitFor('the branch to be gone', async () => !(await e.branchesOf(P)).some((x) => x.name === 'probe-merge' && !x.deletion_scheduled_at), { timeout: 120_000, every: 3000 })
    },
  },
  {
    name: 'update-branch-after-a-parent-function-deploy',
    async run(e) {
      const P = e.fx.projects.P
      const b = await createBranch(e.node, P, 'probe-update')
      await deployFunction(e.node, P, 'later', 'later')
      const page = await e.page()
      await e.goto(page, `/project/${b.ref}/merge`)
      await e.expectText(page, /out of date/i, 120_000, 'the merge page of a branch behind its parent')
      await page.getByRole('button', { name: /^Update branch$/ }).first().click()
      const confirm = page.getByRole('button', { name: /^Update anyway$/ })
      if (await confirm.isVisible().catch(() => false)) await confirm.click()
      await waitFor('the out-of-date notice to clear', async () => {
        await e.goto(page, `/project/${b.ref}/merge`)
        return !/out of date/i.test(await pageText(page))
      }, { timeout: 240_000, every: 5000 })
      const fns = (await e.node.api('GET', `/v1/projects/${b.ref}/functions`)).json
      ok(fns.some((f) => f.slug === 'later'), 'the branch has no copy of the parent function after Update branch')
    },
  },
  {
    name: 'close-a-merge-request',
    async run(e) {
      const P = e.fx.projects.P
      const b = await createBranch(e.node, P, 'probe-mr')
      await e.node.api('PATCH', `/v1/branches/${b.id}`, { request_review: true })
      const page = await e.page()
      await e.goto(page, `/project/${P}/branches/merge-requests`)
      await e.expectText(page, 'probe-mr', 30_000, 'the merge requests page')
      await e.goto(page, `/project/${b.ref}/merge`)
      await page.locator('button:has(svg.lucide-more-vertical), button:has(svg.lucide-ellipsis-vertical)').first().click()
      await page.getByRole('menuitem', { name: 'Close this merge request' }).click()
      await waitFor('the merge request to be closed', async () => !(await e.branchesOf(P)).find((x) => x.name === 'probe-mr')?.review_requested_at, { timeout: 60_000, every: 2000 })
      await e.goto(page, `/project/${P}/branches/merge-requests`)
      await e.expectNoText(page, 'probe-mr', 'the merge requests page after closing')
    },
  },
  {
    name: 'restore-a-branch-scheduled-for-deletion',
    skip: (e) => (e.fx.projects.scheduled ? null : 'no scheduled-deletion fixture'),
    async run(e) {
      const A = e.fx.projects.A
      const b = e.fx.branches['crawl-scheduled']
      const page = await e.page()
      await e.goto(page, `/project/${A}/branches`)
      await e.expectText(page, /Scheduled for deletion branches/i, 30_000, 'the Branches page')
      await e.expectText(page, b.name, 30_000, 'the scheduled branch')
      await waitFor('the scheduled branch to be paused (Restore is enabled for a paused one)', async () => (await e.node.status(b.ref)) === 'INACTIVE', { timeout: 120_000, every: 3000 })
      await e.goto(page, `/project/${A}/branches`)
      await e.clickRowMenu(page, b.name)
      await page.getByRole('menuitem', { name: /Restore branch/ }).click()
      await waitFor('the branch to be restored', async () => {
        const cur = (await e.branchesOf(A)).find((x) => x.name === b.name)
        return cur && !cur.deletion_scheduled_at && (await e.node.status(b.ref)) === 'ACTIVE_HEALTHY'
      }, { timeout: 300_000, every: 3000 })
    },
  },
  {
    name: 'custom-domains-on-projects-and-branches',
    async run(e) {
      const page = await e.page()
      for (const key of ['A', 'B']) {
        const ref = e.fx.projects[key]
        await e.goto(page, `/project/${ref}/settings/general`)
        await e.expectText(page, /Custom domains/, 30_000, `Settings > General of ${key}`)
        await e.expectNoText(page, /Pro Plan add-on|Failed to retrieve custom domain/, `the Custom domains section of ${key}`)
        if (e.fx.hostnames[key] && !e.fx.unavailable?.hostnames) await e.expectText(page, e.fx.hostnames[key], 30_000, `the Custom domains section of ${key}`)
      }
      // the CNAME pre-check of a new hostname passes, and the claim reaches the server
      const P = e.fx.projects.P
      await e.goto(page, `/project/${P}/settings/general`)
      await e.expectText(page, /Custom domains/, 30_000, 'Settings > General of the probe parent')
      await page.getByPlaceholder('subdomain.example.com').fill(e.fx.hostnames.P)
      await page.getByRole('button', { name: /^Add$/ }).click()
      const st = await waitFor('the hostname to be claimed', async () => {
        const r = await e.node.api('GET', `/v1/projects/${P}/custom-hostname`, undefined, { expect: [400, 404] }) // 400 while none is claimed
        return r.status === 200 && r.json?.custom_hostname === e.fx.hostnames.P ? r.json.status : null
      }, { timeout: 60_000, every: 2000 })
      await e.expectNoText(page, /cannot be found/, 'the Custom domains section after the CNAME check')
      return `claimed ${e.fx.hostnames.P}: ${st}`
    },
  },
  {
    name: 'github-surfaces',
    async run(e) {
      const A = e.fx.projects.A
      const page = await e.page()
      const configured = !!e.cfg.github?.app
      await e.goto(page, `/project/${A}/settings/integrations`)
      const card = /Preview branches and production deploys from a connected GitHub repository/
      const t = await pageText(page)
      if (!configured) {
        ok(!card.test(t), 'the GitHub integration card is shown although no GitHub App is configured')
        await e.goto(page, '/account/me')
        await e.expectNoText(page, /Connect GitHub|GitHub/i, 'Account > Preferences without a GitHub App')
        return 'hidden without an App'
      }
      ok(card.test(t), 'the GitHub integration card is hidden although a GitHub App is configured')
      // The connect flow opens GitHub's authorize page in a popup: with the fake of the github lane that is the fake's URL.
      if (e.cfg.github.authorize_url) {
        const ctx = page.context()
        const [popup] = await Promise.all([
          ctx.waitForEvent('page', { timeout: 20_000 }),
          page.getByRole('button', { name: /Authorize|Connect GitHub|Add GitHub/i }).first().click(),
        ])
        await popup.waitForLoadState('domcontentloaded', { timeout: 20_000 }).catch(() => {})
        ok(popup.url().startsWith(e.cfg.github.authorize_url), `the GitHub popup opened ${popup.url()}, want ${e.cfg.github.authorize_url}`)
        return 'shown with an App, and the connect flow reaches the fake'
      }
      return 'shown with an App'
    },
  },
  {
    name: 'review-with-the-assistant-says-the-truth',
    skip: (e) => (e.fx.projects.pending ? null : 'no pending merge request fixture'),
    async run(e) {
      // Open question of the plan: what Review with Assistant does on a node without a provider key. Recorded, and held to
      // the one rule that is not open: the page never shows the name of a server variable.
      const page = await e.page()
      await e.goto(page, `/project/${e.fx.projects.pending}/merge`)
      const btn = page.getByRole('button', { name: /Review with Assistant/ }).first()
      if (!(await btn.isVisible().catch(() => false))) return 'the button is not shown'
      if (!(await btn.isEnabled().catch(() => false))) return 'the button is shown and disabled'
      await btn.click()
      await page.waitForTimeout(6000)
      const t = await pageText(page)
      ok(!/OPENAI_API_KEY/.test(t), 'the page names OPENAI_API_KEY to the user')
      const said = t.split('\n').filter((l) => /assistant|api key|not available|unavailable|provider/i.test(l)).slice(0, 3).join(' | ')
      return `clicked; the page says: ${said.slice(0, 200) || '(nothing about the assistant)'}`
    },
  },
  {
    name: 'template-reset-keeps-the-site-url',
    async run(e) {
      const P = e.fx.projects.P
      const site = 'http://crawl-site.example.test'
      const key = 'MAILER_TEMPLATES_CONFIRMATION_CONTENT'
      // Studio offers Reset template only for a template that the config reports as custom (hasCustomTemplate in TemplateEditor.tsx),
      // that is for one with a saved subject or body: a project that never edited the template has nothing to reset.
      await e.node.api('PATCH', `/platform/auth/${P}/config`, {
        SITE_URL: site,
        MAILER_SUBJECTS_CONFIRMATION: 'Confirm your crawl account',
        [key]: '<h2>Confirm</h2><p><a href="{{ .ConfirmationURL }}">Confirm your address</a></p>',
      })
      const before = (await e.node.api('GET', `/platform/auth/${P}/config`)).json
      ok(before.MAILER_TEMPLATES_CUSTOM_CONTENTS?.[key] === true, 'the config does not report the saved confirmation template as custom')
      const page = await e.page()
      await e.goto(page, `/project/${P}/auth/templates/confirm-sign-up`)
      await page.getByRole('button', { name: /^Reset template$/ }).first().click()
      await dialog(page).getByRole('button', { name: /^Reset$/ }).click()
      await e.expectText(page, /Email template reset to default/, 30_000, 'the template page')
      const cfg = (await e.node.api('GET', `/platform/auth/${P}/config`)).json
      ok(cfg.SITE_URL === site, `the template reset changed SITE_URL from ${site} to ${cfg.SITE_URL}`)
      ok(cfg.MAILER_SUBJECTS_CONFIRMATION === '' && cfg[key] === '', 'the template reset left the subject or the body of the confirmation template')
      ok(cfg.MAILER_TEMPLATES_CUSTOM_CONTENTS?.[key] === false, 'the config still reports the reset template as custom')
      // the answer of the reset is the config Studio now shows, so there is nothing left to reset
      await waitFor('the Reset template button to go', async () => (await page.getByRole('button', { name: /^Reset template$/ }).count()) === 0, { timeout: 15_000, every: 500 })
    },
  },
  {
    name: 'remove-mfa-factors',
    async run(e) {
      const A = e.fx.projects.A
      const count = async () => Number((await e.node.sql(A, `select count(*)::int as n from auth.mfa_factors where user_id = '${e.fx.user.id}'`))[0].n)
      ok((await count()) > 0, 'the fixture user has no MFA factor before the probe')
      const page = await e.page()
      await e.goto(page, `/project/${A}/auth/users`)
      await page.getByText(e.fx.user.email, { exact: true }).first().click()
      await page.getByRole('button', { name: /^Remove MFA factors$/ }).first().click()
      await dialog(page).getByRole('button', { name: /^Remove factors$/ }).click()
      await waitFor('the factors to be gone', async () => (await count()) === 0, { timeout: 60_000, every: 2000 })
    },
  },
  {
    name: 'data-api-docs-list-the-table',
    async run(e) {
      const page = await e.page()
      await e.goto(page, `/project/${e.fx.projects.A}/api`)
      await e.expectText(page, 'crawl_items', 60_000, 'the Data API docs')
    },
  },
  {
    name: 'graphiql-introspection',
    async run(e) {
      const A = e.fx.projects.A
      const page = await e.page()
      const seen = page.waitForResponse((r) => r.url().includes(`/projects/${A}/api/graphql`) && r.request().method() === 'POST', { timeout: 60_000 })
      await e.goto(page, `/project/${A}/integrations/graphiql/graphiql`)
      const res = await seen
      ok(res.status() === 200, `the GraphiQL introspection answered ${res.status()}`)
      const body = await res.json().catch(() => null)
      ok(body && (body.data?.__schema || body.data?.__type || body.data), `the GraphiQL introspection has no schema: ${JSON.stringify(body)?.slice(0, 200)}`)
    },
  },
  {
    name: 'auth-toggle-survives-a-reload',
    async run(e) {
      const P = e.fx.projects.P
      const read = async () => (await e.node.api('GET', `/platform/auth/${P}/config`)).json.EXTERNAL_ANONYMOUS_USERS_ENABLED
      const before = await read()
      const page = await e.page()
      await e.goto(page, `/project/${P}/auth/providers`)
      const sw = () => page.getByText('Allow anonymous sign-ins', { exact: true }).locator('xpath=ancestor::div[.//*[@role="switch"]][1]').getByRole('switch').first()
      await sw().waitFor()
      await sw().click()
      await page.getByRole('button', { name: /^Save changes$/ }).first().click()
      await waitFor('the setting to be saved', async () => (await read()) === !before, { timeout: 60_000, every: 2000 })
      await page.reload({ waitUntil: 'domcontentloaded' })
      await settle(page)
      await sw().waitFor()
      ok(((await sw().getAttribute('aria-checked')) === 'true') === !before, 'the toggle shows its old value after a reload')
      ok((await read()) === !before, 'the server lost the setting')
    },
  },
  {
    name: 'edge-function-test-tab',
    skip: (e) => (e.cfg.functions === false ? 'Edge Functions are off on this node' : null),
    async run(e) {
      const A = e.fx.projects.A
      const page = await e.page()
      await e.goto(page, `/project/${A}/functions/hello`)
      await page.getByRole('button', { name: /^Test$/ }).first().click()
      await page.getByRole('button', { name: /^Send Request$/ }).click()
      const sheet = dialog(page)
      await waitFor('a response in the tester', async () => /\b200\b/.test(await sheet.innerText()), { timeout: 60_000, every: 1000 })
    },
  },
  {
    name: 'image-transformation-outcome',
    async run(e) {
      const A = e.fx.projects.A
      const { secret } = await e.node.keys(A)
      const conf = (await e.node.api('GET', `/platform/storage/${A}/config`)).json
      const enabled = conf?.features?.imageTransformation?.enabled === true
      const r = await e.node.project(A, 'GET', '/storage/v1/render/image/authenticated/crawl-bucket/crawl.png?width=1&height=1', secret)
      if (enabled) ok(r.status === 200 && /^image\//.test(String(r.headers['content-type'])), `image transformation is on and a render request answers ${r.status} ${r.headers['content-type']}`)
      else ok(r.status >= 400 && r.status < 500, `image transformation is off and a render request answers ${r.status}, want a 4xx`)
      const page = await e.page()
      await e.goto(page, `/project/${A}/storage/files/settings`)
      await e.expectText(page, /image transformation/i, 30_000, 'the Storage settings page')
      return `transformation ${enabled ? 'on' : 'off'}, render answers ${r.status}`
    },
  },
  {
    name: 'organization-audit-logs',
    async run(e) {
      const page = await e.page()
      await e.goto(page, `/org/${e.cfg.org}/audit`)
      await e.expectText(page, /Audit Logs/i, 30_000, 'the audit logs page')
      await e.expectNoText(page, /does not have any audit logs available yet/, 'the audit logs page')
      // Studio draws the actor as the member's username, and the email only for an actor that is no member (AuditLogs.columns.tsx);
      // a node's username is the part of the email before the @ unless the user set one
      const actor = e.cfg.owner.email.split('@')[0].replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
      await e.expectText(page, new RegExp(`(?:^|\\s)${actor}(?:\\s|$)`, 'm'), 30_000, 'the audit logs page (the actor of the seeding)')
    },
  },
  {
    name: 'hosted-only-features-are-hidden',
    async run(e) {
      const page = await e.page()
      await e.goto(page, `/project/${e.fx.projects.A}/settings/general`)
      await e.expectText(page, /Project Settings/, 30_000, 'Settings > General')
      await e.expectNoText(page, /Transfer project/, 'Settings > General')
      await e.goto(page, `/org/${e.cfg.org}/general`)
      await e.expectNoText(page, /Legal Documents/, 'the organization menu')
      await e.goto(page, '/account/me')
      await e.expectText(page, /Preferences|Account/i, 30_000, 'the account page')
      await e.expectNoText(page, /Danger zone/, 'the account page')
    },
  },
  {
    name: 'sso-page-loads',
    async run(e) {
      const page = await e.page()
      await e.goto(page, `/org/${e.cfg.org}/sso`)
      const t = await e.expectText(page, /Single Sign-On/, 30_000, 'the SSO page')
      ok(!/Failed to|Error fetching|Something went wrong/.test(t), `the SSO page shows an error: ${t.replace(/\s+/g, ' ').slice(0, 300)}`)
    },
  },
  {
    name: 'restore-to-a-new-project',
    async run(e) {
      const A = e.fx.projects.A
      const clone = await e.node.api('GET', `/platform/database/${A}/clone`)
      const backups = clone.json?.backups ?? []
      ok(backups.length > 0, `project A has no backup to restore from (GET /platform/database/{ref}/clone: ${JSON.stringify(clone.json)?.slice(0, 200)})`)
      const page = await e.page()
      await e.goto(page, `/project/${A}/database/backups/restore-to-new-project`)
      // Studio's flow: Continue under the restore time (the page lists physical backups with a Restore button each when the
      // project has no point-in-time recovery), Continue in the confirmation, then the dialog that names the new project
      await page.getByRole('button', { name: /^(?:Continue|Restore)$/ }).first().click()
      await dialog(page).getByRole('button', { name: /^Continue$/ }).click()
      const d = dialog(page)
      await d.getByPlaceholder('Enter a name').fill('crawl-restored')
      await d.getByPlaceholder('Type in a strong password').fill(`Crawl-${Date.now()}-pw`)
      await d.getByRole('button', { name: /^Restore to new project$/ }).click()
      const proj = await waitFor('the restored project to be listed', async () => {
        const r = await e.node.api('GET', `/platform/organizations/${e.cfg.org}/projects?limit=96`)
        return r.json.projects.find((p) => p.name === 'crawl-restored')
      }, { timeout: 120_000, every: 3000 })
      await waitFor('the restored project to be healthy', async () => (await e.node.status(proj.ref)) === 'ACTIVE_HEALTHY', { timeout: 900_000, every: 5000 })
      const rows = await e.node.sql(proj.ref, 'select count(*)::int as n from public.crawl_items')
      ok(rows[0].n > 0, 'the restored project has no rows of the original table')
    },
  },
]

export async function runProbes({ browser, cfg, fx, node, tracker, log, outDir }) {
  const env = new Env({ browser, cfg, fx, node, tracker, outDir })
  const results = []
  for (const p of PROBES) {
    const t0 = Date.now()
    const r = { name: p.name, ok: false, ms: 0, detail: '', findings: [] }
    const rec = tracker.begin(`probe ${p.name}`)
    try {
      const skip = p.skip?.(env)
      if (skip) {
        r.ok = null
        r.detail = `skipped: ${skip}`
      } else {
        r.detail = (await p.run(env)) ?? ''
        r.ok = true
      }
    } catch (e) {
      r.ok = false
      r.detail = redact(String(e?.message ?? e)).replace(/\s+/g, ' ').slice(0, 600)
      if (env.last) r.screenshot = await shot(env.last, join(outDir, 'screenshots'), `probe-${p.name}`)
    } finally {
      await env.close()
    }
    tracker.finish()
    r.findings = rec.findings
    r.ms = Date.now() - t0
    results.push(r)
    log(`probe ${r.ok === true ? 'ok     ' : r.ok === null ? 'skipped' : 'FAILED '} ${p.name} (${Math.round(r.ms / 1000)} s)${r.ok === false ? `: ${r.detail}` : ''}`)
  }
  return results
}

