// Package jsbridge runs a pool of Node.js worker processes so the harness can
// evaluate JavaScript the way the official runtime does — `!!js` config
// expressions and third-party plugin code — without embedding a JS engine in
// the Go binary.
//
// Two properties drive the design:
//
//   - SAFETY. Worker code is untrusted (third-party plugins). A runaway or
//     crashing worker must never take the harness down, so every worker is a
//     separate OS process with a hard per-call timeout, a heap cap, a scrubbed
//     environment, and a private working directory.
//   - THROUGHPUT. Node startup costs tens of milliseconds, so workers are
//     pooled and reused: a call is one newline-delimited JSON round trip.
//     Concurrency comes from the pool size, since each Node process is
//     single-threaded.
package jsbridge

// Request is one call into a worker.
type Request struct {
	ID     uint64 `json:"id"`
	Method string `json:"method"`
	// Expr carries the JavaScript source for Method "eval".
	Expr string `json:"expr,omitempty"`
	// Scope is the evaluation scope: it becomes the `ctx` object visible to the
	// expression, matching the official
	// `new Function('ctx','expr','with (ctx) { return eval(expr) }')`.
	Scope *Scope `json:"scope,omitempty"`
	// Module carries the specifier for Method "import".
	Module string `json:"module,omitempty"`
}

// Scope is the evaluation environment handed to a worker.
//
// It mirrors the globals the official loader exposes: `process.env`,
// `process.cwd()`, `process.platform`, plus the harness-provided helpers.
type Scope struct {
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	Platform string            `json:"platform,omitempty"`
	// Services are the values `ctx.get(name)` resolves. The official loader
	// evaluates against a live cordis context; here the harness projects the
	// handful of services a config expression may read.
	Services map[string]any `json:"services,omitempty"`
	// Helpers are extra functions exposed by name (e.g. `dshHomePath`).
	// A helper is expressed as a method the worker implements, not as Go code:
	// "dshHomePath" → the worker joins the harness home with its arguments.
	Helpers []string `json:"helpers,omitempty"`
	// Home is the harness home directory, used by the path helpers.
	Home string `json:"home,omitempty"`
}

// Response is one worker reply.
type Response struct {
	ID     uint64 `json:"id"`
	OK     bool   `json:"ok"`
	Value  any    `json:"value,omitempty"`
	Error  string `json:"error,omitempty"`
	Stack  string `json:"stack,omitempty"`
	Worker string `json:"worker,omitempty"`
}

// Method names accepted by a worker. An unknown method is refused, so a
// protocol desync cannot silently execute something else.
const (
	MethodEval   = "eval"
	MethodImport = "import"
	MethodPing   = "ping"
)
