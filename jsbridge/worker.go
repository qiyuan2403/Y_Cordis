package jsbridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// worker is one Node process plus its protocol transport.
//
// Calls are serialized by mu: a Node process is single-threaded, so two
// concurrent calls would interleave frames. The pool, not the worker, provides
// concurrency.
type worker struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	protocol io.ReadCloser
	reader   *bufio.Reader
	dir      string
	done     chan struct{}
	maxFrame int

	mu     sync.Mutex
	broken bool
}

// alive reports whether the process is still running.
func (w *worker) alive() bool {
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

// roundTrip sends one request and waits for its reply.
//
// The read runs in a goroutine so the timeout can fire while the worker is
// stuck in a loop: a single-threaded runtime has no cooperative interrupt, so
// the only way out is to abandon the read and kill the process.
func (w *worker) roundTrip(req Request, timeout time.Duration, maxFrame int) (*Response, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken {
		return nil, ErrWorkerDead
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("jsbridge: encode request: %w", err)
	}
	payload = append(payload, '\n')
	if _, err := w.stdin.Write(payload); err != nil {
		w.broken = true
		return nil, fmt.Errorf("%w: write: %v", ErrWorkerDead, err)
	}

	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := readFrame(w.reader, maxFrame)
		ch <- result{line, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			w.broken = true
			switch {
			case errors.Is(r.err, ErrFrameTooLarge):
				return nil, r.err
			case errors.Is(r.err, io.EOF):
				return nil, fmt.Errorf("%w: worker exited", ErrWorkerDead)
			default:
				return nil, fmt.Errorf("%w: read: %v", ErrWorkerDead, r.err)
			}
		}
		var resp Response
		if err := json.Unmarshal(r.line, &resp); err != nil {
			w.broken = true
			return nil, fmt.Errorf("jsbridge: malformed response frame: %w", err)
		}
		if resp.ID != req.ID {
			// A desynced frame means the stream is no longer trustworthy.
			w.broken = true
			return nil, fmt.Errorf("jsbridge: response id %d does not match request id %d", resp.ID, req.ID)
		}
		return &resp, nil
	case <-time.After(timeout):
		w.broken = true
		return nil, fmt.Errorf("jsbridge: call timed out after %s (worker killed)", timeout)
	case <-w.done:
		w.broken = true
		return nil, fmt.Errorf("%w: worker exited during call", ErrWorkerDead)
	}
}

// readFrame reads one newline-terminated frame, refusing anything above limit.
//
// A bare ReadBytes would happily buffer an arbitrarily long line, so a runaway
// result could exhaust the harness. Reading in bounded chunks and failing at
// the limit keeps the damage inside the worker that produced it.
func readFrame(r *bufio.Reader, limit int) ([]byte, error) {
	var buf bytes.Buffer
	for {
		chunk, err := r.ReadSlice('\n')
		if buf.Len()+len(chunk) > limit {
			return nil, ErrFrameTooLarge
		}
		buf.Write(chunk)
		if err == nil {
			return buf.Bytes(), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // partial line, keep reading
		}
		return nil, err
	}
}

// kill terminates the process and removes its working directory. It is safe to
// call concurrently and more than once.
//
// Shutdown escalates EOF → SIGTERM → SIGKILL the way the official client does:
// closing stdin asks a cooperative worker to exit, and a worker that ignores it
// is terminated.
func (w *worker) kill() {
	_ = w.stdin.Close()
	_ = w.protocol.Close()
	if w.cmd.Process != nil {
		select {
		case <-w.done:
		case <-time.After(200 * time.Millisecond):
			_ = w.cmd.Process.Signal(os.Interrupt)
			select {
			case <-w.done:
			case <-time.After(200 * time.Millisecond):
				_ = w.cmd.Process.Kill()
			}
		}
	}
	<-w.done
	os.RemoveAll(w.dir)
}
