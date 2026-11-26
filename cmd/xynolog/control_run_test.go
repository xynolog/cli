package main_test

// The control API's runs (protocol/contract/LAUNCHER-CONTROL.md, "Runs",
// R1–R8): service.run, service.signal and service.host, and the commands
// xynolog run and xynolog signal, against the fake runtime's attached run
// (internal/launcher/fakert/runs.go).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// send writes a request of method, apiVersion stated, and gives its id. Its
// answer is read by readUntil, among whatever else arrives.
func (c *controlClient) send(method string, params map[string]any) int {
	c.t.Helper()
	c.lastID++
	params["apiVersion"] = c.apiVersion
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.lastID, "method": method, "params": params})
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		c.fail("%s: writing the request: %v", method, err)
	}
	return c.lastID
}

// cancel sends notifications/cancelled of the request id.
func (c *controlClient) cancel(id int) {
	c.t.Helper()
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id}})
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		c.fail("cancelling %d: %v", id, err)
	}
}

// readUntil reads messages until last is true of one, and gives every one
// read, in order, that one last.
func (c *controlClient) readUntil(what string, last func(controlMessage) bool) []controlMessage {
	c.t.Helper()
	var read []controlMessage
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case <-deadline:
			c.fail("no %s within two minutes; read: %s", what, describe(read))
		case text, open := <-c.lines:
			if !open {
				c.fail("xynolog control ended before %s; read: %s", what, describe(read))
			}
			var m controlMessage
			if err := json.Unmarshal([]byte(text), &m); err != nil || m.JSONRPC != "2.0" {
				c.fail("xynolog control wrote a line that is not a JSON-RPC message: %q", text)
			}
			read = append(read, m)
			if last(m) {
				return read
			}
		}
	}
}

// answerTo: whether m answers the request id.
func answerTo(id int) func(controlMessage) bool {
	return func(m controlMessage) bool { return m.Method == "" && string(m.ID) == itoa(id) }
}

// notified: whether m is a notification of method.
func notified(method string) func(controlMessage) bool {
	return func(m controlMessage) bool { return m.Method == method }
}

func describe(read []controlMessage) string {
	var b strings.Builder
	for _, m := range read {
		line, _ := json.Marshal(m)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// refusedWith: the control code m's error states, "" for an answer that is
// not an error.
func refusedWith(t *testing.T, m controlMessage) string {
	t.Helper()
	if m.Error == nil {
		return ""
	}
	var data struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(m.Error.Data, &data); err != nil {
		t.Fatalf("an error whose data states no code: %s", m.Error.Data)
	}
	return data.Code
}

// runParams: a service.run of the scenario's knowledge base's smelter, as
// instance, under Apple container.
func runParams(s *scenario, instance string) map[string]any {
	return map[string]any{
		"target": map[string]any{"kind": "local", "root": s.kb}, "runtime": "container",
		"service": "smelter", "instance": instance,
		"environment": map[string]any{}, "mounts": []string{}, "pathFirst": []string{}, "ports": []int{},
	}
}

// runCtr: the fake runtime's file for the run container name, as the run
// writes it, or as a test writes one a launcher left.
func runCtrFile(s *scenario, name string) string {
	return filepath.Join(s.fakertDir, "runctr-"+name+".json")
}

func leaveRunContainer(t *testing.T, s *scenario, name, state string) {
	t.Helper()
	text := `{"state":"` + state + `","label":"` + testKBKey + `","started":true}`
	if err := os.WriteFile(runCtrFile(s, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runArgv: the runtime's `run` of the container name, as the log records it.
func runArgv(t *testing.T, s *scenario, name string) string {
	t.Helper()
	for _, line := range strings.Split(s.argv(t), "\n") {
		if strings.HasPrefix(line, "container run --name "+name+" ") {
			return line
		}
	}
	t.Fatalf("no run of %s in:\n%s", name, s.argv(t))
	return ""
}

// waitForArgv waits, ten seconds at most, for the log to record a line
// holding text.
func waitForArgv(t *testing.T, s *scenario, text string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(s.argv(t), text) {
			return
		}
	}
	t.Fatalf("the runtime was asked no %q within ten seconds:\n%s", text, s.argv(t))
}

// A run is unsupervised and runs the documents' image as its caller states
// it (R1, R2): its document at the path the image's own arguments name, its
// arguments in place of the image's, its environment and PATH from an
// env-file alone and nothing of the launcher's; its ports on the host
// address service.host names (R3); its mounts and PATH's first directories
// read-write at their own paths, as the launcher's own user (R4). It says
// itself once it runs, then each line on its stream, then answers its exit
// (R6, R8); its environment is a secret to progress and its own lines carry
// it as written (R5); and its container is gone after.
func TestControlRunsAServiceUnsupervised(t *testing.T) {
	s := newScenario(t, "container")
	const secret = "s3cret-value-123"
	s.extraEnv = append(s.extraEnv,
		"FAKERT_RUN_STDOUT=ready\nholding "+secret, "FAKERT_RUN_STDERR=warned", "FAKERT_RUN_EXIT=3")
	c := startControl(t, s)

	host, _ := c.call("service.host", map[string]any{"runtime": "container"})
	var at struct{ Runtime, Address string }
	if err := json.Unmarshal(host, &at); err != nil || at.Runtime != "container" || at.Address == "" {
		t.Fatalf("service.host answered %s", host)
	}

	params := runParams(s, "smelter-1")
	const document = "{\"smelter\": \"the caller's\"}\n"
	params["document"] = document
	params["args"] = []string{"--config", "/etc/xynolog/smelter.json", "--verbose"}
	params["environment"] = map[string]any{"SMELTER_TOKEN": secret}
	params["mounts"] = []string{"/work/records"}
	params["pathFirst"] = []string{"/work/tripwire", "/work/records"}
	params["ports"] = []int{9123}
	id := c.send("service.run", params)
	read := c.readUntil("service.run's answer", answerTo(id))
	answer := read[len(read)-1]
	if answer.Error != nil {
		t.Fatalf("service.run refused %s %s; read:\n%s\nstderr:\n%s", answer.Error.Message, answer.Error.Data, describe(read), c.stderr)
	}
	var ended struct {
		Instance string `json:"instance"`
		ExitCode int    `json:"exitCode"`
		RanMs    *int   `json:"ranMs"`
	}
	if err := json.Unmarshal(answer.Result, &ended); err != nil || ended.Instance != "smelter-1" || ended.ExitCode != 3 || ended.RanMs == nil {
		t.Errorf("service.run answered %s, want smelter-1's exit 3 and how long it ran", answer.Result)
	}

	container := "xynolog-" + testKBKey + "-run-smelter-1"
	type started struct {
		RequestID    int    `json:"requestId"`
		Instance     string `json:"instance"`
		Container    string `json:"container"`
		DocumentPath string `json:"documentPath"`
		Address      string `json:"address"`
	}
	type output struct {
		RequestID int    `json:"requestId"`
		Instance  string `json:"instance"`
		Stream    string `json:"stream"`
		Line      string `json:"line"`
	}
	var starts []started
	var lines []string
	for _, m := range read {
		switch m.Method {
		case "notifications/serviceStarted":
			var n started
			if err := json.Unmarshal(m.Params, &n); err != nil {
				t.Fatal(err)
			}
			starts = append(starts, n)
		case "notifications/serviceOutput":
			var n output
			if err := json.Unmarshal(m.Params, &n); err != nil {
				t.Fatal(err)
			}
			if len(starts) == 0 {
				t.Errorf("serviceOutput %q came before serviceStarted", n.Line)
			}
			if n.RequestID != id || n.Instance != "smelter-1" {
				t.Errorf("serviceOutput %s is not the run's", m.Params)
			}
			lines = append(lines, n.Stream+": "+n.Line)
		case "notifications/progress":
			if strings.Contains(string(m.Params), secret) {
				t.Errorf("progress carries the run's environment: %s", m.Params)
			}
		}
	}
	want := started{RequestID: id, Instance: "smelter-1", Container: container, DocumentPath: "/etc/xynolog/smelter.json", Address: at.Address}
	if len(starts) != 1 || starts[0] != want {
		t.Errorf("serviceStarted: %+v, want once %+v", starts, want)
	}
	slices.Sort(lines)
	if wantLines := []string{"stderr: warned", "stdout: holding " + secret, "stdout: ready"}; !slices.Equal(lines, wantLines) {
		t.Errorf("serviceOutput lines %q, want %q", lines, wantLines)
	}
	if strings.Contains(c.stderr.String(), secret) {
		t.Errorf("a diagnostic carries the run's environment:\n%s", c.stderr)
	}

	argv := runArgv(t, s, container)
	all := s.argv(t)
	if strings.Contains(all, "XYNOLOG_SUPERVISE") || strings.Contains(all, secret) {
		t.Errorf("the runtime was given the supervisor or the environment's value on its command line:\n%s", all)
	}
	for _, part := range []string{
		" --label org.xynolog.run=" + testKBKey + " ",
		" --volume /work/records:/work/records ",
		" --volume /work/tripwire:/work/tripwire ",
		" --publish " + at.Address + ":9123:9123 ",
	} {
		if !strings.Contains(argv, part) {
			t.Errorf("the run holds no %q:\n%s", part, argv)
		}
	}
	if n := strings.Count(argv, "--volume /work/records:/work/records"); n != 1 {
		t.Errorf("/work/records, both a mount and first on PATH, is mounted %d times:\n%s", n, argv)
	}
	user := " --user " + strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()) + " "
	if runtime.GOOS == "windows" {
		if strings.Contains(argv, " --user ") {
			t.Errorf("a run on Windows names a user, which Windows has no uid for:\n%s", argv)
		}
	} else if !strings.Contains(argv, user) {
		t.Errorf("the run holds no %q: its instance is not the launcher's user:\n%s", user, argv)
	}
	if !regexp.MustCompile(` --volume \S+:/etc/xynolog/smelter\.json:ro `).MatchString(argv) {
		t.Errorf("the document is not mounted read-only at the image's --config:\n%s", argv)
	}
	if !regexp.MustCompile(` ghcr\.io/xynolog/xynolog-smelter:latest@sha256:\S+ /usr/local/bin/xynolog-smelter --config /etc/xynolog/smelter\.json --verbose$`).MatchString(argv) {
		t.Errorf("the run is not the verified image's program given the run's arguments:\n%s", argv)
	}
	if b, err := os.ReadFile(filepath.Join(s.fakertDir, "document-"+container)); err != nil || string(b) != document {
		t.Errorf("the instance's document is %q (%v), want %q", b, err, document)
	}
	env, _ := os.ReadFile(filepath.Join(s.fakertDir, "env-"+container))
	wantEnv := "SMELTER_TOKEN=" + secret + "\nPATH=/work/tripwire:/work/records:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n"
	if string(env) != wantEnv {
		t.Errorf("the instance's env-file is %q, want %q: the run's variables and PATH, and nothing of the launcher's", env, wantEnv)
	}
	if _, err := os.Stat(runCtrFile(s, container)); !os.IsNotExist(err) {
		t.Errorf("the run's container is left after its answer (%v)", err)
	}

	if err := c.end(); err != nil {
		t.Errorf("xynolog control ended with %v", err)
	}
}

// An instance's name is one run's while its run is in flight, whoever runs
// it (R6): a second run of it is refused, here and when a launcher left its
// container. A signal is answered while the run waits on its instance (C2),
// and once the run is answered its name has no run.
func TestControlRunRefusesAnInstanceInUse(t *testing.T) {
	s := newScenario(t, "container")
	s.extraEnv = append(s.extraEnv, "FAKERT_RUN_UNTIL=signal")
	leaveRunContainer(t, s, "xynolog-"+testKBKey+"-run-left", "running")
	c := startControl(t, s)

	left := c.send("service.run", runParams(s, "left"))
	read := c.readUntil("the refusal of a run whose container a launcher left", answerTo(left))
	if code := refusedWith(t, read[len(read)-1]); code != "control.instance-in-use" {
		t.Errorf("a run of an instance whose container is left answered %s, want control.instance-in-use", describe(read))
	}

	first := c.send("service.run", runParams(s, "busy"))
	c.readUntil("serviceStarted", notified("notifications/serviceStarted"))
	second := c.send("service.run", runParams(s, "busy"))
	read = c.readUntil("the second run's answer", answerTo(second))
	if code := refusedWith(t, read[len(read)-1]); code != "control.instance-in-use" {
		t.Errorf("a second run of an instance in flight answered %s, want control.instance-in-use", describe(read))
	}

	signal := c.send("service.signal", map[string]any{"target": map[string]any{"kind": "local", "root": s.kb}, "instance": "busy", "signal": "kill"})
	read = c.readUntil("the signal's answer", answerTo(signal))
	if got := read[len(read)-1]; got.Error != nil || string(got.Result) != `{"delivered":true}` {
		t.Errorf("service.signal of a running instance answered %s, want delivered", describe(read))
	}
	read = c.readUntil("the killed run's answer", answerTo(first))
	var ended struct {
		ExitCode int `json:"exitCode"`
	}
	if got := read[len(read)-1]; got.Error != nil || json.Unmarshal(got.Result, &ended) != nil || ended.ExitCode != 137 {
		t.Errorf("the killed run answered %s, want exit 137", describe(read))
	}

	after := c.send("service.signal", map[string]any{"target": map[string]any{"kind": "local", "root": s.kb}, "instance": "busy", "signal": "term"})
	read = c.readUntil("the late signal's answer", answerTo(after))
	if code := refusedWith(t, read[len(read)-1]); code != "control.instance-not-found" {
		t.Errorf("a signal of an instance whose run is answered answered %s, want control.instance-not-found", describe(read))
	}

	if err := c.end(); err != nil {
		t.Errorf("xynolog control ended with %v", err)
	}
}

// A signal of an instance that has exited, its container not yet removed, is
// delivered nowhere and says so (R6).
func TestControlSignalOfAnExitedInstanceIsNotDelivered(t *testing.T) {
	s := newScenario(t, "container")
	leaveRunContainer(t, s, "xynolog-"+testKBKey+"-run-gone", "exited")
	c := startControl(t, s)
	got, _ := c.call("service.signal", map[string]any{"target": map[string]any{"kind": "local", "root": s.kb}, "runtime": "container", "instance": "gone", "signal": "term"})
	if string(got) != `{"delivered":false}` {
		t.Errorf("service.signal of an exited instance answered %s, want delivered: false", got)
	}
	if strings.Contains(s.argv(t), "kill --signal") {
		t.Errorf("an exited instance was sent a signal:\n%s", s.argv(t))
	}
	if err := c.end(); err != nil {
		t.Errorf("xynolog control ended with %v", err)
	}
}

// A term an instance outlives is followed by SIGKILL 15 seconds later, and
// is answered once the instance has exited (R7).
func TestControlTermOfAnInstanceThatOutlivesItKills(t *testing.T) {
	s := newScenario(t, "container")
	s.extraEnv = append(s.extraEnv, "FAKERT_RUN_UNTIL=signal", "FAKERT_RUN_IGNORE_TERM=1")
	c := startControl(t, s)
	run := c.send("service.run", runParams(s, "stubborn"))
	c.readUntil("serviceStarted", notified("notifications/serviceStarted"))
	began := time.Now()
	signal := c.send("service.signal", map[string]any{"target": map[string]any{"kind": "local", "root": s.kb}, "instance": "stubborn", "signal": "term"})
	read := c.readUntil("both answers", func(m controlMessage) bool { return answerTo(signal)(m) || answerTo(run)(m) })
	read = append(read, c.readUntil("the other answer", func(m controlMessage) bool { return answerTo(signal)(m) || answerTo(run)(m) })...)
	if took := time.Since(began); took < 15*time.Second {
		t.Errorf("the term was answered after %s, before its 15 seconds' grace", took)
	}
	container := "xynolog-" + testKBKey + "-run-stubborn"
	log := s.argv(t)
	term, kill := strings.Index(log, "kill --signal TERM "+container), strings.Index(log, "kill --signal KILL "+container)
	if term < 0 || kill < term {
		t.Errorf("the instance was not sent TERM and then KILL:\n%s", log)
	}
	for _, m := range read {
		switch {
		case answerTo(signal)(m) && string(m.Result) != `{"delivered":true}`:
			t.Errorf("the term answered %s, want delivered", describe([]controlMessage{m}))
		case answerTo(run)(m) && !strings.Contains(string(m.Result), `"exitCode":137`):
			t.Errorf("the killed run answered %s, want exit 137", describe([]controlMessage{m}))
		}
	}
	if err := c.end(); err != nil {
		t.Errorf("xynolog control ended with %v", err)
	}
}

// A cancelled run is answered control.cancelled, and its instance is sent
// SIGTERM and removed; so is each run in flight when the server shuts down
// (R7, S1).
func TestControlCancelledRunTermsItsInstance(t *testing.T) {
	s := newScenario(t, "container")
	s.extraEnv = append(s.extraEnv, "FAKERT_RUN_UNTIL=signal")
	c := startControl(t, s)

	cancelled := c.send("service.run", runParams(s, "cancelled"))
	c.readUntil("serviceStarted", notified("notifications/serviceStarted"))
	c.cancel(cancelled)
	read := c.readUntil("the cancelled run's answer", answerTo(cancelled))
	if code := refusedWith(t, read[len(read)-1]); code != "control.cancelled" {
		t.Errorf("a cancelled run answered %s, want control.cancelled", describe(read))
	}
	waitForArgv(t, s, "container kill --signal TERM xynolog-"+testKBKey+"-run-cancelled")

	shutdown := c.send("service.run", runParams(s, "shutdown"))
	c.readUntil("serviceStarted", notified("notifications/serviceStarted"))
	if err := c.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	read = c.readUntil("the answer of the run in flight at shutdown", answerTo(shutdown))
	if code := refusedWith(t, read[len(read)-1]); code != "control.cancelled" {
		t.Errorf("a run in flight at shutdown answered %s, want control.cancelled", describe(read))
	}
	for text := range c.lines {
		t.Errorf("xynolog control wrote %q after its last answer", text)
	}
	if err := <-c.exited; err != nil {
		t.Errorf("xynolog control ended with %v when its stdin closed, want exit 0", err)
	}
	if !strings.Contains(s.argv(t), "container kill --signal TERM xynolog-"+testKBKey+"-run-shutdown") {
		t.Errorf("a run in flight when the server shut down was not termed:\n%s", s.argv(t))
	}
	for _, instance := range []string{"cancelled", "shutdown"} {
		name := "xynolog-" + testKBKey + "-run-" + instance
		if _, err := os.Stat(runCtrFile(s, name)); !os.IsNotExist(err) {
			t.Errorf("%s is left after its run ended (%v)", name, err)
		}
	}
}

// A stop of a knowledge base with no recorded stack removes each of its run
// instances a launcher left, under every runtime there is, by their label,
// and the run dirs holding their env-files and documents, and leaves
// another's (R7).
func TestStopRemovesRunInstancesItHasNoRecordOf(t *testing.T) {
	s := newScenario(t, "container", "docker")
	mine := "xynolog-" + testKBKey + "-run-left"
	leaveRunContainer(t, s, mine, "running")
	theirs := "xynolog-" + testKBKey + "-other-run-left"
	if err := os.WriteFile(runCtrFile(s, theirs), []byte(`{"state":"running","label":"`+testKBKey+`-other","started":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(s.stagingParent(), "xynolog-run."+testKBKey+".left.123456")
	othersDir := filepath.Join(s.stagingParent(), "xynolog-run."+testKBKey+".uk.left.123456")
	for _, d := range []string{runDir, othersDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, stderr, code := s.run(t, "stop"); code != 0 {
		t.Fatalf("stop exited %d:\n%s", code, stderr)
	}
	log := s.argv(t)
	for _, asked := range []string{
		"container list -a --format json",
		"docker ps -a --filter label=org.xynolog.run=" + testKBKey + " --format {{.Names}}",
		"container stop " + mine,
		"container rm " + mine,
	} {
		if !strings.Contains(log, asked) {
			t.Errorf("the stop asked no %q:\n%s", asked, log)
		}
	}
	if _, err := os.Stat(runCtrFile(s, mine)); !os.IsNotExist(err) {
		t.Errorf("the knowledge base's run instance is left after its stop (%v)", err)
	}
	if _, err := os.Stat(runCtrFile(s, theirs)); err != nil {
		t.Errorf("another knowledge base's run instance went with this one's stop: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("the run dir a run left, with its env-file and document, is left after the stop (%v)", err)
	}
	if _, err := os.Stat(othersDir); err != nil {
		t.Errorf("another knowledge base's run dir went with this one's stop: %v", err)
	}
}

// xynolog run is service.run as a command: the instance's lines are its own,
// on their streams, and its exit is the command's; a variable named alone
// takes the command's own, and never reaches argv. xynolog signal of a name
// with no run fails, saying so.
func TestRunCommandIsTheInstance(t *testing.T) {
	s := newScenario(t, "docker")
	const secret = "the-callers-own-value"
	s.extraEnv = append(s.extraEnv, "SMELTER_TOKEN="+secret,
		"FAKERT_RUN_STDOUT=ready", "FAKERT_RUN_STDERR=warned", "FAKERT_RUN_EXIT=4")
	stdout, stderr, code := s.run(t, "run", "smelter", "--root", s.kb, "--instance", "cli-1",
		"--env", "SMELTER_TOKEN", "--arg", "--verbose", "--publish", "9124")
	if code != 4 {
		t.Errorf("xynolog run exited %d, want the instance's 4\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "ready\n") || !strings.Contains(stderr, "warned\n") || strings.Contains(stdout, "warned") {
		t.Errorf("the instance's lines are not the command's, on their streams\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	container := "xynolog-" + testKBKey + "-run-cli-1"
	log := s.argv(t)
	if strings.Contains(log, secret) {
		t.Errorf("the variable's value reached argv:\n%s", log)
	}
	if v, ok := s.containerEnv(t, container, "SMELTER_TOKEN"); !ok || v != secret {
		t.Errorf("the instance's SMELTER_TOKEN is %q (%v), want the command's own", v, ok)
	}
	if !regexp.MustCompile(`docker run --name ` + regexp.QuoteMeta(container) + ` .* --publish 9124:9124 .*/usr/local/bin/xynolog-smelter --verbose\n`).MatchString(log) {
		t.Errorf("the run does not publish 9124 or give the program --verbose alone:\n%s", log)
	}

	_, stderr, code = s.run(t, "signal", "ghost", "term", "--root", s.kb, "--runtime", "docker")
	if code != 1 || !strings.Contains(stderr, "No run of ghost") {
		t.Errorf("xynolog signal of no run exited %d:\n%s", code, stderr)
	}
}

// waitForFile waits, ten seconds at most, for path to exist.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("no %s within ten seconds", path)
}

// waitForGone waits, ten seconds at most, for path to be gone.
func waitForGone(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
	}
	t.Fatalf("%s is still there ten seconds on", path)
}

// A run whose instance never starts — the runtime refused it a port — fails,
// saying the runtime's own account, and says nothing of it as started. The
// run's environment is a secret in that account too (R5).
func TestControlRunThatNeverStartsSaysWhy(t *testing.T) {
	s := newScenario(t, "container")
	const secret = "s3cret-boot-value-9"
	s.extraEnv = append(s.extraEnv, "FAKERT_RUN_BOOT_FAIL=port 9123 is in use, asked for by "+secret)
	c := startControl(t, s)
	params := runParams(s, "refused")
	params["environment"] = map[string]any{"SMELTER_TOKEN": secret}
	params["ports"] = []int{9123}
	id := c.send("service.run", params)
	read := c.readUntil("the run's answer", answerTo(id))
	answer := read[len(read)-1]
	if code := refusedWith(t, answer); code != "control.failed" {
		t.Fatalf("a run that never started answered %s, want control.failed", describe(read))
	}
	if !strings.Contains(answer.Error.Message, "port 9123 is in use") {
		t.Errorf("the failure %q does not say the runtime's account", answer.Error.Message)
	}
	for _, m := range read {
		if m.Method == "notifications/serviceStarted" {
			t.Errorf("a run that never started said it started: %s", m.Params)
		}
		if strings.Contains(string(m.Params), secret) || (m.Error != nil && strings.Contains(m.Error.Message, secret)) {
			t.Errorf("the run's environment is in what the launcher sent: %s", describe([]controlMessage{m}))
		}
	}
	if err := c.end(); err != nil {
		t.Errorf("xynolog control ended with %v", err)
	}
}

// A document needs the path the image's own arguments name: an image whose
// CMD gives no --config is refused one, and nothing is run.
func TestControlRunRefusesADocumentAnImageNamesNoPathFor(t *testing.T) {
	s := newScenario(t, "container")
	s.extraEnv = append(s.extraEnv, `FAKERT_IMAGE_CMD=["/usr/local/bin/xynolog-smelter"]`)
	c := startControl(t, s)
	params := runParams(s, "pathless")
	params["document"] = "{}"
	id := c.send("service.run", params)
	read := c.readUntil("the run's answer", answerTo(id))
	if code := refusedWith(t, read[len(read)-1]); code != "control.invalid-request" {
		t.Errorf("a document for an image with no --config answered %s, want control.invalid-request", describe(read))
	}
	if strings.Contains(s.argv(t), "container run --name") {
		t.Errorf("a refused run ran:\n%s", s.argv(t))
	}
	if err := c.end(); err != nil {
		t.Errorf("xynolog control ended with %v", err)
	}
}

// A run cancelled while its container is made and not yet started terms its
// instance once it starts, and removes it (R7).
func TestControlRunCancelledBeforeItsInstanceStartsTermsIt(t *testing.T) {
	s := newScenario(t, "container")
	container := "xynolog-" + testKBKey + "-run-early"
	s.extraEnv = append(s.extraEnv, "FAKERT_RUN_UNTIL=signal", "FAKERT_RUN_HOLD="+container)
	c := startControl(t, s)
	id := c.send("service.run", runParams(s, "early"))
	waitForFile(t, filepath.Join(s.fakertDir, "holding-"+container))
	c.cancel(id)
	read := c.readUntil("the cancelled run's answer", answerTo(id))
	if code := refusedWith(t, read[len(read)-1]); code != "control.cancelled" {
		t.Errorf("a run cancelled before it started answered %s, want control.cancelled", describe(read))
	}
	if err := os.WriteFile(filepath.Join(s.fakertDir, "release-"+container), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitForArgv(t, s, "container kill --signal TERM "+container)
	waitForGone(t, runCtrFile(s, container))
	if err := c.end(); err != nil {
		t.Errorf("xynolog control ended with %v", err)
	}
}
