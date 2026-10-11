// Branches through the Management API, the endpoints the Supabase CLI (`supabase branches`) and
// Studio call: create a schema-only branch of project A, list it, read it, see that it carries the
// parent's migrations and none of its data, and delete it. A second suite reads what Studio, the CLI
// and an agent read of a branch that was created: its action run with the seven steps of hosted
// (and the run under the other ref answering 404), the parent's Edge Function copied and called with
// supabase-js at the branch's own URL, the review request set and cleared, and persistence and the
// notify URL.
import assert from 'node:assert/strict'
import { after, before, describe, test } from 'node:test'
import { client, mgmt, projects, proxyFetch, sql, suffix, waitFor } from './lib.mjs'

const p = projects.a
const table = `conf_branch_probe_${suffix}`
const name = `conf-branch-${suffix}`

describe('branches', () => {
  let branch

  before(async () => {
    // A migration the branch replays, and a row it must not get: a schema-only branch has the
    // parent's schema and none of its data.
    const m = await mgmt('POST', `/v1/projects/${p.ref}/database/migrations`, {
      name: `conf_branch_${suffix}`,
      query: `create table public.${table} (id int primary key, note text)`,
    })
    assert.ok(m.status < 300, `apply migration: ${m.status} ${JSON.stringify(m.body)}`)
    await sql(p, `insert into public.${table} values (1, 'parent data')`)
  })

  after(async () => {
    if (branch) await mgmt('DELETE', `/v1/branches/${branch.id}`)
    await mgmt('POST', `/v1/projects/${p.ref}/database/query`, { query: `drop table if exists public.${table}` })
    await sql(p, `delete from supabase_migrations.schema_migrations where name = 'conf_branch_${suffix}'`).catch(() => {})
  })

  test('a schema-only branch is created and becomes ready', async () => {
    const r = await mgmt('POST', `/v1/projects/${p.ref}/branches`, { branch_name: name })
    assert.equal(r.status, 201, JSON.stringify(r.body))
    branch = r.body
    assert.equal(branch.name, name)
    assert.equal(branch.parent_project_ref, p.ref)
    assert.equal(branch.with_data, false)
    assert.equal(branch.is_default, false)
    assert.match(branch.project_ref, /^[a-z]{20}$/)
    // The branch's status (BranchResponse) is read from the project's branch list; the detail of a branch
    // (GET /v1/branches/{id}) reports the branch project's own status instead.
    branch = await waitFor('the branch to finish its migrations', async () => {
      const g = await mgmt('GET', `/v1/projects/${p.ref}/branches/${name}`)
      assert.equal(g.status, 200, JSON.stringify(g.body))
      assert.ok(!/FAILED$/.test(g.body.status), JSON.stringify(g.body))
      // MIGRATIONS_PASSED ends a create that deploys no functions; a build with action runs goes on to FUNCTIONS_DEPLOYED
      return g.body.status === 'MIGRATIONS_PASSED' || g.body.status === 'FUNCTIONS_DEPLOYED' ? g.body : null
    }, { timeout: 200_000, interval: 3000 })
    const detail = await waitFor('the branch project to be ACTIVE_HEALTHY', async () => {
      const g = await mgmt('GET', `/v1/branches/${branch.id}`)
      assert.equal(g.status, 200, JSON.stringify(g.body))
      return g.body.status === 'ACTIVE_HEALTHY' ? g : null
    }, { timeout: 60_000, interval: 2000 })
    assert.equal(detail.body.ref, branch.project_ref)
    assert.ok(detail.body.db_host && detail.body.db_port, 'the detail names the database host and port')
  })

  test('the branch is listed next to the default branch, and found by name', async () => {
    const list = await mgmt('GET', `/v1/projects/${p.ref}/branches`)
    assert.equal(list.status, 200)
    const found = list.body.find((b) => b.id === branch.id)
    assert.ok(found, `the branch is not in the list: ${JSON.stringify(list.body.map((b) => b.name))}`)
    assert.equal(found.project_ref, branch.project_ref)
    assert.equal(list.body.filter((b) => b.is_default).length, 1, 'exactly one default branch')
    const byName = await mgmt('GET', `/v1/projects/${p.ref}/branches/${name}`)
    assert.equal(byName.status, 200)
    assert.equal(byName.body.id, branch.id)
  })

  test('the branch has the parent schema and none of its data', async () => {
    const b = { ref: branch.project_ref }
    const t = await sql(b, `select to_regclass('public.${table}') is not null as present`)
    assert.equal(t[0].present, true, 'the parent migration was not replayed in the branch')
    const n = await sql(b, `select count(*)::int as n from public.${table}`)
    assert.equal(n[0].n, 0, 'a schema-only branch has data of its parent')
    assert.equal((await sql(p, `select count(*)::int as n from public.${table}`))[0].n, 1, 'the parent lost its data')
  })

  test('deleting the branch removes it', async () => {
    const d = await mgmt('DELETE', `/v1/branches/${branch.id}`)
    assert.equal(d.status, 200, JSON.stringify(d.body))
    const gone = await waitFor('the branch to be gone', async () => {
      const list = await mgmt('GET', `/v1/projects/${p.ref}/branches`)
      return list.status === 200 && !list.body.some((b) => b.id === branch.id)
    }, { timeout: 120_000, interval: 2000 })
    assert.ok(gone)
    assert.equal((await mgmt('GET', `/v1/branches/${branch.id}`)).status, 404)
    assert.equal((await mgmt('GET', `/v1/projects/${p.ref}/branches/${name}`)).status, 404)
    branch = undefined
  })
})

// ---- what Studio, the CLI and an agent read of a created branch ---------------------------------------------------
const SEVEN = ['clone', 'pull', 'health', 'configure', 'migrate', 'seed', 'deploy']
const STEP_STATUSES = ['CREATED', 'DEAD', 'EXITED', 'PAUSED', 'REMOVING', 'RESTARTING', 'RUNNING']

describe('a created branch: runs, functions, review and its own URL', () => {
  const fn = `conf-bfn-${suffix}`
  const bname = `conf-branch-b-${suffix}`
  let b
  let branchUrl

  before(async () => {
    // a function of the parent that the branch must start with
    const form = new FormData()
    form.append('metadata', new Blob([JSON.stringify({ entrypoint_path: 'index.ts', name: fn, verify_jwt: false })], { type: 'application/json' }))
    form.append('file', new Blob([`Deno.serve(() => new Response(JSON.stringify({ from: 'parent', fn: '${fn}' }), { headers: { 'Content-Type': 'application/json' } }))`], { type: 'application/typescript' }), 'index.ts')
    const d = await mgmt('POST', `/v1/projects/${p.ref}/functions/deploy?slug=${fn}`, form)
    assert.ok(d.status === 200 || d.status === 201, `deploy ${fn}: ${d.status} ${JSON.stringify(d.body)}`)
    const r = await mgmt('POST', `/v1/projects/${p.ref}/branches`, { branch_name: bname })
    assert.equal(r.status, 201, JSON.stringify(r.body))
    b = r.body
    branchUrl = p.url.replace(p.ref, b.project_ref)
    await waitFor('the branch to finish', async () => {
      const g = await mgmt('GET', `/v1/projects/${p.ref}/branches/${bname}`)
      assert.ok(!/FAILED$/.test(g.body.status), JSON.stringify(g.body))
      return g.body.status === 'MIGRATIONS_PASSED' || g.body.status === 'FUNCTIONS_DEPLOYED'
    }, { timeout: 200_000, interval: 3000 })
    await waitFor('the branch project to be ACTIVE_HEALTHY', async () => (await mgmt('GET', `/v1/branches/${b.id}`)).body?.status === 'ACTIVE_HEALTHY', { timeout: 90_000, interval: 2000 })
  })

  after(async () => {
    if (b) await mgmt('DELETE', `/v1/branches/${b.id}`)
    await mgmt('DELETE', `/v1/projects/${p.ref}/functions/${fn}`)
  })

  test('the create is an action run of the branch, with the seven steps of hosted', async () => {
    const list = await mgmt('GET', `/v1/projects/${b.project_ref}/actions`)
    assert.equal(list.status, 200, JSON.stringify(list.body))
    assert.ok(Array.isArray(list.body) && list.body.length >= 1, `no action run under the branch ref: ${JSON.stringify(list.body)}`)
    const run = list.body[0]
    assert.equal(run.branch_id, b.id)
    assert.deepEqual(run.run_steps.map((s) => s.name), SEVEN, 'a run is created with all its steps (a run with none reads as RUNNING for ever)')
    for (const s of run.run_steps) {
      assert.ok(STEP_STATUSES.includes(s.status), `step ${s.name} has status ${s.status}`)
      assert.notEqual(s.status, 'RUNNING', `step ${s.name} is still running after the branch finished`)
      assert.notEqual(s.status, 'DEAD', `step ${s.name} failed: see the log`)
    }
    const one = await mgmt('GET', `/v1/projects/${b.project_ref}/actions/${run.id}`)
    assert.equal(one.status, 200)
    assert.equal(one.body.id, run.id)
  })

  test('the run answers under its own ref and 404 under the other, as Studio polls both', async () => {
    const run = (await mgmt('GET', `/v1/projects/${b.project_ref}/actions`)).body[0]
    const other = await mgmt('GET', `/v1/projects/${p.ref}/actions/${run.id}`)
    assert.equal(other.status, 404, `a create run is owned by the branch: ${JSON.stringify(other.body)}`)
    const logs = await mgmt('GET', `/v1/projects/${b.project_ref}/actions/${run.id}/logs`)
    assert.equal(logs.status, 200)
    assert.ok(logs.body === null || typeof logs.body === 'string', 'the log of a run is text/plain')
  })

  test('the parent function is copied, and supabase-js calls it at the branch URL', async () => {
    const fns = await mgmt('GET', `/v1/projects/${b.project_ref}/functions`)
    assert.equal(fns.status, 200, JSON.stringify(fns.body))
    const copied = fns.body.find((f) => f.slug === fn)
    assert.ok(copied, `the branch has no copy of ${fn}: ${JSON.stringify(fns.body.map((f) => f.slug))}`)
    const parent = (await mgmt('GET', `/v1/projects/${p.ref}/functions`)).body.find((f) => f.slug === fn)
    assert.equal(copied.verify_jwt, parent.verify_jwt)
    assert.equal(copied.entrypoint_path ?? parent.entrypoint_path, parent.entrypoint_path)
    // the branch has keys of its own, and the node serves it at its own host
    const k = await mgmt('GET', `/v1/projects/${b.project_ref}/api-keys?reveal=true`)
    assert.equal(k.status, 200)
    const anon = k.body.find((x) => x.name === 'anon')?.api_key
    assert.ok(anon, 'the branch has no anon key')
    const parentAnon = (await mgmt('GET', `/v1/projects/${p.ref}/api-keys?reveal=true`)).body.find((x) => x.name === 'anon')?.api_key
    assert.notEqual(anon, parentAnon, 'a branch has its own keys')
    const c = client({ url: branchUrl }, anon, { global: { fetch: proxyFetch } })
    const res = await waitFor('the copied function to answer', async () => {
      const r = await c.functions.invoke(fn)
      return r.error ? null : r.data
    }, { timeout: 60_000, interval: 2000 })
    assert.deepEqual(res, { from: 'parent', fn })
    const rest = await c.from('nothing_here').select('*').limit(1)
    assert.ok(rest.error, 'REST at the branch URL answers (an error for a table that does not exist)')
    assert.ok(/relation|schema cache|Could not find/.test(rest.error.message), rest.error.message)
  })

  test('a review request is set and cleared, and persistence and the notify URL are kept', async () => {
    const on = await mgmt('PATCH', `/v1/branches/${b.id}`, { request_review: true })
    assert.equal(on.status, 200, JSON.stringify(on.body))
    let got = (await mgmt('GET', `/v1/projects/${p.ref}/branches/${bname}`)).body
    assert.ok(got.review_requested_at, `request_review true set no review_requested_at: ${JSON.stringify(got)}`)
    const off = await mgmt('PATCH', `/v1/branches/${b.id}`, { request_review: false })
    assert.equal(off.status, 200, JSON.stringify(off.body))
    got = (await mgmt('GET', `/v1/projects/${p.ref}/branches/${bname}`)).body
    assert.ok(!got.review_requested_at, `request_review false left review_requested_at: ${JSON.stringify(got)}`)
    const url = `https://example.com/supavise-conformance-${suffix}`
    const up = await mgmt('PATCH', `/v1/branches/${b.id}`, { persistent: true, notify_url: url })
    assert.equal(up.status, 200, JSON.stringify(up.body))
    got = (await mgmt('GET', `/v1/projects/${p.ref}/branches/${bname}`)).body
    assert.equal(got.persistent, true)
    assert.equal(got.notify_url, url)
    const back = await mgmt('PATCH', `/v1/branches/${b.id}`, { persistent: false })
    assert.equal(back.status, 200, JSON.stringify(back.body))
    assert.equal((await mgmt('GET', `/v1/projects/${p.ref}/branches/${bname}`)).body.persistent, false)
  })

  test('the project route of a branch ref answers 404, so that the CLI keeps linking the parent', async () => {
    const r = await mgmt('GET', `/v1/projects/${b.project_ref}`)
    assert.equal(r.status, 404, `GET /v1/projects/{branchRef} answers ${r.status}: hosted answers 404 and the CLI relies on it (supabase link <branch> keeps the parent cached)`)
  })
})
