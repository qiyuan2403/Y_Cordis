// Package workspace resolves and validates the directories a harness run may
// use, on hosts where different filesystems carry different capabilities.
//
// Android makes the distinction load-bearing. Verified on this device:
//
//	/storage/emulated/0   tmpfs, noexec   readable + writable, NOT executable
//	/data/... or app home ext4            readable + writable + executable
//
// So a workspace has TWO roots, and conflating them breaks in a specific way:
// putting the Node worker script (or any helper binary) on shared storage makes
// it unrunnable, while putting user documents only in the private directory
// makes them invisible to the user's file manager.
//
// Capabilities are PROBED, never inferred from the path or the OS name: the
// same Android device can mount shared storage differently per app (scoped
// storage, adopted storage, a real SD card), and a desktop can mount a
// noexec /home.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Capabilities describes what a directory actually supports.
type Capabilities struct {
	// Exists reports whether the directory is present.
	Exists bool
	// Readable reports whether a file in it can be opened for reading.
	Readable bool
	// Writable reports whether a file can be created in it.
	Writable bool
	// Executable reports whether a file created in it can be executed. This is
	// false on Android's shared storage (mounted noexec) and on a noexec
	// desktop mount.
	Executable bool
	// Reason carries the probe failure for diagnostics when a capability is
	// missing.
	Reason string
}

// Usable reports whether the directory can hold harness data at all.
func (c Capabilities) Usable() bool { return c.Exists && c.Readable && c.Writable }

// Roots is a resolved pair of directories.
//
// Data holds documents the user should see (character cards, exported images,
// session archives); it is often shared storage and need not be executable.
// Exec holds anything that must be run or mapped (the Node worker script,
// helper binaries, temp files feeding exec); it must be executable and is
// normally the private directory.
//
// When the data root is executable the two may be the same directory; Roots
// keeps them separate anyway so callers never have to ask which case they are
// in.
type Roots struct {
	Data string
	Exec string

	DataCaps Capabilities
	ExecCaps Capabilities
}

// Same reports whether data and exec share one directory.
func (r Roots) Same() bool { return r.Data == r.Exec }

// Options configures resolution.
type Options struct {
	// Data is the requested data root. Empty means "pick a default".
	Data string
	// Exec is the requested exec root. Empty means "pick a default".
	Exec string
	// Home is the harness home directory (private, always executable). It is
	// the fallback for Exec and the parent of the default Data root.
	Home string
	// SharedStorage, when non-empty, is a candidate shared-storage root (for
	// example /storage/emulated/0). It is probed, not assumed usable.
	SharedStorage string
}

// Resolve picks and validates the data and exec roots.
//
// Resolution order for Data: the explicit option, then the harness home, then
// the shared-storage candidate. For Exec: the explicit option, then the data
// root IF it is executable, then the harness home.
//
// A root that cannot hold data at all is an error: silently continuing would
// fail later, far from the cause.
func Resolve(opts Options) (Roots, error) {
	var out Roots

	execCandidates := nonEmpty(opts.Exec, opts.Home)
	dataCandidates := nonEmpty(opts.Data, opts.Home, opts.SharedStorage)

	data, dataCaps, err := firstUsable(dataCandidates)
	if err != nil {
		return out, fmt.Errorf("workspace: no usable data root: %w", err)
	}
	out.Data = data
	out.DataCaps = dataCaps

	// Exec must be executable; a usable-but-noexec data root is not enough.
	exec, execCaps, err := firstExecutable(execCandidates)
	if err != nil {
		// Fall back to the data root only when it is itself executable.
		if dataCaps.Executable {
			out.Exec = data
			out.ExecCaps = dataCaps
			return out, nil
		}
		return out, fmt.Errorf("workspace: no executable root: %w", err)
	}
	out.Exec = exec
	out.ExecCaps = execCaps
	return out, nil
}

func nonEmpty(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstUsable(candidates []string) (string, Capabilities, error) {
	var reasons []string
	for _, c := range candidates {
		caps := Probe(c)
		if caps.Usable() {
			return c, caps, nil
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", c, caps.Reason))
	}
	return "", Capabilities{}, errors.New(strings.Join(reasons, "; "))
}

func firstExecutable(candidates []string) (string, Capabilities, error) {
	var reasons []string
	for _, c := range candidates {
		caps := Probe(c)
		if caps.Usable() && caps.Executable {
			return c, caps, nil
		}
		reasons = append(reasons, fmt.Sprintf("%s: %s", c, caps.Reason))
	}
	return "", Capabilities{}, errors.New(strings.Join(reasons, "; "))
}

// probeCache memoizes probe results per directory: a probe writes and executes
// a file, which is not something to repeat on every call.
var probeCache sync.Map // string -> Capabilities

// Probe reports what a directory actually supports by exercising it.
//
// Every claim is measured, not assumed: existence by stat, writability by
// creating a file, readability by opening it, and executability by chmod'ing it
// and running it. That last one is what distinguishes Android's shared storage
// (noexec) from its private directory.
func Probe(dir string) Capabilities {
	if v, ok := probeCache.Load(dir); ok {
		return v.(Capabilities)
	}
	caps := probe(dir)
	probeCache.Store(dir, caps)
	return caps
}

// Forget drops a cached probe, for tests that change a directory's mode.
func Forget(dir string) { probeCache.Delete(dir) }

func probe(dir string) Capabilities {
	caps := Capabilities{}
	info, err := os.Stat(dir)
	if err != nil {
		caps.Reason = "does not exist"
		return caps
	}
	if !info.IsDir() {
		caps.Reason = "not a directory"
		return caps
	}
	caps.Exists = true

	// Readability: can a file in it be opened?
	entries, err := os.ReadDir(dir)
	if err != nil {
		caps.Reason = "not readable: " + err.Error()
		return caps
	}
	caps.Readable = true
	_ = entries

	// Writability and executability share one temp file.
	f, err := os.CreateTemp(dir, ".dshcard-probe-*")
	if err != nil {
		caps.Reason = "not writable: " + err.Error()
		return caps
	}
	name := f.Name()
	defer func() {
		_ = os.Remove(name)
	}()
	if _, err := f.WriteString("#!/bin/sh\nexit 0\n"); err != nil {
		_ = f.Close()
		caps.Reason = "write failed: " + err.Error()
		return caps
	}
	if err := f.Close(); err != nil {
		caps.Reason = "close failed: " + err.Error()
		return caps
	}
	caps.Writable = true

	if err := os.Chmod(name, 0o700); err != nil {
		caps.Reason = "chmod failed: " + err.Error()
		return caps
	}
	if err := runProbe(name); err != nil {
		caps.Reason = "not executable: " + err.Error()
		return caps
	}
	caps.Executable = true
	return caps
}

// ExecDir returns the directory a run must use for anything executable,
// creating it under the exec root when needed.
func (r Roots) ExecDir(name string) (string, error) {
	dir := filepath.Join(r.Exec, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("workspace: exec dir %s: %w", dir, err)
	}
	return dir, nil
}

// DataDir returns a directory under the data root, creating it when needed.
func (r Roots) DataDir(name string) (string, error) {
	dir := filepath.Join(r.Data, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("workspace: data dir %s: %w", dir, err)
	}
	return dir, nil
}

// Describe renders the resolved roots and their measured capabilities.
func (r Roots) Describe() string {
	return fmt.Sprintf(
		"data=%s (read=%t write=%t exec=%t) exec=%s (read=%t write=%t exec=%t) same=%t",
		r.Data, r.DataCaps.Readable, r.DataCaps.Writable, r.DataCaps.Executable,
		r.Exec, r.ExecCaps.Readable, r.ExecCaps.Writable, r.ExecCaps.Executable,
		r.Same(),
	)
}
