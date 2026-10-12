// A stand-in for the node under crawl: one HTTP server that plays the proxy, a toy "Studio" (a sign-in form and a few
// pages) and a toy Management API, so that selftest.mjs can run the real crawl.mjs in a real browser on a developer
// machine and see that it finds what it should: a stub answer, a 5xx, a dead-end text, a console error, a page error, a
// mutation attempt. It knows nothing of the real Studio; what it proves is the driver (the host mapping, the CSP
// rewrite, the mutation guard, the click deny list, the report and the exit code).
import http from 'node:http'

export const REF = 'aaaaaaaaaaaaaaaaaaaa'

const page = (title, body, script = '') =>
  `<!doctype html><html><head><meta charset="utf-8"><title>${title}</title></head><body><main>${body}</main><script>${script}</script></body></html>`

// api(path) is a script snippet: fetch a path of the toy API from the page, with the session token, ignoring the outcome.
const call = (path, init = '{}') =>
  `fetch(API + ${JSON.stringify(path)}, Object.assign({headers: {authorization: 'Bearer t'}}, ${init})).catch(() => {});`

export async function startStandin({ domain }) {
  const calls = []
  const server = http.createServer((req, res) => {
    const host = (req.headers.host ?? '').replace(/:\d+$/, '')
    const url = new URL(req.url, 'http://x')
    const path = url.pathname
    calls.push({ method: req.method, host, path })
    const send = (status, body, headers = {}) => {
      res.writeHead(status, { 'content-type': typeof body === 'string' && body.startsWith('<') ? 'text/html' : 'application/json', ...headers })
      res.end(typeof body === 'string' ? body : JSON.stringify(body))
    }
    const port = server.address().port
    const API = `http://api.${domain}:${port}`
    const cors = { 'access-control-allow-origin': req.headers.origin ?? '*', 'access-control-allow-headers': 'authorization, content-type', 'access-control-allow-methods': 'GET, POST, DELETE, PATCH, OPTIONS' }

    if (host === `api.${domain}`) {
      if (req.method === 'OPTIONS') return send(204, '', cors)
      if (path === '/auth/v1/token') return send(200, { access_token: 'tok', refresh_token: 'r', user: { id: 'u' } }, cors)
      if (path === '/platform/profile') return send(200, { id: 1 }, cors)
      if (path === '/platform/notifications') return send(200, {}, { ...cors, 'x-supavise-stub': 'true' })
      if (path === '/platform/stubbed') return send(200, {}, { ...cors, 'x-supavise-stub': 'true' })
      if (path === '/platform/boom') return send(500, { message: 'boom' }, cors)
      if (path === '/platform/missing') return send(404, { message: 'not found' }, cors)
      return send(200, {}, cors)
    }
    if (host === `studio.${domain}`) {
      // Studio's policy upgrades every sub-resource to https; the crawl must rewrite it
      const csp = { 'content-security-policy': "upgrade-insecure-requests; default-src * 'unsafe-inline' data:" }
      const doc = (title, body, script) => send(200, page(title, body, `const API = ${JSON.stringify(API)};${script ?? ''}`), csp)
      if (path === '/sign-in') {
        return doc('Sign in', '<input placeholder="you@example.com"><input type="password"><button id="go">Sign in</button>', `
          document.getElementById('go').onclick = async () => {
            const r = await fetch(API + '/auth/v1/token?grant_type=password', {method: 'POST', headers: {'content-type': 'application/json'}, body: '{}'});
            localStorage.setItem('supabase.dashboard.auth.token', JSON.stringify(await r.json()));
            location.href = '/organizations';
          };`)
      }
      if (path === '/organizations') return doc('Organizations', '<h1>Organizations</h1><a href="/org/default">default</a>', call('/platform/profile') + call('/platform/notifications'))
      if (path === '/org/default') return doc('Org', '<h1>default</h1><p>crawl-a</p>', call('/platform/profile'))
      if (path === `/project/${REF}`) return doc('Project', '<h1>Project home</h1><p>all good</p>', call('/platform/profile'))
      if (path === `/project/${REF}/tabs`) {
        return doc('Tabs', `<h1>Tabs</h1>
          <button role="tab" id="t1">Overview</button><button role="tab" id="t2">Delete everything</button>
          <button id="safe">Open details</button><button id="danger">Reset project</button>
          <a href="/organizations">Organizations</a>`, `
          document.getElementById('t1').onclick = () => { ${call('/platform/clicked-tab')} };
          document.getElementById('t2').onclick = () => { ${call('/platform/clicked-delete')} };`)
      }
      if (path === `/project/${REF}/stubbed`) return doc('Stubbed', '<h1>Stubbed</h1>', call('/platform/stubbed'))
      if (path === `/project/${REF}/boom`) return doc('Boom', '<h1>Boom</h1>', call('/platform/boom'))
      if (path === `/project/${REF}/missing`) return doc('Missing', '<h1>Missing</h1>', call('/platform/missing'))
      if (path === `/project/${REF}/bad`) return doc('Bad', '<h1>Bad</h1><p>Failed to load the thing</p>')
      if (path === `/project/${REF}/mutate`) return doc('Mutate', '<h1>Mutate</h1>', call(`/platform/projects/${REF}`, "{method: 'DELETE'}") + call('/platform/pg-meta/x/query', "{method: 'POST', body: JSON.stringify({query: 'select 1'})}"))
      // a read the guard does not know (a statement that sets a role first), as a fetch wrapper that logs the failure of a fetch
      if (path === `/project/${REF}/guarded`) {
        return doc('Guarded', '<h1>Guarded</h1>', `fetch(API + '/platform/pg-meta/x/query', {method: 'POST', headers: {authorization: 'Bearer t', 'content-type': 'application/json'}, body: JSON.stringify({query: 'set role postgres;\\nselect 1'})}).catch((e) => console.error(e));`)
      }
      if (path === `/project/${REF}/console`) return doc('Console', '<h1>Console</h1>', "console.error('something broke'); console.error('usercentrics said no');")
      if (path === `/project/${REF}/throws`) return doc('Throws', '<h1>Throws</h1>', "setTimeout(() => { throw new Error('page exploded') }, 100);")
      if (path === `/project/${REF}/external`) {
        return doc('External', '<h1>External</h1><img src="https://cdn.example.invalid/x.png">', "fetch('https://api.posthog.example.invalid/e', {method: 'POST', mode: 'no-cors'}).catch(() => {});")
      }
      return send(404, page('Not found', '<h1>404</h1>'))
    }
    return send(404, { message: 'unknown host' })
  })
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  return { server, port: server.address().port, calls, close: () => new Promise((r) => server.close(r)) }
}
