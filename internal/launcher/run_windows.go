//go:build windows

package launcher

import "syscall"

// runAsLauncher: Windows has no uid to run an instance as, so it runs as its
// image's USER. Whether a file it writes in a mount is then the caller's
// (LAUNCHER-CONTROL.md R4) is unmeasured on Windows.
func runAsLauncher() []string {
	return nil
}

// ownProcessGroup: the runtime's client of a run in a process group of its
// own, so that a console's Ctrl+C reaches the launcher alone, which terms
// the instance (R7).
func ownProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
