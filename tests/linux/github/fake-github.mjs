#!/usr/bin/env node
// A stand-in for GitHub: the calls Supavise's GitHub integration makes (internal/github), and a control
// API that a test uses to put commits in the repository and read back what the server reported.
// Real GitHub is never contacted by a test.
//
//   node fake-github.mjs --port 8765 --app-id 918273 --key key.pem --client-secret S [--file ready.json] [--callback URL]
//
// It listens on 127.0.0.1:<port>, answers GitHub's API and web endpoints on the same origin
// (api_url = web_url = http://127.0.0.1:<port>), and writes {url} to --file when it is ready.
//
// App and OAuth
//   POST /app-manifests/{code}/conversions   the manifest flow's last step ("manifest-code" is good once)
//   POST /app/installations/{id}/access_tokens  needs a JWT signed with the App's key, valid for at most 10 minutes
//   POST /login/oauth/access_token           code "good-code", or a refresh token it issued
//   GET  /user, /user/installations, /user/installations/{id}/repositories   with the user's token
//   DELETE /applications/{client}/token
//   GET  /login/oauth/authorize, /apps/{slug}/installations/new   the pages Studio's windows open on github.com; with
//        --callback (the dashboard's /integrations/github/authorize) they redirect there like GitHub does, with
//        code=good-code and the state, or setup_action=install. A browser test that sends github.com to this origin
//        reaches Studio's landing page with them.
// Repository acme/app (installation token "inst-token")
//   GET  /repos/{o}/{r}/git/trees/{sha}, git/blobs/{sha}, contents/{path}, pulls/{n}/files, branches, commits/{ref}
//   POST /repos/{o}/{r}/check-runs, PATCH .../check-runs/{id}
//   GET, POST /repos/{o}/{r}/issues/{n}/comments, PATCH .../issues/comments/{id}
// Control (no authentication)
//   PUT  /__fake/commits/{sha}      body {files: {path: content}}
//   PUT  /__fake/pulls/{n}/files    body ["path", ...]
//   GET  /__fake/state              {checks, comments, requests, refreshed}
//   POST /__fake/expire-user-token  makes the user's access token expire now (the next call must refresh)

import { createServer } from 'node:http'
import { createHash, createPublicKey, verify } from 'node:crypto'
import { readFileSync, writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'

const { values: opt } = parseArgs({
  options: {
    port: { type: 'string', default: '0' },
    'app-id': { type: 'string', default: '918273' },
    key: { type: 'string' },
    'client-secret': { type: 'string', default: 'client-secret' },
    file: { type: 'string' },
    callback: { type: 'string' },
  },
})
const APP_ID = opt['app-id']
const REPO_ID = 750123456
const INSTALLATION_ID = 49123456
const REPO = 'acme/app'
const publicKey = opt.key ? createPublicKey(readFileSync(opt.key)) : null

const files = new Map() // commit sha -> Map(path -> content)
const prFiles = new Map() // pr number -> [paths]
const checks = new Map()
const comments = new Map()
const requests = []
let nextId = 1000
let userToken = 'ghu_user_1'
let refreshToken = 'ghr_user_1'
let userExpired = false
let refreshed = 0
let manifestUsed = false

const blobSha = (content) => createHash('sha1').update(`blob ${Buffer.byteLength(content)}\0`).update(content).digest('hex')

function json(res, status, body, headers = {}) {
  const b = JSON.stringify(body)
  res.writeHead(status, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(b), ...headers })
  res.end(b)
}
const fail = (res, status, message) => json(res, status, { message })

async function readBody(req) {
  const chunks = []
  for await (const c of req) chunks.push(c)
  return Buffer.concat(chunks).toString('utf8')
}

function b64u(s) {
  return Buffer.from(s.replace(/-/g, '+').replace(/_/g, '/'), 'base64')
}

// appJWT checks what GitHub checks: RS256 under the App's key, issuer, and a lifetime of at most ten minutes.
function appJWT(req) {
  const auth = req.headers.authorization || ''
  const tok = auth.replace(/^Bearer /, '')
  const parts = tok.split('.')
  if (parts.length !== 3 || !publicKey) return false
  try {
    const header = JSON.parse(b64u(parts[0]).toString())
    const claims = JSON.parse(b64u(parts[1]).toString())
    if (header.alg !== 'RS256') return false
    if (!verify('RSA-SHA256', Buffer.from(parts[0] + '.' + parts[1]), publicKey, b64u(parts[2]))) return false
    const now = Math.floor(Date.now() / 1000)
    return String(claims.iss) === APP_ID && claims.exp > now && claims.exp - claims.iat <= 600 && claims.iat <= now + 60
  } catch {
    return false
  }
}

const bearer = (req) => (req.headers.authorization || '').replace(/^Bearer /, '')

function treeOf(commit, dir, recursive) {
  const all = files.get(commit)
  if (!all) return null
  const prefix = dir ? dir + '/' : ''
  const seen = new Set()
  const tree = []
  for (const p of [...all.keys()].filter((p) => p.startsWith(prefix)).sort()) {
    const rel = p.slice(prefix.length)
    if (!recursive && rel.includes('/')) {
      const first = rel.split('/')[0]
      if (!seen.has(first)) {
        seen.add(first)
        tree.push({ path: first, mode: '040000', type: 'tree', sha: `tree-${commit}|${(prefix + first).replace(/\//g, ':')}` })
      }
      continue
    }
    const c = all.get(p)
    tree.push({ path: rel, mode: '100644', type: 'blob', sha: blobSha(c), size: Buffer.byteLength(c) })
  }
  return tree
}

const handlers = []
const route = (method, pattern, fn) => {
  const names = []
  const re = new RegExp('^' + pattern.replace(/\{(\w+)(\.\.\.)?\}/g, (_, n, rest) => (names.push(n), rest ? '(.+)' : '([^/]+)')) + '$')
  handlers.push({ method, re, names, fn })
}

// ---- App and OAuth ----------------------------------------------------------------------------
route('POST', '/app-manifests/{code}/conversions', async (req, res, p) => {
  if (p.code !== 'manifest-code' || manifestUsed) return fail(res, 404, 'Not Found')
  manifestUsed = true
  json(res, 201, {
    id: Number(APP_ID), slug: 'supavise-fake', name: 'Supavise fake', client_id: 'Iv1.fakeclient', client_secret: opt['client-secret'],
    webhook_secret: 'fake-webhook-secret', pem: opt.key ? readFileSync(opt.key, 'utf8') : '', html_url: '/apps/supavise-fake', owner: { login: 'acme' },
  })
})
route('POST', '/app/installations/{id}/access_tokens', async (req, res, p) => {
  if (!appJWT(req)) return fail(res, 401, 'A JSON web token could not be decoded')
  if (p.id !== String(INSTALLATION_ID)) return fail(res, 404, 'Not Found')
  json(res, 201, { token: 'inst-token', expires_at: '2099-01-01T00:00:00Z' })
})
route('POST', '/login/oauth/access_token', async (req, res) => {
  const f = new URLSearchParams(await readBody(req))
  if (f.get('client_secret') !== opt['client-secret']) return json(res, 200, { error: 'incorrect_client_credentials' })
  if (f.get('grant_type') === 'refresh_token') {
    if (f.get('refresh_token') !== refreshToken) return json(res, 200, { error: 'bad_refresh_token' })
    refreshed++
    userToken = `ghu_user_r${refreshed}`
    refreshToken = `ghr_user_r${refreshed}`
    userExpired = false
  } else if (f.get('code') !== 'good-code') {
    return json(res, 200, { error: 'bad_verification_code', error_description: 'The code passed is incorrect or expired.' })
  }
  json(res, 200, { access_token: userToken, refresh_token: refreshToken, expires_in: 28800, refresh_token_expires_in: 15811200, token_type: 'bearer' })
})
const needUser = (req, res) => {
  if (bearer(req) !== userToken || userExpired) {
    fail(res, 401, 'Bad credentials')
    return false
  }
  return true
}
route('GET', '/user', async (req, res) => needUser(req, res) && json(res, 200, { id: 7, login: 'octo' }))
route('GET', '/user/installations', async (req, res) =>
  needUser(req, res) && json(res, 200, { total_count: 1, installations: [{ id: INSTALLATION_ID, app_id: Number(APP_ID) }] }))
route('GET', '/user/installations/{id}/repositories', async (req, res) =>
  needUser(req, res) && json(res, 200, { total_count: 1, repositories: [{ id: REPO_ID, full_name: REPO, default_branch: 'main', archived: false }] }))
route('DELETE', '/applications/{client}/token', async (req, res) => {
  if (!(req.headers.authorization || '').startsWith('Basic ')) return fail(res, 401, 'Requires authentication')
  res.writeHead(204)
  res.end()
})

route('GET', '/login/oauth/authorize', async (req, res) => {
  const q = new URL(req.url, 'http://x').searchParams
  if (!opt.callback) return json(res, 200, { client_id: q.get('client_id'), state: q.get('state') })
  const to = new URL(opt.callback)
  to.searchParams.set('code', 'good-code')
  to.searchParams.set('state', q.get('state') || '')
  res.writeHead(302, { location: to.toString() })
  res.end()
})
route('GET', '/apps/{slug}/installations/new', async (req, res) => {
  if (!opt.callback) return json(res, 200, { app: 'installation page' })
  const to = new URL(opt.callback)
  to.searchParams.set('installation_id', String(INSTALLATION_ID))
  to.searchParams.set('setup_action', 'install')
  res.writeHead(302, { location: to.toString() })
  res.end()
})

// ---- the repository ---------------------------------------------------------------------------
const repoCall = (fn) => async (req, res, p) => {
  if (bearer(req) !== 'inst-token') return fail(res, 401, 'Bad credentials')
  if (`${p.owner}/${p.repo}` !== REPO) return fail(res, 404, 'Not Found')
  return fn(req, res, p)
}
route('GET', '/repos/{owner}/{repo}/git/commits/{sha}', repoCall(async (req, res, p) => {
  if (!files.has(p.sha)) return fail(res, 404, 'Not Found')
  json(res, 200, { sha: p.sha, tree: { sha: `tree-${p.sha}|` } })
}))
route('GET', '/repos/{owner}/{repo}/git/trees/{sha}', repoCall(async (req, res, p) => {
  const q = new URL(req.url, 'http://x').searchParams
  let commit = ''
  let dir = ''
  const named = decodeURIComponent(p.sha)
  if (named.startsWith('tree-')) {
    const rest = named.slice(5)
    commit = rest.slice(0, rest.indexOf('|'))
    dir = rest.slice(rest.indexOf('|') + 1).replace(/:/g, '/')
  }
  const tree = treeOf(commit, dir, q.get('recursive') === '1')
  if (!tree) return fail(res, 404, 'Not Found')
  json(res, 200, { sha: p.sha, tree, truncated: false })
}))
route('GET', '/repos/{owner}/{repo}/git/blobs/{sha}', repoCall(async (req, res, p) => {
  for (const all of files.values()) {
    for (const c of all.values()) {
      if (blobSha(c) === p.sha) {
        res.writeHead(200, { 'content-type': 'application/vnd.github.raw+json' })
        return res.end(c)
      }
    }
  }
  fail(res, 404, 'Not Found')
}))
route('GET', '/repos/{owner}/{repo}/contents/{path...}', repoCall(async (req, res, p) => {
  const ref = new URL(req.url, 'http://x').searchParams.get('ref') || 'head-main'
  const c = files.get(ref)?.get(decodeURIComponent(p.path))
  if (c === undefined) return fail(res, 404, 'Not Found')
  json(res, 200, { type: 'file', sha: blobSha(c), size: Buffer.byteLength(c), encoding: 'none', content: '' })
}))
route('GET', '/repos/{owner}/{repo}/pulls/{n}/files', repoCall(async (req, res, p) => {
  json(res, 200, (prFiles.get(Number(p.n)) || []).map((filename) => ({ filename, status: 'modified' })))
}))
route('GET', '/repos/{owner}/{repo}/branches', repoCall(async (req, res) => json(res, 200, [{ name: 'main' }, { name: 'feature' }])))
route('GET', '/repos/{owner}/{repo}/branches/{b...}', repoCall(async (req, res, p) => {
  const b = decodeURIComponent(p.b)
  if (b !== 'main' && b !== 'feature') return fail(res, 404, 'Branch not found')
  json(res, 200, { name: b })
}))
route('GET', '/repos/{owner}/{repo}/commits/{b...}', repoCall(async (req, res, p) => {
  const sha = 'head-' + decodeURIComponent(p.b)
  if (!files.has(sha)) return fail(res, 422, 'No commit found for SHA: ' + p.b)
  json(res, 200, { sha })
}))
route('POST', '/repos/{owner}/{repo}/check-runs', repoCall(async (req, res) => {
  const b = JSON.parse(await readBody(req))
  const id = ++nextId
  checks.set(id, { id, name: b.name, head_sha: b.head_sha, status: b.status, conclusion: null, output: b.output || {}, updates: 0 })
  json(res, 201, { id })
}))
route('PATCH', '/repos/{owner}/{repo}/check-runs/{id}', repoCall(async (req, res, p) => {
  const c = checks.get(Number(p.id))
  if (!c) return fail(res, 404, 'Not Found')
  const b = JSON.parse(await readBody(req))
  Object.assign(c, { status: b.status ?? c.status, conclusion: b.conclusion ?? c.conclusion, output: b.output ?? c.output })
  c.updates++
  json(res, 200, { id: c.id })
}))
route('GET', '/repos/{owner}/{repo}/issues/{n}/comments', repoCall(async (req, res, p) => {
  json(res, 200, [...comments.values()].filter((c) => c.pr === Number(p.n)).map((c) => ({ id: c.id, body: c.body })))
}))
route('POST', '/repos/{owner}/{repo}/issues/{n}/comments', repoCall(async (req, res, p) => {
  const b = JSON.parse(await readBody(req))
  const id = ++nextId
  comments.set(id, { id, pr: Number(p.n), body: b.body, updates: 0 })
  json(res, 201, { id })
}))
route('PATCH', '/repos/{owner}/{repo}/issues/comments/{id}', repoCall(async (req, res, p) => {
  const c = comments.get(Number(p.id))
  if (!c) return fail(res, 404, 'Not Found')
  c.body = JSON.parse(await readBody(req)).body
  c.updates++
  json(res, 200, { id: c.id })
}))

// ---- control ----------------------------------------------------------------------------------
route('PUT', '/__fake/commits/{sha}', async (req, res, p) => {
  const b = JSON.parse(await readBody(req))
  files.set(p.sha, new Map(Object.entries(b.files)))
  json(res, 200, { ok: true })
})
route('PUT', '/__fake/pulls/{n}/files', async (req, res, p) => {
  prFiles.set(Number(p.n), JSON.parse(await readBody(req)))
  json(res, 200, { ok: true })
})
route('GET', '/__fake/state', async (req, res) => {
  json(res, 200, { checks: [...checks.values()], comments: [...comments.values()], requests, refreshed })
})
route('POST', '/__fake/expire-user-token', async (req, res) => {
  userExpired = true
  json(res, 200, { ok: true })
})

const server = createServer(async (req, res) => {
  const path = new URL(req.url, 'http://x').pathname
  if (!path.startsWith('/__fake/')) requests.push(`${req.method} ${path}`)
  for (const h of handlers) {
    if (h.method !== req.method) continue
    const m = h.re.exec(path)
    if (!m) continue
    const params = {}
    h.names.forEach((n, i) => (params[n] = m[i + 1]))
    try {
      return await h.fn(req, res, params)
    } catch (e) {
      console.error('fake-github:', req.method, path, e)
      return fail(res, 500, String(e))
    }
  }
  fail(res, 404, 'Not Found')
})

server.listen(Number(opt.port), '127.0.0.1', () => {
  const url = `http://127.0.0.1:${server.address().port}`
  console.error(`fake-github: listening on ${url}`)
  if (opt.file) writeFileSync(opt.file, JSON.stringify({ url, repo_id: REPO_ID, installation_id: INSTALLATION_ID, repo: REPO }))
})
