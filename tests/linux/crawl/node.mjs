// A small client of the node under test, shared by seed.mjs and probes.mjs. Plain HTTP to the node's proxy with a
// Host header, as tests/linux/oauth/oauth-walk.mjs does, so nothing needs DNS; no dependencies.
//
// Config fields (written by tests/linux/studio-crawl.sh, mode 0600): domain, proxy {host, port}, owner {email,
// password}, owner_pat, org (the claimed organization's slug), org2, out (the output directory).
import crypto from 'node:crypto'
import http from 'node:http'

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const SECRET = /\b(sbp_oauth_|sbp_|sbr_|sbc_|sba_|sb_secret_|sb_publishable_)[0-9a-zA-Z_]{8,}/g
const JWT = /\beyJ[\w-]+\.[\w-]+\.[\w-]+/g
export const redact = (s) => String(s).replace(SECRET, '$1<cut>').replace(JWT, '<jwt cut>')

export class Failure extends Error {}

export class Node {
  constructor(cfg) {
    this.cfg = cfg
    this.domain = cfg.domain
    this.proxy = cfg.proxy ?? { host: '127.0.0.1', port: 80 }
    this._jwt = null
    this._jwtAt = 0
  }

  // raw: one request to host (a name of the node) through the proxy. Returns {status, headers, text, json}.
  raw(method, host, path, { headers = {}, body, timeout = 120_000 } = {}) {
    const payload = body === undefined ? undefined : typeof body === 'string' || Buffer.isBuffer(body) ? body : JSON.stringify(body)
    const h = { Host: host, ...headers }
    if (payload !== undefined && !Object.keys(h).some((k) => k.toLowerCase() === 'content-type')) h['Content-Type'] = 'application/json'
    if (payload !== undefined) h['Content-Length'] = Buffer.byteLength(payload)
    return new Promise((resolve, reject) => {
      const req = http.request({ host: this.proxy.host, port: this.proxy.port, method, path, headers: h, timeout }, (res) => {
        const chunks = []
        res.on('data', (c) => chunks.push(c))
        res.on('end', () => {
          const text = Buffer.concat(chunks).toString('utf8')
          let json
          try {
            json = text ? JSON.parse(text) : null
          } catch {
            json = undefined
          }
          resolve({ status: res.statusCode, headers: res.headers, text, json })
        })
      })
      req.on('timeout', () => req.destroy(new Error(`timeout: ${method} ${host}${path}`)))
      req.on('error', reject)
      if (payload !== undefined) req.write(payload)
      req.end()
    })
  }

  get apiHost() {
    return `api.${this.domain}`
  }
  hostOf(ref) {
    return `${ref}.api.${this.domain}`
  }

  // jwt: the owner's dashboard session (the /platform routes do not take a personal access token).
  async jwt() {
    if (this._jwt && Date.now() - this._jwtAt < 20 * 60_000) return this._jwt
    const r = await this.raw('POST', this.apiHost, '/auth/v1/token?grant_type=password', { body: { email: this.cfg.owner.email, password: this.cfg.owner.password } })
    if (r.status !== 200 || !r.json?.access_token) throw new Failure(`owner sign-in answered ${r.status}: ${redact(r.text).slice(0, 200)}`)
    this._jwt = r.json.access_token
    this._jwtAt = Date.now()
    return this._jwt
  }

  // api: a Management API call as the owner. /platform routes use the session, the others the personal access token.
  // Throws on a status of 400 and above unless `expect` lists it. Returns {status, json, text, headers}.
  async api(method, path, body, { expect = [], headers = {} } = {}) {
    const bearer = path.startsWith('/platform/') ? await this.jwt() : this.cfg.owner_pat
    const r = await this.raw(method, this.apiHost, path, { body, headers: { Authorization: `Bearer ${bearer}`, ...headers } })
    if (r.status >= 400 && !expect.includes(r.status)) throw new Failure(`${method} ${path} answered ${r.status}: ${redact(r.text).slice(0, 400)}`)
    return r
  }

  // sql: a statement on a project's database through the Management API.
  async sql(ref, query) {
    const r = await this.api('POST', `/v1/projects/${ref}/database/query`, { query })
    return r.json
  }

  // status: the project status the dashboard reads (/platform works for branches, /v1/projects/{branch} does not).
  async status(ref) {
    return (await this.api('GET', `/platform/projects/${ref}`)).json.status
  }

  async keys(ref) {
    const r = await this.api('GET', `/v1/projects/${ref}/api-keys?reveal=true`)
    const pick = (prefix) => r.json.find((k) => String(k.api_key ?? '').startsWith(prefix))?.api_key
    return { publishable: pick('sb_publishable_'), secret: pick('sb_secret_') }
  }

  // project: a call to a project's own host (Auth, Storage, REST) with a key.
  project(ref, method, path, key, { body, headers = {}, token } = {}) {
    return this.raw(method, this.hostOf(ref), path, { body, headers: { apikey: key, Authorization: `Bearer ${token ?? key}`, ...headers } })
  }
}

// waitFor polls fn until it returns a truthy value; on timeout it throws with `what` and the last error.
export async function waitFor(what, fn, { timeout = 300_000, every = 2000 } = {}) {
  const end = Date.now() + timeout
  let last
  for (;;) {
    try {
      const v = await fn()
      if (v) return v
    } catch (e) {
      last = e
    }
    if (Date.now() > end) throw new Failure(`timed out after ${Math.round(timeout / 1000)} s waiting for ${what}${last ? `: ${redact(last.message)}` : ''}`)
    await sleep(every)
  }
}

// totp: the six-digit code of RFC 6238 (HMAC-SHA1, 30-second step) for a base32 secret.
export function totp(secretBase32, now = Date.now()) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = ''
  for (const c of secretBase32.replace(/=+$/, '').replace(/\s/g, '').toUpperCase()) {
    const v = alphabet.indexOf(c)
    if (v < 0) throw new Error('not base32')
    bits += v.toString(2).padStart(5, '0')
  }
  const key = Buffer.from(bits.match(/.{8}/g).map((b) => parseInt(b, 2)))
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(Math.floor(now / 1000 / 30)))
  const h = crypto.createHmac('sha1', key).update(counter).digest()
  const o = h[h.length - 1] & 0xf
  const n = ((h[o] & 0x7f) << 24) | (h[o + 1] << 16) | (h[o + 2] << 8) | h[o + 3]
  return String(n % 1_000_000).padStart(6, '0')
}

// ---- shared fixtures helpers ------------------------------------------------------------------

// deployFunction uploads the sources of an Edge Function through the Management API, as `supabase functions deploy
// --use-api` does: a multipart body with the metadata and the file.
export async function deployFunction(node, ref, slug, body = 'hello') {
  const meta = JSON.stringify({ entrypoint_path: 'index.ts', name: slug, verify_jwt: false })
  const form = new FormData()
  form.append('metadata', new Blob([meta], { type: 'application/json' }))
  form.append('file', new Blob([`Deno.serve(() => new Response(JSON.stringify({ ${slug.replace(/[^A-Za-z0-9_]/g, '_')}: '${body}' }), { headers: { 'content-type': 'application/json' } }))\n`], { type: 'application/typescript' }), 'index.ts')
  const res = new Response(form) // the serialized body and the Content-Type with its boundary come from one Response
  const payload = Buffer.from(await res.arrayBuffer())
  const r = await node.raw('POST', node.apiHost, `/v1/projects/${ref}/functions/deploy?slug=${encodeURIComponent(slug)}`, {
    body: payload, headers: { Authorization: `Bearer ${node.cfg.owner_pat}`, 'Content-Type': res.headers.get('content-type') }, timeout: 300_000,
  })
  if (r.status >= 300) throw new Failure(`function deploy answered ${r.status}: ${redact(r.text).slice(0, 300)}`)
}

// createBranch makes a branch through the Management API and waits until its run is over and its project is healthy.
export async function createBranch(node, parent, name, extra = {}) {
  const r = await node.api('POST', `/v1/projects/${parent}/branches`, { branch_name: name, ...extra })
  const b = { id: r.json.id, ref: r.json.project_ref, name }
  await waitBranch(node, b)
  return b
}

export async function waitBranch(node, b) {
  await waitFor(`branch ${b.name} to finish`, async () => {
    const r = await node.api('GET', `/v1/branches/${b.id}`)
    if (/FAILED$/.test(r.json.status)) throw new Failure(`branch ${b.name} is ${r.json.status}`)
    return r.json.status === 'FUNCTIONS_DEPLOYED' || r.json.status === 'MIGRATIONS_PASSED'
  }, { timeout: 900_000, every: 3000 })
  await waitFor(`branch ${b.name} to be healthy`, async () => (await node.status(b.ref)) === 'ACTIVE_HEALTHY', { timeout: 300_000, every: 3000 })
}
