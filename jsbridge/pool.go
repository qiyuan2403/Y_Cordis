package jsbridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qiyuan2403/Y_Cordis/workspace"
)

// ErrWorkerDead reports that a worker process is gone. The pool discards such a
// worker and builds a fresh one on the next acquisition.
var ErrWorkerDead = errors.New("jsbridge: worker process is dead")

// ErrFrameTooLarge reports a protocol frame above the size bound. The worker is
// discarded rather than resynchronized: the remainder of an oversized frame
// cannot be skipped reliably.
var ErrFrameTooLarge = errors.New("jsbridge: protocol frame exceeds the size limit")

// errPoolClosed reports a call against a closed pool.
var errPoolClosed = errors.New("jsbridge: pool is closed")

// Config configures a Pool.
type Config struct {
	// Node is the node executable. Empty means "node" from PATH.
	Node string
	// WorkerScript is the path to worker.cjs. Empty materializes the embedded
	// copy.
	WorkerScript string
	// Size is the maximum number of concurrent workers. Zero means
	// min(GOMAXPROCS, 4). Workers are started LAZILY up to this bound, so an
	// idle pool costs no processes.
	Size int
	// CallTimeout bounds ONE call. A call that exceeds it kills the worker: a
	// single-threaded runtime cannot be interrupted cooperatively.
	CallTimeout time.Duration
	// MaxOldSpaceMB caps each worker's heap. Zero uses 256.
	MaxOldSpaceMB int
	// MaxFrameBytes bounds one protocol frame. Zero uses 8 MiB.
	MaxFrameBytes int
	// Env is the environment handed to workers. Nil means an empty environment
	// plus PATH — host credentials are NEVER inherited by default.
	Env map[string]string
	// Dir is the base directory for per-worker temporary working directories.
	// Empty uses the system temp dir.
	Dir string
	// RequireRoot is a directory whose node_modules resolves `import()`
	// specifiers. When set, the worker script is placed under it, so module
	// resolution walks up from the script to RequireRoot/node_modules — the
	// path an npm profile installs plugins into. Empty leaves resolution at
	// the ExecRoot/temp default.
	RequireRoot string
	// ExecRoot is a directory that can hold the worker SCRIPT and must be
	// executable. Empty probes the temp dir and falls back to it only when the
	// probe passes.
	//
	// This exists because the script is executed: on Android the shared storage
	// is mounted noexec, so a script written there fails with EACCES. The
	// workspace package resolves which root is executable; this field is how
	// that answer reaches the bridge.
	ExecRoot string
	// AllowFsRead, AllowChildProcess, and AllowNet relax the Node permission
	// model. All default to false: fs, subprocesses, and network are blocked at
	// the runtime level.
	AllowFsRead       bool
	AllowChildProcess bool
	AllowNet          bool
	// AllowFsReadPaths grants read access to specific paths WITHOUT opening the
	// whole filesystem. Loading a module from disk inherently needs read
	// access, so plugin hosts grant it for the plugin's own directory rather
	// than passing AllowFsRead, which would allow reading every file the
	// process can reach — including credentials.
	AllowFsReadPaths []string
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.Node == "" {
		out.Node = "node"
	}
	if out.Size <= 0 {
		out.Size = defaultPoolSize()
	}
	if out.CallTimeout <= 0 {
		out.CallTimeout = 30 * time.Second
	}
	if out.MaxOldSpaceMB <= 0 {
		out.MaxOldSpaceMB = 256
	}
	if out.MaxFrameBytes <= 0 {
		out.MaxFrameBytes = 8 << 20
	}
	return out
}

// Pool is a bounded set of reusable Node worker processes.
//
// CONCURRENCY MODEL. `sem` is the single blocking primitive: it holds exactly
// `Size` tokens, and a caller must hold a token for the whole lifetime of its
// worker loan. Every path that ends a loan — normal release, a faulted worker,
// a spawn failure — returns its token, so a waiter can never be stranded by a
// worker that died. An earlier design gated on worker count and woke nobody on
// discard, which deadlocked a saturated pool the moment a worker timed out.
//
// Workers are created on demand and capped at Size. A worker handles one call
// at a time. A worker that times out, crashes, or returns a malformed frame is
// killed and replaced rather than reused, because its state after such an event
// is unknown.
type Pool struct {
	cfg Config

	sem  chan struct{}
	idle chan *worker

	// live counts worker processes currently owned by the pool, for
	// diagnostics and leak assertions.
	live atomic.Int64
	mu   sync.Mutex
	// workers tracks every live worker so Close can reap them all.
	workers map[*worker]struct{}

	closed atomic.Bool
	done   chan struct{}
	seq    atomic.Uint64
}

// NewPool creates an empty pool. No worker process is started until the first
// call, so an idle pool costs nothing.
func NewPool(cfg Config) (*Pool, error) {
	cfg = cfg.withDefaults()
	if cfg.WorkerScript == "" {
		script, err := writeEmbeddedWorker(cfg)
		if err != nil {
			return nil, err
		}
		cfg.WorkerScript = script
	}
	p := &Pool{
		cfg:     cfg,
		sem:     make(chan struct{}, cfg.Size),
		idle:    make(chan *worker, cfg.Size),
		workers: map[*worker]struct{}{},
		done:    make(chan struct{}),
	}
	for range cfg.Size {
		p.sem <- struct{}{}
	}
	return p, nil
}

// Close stops every worker and removes their temporary directories. It is safe
// to call more than once.
func (p *Pool) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	close(p.done)
	p.mu.Lock()
	workers := make([]*worker, 0, len(p.workers))
	for w := range p.workers {
		workers = append(workers, w)
	}
	p.mu.Unlock()
	for _, w := range workers {
		w.kill()
	}
}

// Eval evaluates one `!!js` expression and returns its JSON value.
func (p *Pool) Eval(ctx context.Context, expr string, scope *Scope) (any, error) {
	return p.call(ctx, Request{Method: MethodEval, Expr: expr, Scope: scope})
}

func exportNames(v any) ([]string, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("jsbridge: import returned %T, want object", v)
	}
	raw, ok := obj["exports"].([]any)
	if !ok {
		return nil, fmt.Errorf("jsbridge: import result missing exports")
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func (p *Pool) call(ctx context.Context, req Request) (any, error) {
	w, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}

	req.ID = p.seq.Add(1)
	timeout := p.cfg.CallTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d < timeout {
			timeout = d
		}
	}

	resp, err := w.roundTrip(req, timeout, p.cfg.MaxFrameBytes)
	if err != nil {
		// The worker's state is unknown after a timeout or a transport fault:
		// kill it and return its token so a waiter can proceed.
		p.discard(w)
		return nil, err
	}
	p.release(w)

	if !resp.OK {
		return nil, &WorkerError{Message: resp.Error, Stack: resp.Stack}
	}
	return resp.Value, nil
}

// WorkerError is an error raised inside the worker's JavaScript.
type WorkerError struct {
	Message string
	Stack   string
}

func (e *WorkerError) Error() string { return e.Message }

// acquire borrows a worker, holding one concurrency token for the whole loan.
func (p *Pool) acquire(ctx context.Context) (*worker, error) {
	if p.closed.Load() {
		return nil, errPoolClosed
	}
	// Take a token first: this is the only place a caller blocks on capacity.
	select {
	case <-p.sem:
	case <-p.done:
		return nil, errPoolClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if p.closed.Load() {
		p.sem <- struct{}{}
		return nil, errPoolClosed
	}

	// Reuse an idle worker, discarding any that died while parked. The token is
	// retained throughout: the caller still owes a loan.
	for {
		select {
		case w := <-p.idle:
			if w.alive() {
				return w, nil
			}
			p.dropDead(w)
			continue
		default:
		}
		w, err := p.spawn()
		if err != nil {
			p.sem <- struct{}{}
			return nil, err
		}
		return w, nil
	}
}

// release parks a healthy worker and returns its token.
func (p *Pool) release(w *worker) {
	if p.closed.Load() || !w.alive() {
		p.discard(w)
		return
	}
	select {
	case p.idle <- w:
		p.sem <- struct{}{}
	default:
		// More workers than the channel can hold: drop this one.
		p.discard(w)
	}
}

// discard kills a faulted worker and returns its token, so a saturated pool
// always wakes a waiter after a fault.
func (p *Pool) discard(w *worker) {
	p.dropDead(w)
	p.sem <- struct{}{}
}

// dropDead removes and kills a worker WITHOUT touching the token: the caller
// still holds the token and will either spawn a replacement or return it.
func (p *Pool) dropDead(w *worker) {
	p.mu.Lock()
	_, present := p.workers[w]
	delete(p.workers, w)
	p.mu.Unlock()
	if present {
		p.live.Add(-1)
	}
	w.kill()
}

// spawn starts one worker process and registers it with the pool.
func (p *Pool) spawn() (*worker, error) {
	w, err := startWorker(p.cfg)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.workers[w] = struct{}{}
	p.mu.Unlock()
	p.live.Add(1)
	return w, nil
}

// startWorker launches one Node process with the given configuration. It is
// shared by Pool (which registers the result) and Isolated (which owns it
// directly).
func startWorker(cfg Config) (*worker, error) {
	dir, err := os.MkdirTemp(cfg.Dir, "jsbridge-")
	if err != nil {
		return nil, fmt.Errorf("jsbridge: temp dir: %w", err)
	}
	args := nodeArgs(cfg)
	cmd := exec.Command(cfg.Node, args...)
	cmd.Dir = dir
	cmd.Env = buildEnv(cfg.Env)

	// stdin carries requests; stdout/stderr are left to the worker for logging
	// (a plugin's stray console.log must never corrupt the protocol stream).
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	protocolRead, protocolWrite, err := os.Pipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	// fd 3 in the child is the write end of the protocol pipe.
	cmd.ExtraFiles = []*os.File{protocolWrite}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("jsbridge: start node: %w", err)
	}
	// The parent keeps only the read end; the child owns the write end.
	protocolWrite.Close()

	w := &worker{
		cmd:      cmd,
		stdin:    stdin,
		protocol: protocolRead,
		reader:   bufio.NewReader(protocolRead),
		dir:      dir,
		done:     make(chan struct{}),
		maxFrame: cfg.MaxFrameBytes,
	}
	go func() { _, _ = io.Copy(io.Discard, stdout) }()
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go func() {
		_ = cmd.Wait()
		close(w.done)
	}()

	return w, nil
}

// nodeArgs builds the node command line.
//
// `--permission` is the load-bearing sandbox: it blocks fs, subprocesses, and
// network at the runtime level unless an explicit `--allow-*` is passed. This
// is what makes the threat table in docs/jsbridge-design.md enforceable —
// `with (ctx)` only shadows unqualified names and leaves `globalThis.process`
// reachable.
func nodeArgs(cfg Config) []string {
	args := []string{
		"--no-warnings",
		fmt.Sprintf("--max-old-space-size=%d", cfg.MaxOldSpaceMB),
		"--permission",
	}
	if cfg.AllowFsRead {
		args = append(args, "--allow-fs-read=*")
	}
	allowed := cfg.AllowFsReadPaths
	if cfg.RequireRoot != "" {
		// The require root exists to be imported from; a read grant over it
		// is the feature, not a leak. Without it --permission blocks the
		// resolution the root was configured for.
		allowed = append(allowed, cfg.RequireRoot)
	}
	for _, path := range allowed {
		args = append(args, "--allow-fs-read="+path)
	}
	if cfg.AllowChildProcess {
		args = append(args, "--allow-child-process")
	}
	if cfg.AllowNet {
		args = append(args, "--allow-net")
	}
	return append(args, cfg.WorkerScript)
}

// buildEnv scrubs the environment: only PATH plus explicitly allowed entries
// reach a worker, so a plugin cannot read host credentials.
func buildEnv(extra map[string]string) []string {
	env := []string{}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// defaultPoolSize bounds concurrency by the machine's parallelism, capped so a
// many-core host does not spawn a swarm of Node processes for config work.
func defaultPoolSize() int {
	if v := os.Getenv("JSBRIDGE_POOL_SIZE"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	n := runtime.GOMAXPROCS(0)
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}

// writeEmbeddedWorker materializes the embedded worker script in a directory
// that can actually execute it.
//
// The script is run by node, so a noexec directory (Android shared storage)
// makes it unusable. The directory is therefore PROBED, and a failing candidate
// falls through to the next rather than producing a script that cannot run.
func writeEmbeddedWorker(cfg Config) (string, error) {
	candidates := []string{}
	if cfg.RequireRoot != "" {
		// First: the require root. The worker script under it makes
		// RequireRoot/node_modules the resolution anchor for `import()`.
		candidates = append(candidates, filepath.Join(cfg.RequireRoot, "jsbridge-scripts"))
	}
	if cfg.ExecRoot != "" {
		candidates = append(candidates, cfg.ExecRoot)
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "jsbridge-scripts"))
	if cfg.Dir != "" {
		candidates = append(candidates, filepath.Join(cfg.Dir, "jsbridge-scripts"))
	}

	var reasons []string
	for _, dir := range candidates {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", dir, err))
			continue
		}
		caps := workspace.Probe(dir)
		if !caps.Usable() || !caps.Executable {
			reasons = append(reasons, fmt.Sprintf("%s: %s", dir, caps.Reason))
			continue
		}
		path := filepath.Join(dir, "worker.cjs")
		if err := os.WriteFile(path, embeddedWorker, 0o600); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", dir, err))
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("jsbridge: no executable directory for the worker script: %s",
		strings.Join(reasons, "; "))
}
