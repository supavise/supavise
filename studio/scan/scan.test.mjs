// Tests of studio/scan-calls.mjs on small trees of its own (no network, no Studio checkout):
//
//   npm ci --prefix studio/scan && node --test studio/scan/scan.test.mjs
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))

function scan(source) {
  const dir = mkdtempSync(join(tmpdir(), 'scan-calls-'))
  try {
    mkdirSync(join(dir, 'apps/studio/data'), { recursive: true })
    mkdirSync(join(dir, 'packages'), { recursive: true })
    writeFileSync(join(dir, 'apps/studio/data/t.ts'), source)
    const out = join(dir, 'calls.json')
    execFileSync(process.execPath, [join(HERE, '..', 'scan-calls.mjs'), '--src', dir, '--out', out], { stdio: 'pipe' })
    return JSON.parse(readFileSync(out, 'utf8'))
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

const HEAD = `import { get } from 'data/fetchers'
declare const c: boolean
const A = c ? '/a1' : c ? '/a2' : c ? '/a3' : c ? '/a4' : '/a5'
const B = c ? '/b1' : c ? '/b2' : c ? '/b3' : c ? '/b4' : '/b5'
const S = c ? '/s1' : '/s2'
`

test('a path with a few alternatives gives every combination', () => {
  const doc = scan(`${HEAD}export const few = () => get(\`/platform\${S}\${S}\`)\n`)
  assert.deepEqual(doc.calls.map((x) => x.path).sort(), ['/platform/s1/s1', '/platform/s1/s2', '/platform/s2/s1', '/platform/s2/s2'])
  assert.equal(doc.unresolved.length, 0)
})

test('a path with more than 16 combinations is unresolved, not cut', () => {
  const doc = scan(`${HEAD}export const many = () => get(\`/platform\${A}\${B}\`)\n`)
  assert.equal(doc.calls.length, 0, 'no combination may be kept when the rest are dropped')
  assert.equal(doc.unresolved.length, 1)
  assert.equal(doc.unresolved[0].line, 6)
})

test('a fetch of such a path is unresolved too', () => {
  const doc = scan(`${HEAD}export const many = () => fetch(\`/platform\${A}\${B}\`)\n`)
  assert.equal(doc.calls.length, 0)
  assert.equal(doc.unresolved.length, 1)
})
