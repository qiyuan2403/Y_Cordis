package jsbridge

import "runtime"

// hostPlatform reports the Node-style platform name for the running host, used
// as the default for `process.platform` when a scope does not set one.
func hostPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return runtime.GOOS
	}
}
