#!/usr/bin/env node
// Drives a node's GitHub integration over HTTP: the dashboard's calls (authorization, repositories,
// connections), signed webhooks as GitHub sends them, and what the node reported back to the fake
// GitHub (tests/linux/github/fake-github.mjs). Used by tests/linux/github-smoke.sh against a real node
// (--db checks the branches' databases too) and by tests/linux/github/selftest.sh against a stand-in.
//
//   node drive.mjs --stack stack.json --fake http://127.0.0.1:8765 --webhook-secret S [--db]
//
// stack.json: {api_url, pat, jwt, parent_ref, org_id}. `pat` is for /v1, `jwt` (a dashboard session)
// for /platform. Exit status 1 when a check fails.

import { createHmac } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'

const { values: opt } = parseArgs({
  options: { stack: { type: 'string' }, fake: { type: 'string' }, 'webhook-secret': { type: 'string' }, db: { type: 'boolean', default: false } },
})
const stack = JSON.parse(readFileSync(opt.stack, 'utf8'))
const API = stack.api_url
const FAKE = opt.fake
const PARENT = stack.parent_ref
const REPO_ID = 750123456
const INSTALLATION_ID = 49123456
const failures = []

function check(cond, what) {
  console.log((cond ? 'ok   ' : 'FAIL ') + what)
  if (!cond) failures.push(what)
  return cond
}

async function call(method, path, { body, token = 'pat', headers = {}, raw = false } = {}) {
  const h = { authorization: 'Bearer ' + (token === 'jwt' ? stack.jwt : stack.pat), ...headers }
  if (body !== undefined) h['content-type'] = 'application/json'
  const r = await fetch(API + path, { method, headers: h, body: body === undefined ? undefined : JSON.stringify(body) })
  const text = await r.text()
  let data = text
  if (!raw) {
    try {
      data = text ? JSON.parse(text) : null
    } catch {
      data = text
    }
  }
  return { code: r.status, data }
}
const fake = async (method, path, body) => {
  const r = await fetch(FAKE + path, { method, body: body === undefined ? undefined : JSON.stringify(body) })
  return r.json()
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function until(what, fn, timeout = 240000) {
  const end = Date.now() + timeout
  let last
  while (Date.now() < end) {
    last = await fn()
    if (last) return last
    await sleep(1000)
  }
  check(false, `${what} (timed out)`)
  return null
}

// deliver posts a webhook the way GitHub does.
async function deliver(event, payload, { id, secret = opt['webhook-secret'], sign = true } = {}) {
  const body = JSON.stringify(payload)
  const headers = { 'content-type': 'application/json', 'x-github-event': event, 'x-github-delivery': id }
  if (sign) headers['x-hub-signature-256'] = 'sha256=' + createHmac('sha256', secret).update(body).digest('hex')
  const r = await fetch(API + '/hooks/github', { method: 'POST', headers, body })
  return { code: r.status, data: await r.json().catch(() => null) }
}

const repo = { id: REPO_ID, full_name: 'acme/app', default_branch: 'main' }
const pr = (action, n, head, sha, headRepo = repo) => ({
  action, number: n,
  pull_request: { number: n, state: 'open', title: 'A change', merged: false,
    head: { ref: head, sha, repo: headRepo }, base: { ref: 'main', sha: 'base', repo } },
  repository: repo, installation: { id: INSTALLATION_ID },
})
const push = (ref, after, modified) => ({ ref, before: '0'.repeat(40), after, repository: repo, installation: { id: INSTALLATION_ID },
  ...(modified ? { commits: [{ modified }] } : {}) })

const MIG1 = 'create table public.todos (id bigint primary key, title text);'
const MIG2 = 'alter table public.todos add column done boolean not null default false;'
const SEED = "insert into public.todos (id, title) values (1, 'first');"
const FN = (s) => `Deno.serve(() => new Response('${s}'))`
const repoFiles = (extra = {}) => ({
  'README.md': '# app',
  'supabase/migrations/20260101000000_init.sql': MIG1,
  'supabase/seed.sql': SEED,
  'supabase/config.toml': 'project_id = "app"\n[api]\nmax_rows = 321\n[auth.third_party.firebase]\nenabled = true\n',
  'supabase/functions/hello/index.ts': FN('hello'),
  ...extra,
})

const branchesOf = async () => (await call('GET', `/v1/projects/${PARENT}/branches`)).data || []
const sqlRows = async (ref, query) => {
  const r = await call('POST', `/v1/projects/${ref}/database/query`, { body: { query } })
  return r.code === 201 || r.code === 200 ? r.data : null
}

async function runsOf(ref) {
  const r = await call('GET', `/v1/projects/${ref}/actions`)
  return Array.isArray(r.data) ? r.data : []
}
const runStatus = (run) => {
  const steps = run.run_steps || []
  if (!steps.length) return 'RUNNING'
  if (steps.some((s) => s.status === 'DEAD')) return 'FAILED'
  return steps.every((s) => s.status === 'EXITED' || s.status === 'PAUSED') ? 'SUCCESS' : 'RUNNING'
}
const stepStatus = (run, name) => (run.run_steps || []).find((s) => s.name === name)?.status

async function main() {
  // ---- the dashboard's side, before anyone authorized -----------------------------------------
  let r = await call('GET', '/platform/profile', { token: 'jwt' })
  check(r.code === 200 && !(r.data.disabled_features || []).includes('integrations:github'), 'the profile offers the GitHub surfaces when the server has an App')
  r = await call('GET', '/platform/integrations/github/authorization', { token: 'jwt' })
  check(r.code === 404, 'the authorization is 404 until the user authorized the App')
  r = await call('GET', '/platform/integrations/github/repositories', { token: 'jwt' })
  check(r.code === 403, 'repositories need an authorization')
  r = await call('POST', '/platform/integrations/github/authorization', { token: 'jwt', body: { code: 'wrong', state: 'x' } })
  check(r.code === 400, 'a code GitHub refuses is a 400')
  r = await call('POST', '/platform/integrations/github/authorization', { token: 'jwt', body: { code: 'good-code', state: 'x' } })
  check(r.code === 201, 'authorizing with the code of the fake GitHub works')
  r = await call('GET', '/platform/integrations/github/authorization', { token: 'jwt' })
  check(r.code === 200 && r.data.sender_id === 7, 'the authorization names the GitHub user')

  // A token that expires is refreshed on the next call.
  await fake('POST', '/__fake/expire-user-token')
  r = await call('GET', '/platform/integrations/github/repositories', { token: 'jwt' })
  check(r.code === 200, 'repositories after the access token expired (a refresh)')
  check((await fake('GET', '/__fake/state')).refreshed === 1, 'the refresh token was used once')
  const repos = r.data?.repositories || []
  check(repos.length === 1 && repos[0].id === REPO_ID && repos[0].installation_id === INSTALLATION_ID && repos[0].name === 'acme/app',
    'the repository comes back with its exact ids')
  r = await call('GET', `/platform/integrations/github/repositories/${REPO_ID}/branches/main`, { token: 'jwt' })
  check(r.code === 200 && r.data.name === 'main', 'the production branch check finds main')
  r = await call('GET', `/platform/integrations/github/repositories/${REPO_ID}/branches/nope`, { token: 'jwt' })
  check(r.code === 404, 'the branch check says 404 for a branch that does not exist')

  // ---- connect the project ---------------------------------------------------------------------
  await fake('PUT', '/__fake/commits/head-main', { files: repoFiles() })
  r = await call('POST', '/platform/integrations/github/connections', {
    token: 'jwt', body: { project_ref: PARENT, installation_id: INSTALLATION_ID, repository_id: REPO_ID, workdir: '', supabase_changes_only: false, branch_limit: 5, new_branch_per_pr: true } })
  check(r.code === 201 && r.data.installation_id === INSTALLATION_ID, 'the project is connected to the repository')
  const connId = r.data?.id
  r = await call('GET', `/platform/integrations/github/connections?organization_id=${stack.org_id}`, { token: 'jwt' })
  check(r.code === 200 && r.data.connections.length === 1 && r.data.connections[0].repository.id === REPO_ID && r.data.connections[0].project.ref === PARENT,
    'the connection is listed with the repository and the project')
  r = await call('GET', `/platform/integrations/github/connections/${connId}/config`, { token: 'jwt' })
  check(r.code === 200 && r.data.config?.project_id === 'app' && r.data.path === 'supabase/config.toml', 'config.toml of the repository is read as JSON')

  // ---- a pull request --------------------------------------------------------------------------
  await fake('PUT', '/__fake/commits/sha-1', { files: repoFiles() })
  let d = await deliver('pull_request', pr('opened', 12, 'feature-x', 'sha-1'), { id: 'd-bad', secret: 'wrong-secret' })
  check(d.code === 401, 'a webhook with a wrong signature is refused')
  d = await deliver('pull_request', pr('opened', 12, 'feature-x', 'sha-1'), { id: 'd-unsigned', sign: false })
  check(d.code === 401, 'a webhook with no signature is refused')
  d = await deliver('pull_request', pr('opened', 12, 'feature-x', 'sha-1'), { id: 'd-open' })
  check(d.code === 202, `a signed pull_request opened is accepted (${d.code} ${JSON.stringify(d.data)})`)

  let branch = await until('the branch of the pull request is deployed', async () => {
    const b = (await branchesOf()).find((x) => x.git_branch === 'feature-x')
    return b && b.status === 'FUNCTIONS_DEPLOYED' ? b : (b && b.status.endsWith('FAILED') ? (check(false, `the branch failed: ${b.status} ${b.supavise_detail}`), b) : null)
  })
  if (!branch) return
  check(branch.pr_number === 12 && branch.name === 'feature-x' && branch.persistent === false && !branch.supavise_expires_at,
    'the branch has pr_number and git_branch, is a preview and has no expiry')
  const ref = branch.project_ref
  let runs = await runsOf(ref)
  check(runs.length === 1, `the branch has one run (${runs.length})`)
  const detail = (await call('GET', `/v1/projects/${ref}/actions/${runs[0].id}`)).data
  check(detail && runStatus(detail) === 'SUCCESS' && (detail.run_steps || []).length === 7, 'the run has seven steps and succeeded')
  for (const s of ['clone', 'health', 'configure', 'migrate', 'seed', 'deploy']) check(stepStatus(detail, s) === 'EXITED', `step ${s} is EXITED (${stepStatus(detail, s)})`)
  const log = (await call('GET', `/v1/projects/${ref}/actions/${runs[0].id}/logs`, { raw: true })).data
  check(/applied 20260101000000_init/.test(log) && /applied seed/.test(log) && /deployed function hello/.test(log), 'the log names the migration, the seed and the function')
  check(/auth\.third_party\.firebase\.enabled is not applied/.test(log), 'an unsupported config.toml section is in the log, not dropped silently')

  let st = await fake('GET', '/__fake/state')
  let c1 = st.checks.find((c) => c.head_sha === 'sha-1' && c.name === 'Supabase Preview')
  check(c1 && c1.status === 'completed' && c1.conclusion === 'success', 'the check run "Supabase Preview" completed with success')
  check(c1 && /\| migrate \| Success \|/.test(c1.output.summary || ''), 'the check run carries the step table')
  let cm = st.comments.filter((c) => c.pr === 12)
  check(cm.length === 1 && cm[0].body.includes('supavise-preview'), 'one comment is on the pull request')

  if (opt.db) {
    let rows = await sqlRows(ref, 'select count(*)::int as n from public.todos')
    check(rows && rows[0].n === 1, 'the seed ran once on the branch')
    rows = await sqlRows(ref, 'select version from supabase_migrations.schema_migrations order by 1')
    check(rows && rows.length === 1 && rows[0].version === '20260101000000', 'the branch has the repository migration in its history')
    const pg = await call('GET', `/v1/projects/${ref}/postgrest`)
    check(pg.code === 200 && pg.data.max_rows === 321, `config.toml reached the branch's PostgREST settings (${pg.data?.max_rows})`)
  }
  const fns = (await call('GET', `/v1/projects/${ref}/functions`)).data || []
  check(fns.some((f) => f.slug === 'hello'), 'the function of the repository is deployed on the branch')

  // ---- a new commit: only what is new --------------------------------------------------------
  await fake('PUT', '/__fake/commits/sha-2', { files: repoFiles({
    'supabase/migrations/20260102000000_done.sql': MIG2,
    'supabase/functions/hello/index.ts': FN('hello v2'),
  }) })
  d = await deliver('pull_request', pr('synchronize', 12, 'feature-x', 'sha-2'), { id: 'd-sync' })
  check(d.code === 202, 'a synchronize event is accepted')
  runs = await until('the second run finishes', async () => {
    const rs = await runsOf(ref)
    return rs.length === 2 && rs.every((x) => runStatus(x) !== 'RUNNING') ? rs : null
  })
  if (runs) {
    const newest = runs.slice().sort((a, b) => String(b.created_at).localeCompare(String(a.created_at)))[0]
    const l2 = (await call('GET', `/v1/projects/${ref}/actions/${newest.id}/logs`, { raw: true })).data
    check(/applied 20260102000000_done/.test(l2) && !/applied 20260101000000/.test(l2), 'the second commit applies only the migration the branch lacks')
    check(!/applied seed/.test(l2), 'the seed does not run again')
    check(/deployed function hello/.test(l2), 'the function that changed is deployed again')
    check(runStatus(newest) === 'SUCCESS', 'the second run succeeded')
  }
  if (opt.db) {
    const rows = await sqlRows(ref, 'select version from supabase_migrations.schema_migrations order by 1')
    check(rows && rows.map((x) => x.version).join() === '20260101000000,20260102000000', 'the history has both migrations once')
    const n = await sqlRows(ref, 'select count(*)::int as n from public.todos')
    check(n && n[0].n === 1, 'the seed was not applied twice')
  }
  st = await fake('GET', '/__fake/state')
  const c2 = st.checks.find((c) => c.head_sha === 'sha-2')
  check(c2 && c2.conclusion === 'success', 'the second commit has its own successful check run')
  cm = st.comments.filter((c) => c.pr === 12)
  check(cm.length === 1 && cm[0].updates >= 1, 'the one comment was updated, not repeated')

  // ---- replay, fork, other repository ----------------------------------------------------------
  const before = (await branchesOf()).length
  d = await deliver('pull_request', pr('opened', 13, 'other', 'sha-1'), { id: 'd-open' })
  check(d.code === 200 && /before/.test(d.data?.message || ''), 'a replayed delivery id does nothing')
  d = await deliver('pull_request', pr('opened', 14, 'evil', 'sha-1', { id: 555, full_name: 'mallory/app' }), { id: 'd-fork' })
  check(d.code === 202 && /fork/.test(d.data?.message || ''), 'a pull request from a fork is ignored')
  await sleep(1500)
  check((await branchesOf()).length === before, 'neither made a branch')

  // ---- closing the pull request ----------------------------------------------------------------
  d = await deliver('pull_request', pr('closed', 12, 'feature-x', 'sha-2'), { id: 'd-close' })
  check(d.code === 202, 'closed is accepted')
  await until('the branch is deleted', async () => !(await branchesOf()).some((b) => b.project_ref === ref))
  check(!(await branchesOf()).some((b) => b.project_ref === ref), 'closing the pull request deleted its branch')

  // ---- production ------------------------------------------------------------------------------
  await fake('PUT', '/__fake/commits/sha-main', { files: repoFiles({ 'supabase/migrations/20260102000000_done.sql': MIG2 }) })
  d = await deliver('push', push('refs/heads/main', 'sha-main'), { id: 'd-push-off' })
  await sleep(1500)
  check((await runsOf(PARENT)).length === 0, 'a push to main does nothing while Deploy to production is off')
  r = await call('PATCH', `/v1/branches/${PARENT}`, { body: { git_branch: 'main' } })
  check(r.code === 200, `the production branch is set (${r.code})`)
  d = await deliver('push', push('refs/heads/main', 'sha-main'), { id: 'd-push' })
  check(d.code === 202, 'a push to the production branch is accepted')
  const prod = await until('the production run finishes', async () => {
    const rs = await runsOf(PARENT)
    return rs.length >= 1 && rs.every((x) => runStatus(x) !== 'RUNNING') ? rs : null
  })
  if (prod) check(prod.every((x) => runStatus(x) === 'SUCCESS'), 'the production run succeeded')
  st = await fake('GET', '/__fake/state')
  const pc = st.checks.find((c) => c.head_sha === 'sha-main')
  check(pc && pc.name === 'Supabase Production' && pc.conclusion === 'success', 'production has its check run')
  if (opt.db) {
    const rows = await sqlRows(PARENT, 'select version from supabase_migrations.schema_migrations order by 1')
    check(rows && rows.map((x) => x.version).join() === '20260101000000,20260102000000', 'production has the repository migrations')
    const n = await sqlRows(PARENT, 'select count(*)::int as n from public.todos')
    check(n && n[0].n === 0, 'production was not seeded')
  }
  r = await call('POST', `/v1/branches/${PARENT}/push`, { body: {} })
  check(r.code === 201 && r.data.workflow_run_id, `Resync on the production row starts a run (${r.code})`)
  await until('the resync run finishes', async () => {
    const rs = await runsOf(PARENT)
    return rs.length >= 2 && rs.every((x) => runStatus(x) !== 'RUNNING')
  })

  // ---- the dashboard lets go -------------------------------------------------------------------
  r = await call('DELETE', `/platform/integrations/github/connections/${connId}`, { token: 'jwt' })
  check(r.code === 204, 'the connection is removed')
  r = await call('DELETE', '/platform/integrations/github/authorization', { token: 'jwt' })
  check(r.code === 200, 'the authorization is removed')
  r = await call('GET', '/platform/integrations/github/authorization', { token: 'jwt' })
  check(r.code === 404, 'and the authorization is 404 again')
}

main()
  .catch((e) => check(false, 'the driver crashed: ' + (e.stack || e)))
  .finally(() => {
    console.log(failures.length ? `\n${failures.length} check(s) failed` : '\nall checks passed')
    process.exit(failures.length ? 1 : 0)
  })
