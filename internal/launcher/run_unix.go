//go:build unix

package launcher

import (
	"os"
	"strconv"
	"syscall"
)

// runAsLauncher: the `run` arguments that make a run's instance the
// launcher's own user, in place of its image's USER, so that a file it
// writes in a mount is the caller's (LAUNCHER-CONTROL.md R4).
func runAsLauncher() []string {
	return []string{"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())}
}

// ownProcessGroup: the runtime's client of a run in a process group of its
// own, so that a terminal's Ctrl+C reaches the launcher alone, which terms
// the instance (R7). A client signalled itself can end and leave its
// instance running.
func ownProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
