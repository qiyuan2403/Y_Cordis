package jsbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Isolated is a dedicated worker for ONE untrusted third-party plugin.
//
// Why not the pool: a pooled worker is discarded after any timeout or fault, so
// plugin state could not survive across calls; and a plugin instance owns
// mutable context state, so two concurrent calls into it are unsafe. An
// Isolated instance therefore owns exactly one process, serializes its calls,
// and rebuilds the process when it dies.
//
// This is the path the design doc's "Isolated" row requires for external npm
// plugins; Pool is for trusted, stateless work (`!!js` config expressions).
type Isolated struct {
	cfg Config

	mu     sync.Mutex
	w      *worker
	closed bool
}

// NewIsolated prepares an isolated worker. The process starts on first use.
func NewIsolated(cfg Config) (*Isolated, error) {
	cfg = cfg.withDefaults()
	if cfg.WorkerScript == "" {
		script, err := writeEmbeddedWorker(cfg)
		if err != nil {
			return nil, err
		}
		cfg.WorkerScript = script
	}
	return &Isolated{cfg: cfg}, nil
}

// Call runs one request against the plugin's process, starting or restarting it
// as needed.
func (i *Isolated) Call(ctx context.Context, req Request) (*Response, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil, errors.New("jsbridge: isolated worker is closed")
	}
	if i.w == nil || !i.w.alive() {
		w, err := i.start()
		if err != nil {
			return nil, err
		}
		i.w = w
	}
	timeout := i.cfg.CallTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d < timeout {
			timeout = d
		}
	}
	resp, err := i.w.roundTrip(req, timeout, i.cfg.MaxFrameBytes)
	if err != nil {
		// The plugin process is in an unknown state: tear it down so the next
		// call starts clean rather than inheriting corruption.
		i.w.kill()
		i.w = nil
		return nil, err
	}
	return resp, nil
}

// Import loads a module in this instance's own process and returns its export
// names.
//
// Module loading needs filesystem read access, which is why it lives HERE and
// not on Pool. The eval pool runs untrusted `!!js` expressions and must hold
// zero fs permission; granting it read access to load a module would hand every
// expression the same privilege, collapsing the sandbox to the widest
// requirement in the process.
//
// An Isolated instance owns one process, so its grant covers exactly the plugin
// directories the caller named — nothing else.
func (i *Isolated) Import(ctx context.Context, specifier string) ([]string, error) {
	resp, err := i.Call(ctx, Request{Method: MethodImport, Module: specifier})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, &WorkerError{Message: resp.Error, Stack: resp.Stack}
	}
	return exportNames(resp.Value)
}

// NewPluginHost prepares an isolated worker granted read access to exactly the
// given plugin directories.
//
// This is the supported way to load a third-party plugin: one process per
// plugin, fs read scoped to that plugin's own directory, no subprocesses, no
// network. The grant is per-path rather than `*` because a plugin has no
// business reading anything it was not handed.
func NewPluginHost(pluginDirs []string, cfg Config) (*Isolated, error) {
	cfg.AllowFsReadPaths = append(append([]string{}, cfg.AllowFsReadPaths...), pluginDirs...)
	// A plugin host never needs these; refusing them explicitly documents the
	// boundary even if a caller sets them by mistake.
	cfg.AllowChildProcess = false
	cfg.AllowNet = false
	return NewIsolated(cfg)
}

// Close tears down the process. Safe to call more than once.
func (i *Isolated) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	if i.w != nil {
		i.w.kill()
		i.w = nil
	}
}

// start launches the dedicated process.
func (i *Isolated) start() (*worker, error) {
	w, err := startWorker(i.cfg)
	if err != nil {
		return nil, fmt.Errorf("jsbridge: isolated start: %w", err)
	}
	return w, nil
}
