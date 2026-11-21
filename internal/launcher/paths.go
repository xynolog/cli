package launcher

// paths.go — where the launcher keeps what it keeps, on each system. Each home
// is a pure function of what the system says of the person's directories, so
// one test runs every system's rule whichever machine it runs on; the
// functions that read the environment call them.
//
// The state home is the one another program finds too (the Rust SDK reads
// tokens.json there), so its rule is the spec's, and the SDK's
// (xynolog.StateDir, which StateDir in statefile.go calls).
// The data home and the log dir are this launcher's own.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// systemName: this build's OS as the path rules name it — "macos", "windows",
// or anything else.
func systemName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	}
	return runtime.GOOS
}

// systemSeparator: the path separator of a system, whichever one this build
// is for. A rule gives one answer on any test host.
func systemSeparator(system string) string {
	if system == "windows" {
		return `\`
	}
	return "/"
}

// pathUnder: names below base, joined with the system's separator.
func pathUnder(system, base string, names ...string) string {
	sep := systemSeparator(system)
	return strings.TrimRight(base, sep) + sep + strings.Join(names, sep)
}

// localAppDataFor: Windows' local application-data directory: LOCALAPPDATA,
// or where it is by default, under the home.
func localAppDataFor(home, localAppData string) string {
	if localAppData != "" {
		return localAppData
	}
	return pathUnder("windows", home, "AppData", "Local")
}

// dataDirFor: the launcher's data home — the databases and stores of each
// knowledge base. The same ~/Library/Application Support/xynolog the state
// home uses on macOS (Apple keeps one home for both) and the same
// %LOCALAPPDATA%\xynolog on Windows; $XDG_DATA_HOME/xynolog (default
// ~/.local/share/xynolog) elsewhere — database contents are XDG data, not
// XDG state. "" when there is no home.
func dataDirFor(system, home, xdgDataHome, localAppData string) string {
	switch {
	case home == "":
		return ""
	case system == "macos":
		return pathUnder(system, home, "Library", "Application Support", "xynolog")
	case system == "windows":
		return pathUnder(system, localAppDataFor(home, localAppData), "xynolog")
	case xdgDataHome != "":
		return pathUnder(system, xdgDataHome, "xynolog")
	}
	return pathUnder(system, home, ".local", "share", "xynolog")
}

// logDirFor: where launcher logs live — ~/Library/Logs/xynolog on macOS (the
// platform's log home), %LOCALAPPDATA%\xynolog\logs on Windows, and
// $XDG_STATE_HOME/xynolog (default ~/.local/state/xynolog) elsewhere: the XDG
// base-dir spec assigns logs and history to the state dir. "" when there is no
// home.
func logDirFor(system, home, xdgStateHome, localAppData string) string {
	switch {
	case home == "":
		return ""
	case system == "macos":
		return pathUnder(system, home, "Library", "Logs", "xynolog")
	case system == "windows":
		return pathUnder(system, localAppDataFor(home, localAppData), "xynolog", "logs")
	case xdgStateHome != "":
		return pathUnder(system, xdgStateHome, "xynolog")
	}
	return pathUnder(system, home, ".local", "state", "xynolog")
}

// userHome: the person's home directory, or "" when the system names none.
func userHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// stagingPattern: every staging dir a start made, of every knowledge base —
// what status prints, and what a stop with no knowledge base to go by sweeps.
func stagingPattern() string {
	return pathUnder(systemName(), stagingParent(), "xynolog-config.*")
}

// stagingPrefix: what each staging dir a start of the knowledge base keyed key
// makes is named with; os.MkdirTemp adds its digits.
func stagingPrefix(key string) string {
	return "xynolog-config." + key + "."
}

// keyStagingPattern: the staging dirs of the knowledge base keyed key, as a
// dry run says they are swept.
func keyStagingPattern(key string) string {
	return pathUnder(systemName(), stagingParent(), stagingPrefix(key)+"*")
}

// runDirPrefix: what the directory a run of the instance of the knowledge
// base keyed key makes (run.go) is named with; os.MkdirTemp adds its digits.
func runDirPrefix(key, instance string) string {
	return "xynolog-run." + key + "." + instance + "."
}

// keyRunDirPattern: the run directories of the knowledge base keyed key, as a
// dry run says they are swept.
func keyRunDirPattern(key string) string {
	return pathUnder(systemName(), stagingParent(), "xynolog-run."+key+".*")
}

// runDirs: the directories runs of the knowledge base keyed key made, each
// holding a run's env-file and document, which a launcher killed mid-run
// leaves. A key may begin another's, and an instance's name holds no dot
// (RunInstance.json), so a dir is this key's only when what follows its key
// is an instance's name and MkdirTemp's digits alone.
func runDirs(key string) []string {
	dirs, _ := filepath.Glob(keyRunDirPattern(key))
	var out []string
	for _, d := range dirs {
		rest := strings.TrimPrefix(filepath.Base(d), "xynolog-run."+key+".")
		instance, digits, ok := strings.Cut(rest, ".")
		if ok && instance != "" && strings.Trim(instance, "abcdefghijklmnopqrstuvwxyz0123456789-") == "" &&
			digits != "" && strings.Trim(digits, "0123456789") == "" {
			out = append(out, d)
		}
	}
	return out
}

// stagedConfigs: the staging dirs a start of the knowledge base keyed key made.
// A key may begin another's ("example.org" and "example.org.uk"), so a dir is
// this key's only when what follows its prefix is MkdirTemp's digits alone:
// another knowledge base's staged documents are live mounts of its running
// stack.
func stagedConfigs(key string) []string {
	dirs, _ := filepath.Glob(keyStagingPattern(key))
	var out []string
	for _, d := range dirs {
		if rest := strings.TrimPrefix(filepath.Base(d), stagingPrefix(key)); rest != "" && strings.Trim(rest, "0123456789") == "" {
			out = append(out, d)
		}
	}
	return out
}
