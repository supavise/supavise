// The fixtures of the Studio crawl, made through the Management API the way a customer's tooling would, on the node
// that tests/linux/studio-crawl.sh started:
//
//   node seed.mjs /path/crawl-config.json            (writes <out>/fixtures.json)
//
//   A         project "crawl-a": a table with an RLS policy and rows, a bucket with an object, a deployed function,
//             a pg_cron job, a pgmq queue, GraphQL introspection on, a second user with a TOTP factor, a custom hostname
//   B         branch "crawl-b" of A (schema only; the function is copied), with a custom hostname
//   pending   branch of A with a table of its own and a merge request (request_review)
//   paused    branch of A that was paused by hand
//   idle      branch of A that the node paused after the idle period (only when the build has the setting)
//   scheduled branch of A whose deletion is scheduled (force=false)
//   other     project "crawl-other" in the second organization
//   P         project "crawl-probe": the parent of the branches the probes make and break (not visited as a route)
//
// A fixture that this build cannot make because an extension is missing is recorded under `unavailable` with the
// reason, and the crawl reports it. Anything else that fails ends the run: a fixture that is not there would make
// the crawl look at an empty page and call it fine.
import { mkdirSync, writeFileSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { join } from 'node:path'
import crypto from 'node:crypto'

import { readJSON } from './lib.mjs'
import { awake, deployFunction, Failure, Node, redact, sleep, Stop, totp, waitBranch as waitBranchReady, waitFor } from './node.mjs'

const cfg = readJSON(process.argv[2] ?? (() => { throw new Error('usage: node seed.mjs crawl-config.json') })())
const node = new Node(cfg)
const t0 = Date.now()
const log = (m) => console.error(`${String(Math.round((Date.now() - t0) / 1000)).padStart(5)}s seed: ${m}`)
const hex = (n) => crypto.randomBytes(n).toString('hex')

const fx = {
  orgs: [cfg.org, cfg.org2],
  projects: {},
  branches: {},
  byRef: {},
  global: { templates: ['confirm-sign-up', 'invite-user', 'magic-link-or-otp', 'change-email-address', 'reset-password', 'reauthentication'] },
  hostnames: {},
  user: {},
  unavailable: {},
}

async function createProject(name, org) {
  log(`project ${name}`)
  const r = await node.api('POST', '/v1/projects', { name, organization_slug: org, db_pass: `Crawl-${hex(12)}`, region: 'us-east-1' })
  const ref = r.json.ref
  await waitFor(`${name} to be healthy`, async () => {
    const s = await node.status(ref)
    if (s === 'INIT_FAILED') throw new Stop(`${name} is INIT_FAILED`)
    return s === 'ACTIVE_HEALTHY'
  }, { timeout: 600_000, every: 3000 })
  return ref
}

// seedData: the rows every project of the crawl has, so that a page that shows them has something to show.
async function seedData(ref, { rich }) {
  await node.sql(ref, `
    create table public.crawl_items (id bigint generated always as identity primary key, label text not null, created_at timestamptz not null default now());
    insert into public.crawl_items (label) select 'item ' || g from generate_series(1, 25) g;
    alter table public.crawl_items enable row level security;
    create policy "crawl read policy" on public.crawl_items for select to anon, authenticated using (true);
    grant select on public.crawl_items to anon, authenticated;
    grant all on public.crawl_items to service_role;
    notify pgrst, 'reload schema';`)
  if (!rich) return
  // pg_graphql is off in a new project, as on hosted; the GraphiQL page and the Data API docs need it
  await node.sql(ref, 'create extension if not exists pg_graphql')
  // From 1.6.0 pg_graphql answers no introspection until the schema's comment asks for it (the directive Studio writes when a
  // user presses "Enable introspection" on the GraphiQL page); the GraphiQL probe reads the schema through the page
  await node.sql(ref, `comment on schema public is e'@graphql({"introspection": true})'`)
  await node.sql(ref, 'create extension if not exists pg_cron')
  await node.sql(ref, `select cron.schedule('crawl-job', '0 3 * * *', $$select 1$$)`)
  try {
    await node.sql(ref, 'create extension if not exists pgmq')
    await node.sql(ref, `select pgmq.create('crawl_queue')`)
  } catch (e) {
    fx.unavailable.queues = redact(e.message).slice(0, 300)
    log(`no pgmq queue: ${fx.unavailable.queues}`)
  }
}

// Every branch is persistent while the node idle-pauses (awake(), node.mjs), except the three whose state is the fixture.
async function createBranch(parent, name, extra = {}) {
  log(`branch ${name}`)
  const r = await node.api('POST', `/v1/projects/${parent}/branches`, { branch_name: name, ...awake(cfg), ...extra })
  fx.branches[name] = { id: r.json.id, ref: r.json.project_ref, name, parent }
}

async function waitBranch(name) {
  await waitBranchReady(node, fx.branches[name])
  return fx.branches[name].ref
}

// ---- projects ----------------------------------------------------------------------------------
const A = await createProject('crawl-a', cfg.org)
fx.projects.A = A
await seedData(A, { rich: true })

log('a user with a TOTP factor')
const { publishable, secret } = await node.keys(A)
const email = 'crawl-user@example.com'
const password = `Crawl-${hex(8)}`
const created = await node.api('POST', `/platform/auth/${A}/users`, { email, password, email_confirm: true })
fx.user = { email, id: created.json.id }
const login = await node.project(A, 'POST', '/auth/v1/token?grant_type=password', publishable, { body: { email, password } })
if (login.status !== 200) throw new Failure(`sign-in of the crawl user answered ${login.status}: ${redact(login.text).slice(0, 200)}`)
const enroll = await node.project(A, 'POST', '/auth/v1/factors', publishable, { token: login.json.access_token, body: { factor_type: 'totp', friendly_name: 'crawl' } })
if (enroll.status >= 300) throw new Failure(`TOTP enrollment answered ${enroll.status}: ${redact(enroll.text).slice(0, 200)}`)
const challenge = await node.project(A, 'POST', `/auth/v1/factors/${enroll.json.id}/challenge`, publishable, { token: login.json.access_token, body: {} })
const verify = await node.project(A, 'POST', `/auth/v1/factors/${enroll.json.id}/verify`, publishable, {
  token: login.json.access_token, body: { challenge_id: challenge.json.id, code: totp(enroll.json.totp.secret) },
})
if (verify.status >= 300) throw new Failure(`TOTP verification answered ${verify.status}: ${redact(verify.text).slice(0, 200)}`)
fx.user.factor_id = enroll.json.id

log('a bucket with an object')
const bucket = await node.project(A, 'POST', '/storage/v1/bucket', secret, { body: { name: 'crawl-bucket', public: false } })
if (bucket.status >= 300) throw new Failure(`bucket answered ${bucket.status}: ${redact(bucket.text).slice(0, 200)}`)
const object = await node.project(A, 'POST', '/storage/v1/object/crawl-bucket/crawl.txt', secret, { body: 'crawl fixture', headers: { 'Content-Type': 'text/plain' } })
if (object.status >= 300) throw new Failure(`object upload answered ${object.status}: ${redact(object.text).slice(0, 200)}`)

// a 1x1 PNG, for the image transformation probe
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64')
const png = await node.project(A, 'POST', '/storage/v1/object/crawl-bucket/crawl.png', secret, { body: PNG, headers: { 'Content-Type': 'image/png' } })
if (png.status >= 300) throw new Failure(`image upload answered ${png.status}: ${redact(png.text).slice(0, 200)}`)

if (cfg.functions !== false) {
  log('an Edge Function')
  await deployFunction(node, A, 'hello')
}

log('a SQL snippet')
const snippetId = crypto.randomUUID()
await node.api('PUT', `/platform/projects/${A}/content`, { id: snippetId, name: 'crawl snippet', type: 'sql', visibility: 'user', content: { sql: 'select 1 as crawl', content_id: snippetId, schema_version: '1', favorite: false } })

// ids Studio puts in a URL: tables and publications come from the database.
const tables = await node.sql(A, `select c.oid::int as id from pg_class c join pg_namespace n on n.oid = c.relnamespace where n.nspname = 'public' and c.relname = 'crawl_items'`)
const pubs = await node.sql(A, `select oid::int as id from pg_publication where pubname = 'supabase_realtime'`)
fx.byRef[A] = {
  tables: tables.map((t) => String(t.id)), publications: pubs.map((p) => String(p.id)), snippets: [snippetId],
  functions: cfg.functions === false ? [] : ['hello'], buckets: ['crawl-bucket'],
}

// ---- branches of A -------------------------------------------------------------------------------
// created one after another: each is a Postgres cluster, and the machine is a shared runner.
await createBranch(A, 'crawl-b')
fx.projects.B = await waitBranch('crawl-b')

await createBranch(A, 'crawl-pending')
fx.projects.pending = await waitBranch('crawl-pending')
await node.sql(fx.projects.pending, 'create table public.crawl_branch_only (id bigint generated always as identity primary key, note text)')
await node.api('PATCH', `/v1/branches/${fx.branches['crawl-pending'].id}`, { request_review: true })

await createBranch(A, 'crawl-paused', { persistent: false }) // paused by hand just below: the node never undoes that
fx.projects.paused = await waitBranch('crawl-paused')
await node.api('POST', `/v1/projects/${fx.projects.paused}/pause`)
await waitFor('the paused branch to be INACTIVE', async () => (await node.status(fx.projects.paused)) === 'INACTIVE', { timeout: 300_000, every: 3000 })

await createBranch(A, 'crawl-scheduled', { persistent: false }) // scheduled for deletion just below: never idle-paused
fx.projects.scheduled = await waitBranch('crawl-scheduled')
await node.api('DELETE', `/v1/branches/${fx.branches['crawl-scheduled'].id}?force=false`)

if (cfg.idle_pause) {
  // the one branch the node pauses: non-persistent, and nothing here or in the route pass uses it before the probe
  await createBranch(A, 'crawl-idle', { persistent: false })
  fx.projects.idle = await waitBranch('crawl-idle')
}

// the branch has the parent's function (copied at create); its pages are visited like the parent's
fx.byRef[fx.projects.B] = { functions: cfg.functions === false ? [] : ['hello'] }

// ---- the other organization and the probe parent ------------------------------------------------------
fx.projects.other = await createProject('crawl-other', cfg.org2)
fx.projects.P = await createProject('crawl-probe', cfg.org)
await seedData(fx.projects.P, { rich: false })
if (cfg.functions !== false) await deployFunction(node, fx.projects.P, 'hello')
fx.byRef[fx.projects.P] = { tables: [], functions: cfg.functions === false ? [] : ['hello'] }

// ---- custom hostnames on A and B -----------------------------------------------------------------------
// The records go to the node's resolver (dnsmasq, started by studio-crawl.sh), which is restarted to read them.
const zone = []
for (const key of ['A', 'B']) {
  const ref = fx.projects[key]
  const host = `crawl-${key.toLowerCase()}.example.test`
  log(`custom hostname ${host} on ${key}`)
  const init = await node.api('POST', `/v1/projects/${ref}/custom-hostname/initialize`, { custom_hostname: host })
  const ov = init.json?.data?.result?.ownership_verification ?? init.json?.result?.ownership_verification
  if (!ov?.name || !ov?.value) throw new Failure(`custom-hostname/initialize answered without an ownership_verification: ${redact(init.text).slice(0, 300)}`)
  // dnsmasq answers a CNAME only for a target it knows itself, so the project's host is a record of its own too
  zone.push(`cname=${host},${ref}.api.${cfg.domain}`, `host-record=${ref}.api.${cfg.domain},127.0.0.1`, `txt-record=${ov.name},${ov.value}`)
  fx.hostnames[key] = host
}
// the probe parent has a CNAME for a hostname the custom-domain probe adds through the dashboard
zone.push(`cname=crawl-p.example.test,${fx.projects.P}.api.${cfg.domain}`, `host-record=${fx.projects.P}.api.${cfg.domain},127.0.0.1`)
fx.hostnames.P = 'crawl-p.example.test'
if (cfg.dns_dir) {
  writeFileSync(join(cfg.dns_dir, 'zone.conf'), `${zone.join('\n')}\n`)
  execFileSync('systemctl', ['restart', cfg.dns_unit ?? 'crawl-dnsmasq'])
  await sleep(1500)
  for (const key of ['A', 'B']) {
    const ref = fx.projects[key]
    await waitFor(`the DNS check of ${fx.hostnames[key]}`, async () => {
      const r = await node.api('POST', `/v1/projects/${ref}/custom-hostname/reverify`, undefined, { expect: [429] })
      return r.status < 300 && /^[45]_/.test(String(r.json?.status))
    }, { timeout: 120_000, every: 16_000 })
    await node.api('POST', `/v1/projects/${ref}/custom-hostname/activate`)
  }
} else {
  fx.unavailable.hostnames = 'no resolver for the node (dns_dir is not set): the hostnames are claimed and not verified'
}

// ---- the idle branch must have been paused by now -------------------------------------------------------
if (fx.projects.idle) {
  log('waiting for the node to pause the idle branch')
  await waitFor('the idle branch to be paused by the node', async () => (await node.status(fx.projects.idle)) === 'INACTIVE', { timeout: 600_000, every: 5000 })
} else {
  fx.unavailable.idle = 'this build has no idle auto-pause setting'
}

mkdirSync(cfg.out, { recursive: true })
writeFileSync(join(cfg.out, 'fixtures.json'), `${JSON.stringify(fx, null, 1)}\n`)
log(`done: ${Object.keys(fx.projects).length} projects, ${Object.keys(fx.branches).length} branches`)
