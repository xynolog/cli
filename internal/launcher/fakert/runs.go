package main

// runs.go — a run's instance (xynolog run, service.run): the runtime's `run`
// attached, with --cidfile, as Apple container 0.11.0 was measured to run
// one. The client carries the instance's stdout and stderr apart and exits
// with its status; a bootstrap failure (a published port in use) exits 1 and
// writes no cidfile and leaves no container; `kill --signal` of a stopped
// container fails; and a container lives until it is removed, whatever
// became of its client.
//
// A run container is a file in FAKERT_DIR, runctr-<name>.json: its state, its
// label and whether it started. A test that writes one scripts a container a
// launcher left (an orphan).
//
//	FAKERT_RUN_STDOUT     lines the instance writes on stdout, newline separated
//	FAKERT_RUN_STDERR     lines the instance writes on stderr, newline separated
//	FAKERT_RUN_EXIT       the instance's exit status once it has written them
//	FAKERT_RUN_UNTIL      "signal": it runs until it is signalled — TERM ends
//	                      it 0, as tini hands a handled TERM back; KILL ends it 137
//	FAKERT_RUN_IGNORE_TERM "1": it outlives a TERM, so only a KILL ends it
//	FAKERT_RUN_BOOT_FAIL  the runtime's account of a bootstrap that fails
//	FAKERT_RUN_HOLD       (main.go) parks a run of the container it names made
//	                      and not yet started
//	FAKERT_IMAGE_CMD      the CMD every image states, as JSON; unset: the
//	                      images' own, [/usr/local/bin/xynolog-<svc>, --config,
//	                      /etc/xynolog/<svc>.json]

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// imagePATH: the PATH an alpine image states.
const imagePATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

type runCtr struct {
	State   string `json:"state"` // running or exited
	Label   string `json:"label"` // org.xynolog.run's value
	Started bool   `json:"started"`
}

func runCtrPath(name string) string {
	return filepath.Join(os.Getenv("FAKERT_DIR"), "runctr-"+name+".json")
}

func readRunCtr(name string) (runCtr, bool) {
	b, err := os.ReadFile(runCtrPath(name))
	if err != nil {
		return runCtr{}, false
	}
	var c runCtr
	if json.Unmarshal(b, &c) != nil {
		fmt.Fprintf(os.Stderr, "fakert: %s is no run container: %s\n", runCtrPath(name), b)
		os.Exit(64)
	}
	return c, true
}

// writeRunCtr replaces name's file whole, so that a reader beside it — an
// inspect, a kill, the attached run — never reads half of it.
func writeRunCtr(name string, c runCtr) {
	b, _ := json.Marshal(c)
	tmp := runCtrPath(name) + "." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "fakert: %v\n", err)
		os.Exit(64)
	}
	if err := os.Rename(tmp, runCtrPath(name)); err != nil {
		fmt.Fprintf(os.Stderr, "fakert: %v\n", err)
		os.Exit(64)
	}
}

// runCtrNames: every run container there is, by name.
func runCtrNames() []string {
	paths, _ := filepath.Glob(filepath.Join(os.Getenv("FAKERT_DIR"), "runctr-*.json"))
	var names []string
	for _, p := range paths {
		names = append(names, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "runctr-"), ".json"))
	}
	sort.Strings(names)
	return names
}

// stoppedState: the word base's runtime states a container that has exited by.
func stoppedState(base string) string {
	if base == "container" {
		return "stopped"
	}
	return "exited"
}

// signalPath: where `kill --signal` leaves each signal for the instance.
func signalPath(name string) string {
	return filepath.Join(os.Getenv("FAKERT_DIR"), "run-signals-"+name)
}

// attachedRun: `run --cidfile …` — a run's instance, attached.
func attachedRun(args []string) {
	var name, cidfile, label, envFile, document string
	var rest []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			rest = args[i:]
			break
		}
		if i+1 >= len(args) {
			fmt.Fprintf(os.Stderr, "fakert run: %s takes a value\n", a)
			os.Exit(64)
		}
		i++
		v := args[i]
		switch a {
		case "--name":
			name = v
		case "--cidfile":
			cidfile = v
		case "--label":
			k, val, _ := strings.Cut(v, "=")
			if k == "org.xynolog.run" {
				label = val
			}
		case "--env-file":
			envFile = v
		case "--volume":
			// source:target:ro, the source this machine's path, which on
			// Windows holds a colon of its own.
			if src, ok := strings.CutSuffix(v, ":ro"); ok && strings.LastIndex(src, ":") > 0 {
				document = src[:strings.LastIndex(src, ":")]
			}
		case "--user", "--memory", "--publish":
		default:
			fmt.Fprintf(os.Stderr, "fakert run: unscripted flag %s in an attached run\n", a)
			os.Exit(64)
		}
	}
	if name == "" || len(rest) == 0 {
		fmt.Fprintf(os.Stderr, "fakert run: an attached run names its container and its image (%v)\n", args)
		os.Exit(64)
	}
	if why := os.Getenv("FAKERT_RUN_BOOT_FAIL"); why != "" {
		fmt.Fprintln(os.Stderr, "Error: "+why)
		os.Exit(1)
	}
	if _, held := readRunCtr(name); held || scriptedAlive(name) {
		fmt.Fprintf(os.Stderr, "Error: the container name %q is already in use\n", name)
		os.Exit(125)
	}
	// What the instance was given, as `inspect` and its mount would show it.
	dir := os.Getenv("FAKERT_DIR")
	if envFile != "" {
		b, err := os.ReadFile(envFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: --env-file %s: %v\n", envFile, err)
			os.Exit(125)
		}
		_ = os.WriteFile(filepath.Join(dir, "env-"+name), b, 0o600)
	}
	if document != "" {
		b, err := os.ReadFile(document)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: mount %s: %v\n", document, err)
			os.Exit(125)
		}
		_ = os.WriteFile(filepath.Join(dir, "document-"+name), b, 0o600)
	}
	// Made, then started, as Apple container's run does: the cidfile names
	// the container before it runs, and FAKERT_RUN_HOLD parks it between —
	// where a cancel can come.
	writeRunCtr(name, runCtr{State: "created", Label: label})
	if err := os.WriteFile(cidfile, []byte(name), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Error: --cidfile %s: %v\n", cidfile, err)
		os.Exit(125)
	}
	holdIfNamed(name)
	writeRunCtr(name, runCtr{State: "running", Label: label, Started: true})
	for _, l := range lines(os.Getenv("FAKERT_RUN_STDOUT")) {
		fmt.Println(l)
	}
	for _, l := range lines(os.Getenv("FAKERT_RUN_STDERR")) {
		fmt.Fprintln(os.Stderr, l)
	}
	code := 0
	if os.Getenv("FAKERT_RUN_UNTIL") == "signal" {
		code = untilSignalled(name)
	} else if s := os.Getenv("FAKERT_RUN_EXIT"); s != "" {
		code, _ = strconv.Atoi(s)
	}
	if c, ok := readRunCtr(name); ok && c.State == "running" {
		c.State = "exited"
		writeRunCtr(name, c)
	}
	os.Exit(code)
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// untilSignalled: the exit status of an instance that runs until a signal
// ends it, or its container is removed from under it.
func untilSignalled(name string) int {
	for {
		b, _ := os.ReadFile(signalPath(name))
		for _, sig := range strings.Fields(string(b)) {
			switch {
			case sig == "KILL":
				return 137
			case sig == "TERM" && os.Getenv("FAKERT_RUN_IGNORE_TERM") != "1":
				return 0
			}
		}
		if _, ok := readRunCtr(name); !ok {
			return 137
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killRun: `kill --signal SIG <name>`.
func killRun(args []string) {
	if len(args) != 4 || args[1] != "--signal" {
		fmt.Fprintf(os.Stderr, "fakert kill: unscripted %v\n", args)
		os.Exit(64)
	}
	sig, name := args[2], args[3]
	c, ok := readRunCtr(name)
	switch {
	case !ok:
		fmt.Fprintln(os.Stderr, "Error: no such container")
		os.Exit(1)
	case c.State != "running":
		fmt.Fprintln(os.Stderr, "Error: container is stopped")
		os.Exit(1)
	}
	// A KILL ends a container at once, whether or not a client waits on it:
	// stopped before the client hears it, so its own write cannot come after
	// the rm that follows its exit.
	if sig == "KILL" {
		c.State = "exited"
		writeRunCtr(name, c)
	}
	f, err := os.OpenFile(signalPath(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(64)
	}
	fmt.Fprintln(f, sig)
	f.Close()
}

// stopRunCtr: `stop` of a run container — TERM, and stopped on return, as a
// real stop is. false: name is no run container.
func stopRunCtr(name string) bool {
	c, ok := readRunCtr(name)
	if !ok {
		return false
	}
	if c.State == "running" {
		c.State = "exited"
		writeRunCtr(name, c)
		f, err := os.OpenFile(signalPath(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(f, "TERM")
			f.Close()
		}
	}
	return true
}

// rmRunCtr: `rm` of a run container. false: name is no run container.
func rmRunCtr(name string) bool {
	if _, ok := readRunCtr(name); !ok {
		return false
	}
	_ = os.Remove(runCtrPath(name))
	return true
}

// inspectRunCtr: `inspect` of a run container, in base's shape. false: name
// is no run container.
func inspectRunCtr(base string, args []string, name string) bool {
	c, ok := readRunCtr(name)
	if !ok {
		return false
	}
	state := c.State
	if state == "exited" {
		state = stoppedState(base)
	}
	switch {
	case len(args) > 1 && args[1] == "-f":
		fmt.Println(state)
	case base == "container":
		entry := map[string]any{
			"configuration": map[string]any{"id": name, "labels": map[string]string{"org.xynolog.run": c.Label}},
			"status":        state,
		}
		if c.Started {
			entry["startedDate"] = 785000000.0
		}
		b, _ := json.Marshal([]any{entry})
		fmt.Println(string(b))
	default:
		started := "0001-01-01T00:00:00Z"
		if c.Started {
			started = "2026-11-20T12:00:00.000000000Z"
		}
		b, _ := json.Marshal([]any{map[string]any{
			"Config": map[string]any{"Env": []string{}, "Labels": map[string]string{"org.xynolog.run": c.Label}},
			"State":  map[string]any{"Status": state, "StartedAt": started},
		}})
		fmt.Println(string(b))
	}
	return true
}

// listJSON: `container list -a --format json` — every container there is,
// each run container with its label.
func listJSON() {
	entries := []any{}
	for _, n := range existingContainers() {
		labels := map[string]string{}
		if c, ok := readRunCtr(n); ok {
			labels["org.xynolog.run"] = c.Label
		}
		entries = append(entries, map[string]any{"configuration": map[string]any{"id": n, "labels": labels}})
	}
	b, _ := json.Marshal(entries)
	fmt.Println(string(b))
}

// psLabelled: `ps -a --filter label=org.xynolog.run=<key> --format {{.Names}}`.
func psLabelled(filter string) {
	key, ok := strings.CutPrefix(filter, "label=org.xynolog.run=")
	if !ok {
		fmt.Fprintf(os.Stderr, "fakert ps: unscripted filter %s\n", filter)
		os.Exit(64)
	}
	for _, n := range runCtrNames() {
		if c, _ := readRunCtr(n); c.Label == key {
			fmt.Println(n)
		}
	}
}

// imageCmd: the CMD img states.
func imageCmd(img string) []string {
	if s := os.Getenv("FAKERT_IMAGE_CMD"); s != "" {
		var cmd []string
		if err := json.Unmarshal([]byte(s), &cmd); err != nil {
			fmt.Fprintf(os.Stderr, "fakert: FAKERT_IMAGE_CMD is no JSON list: %v\n", err)
			os.Exit(64)
		}
		return cmd
	}
	repo, _, _ := strings.Cut(img, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	svc := strings.TrimPrefix(repo[strings.LastIndex(repo, "/")+1:], "xynolog-")
	return []string{"/usr/local/bin/xynolog-" + svc, "--config", "/etc/xynolog/" + svc + ".json"}
}

// imageInspect: `image inspect`, in base's shape.
func imageInspect(base string, args []string) {
	img := args[len(args)-1]
	config := map[string]any{"Cmd": imageCmd(img), "Env": []string{"PATH=" + imagePATH}, "User": "xynolog"}
	var out any = config
	if base == "container" {
		out = []any{map[string]any{"name": img, "variants": []any{
			map[string]any{"config": map[string]any{"os": "linux", "architecture": runtime.GOARCH, "config": config}},
		}}}
	} else if len(args) != 5 || args[2] != "--format" || args[3] != "{{json .Config}}" {
		fmt.Fprintf(os.Stderr, "fakert image inspect: unscripted %v\n", args)
		os.Exit(64)
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}
