package launcher

// control_ops.go — each method of the control API, run by the code its
// command runs: the command itself, given the params as the command line it
// reads (stack.start, stack.stop), or the operation the command prints
// (the rest), with the request's UI saying its lines.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/xynolog/cli/internal/launcher/stack"
	"github.com/xynolog/sdk/go/api"
)

// controlHandler runs one request's operation, saying what it does through u:
// its result, or why it failed.
type controlHandler func(req *controlRequest, u *UI) (any, *controlFailure)

// controlHandlers: the handler of each method of controlMethods
// (TestEveryControlMethodHasAHandler).
var controlHandlers = map[string]controlHandler{
	"launcher.about":      controlAbout,
	"roots.list":          controlRootsList,
	"roots.init":          controlRootsInit,
	"roots.forget":        controlRootsForget,
	"status.snapshot":     controlStatusSnapshot,
	"stack.start":         controlStackStart,
	"stack.stop":          controlStackStop,
	"auth.login":          controlAuthLogin,
	"auth.logout":         controlAuthLogout,
	"identity.useradd":    controlIdentityUseradd,
	"identity.users.list": controlIdentityUsersList,
	"logs.follow":         controlLogsFollow,
	"settings.list":       controlSettingsList,
	"settings.get":        controlSettingsGet,
	"settings.set":        controlSettingsSet,
	"settings.unset":      controlSettingsUnset,
	"settings.path":       controlSettingsPath,
	"service.host":        controlServiceHost,
	"service.run":         controlServiceRun,
	"service.signal":      controlServiceSignal,
}

// operate runs req's operation. A panic in it is the launcher's failure, and
// fails the request alone.
func (req *controlRequest) operate() (result any, failure *controlFailure) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(req.server.diag, "✗ control: %s panicked: %v\n", req.method.name, p)
			result, failure = nil, &controlFailure{code: controlCodeFailed, rpcCode: api.ControlRpcCodeInternalError,
				message: fmt.Sprintf("the launcher failed: %v", p)}
		}
	}()
	handler := controlHandlers[req.method.name]
	if req.server.handlerFor != nil {
		handler = req.server.handlerFor(req.method.name)
	}
	return handler(req, req.ui())
}

// decode reads req's params, which checkParams held to their schema, into p.
func (req *controlRequest) decode(p any) *controlFailure {
	if err := json.Unmarshal(req.params, p); err != nil {
		return &controlFailure{code: controlCodeInvalidParams, rpcCode: api.ControlRpcCodeInvalidParams,
			message: fmt.Sprintf("%s's params: %v", req.method.name, err)}
	}
	return nil
}

// stated: v's address, or nil for its zero value — an optional member of a
// result stated only when there is something to state.
func stated[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// listed: v as an optional list param, absent when it is empty.
func listed[T any](v []T) *[]T {
	if len(v) == 0 {
		return nil
	}
	return &v
}

// given: what an optional param names, "" when it is absent.
func given[T ~string](v *T) T {
	if v == nil {
		return ""
	}
	return *v
}

// asked: whether an optional flag param is given, and true.
func asked(v *bool) bool { return v != nil && *v }

func controlAbout(req *controlRequest, u *UI) (any, *controlFailure) {
	about := api.AboutResult{
		Development:   BuildVersion == developmentVersion,
		ApiVersions:   []string{controlAPIVersion},
		ImageRegistry: imageRegistry,
		Runtimes:      []api.RuntimeState{},
	}
	if !about.Development {
		about.Version = stated(BuildVersion)
	}
	if BuildCommit != unstampedCommit {
		about.Commit = stated(BuildCommit)
	}
	if BuildDate != unstampedDate {
		about.Built = stated(BuildDate)
	}
	for _, rt := range runtimeOrder {
		about.Runtimes = append(about.Runtimes, api.RuntimeState{Name: api.Runtime(rt), Found: onPath(rt)})
	}
	return about, nil
}

func controlRootsList(req *controlRequest, u *UI) (any, *controlFailure) {
	ss := LoadStackSet()
	if ss.refuseUnreadable(u) {
		return nil, failed(u, controlCodeUnreadableState)
	}
	known, warning := knownRoots(localStacks(ss))
	out := api.RootsListResult{Roots: []api.Root{}}
	if warning != nil {
		out.Warnings = &[]string{warning.Error()}
	}
	for _, r := range known {
		out.Roots = append(out.Roots, r.control())
	}
	codespaces, _ := knownCodespaces(codespaceStacks(ss))
	for _, c := range codespaces {
		out.Roots = append(out.Roots, c.control())
	}
	return out, nil
}

// control: r as roots.list states it.
func (r *knownRoot) control() api.Root {
	path, present := r.path, !r.missing
	root := api.Root{Placement: api.KbPlacementLocal, Path: &path, Present: &present,
		Current: stated(r.current), Registered: r.entry != nil, Running: r.running,
		Did: stated(r.did), SiteName: stated(r.siteName), Config: stated(r.config)}
	if r.identityErr != nil {
		root.IdentityError = stated(r.identityErr.Error())
	}
	if r.entry != nil && !r.entry.LastUsed.IsZero() {
		root.LastUsed = stated(r.entry.LastUsed.UTC().Format(time.RFC3339))
	}
	if present {
		root.Configs = listed(stack.InferenceNames(r.path))
	}
	return root
}

// control: c as roots.list states it.
func (c knownCodespace) control() api.Root {
	cs := c.stack.Codespace
	root := api.Root{Placement: api.KbPlacementCodespace, Running: true,
		Repo: stated(cs.Repo), Codespace: stated(cs.Name), Did: stated(c.stack.KBDid), CodespaceState: stated(c.state)}
	if cs.ForwardPort != 0 {
		port, forwarded, reachable := cs.ForwardPort, c.forwarded, c.reachable
		root.ForwardPort, root.Forwarded, root.Reachable = &port, &forwarded, &reachable
	}
	return root
}

func controlRootsForget(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.RootsForgetParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	f, ok := forgetRoot(u, p.Root)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	return api.RootsForgetResult{Path: f.path, DirectoryMissing: f.directoryMissing, PersistentState: stated(f.persistentState)}, nil
}

func controlStatusSnapshot(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.StatusSnapshotParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	if refuseCodespaceRuntime(u, p.Target, p.Runtime) {
		return nil, failed(u, controlCodeInvalidRequest)
	}
	ss := LoadStackSet()
	if ss.refuseUnreadable(u) {
		return nil, failed(u, controlCodeUnreadableState)
	}
	if p.Target != nil && p.Target.Kind == "codespace" {
		st, ok := repoCodespaceStack(u, ss, *p.Target.Repo)
		if !ok {
			return nil, failed(u, controlCodeFailed)
		}
		return statusSnapshot(ss, nil, kbRoot{}, []*StackState{st}), nil
	}
	root := ""
	if p.Target != nil {
		root = given(p.Target.Root)
	}
	chosen, ok := reportedStacks(u, ss, root)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	views, ok := statusViews(u, chosen, string(given(p.Runtime)))
	if !ok {
		return nil, failed(u, controlCodeRuntimeUnavailable)
	}
	var codespaces []*StackState
	if chosen.every && p.Target == nil {
		codespaces = codespaceStacks(ss)
	}
	return statusSnapshot(ss, views, chosen.cwd, codespaces), nil
}

// refuseCodespaceRuntime refuses a runtime named for a codespace target: a
// codespace's own launcher chooses its runtime.
func refuseCodespaceRuntime(u *UI, target *api.Target, runtime *api.Runtime) bool {
	if target != nil && target.Kind == "codespace" && runtime != nil {
		u.Refuse(controlCodeInvalidRequest, "runtime names a local stack's container runtime, and the target is a codespace's stack, whose own launcher chooses its runtime.")
		return true
	}
	return false
}

// refuseUnknownSelection refuses a service or a group the launcher has none
// of, which xynolog start refuses in the same words.
func refuseUnknownSelection(u *UI, service *string, group *api.ServiceGroup) bool {
	if service != nil && !knownRole(*service) {
		u.Refuse(controlCodeInvalidService, "Unknown --service '%s' (expected: %s)", *service, roleList)
		return true
	}
	if group != nil {
		if _, known := groupNamed(string(*group)); !known {
			u.Refuse(controlCodeInvalidService, "Unknown --group '%s' (expected: %s)", *group, groupList)
			return true
		}
	}
	return false
}

// targetArgs: the flags that name target, as xynolog start and xynolog stop
// take them. A codespace target is --repo; start names its platform too.
func targetArgs(target *api.Target, platform bool) []string {
	switch {
	case target == nil:
		return nil
	case target.Kind == "codespace" && platform:
		return []string{"--runtime", "codespace", "--repo", *target.Repo}
	case target.Kind == "codespace":
		return []string{"--repo", *target.Repo}
	case target.Root != nil:
		return []string{"--root", *target.Root}
	}
	return nil
}

// valueFlag: a flag that takes a value, and the param that gives it.
type valueFlag struct {
	flag  string
	value *string
}

// textOf: the text of an optional enumerated param, nil when it is absent.
func textOf[T ~string](v *T) *string {
	if v == nil {
		return nil
	}
	text := string(*v)
	return &text
}

// valueArgs: each flag whose param is given, and its value.
func valueArgs(flags ...valueFlag) []string {
	var args []string
	for _, f := range flags {
		if f.value != nil {
			args = append(args, f.flag, *f.value)
		}
	}
	return args
}

// startArgs: the command line of the start p asks for, as xynolog start reads
// a person's (protocol/specs/src/control/cli.json maps each flag).
func startArgs(p api.StackStartParams) []string {
	args := targetArgs(p.Target, true)
	args = append(args, valueArgs(
		valueFlag{"--runtime", textOf(p.Runtime)},
		valueFlag{"--config", p.Config},
		valueFlag{"--service", p.Service},
		valueFlag{"--group", textOf(p.Group)},
		valueFlag{"--codespace", p.CodespaceName},
		valueFlag{"--machine", p.Machine},
		valueFlag{"--idle-timeout", p.IdleTimeout},
		valueFlag{"--retention-period", p.RetentionPeriod},
		valueFlag{"--ollama-cache", textOf(p.OllamaCache)},
	)...)
	if p.Observe != nil && !*p.Observe {
		args = append(args, "--no-observe")
	}
	if asked(p.DryRun) {
		args = append(args, "--dry-run")
	}
	return args
}

// stopArgs: the command line of the stop p asks for, as xynolog stop reads a
// person's.
func stopArgs(p api.StackStopParams) []string {
	args := targetArgs(p.Target, false)
	args = append(args, valueArgs(
		valueFlag{"--runtime", textOf(p.Runtime)},
		valueFlag{"--service", p.Service},
		valueFlag{"--group", textOf(p.Group)},
	)...)
	if asked(p.Delete) {
		args = append(args, "--delete")
	}
	if asked(p.DryRun) {
		args = append(args, "--dry-run")
	}
	return args
}

func controlStackStart(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.StackStartParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	if refuseCodespaceRuntime(u, p.Target, p.Runtime) {
		return nil, failed(u, controlCodeInvalidRequest)
	}
	if refuseUnknownSelection(u, p.Service, p.Group) {
		return nil, failed(u, controlCodeInvalidService)
	}
	args := startArgs(p)
	opts, _, _ := parseStart(args)
	code := Start(u, args)
	releaseStartLock()
	if code != 0 {
		return nil, failed(u, controlCodeFailed)
	}
	if opts.dryRun {
		return api.StackStartResult{DryRun: true}, nil
	}
	return startedStatus(u, opts, p.Target), nil
}

// startedStatus: what status reads after a start of opts: the knowledge base
// it started, when it started one, and the shared services.
func startedStatus(u *UI, opts startOptions, target *api.Target) api.StackStartResult {
	ss := LoadStackSet()
	if opts.platform == platformCodespace {
		out := api.StackStartResult{Shared: listed(sharedStatuses(nil, ss))}
		if st := codespaceStack(ss, opts.repo); st != nil {
			kb := codespaceStatus(st, true, publishedKBs(ss), LoadTokens())
			out.KnowledgeBase = &kb
		}
		return out
	}
	var started kbRoot
	if opts.readsConfig() {
		started = startedRoot(target)
	}
	if started.dir == "" {
		return api.StackStartResult{Shared: listed(sharedStatuses(nil, ss))}
	}
	v, code := newStackView(u, localStack(ss, started.key()), started.key(), opts.runtime)
	if code != 0 {
		return api.StackStartResult{Shared: listed(sharedStatuses(nil, ss))}
	}
	views := []stackView{v}
	nameViews(views)
	snapshot := statusSnapshot(ss, views, started, nil)
	out := api.StackStartResult{Shared: listed(snapshot.Shared)}
	if len(snapshot.KnowledgeBases) == 1 {
		out.KnowledgeBase = &snapshot.KnowledgeBases[0]
	}
	return out
}

// startedRoot: the knowledge base a start of target ran: the root it names,
// else the one the start entered, which its turn still stands in.
func startedRoot(target *api.Target) kbRoot {
	if target != nil && target.Root != nil {
		dir, ident, err := resolveRootArg(*target.Root)
		if err != nil {
			return kbRoot{}
		}
		return kbRoot{dir: dir, ident: ident}
	}
	kb, err := cwdKBRoot()
	if err != nil {
		return kbRoot{}
	}
	return kb
}

func controlStackStop(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.StackStopParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	if refuseCodespaceRuntime(u, p.Target, p.Runtime) {
		return nil, failed(u, controlCodeInvalidRequest)
	}
	switch {
	case asked(p.Delete) && !asked(p.DeleteConfirmed):
		u.Refuse(controlCodeConfirmationRequired, "delete destroys the codespace, state and all: send it with deleteConfirmed once a person has confirmed it.")
		return nil, failed(u, controlCodeConfirmationRequired)
	case asked(p.DeleteConfirmed) && !asked(p.Delete):
		u.Refuse(controlCodeInvalidRequest, "deleteConfirmed confirms a delete, and the request asks for none.")
		return nil, failed(u, controlCodeInvalidRequest)
	}
	if Stop(u, stopArgs(p)) != 0 {
		return nil, failed(u, controlCodeFailed)
	}
	if asked(p.DryRun) {
		return api.StackStopResult{DryRun: true}, nil
	}
	ss := LoadStackSet()
	var views []stackView
	for _, st := range localStacks(ss) {
		v, code := newStackView(u, st, rootKey(st.KBDid, st.KBRoot), "")
		if code != 0 {
			return nil, failed(u, controlCodeRuntimeUnavailable)
		}
		views = append(views, v)
	}
	nameViews(views)
	snapshot := statusSnapshot(ss, views, kbRoot{}, codespaceStacks(ss))
	return api.StackStopResult{KnowledgeBases: listed(snapshot.KnowledgeBases), Shared: listed(snapshot.Shared)}, nil
}

// verbTarget: the stack a knowledge verb of target and runtime acts on, as
// SelectVerbStack chooses it for the command line's --repo, --root and
// --runtime; nil when none is recorded and none was named. A local target
// keeps to the local stacks, as --runtime does. ok=false: refused, and said.
func verbTarget(u *UI, verb string, target *api.Target, runtime *api.Runtime) (*StackState, bool) {
	repo, root, wantLocal := "", "", runtime != nil
	if target != nil {
		repo, root = given(target.Repo), given(target.Root)
		wantLocal = wantLocal || target.Kind == "local"
	}
	return SelectVerbStack(u, verb, LoadStackSet(), repo, root, wantLocal)
}

func controlAuthLogin(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.AuthLoginParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	target, ok := verbTarget(u, "login", p.Target, p.Runtime)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	base, ok := StackBase(u, target, "login")
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	key := StackKey(target)
	if held, have := LoadTokens()[key]; have && storedTokenState(base, held.Token) == api.SessionStateValid {
		u.Refuse(controlCodeAlreadySignedIn, "Already signed in to %s: the gateway accepts the stored session.", key)
		return nil, failed(u, controlCodeAlreadySignedIn)
	}
	signedIn, ok := signIn(req.ctx, u, target, func(code api.DeviceCode) {
		action := api.UserAction{RequestId: req.id, VerificationUri: code.VerificationUri,
			VerificationUriComplete: code.VerificationUriComplete, UserCode: code.UserCode,
			ExpiresInSeconds: code.ExpiresIn}
		req.server.notify(req, "notifications/userAction", action)
		u.Log("Waiting for approval at the issuer...")
		// The wait is a person's: the next request takes its turn meanwhile.
		req.endTurn()
	})
	if !ok {
		return nil, failed(u, controlCodeSignInFailed)
	}
	return signedIn, nil
}

func controlAuthLogout(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.AuthLogoutParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	target, ok := verbTarget(u, "logout", p.Target, p.Runtime)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	ended, ok := signOut(req.ctx, u, target)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	return ended, nil
}

func controlIdentityUseradd(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.IdentityUseraddParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	target, ok := rootLocalStack(u, LoadStackSet(), p.Target.Root)
	return controlIdentityUseraddForTarget(u, p, target, ok, useraddLocal)
}

type controlUseraddRunner func(*UI, useraddOpts, string, string, string) (useraddResult, int)

func controlIdentityUseraddForTarget(u *UI, p api.IdentityUseraddParams, target *StackState, ok bool, run controlUseraddRunner) (any, *controlFailure) {
	o := useraddOpts{email: p.Email, generate: p.GeneratePassword}
	password := given(p.Password)
	if !p.GeneratePassword {
		if !useraddValidate(u, o) {
			return nil, failed(u, controlCodeInvalidParams)
		}
		if password == "" {
			u.Refuse(controlCodeInvalidParams, "Supply a password or ask the launcher to generate one.")
			return nil, failed(u, controlCodeInvalidParams)
		}
		// Error messages and progress must never repeat a supplied password.
		noteSecret(password)
	} else if !useraddValidate(u, o) {
		return nil, failed(u, controlCodeInvalidParams)
	}
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	if target == nil || target.KBRoot == "" || target.Codespace != nil {
		u.Refuse(controlCodeStackNotRunning, "No local stack for the selected knowledge base is running, so there is no realm to administer.")
		return nil, failed(u, controlCodeStackNotRunning)
	}
	result, code := run(u, o, password, target.KBRoot, target.KBDid)
	if code != 0 {
		return nil, failed(u, controlCodeFailed)
	}
	return api.IdentityUseraddResult{
		Email: result.email, Identity: result.identity, Created: result.created,
		GeneratedPassword: stated(result.generatedPassword),
	}, nil
}

func controlIdentityUsersList(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.IdentityUsersListParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	target, ok := rootLocalStack(u, LoadStackSet(), p.Target.Root)
	return controlIdentityUsersListForTarget(u, target, ok, resolveRealmAdmin, listRealmUsers)
}

type controlRealmAdminResolver func(*UI, string, string) (realmAdmin, bool)
type controlRealmUserLister func(string, string, string) ([]realmUser, error)

func controlIdentityUsersListForTarget(u *UI, target *StackState, ok bool, resolve controlRealmAdminResolver, list controlRealmUserLister) (any, *controlFailure) {
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	if target == nil || target.KBRoot == "" || target.Codespace != nil {
		u.Refuse(controlCodeStackNotRunning, "No local stack for the selected knowledge base is recorded, so there is no realm to list.")
		return nil, failed(u, controlCodeStackNotRunning)
	}
	admin, ok := resolve(u, target.KBRoot, target.KBDid)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	accounts, err := list(admin.base, admin.realm, admin.token)
	if err != nil {
		u.Fail("%v", err)
		return nil, failed(u, controlCodeFailed)
	}
	users := make([]api.IdentityUser, 0, len(accounts))
	for _, account := range accounts {
		users = append(users, api.IdentityUser{
			Email: account.email, Identity: accountIdentity(admin, account.email, account.id), Enabled: account.enabled,
		})
	}
	return api.IdentityUsersListResult{Users: users}, nil
}

func controlLogsFollow(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.LogsFollowParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	if refuseCodespaceRuntime(u, p.Target, p.Runtime) {
		return nil, failed(u, controlCodeInvalidRequest)
	}
	repo, root := "", ""
	if p.Target != nil {
		repo, root = given(p.Target.Repo), given(p.Target.Root)
	}
	src, ok := logSources(u, string(given(p.Runtime)), given(p.Service), repo, root)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	// A follow runs until its sources end or it is cancelled: the next
	// request takes its turn meanwhile.
	req.endTurn()
	followLogs(req.ctx, src, func(service, stream, line string) {
		req.server.notify(req, "notifications/log", api.LogRecord{RequestId: req.id, Service: service, Stream: api.OutputStream(stream),
			Line: scrubSecrets(line), ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	})
	return api.LogsFollowResult{Services: src.follow}, nil
}

func controlSettingsList(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.SettingsListParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	kb, rootErr, ok := settingsRoot(u, given(p.Root))
	if !ok {
		return nil, failed(u, controlCodeRootNotFound)
	}
	out := api.SettingsListResult{Settings: []api.Setting{}}
	if rootErr != nil {
		out.Unresolved = stated(rootErr.Error())
	} else {
		out.Root = stated(kb.dir)
	}
	for _, appliesTo := range []api.SettingAppliesTo{api.SettingAppliesToMachine, api.SettingAppliesToKnowledgeBase} {
		if appliesTo == api.SettingAppliesToKnowledgeBase && rootErr != nil {
			continue
		}
		for _, s := range settingsTable {
			if s.appliesTo == appliesTo {
				out.Settings = append(out.Settings, controlSetting(u, s, kb))
			}
		}
	}
	return out, nil
}

// controlSetting: s, in kb, as the control API states a setting.
func controlSetting(u *UI, s setting, kb kbRoot) api.Setting {
	out := api.Setting{Name: s.name, Default: s.flag == "--default", AppliesTo: s.appliesTo,
		Values: []api.SettingLine{}, Settable: s.set != nil}
	for _, l := range s.show(u, kb) {
		out.Values = append(out.Values, api.SettingLine{Value: l.value, Source: l.source})
	}
	if s.changedBy != nil {
		out.ChangedBy = stated(s.changedBy())
	}
	return out
}

// askedSetting: the setting name names, its machine-wide form when dflt, and
// the knowledge base it is read in, with why none resolved. ok=false:
// refused, and said.
func askedSetting(u *UI, name string, dflt *bool, root *string) (setting, kbRoot, error, bool) {
	flag := ""
	if asked(dflt) {
		flag = "--default"
	}
	rows, ok := settingsNamed(u, name, flag)
	if !ok {
		return setting{}, kbRoot{}, nil, false
	}
	kb, rootErr, ok := settingsRoot(u, given(root))
	if !ok {
		return setting{}, kbRoot{}, nil, false
	}
	return rows[0], kb, rootErr, true
}

func controlSettingsGet(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.SettingsGetParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	s, kb, rootErr, ok := askedSetting(u, p.Name, p.Default, p.Root)
	if !ok || refuseUnrooted(u, s, rootErr) {
		return nil, failed(u, controlCodeFailed)
	}
	return controlSetting(u, s, kb), nil
}

func controlSettingsSet(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.SettingsSetParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	s, kb, rootErr, ok := askedSetting(u, p.Name, p.Default, p.Root)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	if !changeSetting(u, s, kb, rootErr, p.Value, false) {
		return nil, failed(u, controlCodeInvalidValue)
	}
	return controlSetting(u, s, kb), nil
}

func controlSettingsUnset(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.SettingsUnsetParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	s, kb, rootErr, ok := askedSetting(u, p.Name, p.Default, p.Root)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	if !changeSetting(u, s, kb, rootErr, "", true) {
		return nil, failed(u, controlCodeFailed)
	}
	return controlSetting(u, s, kb), nil
}

func controlSettingsPath(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.SettingsPathParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	path, ok := machineDocuments[string(p.Document)](u)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	return api.SettingsPathResult{Path: path}, nil
}

func controlServiceHost(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.ServiceHostParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	rt, ok := SelectRuntime(u, string(given(p.Runtime)))
	if !ok {
		return nil, failed(u, controlCodeRuntimeUnavailable)
	}
	addr, ok := hostAddress(u, rt)
	if !ok {
		return nil, failed(u, controlCodeRuntimeUnavailable)
	}
	return api.ServiceHostResult{Runtime: api.Runtime(rt), Address: addr}, nil
}

func controlServiceRun(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.ServiceRunParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	// A run's variables and its document are the caller's secrets (R5): never
	// in progress, an error's message or a diagnostic. The instance's own
	// lines carry them as it writes them.
	for _, v := range p.Environment {
		noteSecret(v)
	}
	if p.Document != nil {
		noteSecret(*p.Document)
	}
	ended, ok := runService(req.ctx, u, runSpecOf(p), runSink{
		started: func(s runStarted) {
			req.server.notify(req, "notifications/serviceStarted", api.ServiceStarted{RequestId: req.id, Instance: p.Instance,
				Container: s.container, DocumentPath: stated(s.documentPath), Address: s.address})
			// The instance runs until it exits: the next request takes its
			// turn meanwhile (C2).
			req.endTurn()
		},
		output: func(stream, line string) {
			req.server.notify(req, "notifications/serviceOutput", api.ServiceOutput{RequestId: req.id, Instance: p.Instance,
				Stream: api.OutputStream(stream), Line: line})
		},
	})
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	return api.ServiceRunResult{Instance: p.Instance, ExitCode: ended.exitCode, RanMs: ended.ranMs}, nil
}

func controlServiceSignal(req *controlRequest, u *UI) (any, *controlFailure) {
	var p api.ServiceSignalParams
	if f := req.decode(&p); f != nil {
		return nil, f
	}
	delivered, ok := signalService(u, p.Target.Root, string(given(p.Runtime)), p.Instance, p.Signal)
	if !ok {
		return nil, failed(u, controlCodeFailed)
	}
	return api.ServiceSignalResult{Delivered: delivered}, nil
}
