// jsbridge worker: a newline-delimited JSON request/response server.
//
// TRANSPORT. Frames travel on file descriptor 3, NOT stdout. The official
// bundle warns why (packages/bundle/acp-app/README.md: "User plugins can
// violate stdout purity ... it cannot contain an arbitrary inserted plugin"):
// a plugin's stray `console.log` would inject a non-protocol line into a
// stdout-carried stream. stdout and stderr stay with the plugin for logging.
//
// SANDBOX. The process runs under Node's permission model (`--permission` with
// no `--allow-fs-*`/`--allow-child-process`/`--allow-net`), which blocks file
// access, subprocesses, and network at the runtime level — verified to return
// ERR_ACCESS_DENIED. `with (ctx)` alone is NOT a boundary: it only shadows
// unqualified names, leaving `globalThis.process` reachable.
//
// EVALUATION. The semantics are copied from vendor/loader/src/config/utils.ts:
//
//	export const evaluate = new Function('ctx', 'expr', `
//	  with (ctx) { return eval(expr) }
//	`)
'use strict'

const readline = require('node:readline')
const fs = require('node:fs')

const PROTOCOL_FD = 3
const MAX_EXPR_BYTES = 1 << 20
// One frame must not exceed this; the Go side enforces the same bound, so a
// runaway result is refused on both ends instead of exhausting memory.
const MAX_FRAME_BYTES = 8 << 20

// The official evaluator, reproduced verbatim in shape. `eval` runs in the
// `with` scope, so the scope's own properties are visible unqualified.
const evaluate = new Function('ctx', 'expr', `
  with (ctx) {
    return eval(expr)
  }
`)

function buildScope(spec) {
  spec = spec || {}
  const services = spec.services || {}
  const home = spec.home || ''

  // The scope object doubles as `ctx`. Services are spread as PROPERTIES as
  // well as exposed through `get`, because the shipped bundles use both forms:
  // `ctx.get('profileContext')` (base) and `ctx.webStartup.host` (web-app),
  // `ctx.headlessStartup.task` (headless). In the official cordis context the
  // two are equivalent; here they must both resolve.
  const scope = {}
  for (const [name, value] of Object.entries(services)) {
    scope[name] = value
  }
  scope.get = (name) =>
    Object.prototype.hasOwnProperty.call(services, name) ? services[name] : undefined
  scope.ctx = scope

  // Path helpers the official loader injects.
  const helpers = new Set(spec.helpers || [])
  if (helpers.has('dshHomePath')) {
    scope.dshHomePath = (...parts) => [home, ...parts].filter(Boolean).join('/')
  }

  // A restricted `process` facade. This is defence in depth: `--permission`
  // already blocks fs/net/child_process at the runtime level, and the facade
  // keeps a plugin from reaching them through the global at all.
  const env = { ...(spec.env || {}) }
  const platform = spec.platform || process.platform
  const cwd = spec.cwd || process.cwd()
  scope.process = {
    env,
    cwd: () => cwd,
    platform,
    getBuiltinModule: (name) => {
      throw new Error(
        `jsbridge: process.getBuiltinModule(${JSON.stringify(name)}) is not available in this host`,
      )
    },
  }

  return scope
}

function runEval(req) {
  const expr = req.expr || ''
  if (Buffer.byteLength(expr) > MAX_EXPR_BYTES) {
    throw new Error('jsbridge: expression exceeds the size limit')
  }
  const scope = buildScope(req.scope)
  const value = evaluate(scope, expr)
  return value === undefined ? null : value
}

async function runImport(req) {
  const specifier = req.module || ''
  if (!specifier) throw new Error('jsbridge: import requires a module specifier')
  const mod = await import(specifier)
  // Only the export NAMES cross the boundary; the values are live JS objects
  // that cannot be serialized meaningfully.
  return { exports: Object.keys(mod) }
}

async function handle(req) {
  switch (req.method) {
    case 'ping':
      return 'pong'
    case 'eval':
      return runEval(req)
    case 'import':
      return await runImport(req)
    default:
      throw new Error(`jsbridge: unknown method ${JSON.stringify(req.method)}`)
  }
}

// The protocol stream is fd 3. If it is missing the worker cannot serve, so it
// exits loudly rather than writing protocol frames onto stdout.
let protocol
try {
  fs.fstatSync(PROTOCOL_FD)
  protocol = fs.createWriteStream(null, { fd: PROTOCOL_FD, autoClose: false })
} catch {
  process.stderr.write('jsbridge: protocol fd 3 is not available\n')
  process.exit(2)
}

function send(obj) {
  let line
  try {
    line = JSON.stringify(obj)
  } catch (e) {
    line = JSON.stringify({ id: 0, ok: false, error: `jsbridge: unencodable result: ${e.message}` })
  }
  if (Buffer.byteLength(line) > MAX_FRAME_BYTES) {
    line = JSON.stringify({
      id: 0,
      ok: false,
      error: `jsbridge: response exceeds ${MAX_FRAME_BYTES} bytes`,
    })
  }
  protocol.write(line + '\n')
}

const rl = readline.createInterface({ input: process.stdin, terminal: false })
let chain = Promise.resolve()

rl.on('line', (line) => {
  if (!line.trim()) return
  // Requests are handled strictly in order: one Node process is
  // single-threaded, and the host never has two calls in flight per worker.
  chain = chain.then(async () => {
    let req
    try {
      req = JSON.parse(line)
    } catch (e) {
      // A malformed line is DROPPED, matching the official transport rule
      // ("malformed lines are ignored") — but a request id we cannot read
      // means the host is waiting, so answer with a null id.
      send({ id: 0, ok: false, error: `jsbridge: malformed request: ${e.message}` })
      return
    }
    try {
      const value = await handle(req)
      send({ id: req.id, ok: true, value })
    } catch (e) {
      send({
        id: req.id,
        ok: false,
        error: String(e && e.message ? e.message : e),
        stack: e && e.stack ? String(e.stack) : '',
      })
    }
  })
})

rl.on('close', () => process.exit(0))
