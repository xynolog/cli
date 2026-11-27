package launcher

// run.go — `xynolog run` and `xynolog signal`, and the control API's
// service.run, service.signal and service.host: one instance of one of a
// knowledge base's own services, run once, unsupervised, on its caller's
// document, arguments and environment, so that a program — the conformance
// harness — can hold the service to Protocol (protocol/contract/
// LAUNCHER-CONTROL.md, "Runs", R1–R8). It is not how a stack runs a service:
// start does that, supervised, on the documents the launcher writes.
//
// A run is the runtime's `run`, attached: the runtime's client carries the
// instance's stdout and stderr apart and exits with the instance's status,
// under each of Apple container, Docker and Podman. Apple container keeps
// neither apart in `logs`, and states no exit status in `inspect` and has no
// `wait` (measured on 0.11.0), so nothing is read after the fact. The client
// is not the instance: a client killed leaves its container running, which a
// stop of the target removes (R7).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xynolog/cli/internal/launcher/stack"
	"github.com/xynolog/sdk/go/api"
)

// runLabel: the label each run's container carries, valued with its target's
// key, by which a stop of the target finds every instance of it. A name
// prefix would not do: one key can begin another's.
const runLabel = "org.xynolog.run"

// runStopGrace: how long a term gives an instance to exit before it is sent
// SIGKILL (ServiceSignalParams.json).
const runStopGrace = 15 * time.Second

// runPoll: how often a run asks the runtime whether its instance has started,
// and a term whether it has exited.
const runPoll = 100 * time.Millisecond

// runContainer: the container of the instance of the knowledge base keyed
// key, by the name its caller gave it (RunInstance.json).
func runContainer(key, instance string) string {
	return "xynolog-" + key + "-run-" + instance
}

// runSpec: one run, as its caller states it (ServiceRunParams.json).
type runSpec struct {
	root, runtime, service, instance string
	// document: the text written at the path the image's own arguments name;
	// nil for none.
	document *string
	// args: the arguments the image's program is given in place of its own;
	// nil for the image's own.
	args              []string
	environment       map[string]string
	mounts, pathFirst []string
	ports             []int
}

// runStarted: an instance that runs, as notifications/serviceStarted says it.
type runStarted struct {
	container, documentPath, address string
}

// runEnded: an instance that exited.
type runEnded struct {
	exitCode, ranMs int
}

// runSink: where a run's instance is said: once when it runs, then each line
// it writes, with the stream it was on.
type runSink struct {
	started func(runStarted)
	output  func(stream, line string)
}

// runsHere: the runs this process has in flight, by container, each with the
// runtime it runs under, from the moment the run takes its name to its
// answer. A signal of one whose instance has exited is answered
// delivered: false until then (R6).
var runsHere = struct {
	sync.Mutex
	runtimes map[string]string
}{runtimes: map[string]string{}}

// hostAddresses: the host address each runtime's probe answered, once a
// process: what service.host told a caller is what its runs publish on.
var hostAddresses = struct {
	sync.Mutex
	byRuntime map[string]string
}{byRuntime: map[string]string{}}

// hostAddress: where this machine and a run's instance meet under rt — the
// address the launcher's own stack dials this machine at. ok=false: refused,
// and said.
func hostAddress(u *UI, rt string) (string, bool) {
	hostAddresses.Lock()
	defer hostAddresses.Unlock()
	if addr, ok := hostAddresses.byRuntime[rt]; ok {
		return addr, true
	}
	addr, ok := (&liveExec{u: u, rt: rt}).resolveAddr()
	if ok {
		hostAddresses.byRuntime[rt] = addr
	}
	return addr, ok
}

// runRuntime: the runtime a run or a signal of the knowledge base keyed key
// asks — the one named, else the one its stack's record names, else the
// first installed. ok=false: refused, and said.
func runRuntime(u *UI, key, named string) (string, bool) {
	if named != "" {
		return SelectRuntime(u, named)
	}
	ss := LoadStackSet()
	if ss.refuseUnreadable(u) {
		return "", false
	}
	if st := localStack(ss, key); st != nil && st.Runtime != "" && onPath(st.Runtime) {
		return st.Runtime, true
	}
	return SelectRuntime(u, "")
}

// runTarget: the knowledge base a run or a signal names, by its root.
// ok=false: refused, and said.
func runTarget(u *UI, root string) (kbRoot, bool) {
	dir, ident, err := resolveRootArg(root)
	if err != nil {
		refuseRoot(u, err)
		return kbRoot{}, false
	}
	return kbRoot{dir: dir, ident: ident}, true
}

// runImage: the image the target's stack documents name for service, and the
// memory ceiling they state: the worker's from the machine's worker document.
// ok=false: refused, and said.
func runImage(u *UI, kb kbRoot, service string) (image, memory string, ok bool) {
	if service == "worker" {
		doc, ok := readWorkerPoolDocument(u, false)
		if !ok {
			return "", "", false
		}
		return doc.spec.Worker.Image, doc.spec.Worker.Resources.Limits.Memory, true
	}
	base, err := stack.Read[stack.StackKnowledgeBase](filepath.Join(kb.dir, stack.Dir, stack.KnowledgeBaseFile), "KnowledgeBase")
	if err != nil {
		u.Fail("%v", err)
		return "", "", false
	}
	index, err := stack.Read[stack.KnowledgeIndex](filepath.Join(kb.dir, stack.Dir, stack.KnowledgeIndexFile), "KnowledgeIndex")
	if err != nil {
		u.Fail("%v", err)
		return "", "", false
	}
	images, memories := serviceImages(base.Spec, index.Spec)
	if image = images[service]; image == "" {
		u.Refuse(api.ControlErrorCodeInvalidService, "%s's stack documents name no image for %s.", kb.dir, service)
		return "", "", false
	}
	return image, memories[service], true
}

// imageRun: what an image runs by itself — its CMD, the program first — and
// the PATH it states, "" when it states none.
type imageRun struct {
	cmd  []string
	path string
}

// inspectImage reads what img runs by itself under rt. Apple container states
// it per platform: the variant this machine runs is linux on its own
// architecture.
func inspectImage(rt, img string) (imageRun, error) {
	type config struct {
		Cmd []string `json:"Cmd"`
		Env []string `json:"Env"`
	}
	var c config
	if rt == "container" {
		out, err := capture(rt, "image", "inspect", img)
		if err != nil {
			return imageRun{}, fmt.Errorf("container image inspect %s: %v", img, err)
		}
		var entries []struct {
			Variants []struct {
				Config struct {
					OS           string `json:"os"`
					Architecture string `json:"architecture"`
					Config       config `json:"config"`
				} `json:"config"`
			} `json:"variants"`
		}
		if err := json.Unmarshal([]byte(out), &entries); err != nil || len(entries) == 0 {
			return imageRun{}, fmt.Errorf("container image inspect %s is not the shape the launcher reads: %s", img, out)
		}
		i := slices.IndexFunc(entries[0].Variants, func(v struct {
			Config struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Config       config `json:"config"`
			} `json:"config"`
		}) bool {
			return v.Config.OS == "linux" && v.Config.Architecture == runtime.GOARCH
		})
		if i < 0 {
			return imageRun{}, fmt.Errorf("%s has no linux/%s variant", img, runtime.GOARCH)
		}
		c = entries[0].Variants[i].Config.Config
	} else {
		out, err := capture(rt, "image", "inspect", "--format", "{{json .Config}}", img)
		if err != nil {
			return imageRun{}, fmt.Errorf("%s image inspect %s: %v", rt, img, err)
		}
		if err := json.Unmarshal([]byte(out), &c); err != nil {
			return imageRun{}, fmt.Errorf("%s image inspect %s is not the shape the launcher reads: %s", rt, img, out)
		}
	}
	if len(c.Cmd) == 0 {
		return imageRun{}, fmt.Errorf("%s states no CMD: a run gives its program arguments, and there is no program to give them to", img)
	}
	run := imageRun{cmd: c.Cmd}
	for _, kv := range c.Env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			run.path = v
		}
	}
	return run, nil
}

// documentPath: where an image's own arguments say its program reads its
// document: the value of its --config.
func (r imageRun) documentPath() (string, bool) {
	for i, a := range r.cmd {
		if a == "--config" && i+1 < len(r.cmd) {
			return r.cmd[i+1], true
		}
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			return v, true
		}
	}
	return "", false
}

// runEnvironment: the run's env-file — each of its variables, and PATH when
// it puts directories before the image's — refused when a variable is one
// the file cannot carry, or one the run's own rules give (R1, R2).
// ok=false: refused, and said.
func runEnvironment(u *UI, spec runSpec, image imageRun) (string, bool) {
	names := make([]string, 0, len(spec.environment))
	for name := range spec.environment {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		value := spec.environment[name]
		switch {
		case name == "" || strings.ContainsAny(name, "=\n\r#"):
			u.Refuse(api.ControlErrorCodeInvalidRequest, "environment names %q, which is no variable's name.", name)
			return "", false
		case strings.ContainsAny(value, "\n\r"):
			u.Refuse(api.ControlErrorCodeInvalidRequest, "environment's %s holds a line break, which a variable of the runtime's env-file cannot.", name)
			return "", false
		case name == "XYNOLOG_SUPERVISE":
			u.Refuse(api.ControlErrorCodeInvalidRequest, "environment names XYNOLOG_SUPERVISE, and a run is unsupervised (R1): start runs a service supervised.")
			return "", false
		case name == "PATH" && len(spec.pathFirst) > 0:
			u.Refuse(api.ControlErrorCodeInvalidRequest, "environment names PATH and pathFirst puts directories before the image's: state one.")
			return "", false
		}
		fmt.Fprintf(&b, "%s=%s\n", name, value)
	}
	if len(spec.pathFirst) > 0 {
		if image.path == "" {
			u.Fail("pathFirst puts directories before the image's PATH, and the image states none.")
			return "", false
		}
		fmt.Fprintf(&b, "PATH=%s:%s\n", strings.Join(spec.pathFirst, ":"), image.path)
	}
	return b.String(), true
}

// runArgs: the runtime's `run` of spec's instance, attached: its container
// named and labelled for its target, its identifier written to cidfile, the
// launcher's own user, the documents' memory ceiling, its document mounted
// read-only, each mount and each of PATH's first directories read-write at
// its own path, its variables from envFile, each port published on the host
// address, and the image's program given args in place of its own.
func runArgs(rt, key, container, cidfile, memory string, spec runSpec, image string, run imageRun, document, envFile, addr string) []string {
	a := []string{"run", "--name", container, "--cidfile", cidfile, "--label", runLabel + "=" + key}
	a = append(a, runAsLauncher()...)
	if memory != "" {
		a = append(a, "--memory", memory)
	}
	if document != "" {
		path, _ := run.documentPath()
		a = append(a, "--volume", document+":"+path+":ro")
	}
	var mounted []string
	for _, dir := range slices.Concat(spec.mounts, spec.pathFirst) {
		if !slices.Contains(mounted, dir) {
			mounted = append(mounted, dir)
			a = append(a, "--volume", dir+":"+dir)
		}
	}
	a = append(a, "--env-file", envFile)
	e := exposure{rt: rt, addr: addr}
	for _, port := range spec.ports {
		a = append(a, e.publishRun("--publish", port)...)
	}
	a = append(a, image)
	if spec.args != nil {
		a = append(a, run.cmd[0])
		a = append(a, spec.args...)
	}
	return a
}

// containerRan: whether the container named name has been started: it runs,
// or ran and exited. A container that never started, or none, did not.
func containerRan(rt, name string) bool {
	out, err := capture(rt, "inspect", name)
	if err != nil || out == "" {
		return false
	}
	var entries []map[string]any
	if json.Unmarshal([]byte(out), &entries) != nil || len(entries) == 0 {
		return false
	}
	if rt == "container" {
		_, started := entries[0]["startedDate"]
		return started
	}
	startedAt, _ := digString(entries[0], "State", "StartedAt")
	return startedAt != "" && !strings.HasPrefix(startedAt, "0001-01-01")
}

// runService runs spec's instance until it exits, saying it through sink, and
// answers its exit. Cancelled through ctx, its instance is termed (R7).
// ok=false: refused or failed, and said through u.
func runService(ctx context.Context, u *UI, spec runSpec, sink runSink) (runEnded, bool) {
	kb, ok := runTarget(u, spec.root)
	if !ok {
		return runEnded{}, false
	}
	key := kb.key()
	container := runContainer(key, spec.instance)
	rt, ok := runRuntime(u, key, spec.runtime)
	if !ok {
		return runEnded{}, false
	}
	image, memory, ok := runImage(u, kb, spec.service)
	if !ok {
		return runEnded{}, false
	}
	if !locallyBuilt(image) {
		if image, ok = verifiedImage(u, spec.service, image); !ok {
			return runEnded{}, false
		}
	}

	runsHere.Lock()
	_, here := runsHere.runtimes[container]
	if !here {
		runsHere.runtimes[container] = rt
	}
	runsHere.Unlock()
	if here {
		u.Refuse(api.ControlErrorCodeInstanceInUse, "%s already runs here: its run is in flight.", spec.instance)
		return runEnded{}, false
	}
	defer func() {
		runsHere.Lock()
		delete(runsHere.runtimes, container)
		runsHere.Unlock()
	}()
	if state, _ := containerState([]string{rt}, container); state != "" {
		u.Refuse(api.ControlErrorCodeInstanceInUse, "%s's container %s is %s under %s: another run has it, or one a launcher left (xynolog stop --root %s removes it).", spec.instance, container, state, rt, kb.dir)
		return runEnded{}, false
	}

	if !locallyBuilt(image) {
		args := pullArgs(rt, image)
		u.EchoCmd(rt, args...)
		if err := runVisible(rt, args...); err != nil {
			u.Fail("Pull failed: %s", image)
			return runEnded{}, false
		}
	}
	run, err := inspectImage(rt, image)
	if err != nil {
		u.Fail("%v", err)
		return runEnded{}, false
	}
	documentPath := ""
	if spec.document != nil {
		if documentPath, ok = run.documentPath(); !ok {
			u.Refuse(api.ControlErrorCodeInvalidRequest, "%s's arguments (%s) name no --config, so a document has no path to be written at.", image, strings.Join(run.cmd, " "))
			return runEnded{}, false
		}
	}
	envText, ok := runEnvironment(u, spec, run)
	if !ok {
		return runEnded{}, false
	}
	addr, ok := hostAddress(u, rt)
	if !ok {
		return runEnded{}, false
	}

	// The run's own directory: its document, its variables and the file the
	// runtime writes its container's identifier to. Not a start's staging
	// directory, which a start's preflight and a stop sweep.
	dir, err := os.MkdirTemp(stagingParent(), runDirPrefix(key, spec.instance))
	if err == nil {
		err = ownerOnlyDir(dir)
	}
	if err != nil {
		u.Fail("Cannot make the run's directory: %v", err)
		return runEnded{}, false
	}
	defer os.RemoveAll(dir)
	document := ""
	if spec.document != nil {
		document = filepath.Join(dir, "document")
		if err := os.WriteFile(document, []byte(*spec.document), 0o600); err != nil {
			u.Fail("Writing the run's document: %v", err)
			return runEnded{}, false
		}
	}
	envFile, cidfile := filepath.Join(dir, "environment"), filepath.Join(dir, "cid")
	if err := os.WriteFile(envFile, []byte(envText), 0o600); err != nil {
		u.Fail("Writing the run's environment: %v", err)
		return runEnded{}, false
	}

	argv := runArgs(rt, key, container, cidfile, memory, spec, image, run, document, envFile, addr)
	u.EchoCmd(rt, argv...)
	cmd := exec.Command(rt, argv...)
	cmd.SysProcAttr = ownProcessGroup()
	stdout, err1 := cmd.StdoutPipe()
	stderr, err2 := cmd.StderrPipe()
	if err := errors.Join(err1, err2); err != nil {
		u.Fail("Cannot read the run's output: %v", err)
		return runEnded{}, false
	}
	if ctx.Err() != nil {
		return runEnded{}, false
	}
	began := time.Now()
	if err := cmd.Start(); err != nil {
		u.Fail("Cannot run %s: %v", rt, err)
		return runEnded{}, false
	}

	// Each line is held until the instance is seen to run, then said in the
	// order of its stream. A run whose instance never ran holds the runtime's
	// own account of why.
	var mu sync.Mutex
	saying := false
	var held [][2]string
	heard := func(stream, line string) {
		mu.Lock()
		defer mu.Unlock()
		if saying {
			sink.output(stream, line)
			return
		}
		held = append(held, [2]string{stream, line})
	}
	var readers sync.WaitGroup
	for stream, pipe := range map[string]io.Reader{"stdout": stdout, "stderr": stderr} {
		readers.Add(1)
		go func() {
			defer readers.Done()
			lines := bufio.NewScanner(pipe)
			lines.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for lines.Scan() {
				heard(stream, lines.Text())
			}
		}()
	}
	exited := make(chan error, 1)
	go func() {
		readers.Wait()
		exited <- cmd.Wait()
	}()
	created := func() bool {
		id, err := os.ReadFile(cidfile)
		return err == nil && strings.TrimSpace(string(id)) != ""
	}

	// A cancelled run's instance is termed once it runs: a cancel while its
	// container is still being made waits for it to start, and terms it then.
	// termed: the cancel has done all it will — termed the instance, found it
	// already exited, or outlived the run.
	cancelled := make(chan struct{})
	termed := make(chan struct{})
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		defer close(termed)
		select {
		case <-ctx.Done():
		case <-ended:
			return
		}
		close(cancelled)
		for {
			if created() {
				if state, _ := containerState([]string{rt}, container); state == "running" {
					if _, err := signalContainer(rt, container, "term"); err != nil {
						fmt.Fprintf(os.Stderr, "✗ Cancelling %s: %v\n", spec.instance, err)
					}
					return
				}
				if containerRan(rt, container) {
					return
				}
			}
			select {
			case <-ended:
				return
			case <-time.After(runPoll):
			}
		}
	}()

	var waitErr error
	done := false
	for !done && !(created() && containerRan(rt, container)) {
		select {
		case waitErr = <-exited:
			done = true
		case <-time.After(runPoll):
		}
	}
	started := time.Now()
	if done {
		started = began
	}
	if !created() || !containerRan(rt, container) {
		if !done {
			waitErr = <-exited
		}
		if created() {
			_ = runSilent(rt, "rm", container)
		}
		var said []string
		for _, l := range held {
			said = append(said, l[1])
		}
		select {
		case <-cancelled:
			return runEnded{}, false
		default:
		}
		u.Fail("%s did not run %s's instance %s: %s", rt, spec.service, spec.instance, strings.Join(said, " — "))
		return runEnded{}, false
	}

	mu.Lock()
	sink.started(runStarted{container: container, documentPath: documentPath, address: addr})
	for _, l := range held {
		sink.output(l[0], l[1])
	}
	saying = true
	mu.Unlock()
	if !done {
		waitErr = <-exited
	}
	ran := time.Since(started)
	// A cancel's term goes to its end before the container is removed: one
	// whose client went first still runs its instance.
	if ctx.Err() != nil {
		<-termed
	}
	// A container left holds the instance's name, and the next run of it is
	// refused: said, though the run is answered with its exit all the same.
	if _, why := captureWhy(rt, "rm", container); why != "" {
		u.Warn("%s exited, and removing its container %s failed: %s. xynolog stop --root %s removes it.", spec.instance, container, why, kb.dir)
	}
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(waitErr, &exit) && exit.ExitCode() >= 0:
		code = exit.ExitCode()
	case waitErr != nil:
		u.Fail("%s's client ended without the instance's status: %v", rt, waitErr)
		return runEnded{}, false
	}
	return runEnded{exitCode: code, ranMs: int(ran.Milliseconds())}, true
}

// signalContainer sends name's container signal — "term": SIGTERM, then
// SIGKILL if it still runs runStopGrace later; "kill": SIGKILL. A term is
// answered once the instance has exited. delivered=false: it was not running.
func signalContainer(rt, name string, signal api.ServiceSignalParamsSignal) (delivered bool, err error) {
	if state, _ := containerState([]string{rt}, name); state != "running" {
		return false, nil
	}
	sig := "KILL"
	if signal == "term" {
		sig = "TERM"
	}
	if _, why := captureWhy(rt, "kill", "--signal", sig, name); why != "" {
		if state, _ := containerState([]string{rt}, name); state != "running" {
			return false, nil
		}
		return false, fmt.Errorf("%s kill --signal %s %s: %s", rt, sig, name, why)
	}
	if signal == "kill" {
		return true, nil
	}
	deadline := time.Now().Add(runStopGrace)
	for time.Now().Before(deadline) {
		if state, _ := containerState([]string{rt}, name); state != "running" {
			return true, nil
		}
		time.Sleep(runPoll)
	}
	if _, why := captureWhy(rt, "kill", "--signal", "KILL", name); why != "" {
		if state, _ := containerState([]string{rt}, name); state != "running" {
			return true, nil
		}
		return true, fmt.Errorf("%s kill --signal KILL %s: %s", rt, name, why)
	}
	return true, nil
}

// signalService sends a run's instance signal. delivered=false: the instance
// has exited, and its run is not yet answered. ok=false: refused or failed,
// and said — control.instance-not-found for a name with no run in flight.
func signalService(u *UI, root, named, instance string, signal api.ServiceSignalParamsSignal) (delivered, ok bool) {
	kb, ok := runTarget(u, root)
	if !ok {
		return false, false
	}
	key := kb.key()
	container := runContainer(key, instance)
	runsHere.Lock()
	rt, here := runsHere.runtimes[container]
	runsHere.Unlock()
	if named != "" || !here {
		if rt, ok = runRuntime(u, key, named); !ok {
			return false, false
		}
	}
	if state, _ := containerState([]string{rt}, container); state == "" && !here {
		u.Refuse(api.ControlErrorCodeInstanceNotFound, "No run of %s is in flight under %s for %s.", instance, rt, kb.dir)
		return false, false
	}
	delivered, err := signalContainer(rt, container, signal)
	if err != nil {
		u.Fail("%v", err)
		return false, false
	}
	return delivered, true
}

// runInstances: the containers of the run instances of the knowledge base
// keyed key under rt, running or not, by their label.
func runInstances(rt, key string) []string {
	if rt == "container" {
		out, err := capture(rt, "list", "-a", "--format", "json")
		if err != nil {
			return nil
		}
		var entries []struct {
			Configuration struct {
				ID     string            `json:"id"`
				Labels map[string]string `json:"labels"`
			} `json:"configuration"`
		}
		if json.Unmarshal([]byte(out), &entries) != nil {
			return nil
		}
		var names []string
		for _, e := range entries {
			if e.Configuration.Labels[runLabel] == key {
				names = append(names, e.Configuration.ID)
			}
		}
		return names
	}
	out, err := capture(rt, "ps", "-a", "--filter", "label="+runLabel+"="+key, "--format", "{{.Names}}")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// runSpecOf: the run p asks for.
func runSpecOf(p api.ServiceRunParams) runSpec {
	spec := runSpec{root: p.Target.Root, runtime: string(given(p.Runtime)), service: string(p.Service), instance: p.Instance,
		document: p.Document, environment: p.Environment, mounts: p.Mounts, pathFirst: p.PathFirst, ports: p.Ports}
	if p.Args != nil {
		spec.args = *p.Args
	}
	return spec
}

// heldToMethod: whether params, as a command line states them, are what
// method's schema admits, as the control API holds a request's (P1); each
// fault said through u.
func heldToMethod(u *UI, method string, params any) bool {
	schemas, err := compileControlSchemas()
	if err != nil {
		u.Fail("The control API's schemas do not compile: %v", err)
		return false
	}
	m, _ := controlMethodNamed(method)
	text, err := json.Marshal(params)
	if err != nil {
		u.Fail("%v", err)
		return false
	}
	if f := schemas.checkParams(m, text); f != nil {
		u.Refuse(f.code, "%s", f.message)
		return false
	}
	return true
}

const runUsage = `Usage: xynolog run <service> --root <path> --instance <name> [options]

Run one instance of one of the knowledge base's own services once, from the
image its stack documents name, unsupervised, on your document, arguments and
environment, and exit with its status. Its stdout and stderr are this
command's, line for line. It is for holding a service to Protocol (the
conformance harness); xynolog start runs a stack.

  <service>             archivist, dispatcher, gateway, librarian, smelter,
                        weaver or worker
  --root <path>         the knowledge base whose documents name the image
  --instance <name>     the instance's name: lower case, digits and hyphens,
                        unique among the knowledge base's instances here
  --runtime <name>      container, docker or podman
  --document <file>     the file whose text is written, as it is, where the
                        image's arguments say the service reads its document
  --arg <arg>           an argument, in place of the image's own (repeated,
                        in order)
  --env NAME=VALUE      a variable beside the image's own; NAME alone takes
                        this command's own, which never reaches argv
                        (repeated)
  --mount <dir>         a directory mounted read-write at its own path
                        (repeated)
  --path-first <dir>    a directory put before the image's PATH, mounted at
                        its own path (repeated, in order)
  --publish <port>      a port published at the same number on the host
                        address (repeated)

Ctrl+C sends it SIGTERM, and SIGKILL 15 seconds later; xynolog signal
signals it from elsewhere.
`

// Run implements `xynolog run`.
func Run(args []string) int {
	u := NewUI(false)
	p := api.ServiceRunParams{ApiVersion: controlAPIVersion, Target: api.LocalTarget{Kind: "local"},
		Environment: map[string]string{}, Mounts: []string{}, PathFirst: []string{}, Ports: []int{}}
	var runArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, bool) {
			if i+1 >= len(args) {
				u.Fail("Missing value for %s", a)
				return "", false
			}
			i++
			return args[i], true
		}
		v, ok := "", true
		switch a {
		case "--root", "--runtime", "--instance", "--document", "--arg", "--env", "--mount", "--path-first", "--publish":
			v, ok = value()
		case "--help", "-h":
			fmt.Print(runUsage)
			return 0
		default:
			if strings.HasPrefix(a, "-") || p.Service != "" {
				u.Fail("Unknown argument: %s", a)
				return 1
			}
			p.Service = api.RunService(a)
			continue
		}
		if !ok {
			return 1
		}
		switch a {
		case "--root":
			p.Target.Root = v
		case "--runtime":
			runtime := api.Runtime(v)
			p.Runtime = &runtime
		case "--instance":
			p.Instance = v
		case "--document":
			text, err := os.ReadFile(v)
			if err != nil {
				u.Fail("--document: %v", err)
				return 1
			}
			document := string(text)
			p.Document = &document
		case "--arg":
			runArgs = append(runArgs, v)
		case "--env":
			name, value, stated := strings.Cut(v, "=")
			if !stated {
				if value, stated = os.LookupEnv(name); !stated {
					u.Fail("--env %s names a variable this command's environment does not have", name)
					return 1
				}
			}
			p.Environment[name] = value
		case "--mount":
			p.Mounts = append(p.Mounts, v)
		case "--path-first":
			p.PathFirst = append(p.PathFirst, v)
		case "--publish":
			port, err := strconv.Atoi(v)
			if err != nil {
				u.Fail("--publish %s is no port", v)
				return 1
			}
			p.Ports = append(p.Ports, port)
		}
	}
	p.Args = listed(runArgs)
	if !heldToMethod(u, "service.run", p) {
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ended, ok := runService(ctx, u, runSpecOf(p), runSink{
		started: func(s runStarted) {
			at := ""
			if s.documentPath != "" {
				at = "; its document at " + s.documentPath
			}
			u.Ok("%s runs as %s %s", p.Instance, s.container, u.Dim("(host address "+s.address+at+")"))
		},
		output: func(stream, line string) {
			if stream == "stderr" {
				fmt.Fprintln(os.Stderr, line)
				return
			}
			fmt.Println(line)
		},
	})
	if !ok {
		return 1
	}
	u.Log("%s exited %d after %s", p.Instance, ended.exitCode, took(time.Duration(ended.ranMs)*time.Millisecond))
	return ended.exitCode
}

const signalUsage = `Usage: xynolog signal <instance> term|kill [--root <path>] [--runtime <name>]

Signal a run's instance (xynolog run). term sends SIGTERM, and SIGKILL if it
has not exited 15 seconds later; kill sends SIGKILL, so nothing it would
settle on the way out is settled. The run answers with the instance's exit.

  --root <path>     the knowledge base the run belongs to; else the one you
                    stand in
  --runtime <name>  container, docker or podman
`

// Signal implements `xynolog signal`.
func Signal(args []string) int {
	u := NewUI(false)
	p := api.ServiceSignalParams{ApiVersion: controlAPIVersion, Target: api.LocalTarget{Kind: "local"}}
	var positionals []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--root", "--runtime":
			if i+1 >= len(args) {
				u.Fail("Missing value for %s", a)
				return 1
			}
			i++
			if a == "--root" {
				p.Target.Root = args[i]
			} else {
				runtime := api.Runtime(args[i])
				p.Runtime = &runtime
			}
		case "--help", "-h":
			fmt.Print(signalUsage)
			return 0
		default:
			if strings.HasPrefix(a, "-") {
				u.Fail("Unknown argument: %s", a)
				return 1
			}
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 2 {
		u.Fail("Name the instance and the signal: xynolog signal <instance> term|kill")
		return 1
	}
	p.Instance, p.Signal = positionals[0], api.ServiceSignalParamsSignal(positionals[1])
	if p.Target.Root == "" {
		kb, err := cwdKBRoot()
		if err != nil {
			refuseRoot(u, err)
			return 1
		}
		p.Target.Root = kb.dir
	}
	if !heldToMethod(u, "service.signal", p) {
		return 1
	}
	delivered, ok := signalService(u, p.Target.Root, string(given(p.Runtime)), p.Instance, p.Signal)
	if !ok {
		return 1
	}
	if delivered {
		u.Ok("%s: %s sent", p.Instance, p.Signal)
	} else {
		u.Log("%s has exited: nothing was sent", p.Instance)
	}
	return 0
}
