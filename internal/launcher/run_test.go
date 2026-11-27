package launcher

import (
	"strings"
	"testing"

	"github.com/xynolog/sdk/go/api"
)

// A run's env-file carries its variables and the PATH it puts directories
// before, and refuses what the file cannot carry and what the run's own
// rules give: the supervisor (R1), and PATH stated twice (R2).
func TestRunEnvironment(t *testing.T) {
	alpine := imageRun{cmd: []string{"/usr/local/bin/xynolog-smelter", "--config", "/etc/xynolog/smelter.json"}, path: "/usr/bin:/bin"}
	u := controlTestUI()
	text, ok := runEnvironment(u, runSpec{environment: map[string]string{"B": "two", "A": "one"}, pathFirst: []string{"/t", "/r"}}, alpine)
	if want := "A=one\nB=two\nPATH=/t:/r:/usr/bin:/bin\n"; !ok || text != want {
		t.Errorf("runEnvironment gave %q (%v), want %q", text, ok, want)
	}
	for _, c := range []struct {
		name  string
		spec  runSpec
		image imageRun
		code  api.ControlErrorCode
	}{
		{"the supervisor", runSpec{environment: map[string]string{"XYNOLOG_SUPERVISE": "1"}}, alpine, api.ControlErrorCodeInvalidRequest},
		{"a name holding =", runSpec{environment: map[string]string{"A=B": "x"}}, alpine, api.ControlErrorCodeInvalidRequest},
		{"a name holding a line break", runSpec{environment: map[string]string{"A\nB": "x"}}, alpine, api.ControlErrorCodeInvalidRequest},
		{"a value holding a line break", runSpec{environment: map[string]string{"A": "x\ny"}}, alpine, api.ControlErrorCodeInvalidRequest},
		{"PATH beside pathFirst", runSpec{environment: map[string]string{"PATH": "/x"}, pathFirst: []string{"/t"}}, alpine, api.ControlErrorCodeInvalidRequest},
		{"pathFirst with an image that states no PATH", runSpec{pathFirst: []string{"/t"}}, imageRun{cmd: alpine.cmd}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := controlTestUI()
			if text, ok := runEnvironment(u, c.spec, c.image); ok {
				t.Fatalf("runEnvironment gave %q, want a refusal", text)
			}
			if !u.refused.made || u.refused.code != c.code {
				t.Errorf("refused with %+v, want code %q", *u.refused, c.code)
			}
		})
	}
}

// A document's path is the value of the image's own --config, in either of
// its spellings.
func TestImageRunDocumentPath(t *testing.T) {
	for _, c := range []struct {
		cmd  []string
		path string
		ok   bool
	}{
		{[]string{"/bin/x", "--config", "/etc/x.json"}, "/etc/x.json", true},
		{[]string{"/bin/x", "--config=/etc/x.json"}, "/etc/x.json", true},
		{[]string{"/bin/x", "--config"}, "", false},
		{[]string{"/bin/x"}, "", false},
	} {
		if path, ok := (imageRun{cmd: c.cmd}).documentPath(); path != c.path || ok != c.ok {
			t.Errorf("%s: documentPath %q %v, want %q %v", strings.Join(c.cmd, " "), path, ok, c.path, c.ok)
		}
	}
}
