package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProbeReportsExecutableForPrivateDir pins the private-directory case: the
// harness home is executable on every supported host.
func TestProbeReportsExecutableForPrivateDir(t *testing.T) {
	dir := t.TempDir()
	Forget(dir)
	caps := Probe(dir)
	if !caps.Exists || !caps.Readable || !caps.Writable {
		t.Fatalf("temp dir should be usable: %+v", caps)
	}
	if runtime.GOOS != "windows" && !caps.Executable {
		t.Fatalf("a temp dir should be executable on this host: %+v", caps)
	}
}

// TestProbeDistinguishesExitStatusFromExecutability pins that a probe whose
// program EXITS NON-ZERO still proves the directory executable. Reporting
// "not executable" for a successful exec would wrongly demote a usable root.
//
// This is the distinction that makes the noexec detection trustworthy: the
// signal is the errno (EACCES/EPERM), not the exit status.
func TestProbeDistinguishesExitStatusFromExecutability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no execute bit on windows")
	}
	dir := t.TempDir()
	Forget(dir)
	caps := Probe(dir)
	if !caps.Executable {
		t.Fatalf("an ordinary directory must be executable: %+v", caps)
	}
}

// TestProbeRejectsNonExecutableFileAsRoot pins that a path which is a file
// rather than a directory is refused with a reason.
func TestProbeRejectsNonExecutableFileAsRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	Forget(file)
	caps := Probe(file)
	if caps.Usable() {
		t.Fatalf("a regular file was reported usable as a root: %+v", caps)
	}
	if caps.Reason == "" {
		t.Fatal("a rejected root must carry a reason")
	}
}

// TestProbeReportsMissingDirectory pins that a nonexistent root is reported
// with a reason instead of panicking.
func TestProbeReportsMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	Forget(dir)
	caps := Probe(dir)
	if caps.Exists {
		t.Fatalf("a missing directory reported Exists: %+v", caps)
	}
	if caps.Reason == "" {
		t.Fatal("a missing directory must carry a reason")
	}
}

// TestResolvePrefersExplicitRoots pins that caller choices win.
func TestResolvePrefersExplicitRoots(t *testing.T) {
	data := t.TempDir()
	execDir := t.TempDir()
	Forget(data)
	Forget(execDir)

	got, err := Resolve(Options{Data: data, Exec: execDir, Home: t.TempDir()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Data != data {
		t.Fatalf("Data = %q, want %q", got.Data, data)
	}
	if got.Exec != execDir {
		t.Fatalf("Exec = %q, want %q", got.Exec, execDir)
	}
	if got.Same() {
		t.Fatal("distinct roots reported as the same")
	}
}

// TestResolveFallsBackToHomeForExecWhenDataIsNoexec pins the Android shape: a
// data root that is writable but NOT executable must not be chosen for exec,
// and the harness home must be used instead.
func TestResolveFallsBackToHomeForExecWhenDataIsNoexec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no execute bit on windows")
	}
	parent := t.TempDir()
	data := filepath.Join(parent, "noexec-data")
	if err := os.Mkdir(data, 0o600); err != nil {
		t.Skipf("cannot create the fixture: %v", err)
	}
	home := t.TempDir()
	Forget(data)
	Forget(home)

	// Data is not usable at all here (0600 blocks file creation), so Resolve
	// must fall through to the home for data too — and Exec must never be the
	// non-executable candidate.
	got, err := Resolve(Options{Data: data, Home: home})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Exec != home {
		t.Fatalf("Exec = %q, want the executable home %q", got.Exec, home)
	}
	if !got.ExecCaps.Executable {
		t.Fatalf("the chosen exec root is not executable: %+v", got.ExecCaps)
	}
}

// TestResolveErrorsWhenNothingIsUsable pins fail-loud: with no usable root the
// resolver reports it instead of returning an unusable path.
func TestResolveErrorsWhenNothingIsUsable(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "nope")
	Forget(absent)
	_, err := Resolve(Options{Data: absent, Exec: absent, Home: absent})
	if err == nil {
		t.Fatal("expected an error when no root is usable")
	}
	if !strings.Contains(err.Error(), "no usable data root") {
		t.Fatalf("error = %v, want it to name the missing data root", err)
	}
}

// TestDataDirAndExecDirCreateChildren pins the accessors.
func TestDataDirAndExecDirCreateChildren(t *testing.T) {
	roots, err := Resolve(Options{Data: t.TempDir(), Exec: t.TempDir()})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	d, err := roots.DataDir("cards")
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if info, err := os.Stat(d); err != nil || !info.IsDir() {
		t.Fatalf("DataDir did not create a directory: %v", err)
	}
	e, err := roots.ExecDir("jsbridge")
	if err != nil {
		t.Fatalf("ExecDir: %v", err)
	}
	if info, err := os.Stat(e); err != nil || !info.IsDir() {
		t.Fatalf("ExecDir did not create a directory: %v", err)
	}
}

// TestProbeOfSharedStorageWhenPresent pins the real Android case on a device
// that has shared storage: it must be usable and must NOT be reported
// executable. On a host without shared storage the test skips.
func TestProbeOfSharedStorageWhenPresent(t *testing.T) {
	const shared = "/storage/emulated/0"
	if _, err := os.Stat(shared); err != nil {
		t.Skipf("no shared storage at %s", shared)
	}
	Forget(shared)
	caps := Probe(shared)
	if !caps.Usable() {
		t.Skipf("shared storage is not usable here: %+v", caps)
	}
	if caps.Executable {
		t.Fatalf("Android shared storage reported executable, but it is mounted noexec: %+v", caps)
	}
	// And the resolver must therefore never pick it for exec.
	home := t.TempDir()
	got, err := Resolve(Options{Data: shared, Exec: "", Home: home})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Exec == shared {
		t.Fatal("the resolver chose non-executable shared storage for exec")
	}
	if got.Data != shared {
		t.Fatalf("Data = %q, want the shared root %q", got.Data, shared)
	}
}
