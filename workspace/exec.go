package workspace

import (
	"errors"
	"os/exec"
	"runtime"
	"syscall"
)

// runProbe executes the probe script, which is what distinguishes a directory
// mounted noexec (Android shared storage) from one that is not.
//
// The distinction is the ERRNO, not the exit status: a successful exec whose
// program exits non-zero proves the directory IS executable, while EACCES or
// EPERM proves it is not. Treating any error as "not executable" would
// misreport a perfectly executable directory whose probe script failed.
func runProbe(path string) error {
	if runtime.GOOS == "windows" {
		// Windows has no execute bit: a created file is runnable if its
		// extension is executable. The probe file has none, so executability
		// is reported by the ability to create the file at all.
		return nil
	}
	err := exec.Command(path).Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// The program ran; its exit status is irrelevant to executability.
		return nil
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EACCES, syscall.EPERM:
			return errors.New("execute permission denied (noexec mount or mode)")
		}
	}
	return err
}
