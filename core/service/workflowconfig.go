package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/config"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// Every mutating workflow configuration this product has, reachable from
// something other than a text editor (#813).
//
// # Why these are here at all, when config.yaml already holds them
//
// The same argument settings.go makes for retention and capacity, and it
// applies harder here. A patch through this package is HOT-RELOADED into
// the process that served it; an edit to config.yaml is not, because
// nothing watches that file. So on the one deployment where this matters
// -- a NAS with a daemon serving, which is every real one -- "edit the
// file" is advice that leaves the change unread until somebody restarts.
// EPIC G's rule is that every capability is reachable from `backupd`
// as well as from a browser, and a browser-only workflow configuration is
// one nobody can roll out across a fleet.
//
// The READ is here for a second reason: it reports the RESOLVED
// configuration -- the timeout a set will actually use, inherited or not;
// the environment a hook will actually see, merged -- which a
// re-serialization of the file cannot, because the file's whole point is
// that it omits what it inherits.
//
// # The one rule that governs every shape in this file
//
// A secret is a LOCATION and never a value. Every env surface here
// carries config.SecretSource's three spellings -- a file path, a
// variable NAME, an argv -- and there is no field anywhere that a
// resolved secret could be written into or read out of. That is not a
// convention: it is the type. internal/secretref resolves a reference to
// an obs.Secret at the moment a hook is about to be executed, inside
// internal/workflow's Resolved, and nothing in this package ever calls
// it.
//
// The consequence worth stating, because it looks like a gap: `env list`
// cannot show you the value of a secret variable, ever, on any surface.
// It shows the name and where the value comes from. A surface that could
// print it would be a surface an attacker with a session can read every
// credential in the deployment from.

// ErrWorkflowsNotConfigured is what a per-set workflow write reports for
// a deployment that has no workflow root.
//
// Its own sentinel rather than ErrInvalidRequest, because the request is
// fine and the deployment is not ready: the remedy is `settings workflow
// patch --root`, which is a different action from correcting a field.
var ErrWorkflowsNotConfigured = errors.New("service: this deployment configures no workflow root, so there is no approved tree for hook scripts to live in")

// ErrWorkflowEnvNotFound is what an unset reports for a variable that is
// not there.
//
// A refusal rather than a silent success, because "unset PGPASSWORD" that
// quietly does nothing is indistinguishable, from a script, from one that
// worked -- and the case it hides is the one that matters: an operator
// clearing a credential from the wrong scope and believing they have
// cleared it.
var ErrWorkflowEnvNotFound = errors.New("service: no workflow environment variable of that name is configured here")

// WorkflowSecretRef is where a workflow environment value comes from.
//
// Exactly one of the three is set. It is the same file/env/command triple
// every other credential reference in this product uses, spelled the same
// way, so an operator who has configured a repository passphrase already
// knows this one.
type WorkflowSecretRef struct {
	File    string
	Env     string
	Command []string
}

// IsZero reports whether this names no location, which is what a literal
// variable carries.
func (r WorkflowSecretRef) IsZero() bool {
	return r.File == "" && r.Env == "" && len(r.Command) == 0
}

func (r WorkflowSecretRef) sources() int {
	n := 0
	for _, set := range []bool{r.File != "", r.Env != "", len(r.Command) != 0} {
		if set {
			n++
		}
	}

	return n
}

// WorkflowEnvVar is one configured environment entry, as every read and
// write surface carries it.
type WorkflowEnvVar struct {
	Name string

	// Value is the literal. HasValue is what tells a configured empty
	// string apart from a variable with no literal at all, which is the
	// same distinction config.EnvironmentVariable's pointer exists for:
	// `value: ""` is a deliberately empty variable and operators write
	// it.
	Value    string
	HasValue bool

	// Secret is WHERE the value comes from when it is not a literal. It
	// is never the value; see this file's doc.
	Secret WorkflowSecretRef
}

// WorkflowRunnerSettings is the engine's side of the host runner socket,
// reported so an operator can see what this process will try to reach.
//
// Reported and not writable here. The two paths are how THIS PROCESS sees
// the host, and they differ between a container and a bare-metal install
// of the same deployment (config.WorkflowRunner's own doc), so they are a
// deployment-shape fact the installer writes rather than a policy an
// operator tunes. Every other path of that class -- the SSH key, the
// known_hosts file, the state database -- is config-file-only for the
// same reason.
type WorkflowRunnerSettings struct {
	Configured bool
	Socket     string
	TokenFile  string
}

// WorkflowSettings is the deployment-wide workflow configuration,
// resolved.
type WorkflowSettings struct {
	// Configured is whether this deployment runs workflows at all, which
	// is exactly "is a root set" and is answered in one place
	// (config.WorkflowsConfigured).
	Configured bool

	Root      string
	BeforeDir string
	AfterDir  string

	// ScriptTimeout is the bound a hook gets when its backup set does not
	// say. It is RESOLVED: a deployment that configures none reports
	// internal/workflow's default rather than a zero an operator would
	// read as "no bound".
	ScriptTimeout time.Duration

	// ScriptTimeoutConfigured says the resolved value above came from the
	// file rather than from the default, which is the difference between
	// "we chose five minutes" and "nobody has chosen".
	ScriptTimeoutConfigured bool

	MaxScriptSizeBytes int64

	Environment []WorkflowEnvVar

	// ExecConnections names the declared remote execution connections a
	// backup set's remote_exec_connection_ref may point at. Names only:
	// each one carries a whole Remote, credentials included, and this is
	// a read surface.
	ExecConnections []string

	Runner WorkflowRunnerSettings
}

// UpdateWorkflowSettingsRequest patches the deployment-wide block.
//
// Every field is a pointer, for buildSettingsPatch's reason: this is a
// PATCH, so "the caller did not mention this" and "the caller wants this
// cleared" are different requests, and a plain string can only say one of
// them. Clearing a stage directory is a real operation -- it disables
// that stage -- so it must be expressible.
type UpdateWorkflowSettingsRequest struct {
	Root      *string
	BeforeDir *string
	AfterDir  *string

	// ScriptTimeout of zero clears the deployment's own bound, which
	// means hooks fall back to internal/workflow's default. A NEGATIVE
	// one is refused by config.Validate rather than here, so this
	// package holds no second copy of that rule.
	ScriptTimeout *time.Duration

	MaxScriptSizeBytes *int64
}

func (r UpdateWorkflowSettingsRequest) isEmpty() bool {
	return r.Root == nil && r.BeforeDir == nil && r.AfterDir == nil &&
		r.ScriptTimeout == nil && r.MaxScriptSizeBytes == nil
}

// BackupSetWorkflow is one backup set's workflow configuration, resolved.
type BackupSetWorkflow struct {
	BackupSetID string

	// Configured says this set has a workflow block of its own. A set
	// that says nothing still runs the deployment's global hooks, which
	// is why this is not the same question as "does this set run hooks".
	Configured bool

	BeforeDir string
	AfterDir  string

	// ScriptTimeout is this set's OWN bound, zero when it inherits, and
	// EffectiveScriptTimeout is what a hook will actually get. Both,
	// because an operator changing the deployment default needs to know
	// which sets are pinned and which will follow.
	ScriptTimeout          time.Duration
	EffectiveScriptTimeout time.Duration

	RemoteExecConnectionRef string

	// Environment is this set's OWN entries. The merged view a hook sees
	// is ResolvedEnvironmentNames below, because the merge includes the
	// deployment's layer and the sanitized baseline.
	Environment []WorkflowEnvVar

	// ResolvedEnvironmentNames is every variable a hook of this set will
	// have, in sorted order, NAMES ONLY.
	//
	// Names only for this file's standing reason, and it is not a
	// limitation here: the question this answers is "which of my
	// variables win", and a name plus the layer it came from answers it.
	ResolvedEnvironmentNames []string

	// Stages are the hook directories this set will actually run, in
	// execution order, resolved from the deployment's and this set's.
	Stages []WorkflowStage
}

// WorkflowStage is one scope-and-phase pair that has a directory.
type WorkflowStage struct {
	Scope string
	Phase string
	Dir   string
}

// UpdateBackupSetWorkflowRequest patches one set's block.
type UpdateBackupSetWorkflowRequest struct {
	BeforeDir               *string
	AfterDir                *string
	ScriptTimeout           *time.Duration
	RemoteExecConnectionRef *string
}

func (r UpdateBackupSetWorkflowRequest) isEmpty() bool {
	return r.BeforeDir == nil && r.AfterDir == nil &&
		r.ScriptTimeout == nil && r.RemoteExecConnectionRef == nil
}

// WorkflowSettings reports the deployment-wide workflow configuration as
// this process is currently running it.
func (b *BackupService) WorkflowSettings(_ context.Context) (WorkflowSettings, error) {
	cfg := b.state.Load().inner.Config

	return workflowSettingsOf(cfg), nil
}

func workflowSettingsOf(cfg *config.Config) WorkflowSettings {
	w := cfg.Workflows

	s := WorkflowSettings{
		Configured:              cfg.WorkflowsConfigured(),
		Root:                    w.Root,
		BeforeDir:               w.Global.BeforeDir,
		AfterDir:                w.Global.AfterDir,
		ScriptTimeout:           cfg.EffectiveScriptTimeout(nil),
		ScriptTimeoutConfigured: w.ScriptTimeout.Duration() > 0,
		MaxScriptSizeBytes:      cfg.EffectiveMaxScriptSize(),
		Environment:             toWorkflowEnvVars(w.Environment),
		Runner: WorkflowRunnerSettings{
			Configured: w.Runner.Configured(),
			Socket:     w.Runner.Socket,
			TokenFile:  w.Runner.TokenFile,
		},
	}

	for _, c := range w.ExecConnections {
		s.ExecConnections = append(s.ExecConnections, c.Name)
	}

	return s
}

// UpdateWorkflowSettings patches the deployment-wide block and
// hot-reloads this service.
func (b *BackupService) UpdateWorkflowSettings(_ context.Context, req UpdateWorkflowSettingsRequest) (WorkflowSettings, error) {
	if b.configPath == "" {
		return WorkflowSettings{}, ErrConfigNotFileBacked
	}
	if req.isEmpty() {
		return WorkflowSettings{}, fmt.Errorf("%w: this patch changes nothing; name at least one of root, before_dir, after_dir, script_timeout or max_script_size_bytes", ErrInvalidRequest)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	// Re-read from disk, the "always read fresh" discipline every
	// configuration write in this package shares (mediums.go,
	// repositorydomain.go): the write below is based on the file's actual
	// current content, so a hand edit made since this service loaded is
	// not lost by a patch that does not touch it.
	cfg, err := config.Load(b.configPath)
	if err != nil {
		return WorkflowSettings{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	w := &cfg.Workflows
	if req.Root != nil {
		w.Root = strings.TrimSpace(*req.Root)
	}
	if req.BeforeDir != nil {
		w.Global.BeforeDir = strings.TrimSpace(*req.BeforeDir)
	}
	if req.AfterDir != nil {
		w.Global.AfterDir = strings.TrimSpace(*req.AfterDir)
	}
	if req.ScriptTimeout != nil {
		w.ScriptTimeout = config.Duration(*req.ScriptTimeout)
	}
	if req.MaxScriptSizeBytes != nil {
		w.MaxScriptSizeBytes = *req.MaxScriptSizeBytes
	}

	// The shared encode / validate / write / hot-reload tail. Every rule
	// about what these fields may hold -- an absolute root, no "..", a
	// positive timeout, a size under the ceiling -- is config.Validate's
	// and comes back as ErrInvalidRequest with that package's own
	// sentence, so this method holds no second copy of any of them.
	if err := b.persistConfig(cfg); err != nil {
		return WorkflowSettings{}, err
	}

	return workflowSettingsOf(b.state.Load().inner.Config), nil
}

// BackupSetWorkflow reports one set's workflow configuration, resolved
// against the deployment's.
func (b *BackupService) BackupSetWorkflow(_ context.Context, id string) (BackupSetWorkflow, error) {
	cfg := b.state.Load().inner.Config

	bs, err := lookupConfiguredBackupSet(cfg, id)
	if err != nil {
		return BackupSetWorkflow{}, err
	}

	return backupSetWorkflowOf(cfg, bs), nil
}

func backupSetWorkflowOf(cfg *config.Config, bs config.BackupSet) BackupSetWorkflow {
	out := BackupSetWorkflow{
		BackupSetID:            bs.ID.String(),
		Configured:             bs.Workflow != nil,
		EffectiveScriptTimeout: cfg.EffectiveScriptTimeout(&bs),
		Environment:            toWorkflowEnvVars(bs.Environment),
	}

	if bs.Workflow != nil {
		out.BeforeDir = bs.Workflow.BeforeDir
		out.AfterDir = bs.Workflow.AfterDir
		out.ScriptTimeout = bs.Workflow.ScriptTimeout.Duration()
		out.RemoteExecConnectionRef = bs.Workflow.RemoteExecConnectionRef
	}

	for _, st := range cfg.WorkflowStagesFor(&bs) {
		out.Stages = append(out.Stages, WorkflowStage{
			Scope: string(st.Scope),
			Phase: string(st.Phase),
			Dir:   st.Dir,
		})
	}

	// The merged view, names only. It is config.Validate's own merge
	// (resolveBackupSetWorkflowEnvironments) read back rather than
	// recomputed here, so what this reports is exactly what a hook will
	// be handed -- a second merge in this file would be a second
	// precedence order.
	out.ResolvedEnvironmentNames = bs.WorkflowEnvironment.Names()

	return out
}

// UpdateBackupSetWorkflow patches one set's block and hot-reloads.
//
// A patch on a set that has NO workflow block creates one, which is the
// only sensible reading of "set this set's before_dir": there is nothing
// else the request could mean. It is created with only the fields the
// request named, so a set given a before_dir does not silently acquire a
// pinned timeout copied from today's global value -- config.SetWorkflow's
// own doc argues that inheritance must follow the deployment rather than
// snapshot it.
func (b *BackupService) UpdateBackupSetWorkflow(_ context.Context, id string, req UpdateBackupSetWorkflowRequest) (BackupSetWorkflow, error) {
	if b.configPath == "" {
		return BackupSetWorkflow{}, ErrConfigNotFileBacked
	}
	if req.isEmpty() {
		return BackupSetWorkflow{}, fmt.Errorf("%w: this patch changes nothing; name at least one of before_dir, after_dir, script_timeout or remote_exec_connection", ErrInvalidRequest)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return BackupSetWorkflow{}, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	bs, err := findBackupSetForWrite(cfg, id)
	if err != nil {
		return BackupSetWorkflow{}, err
	}

	if !cfg.WorkflowsConfigured() {
		return BackupSetWorkflow{}, ErrWorkflowsNotConfigured
	}

	if bs.Workflow == nil {
		bs.Workflow = &config.SetWorkflow{}
	}
	if req.BeforeDir != nil {
		bs.Workflow.BeforeDir = strings.TrimSpace(*req.BeforeDir)
	}
	if req.AfterDir != nil {
		bs.Workflow.AfterDir = strings.TrimSpace(*req.AfterDir)
	}
	if req.ScriptTimeout != nil {
		bs.Workflow.ScriptTimeout = config.Duration(*req.ScriptTimeout)
	}
	if req.RemoteExecConnectionRef != nil {
		bs.Workflow.RemoteExecConnectionRef = strings.TrimSpace(*req.RemoteExecConnectionRef)
	}

	// A block that now configures nothing is REMOVED rather than left
	// empty. config.Validate refuses "workflow: {}" in words -- a block
	// that configures no directory is a mistake rather than an elaborate
	// way of writing nothing -- so a patch that clears the last field
	// would otherwise write a file this product then refuses to load.
	// Removing it is what the operator meant: this set no longer has its
	// own workflow configuration.
	if emptySetWorkflow(*bs.Workflow) {
		bs.Workflow = nil
	}

	if err := b.persistConfig(cfg); err != nil {
		return BackupSetWorkflow{}, err
	}

	return b.BackupSetWorkflow(context.Background(), id)
}

func emptySetWorkflow(w config.SetWorkflow) bool {
	return w.BeforeDir == "" && w.AfterDir == "" &&
		w.ScriptTimeout.Duration() == 0 && w.RemoteExecConnectionRef == ""
}

// ListWorkflowEnv reports one scope's configured environment entries.
//
// An empty backupSetID is the deployment-wide layer. One method for both
// rather than two, because the two are the same operation on two lists
// and a second method would be a second place the secret-reference rule
// has to be kept.
func (b *BackupService) ListWorkflowEnv(_ context.Context, backupSetID string) ([]WorkflowEnvVar, error) {
	cfg := b.state.Load().inner.Config

	if strings.TrimSpace(backupSetID) == "" {
		return toWorkflowEnvVars(cfg.Workflows.Environment), nil
	}

	bs, err := lookupConfiguredBackupSet(cfg, backupSetID)
	if err != nil {
		return nil, err
	}

	return toWorkflowEnvVars(bs.Environment), nil
}

// SetWorkflowEnv writes one environment entry, creating or replacing it.
//
// Replacing and not merging: a variable is a name and one source, and a
// patch that merged would make "change this from a literal to a secret"
// impossible to express -- the literal would survive alongside the
// reference, which config.Validate then refuses as a contradiction.
func (b *BackupService) SetWorkflowEnv(_ context.Context, backupSetID string, v WorkflowEnvVar) ([]WorkflowEnvVar, error) {
	if b.configPath == "" {
		return nil, ErrConfigNotFileBacked
	}
	if err := validateWorkflowEnvVar(v); err != nil {
		return nil, err
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return nil, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	if !cfg.WorkflowsConfigured() {
		return nil, ErrWorkflowsNotConfigured
	}

	list, err := workflowEnvListFor(cfg, backupSetID)
	if err != nil {
		return nil, err
	}

	*list = upsertEnvVar(*list, v)

	if err := b.persistConfig(cfg); err != nil {
		return nil, err
	}

	return b.ListWorkflowEnv(context.Background(), backupSetID)
}

// UnsetWorkflowEnv removes one environment entry.
func (b *BackupService) UnsetWorkflowEnv(_ context.Context, backupSetID, name string) ([]WorkflowEnvVar, error) {
	if b.configPath == "" {
		return nil, ErrConfigNotFileBacked
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: unsetting a workflow environment variable needs its name", ErrInvalidRequest)
	}

	b.configMu.Lock()
	defer b.configMu.Unlock()

	cfg, err := config.Load(b.configPath)
	if err != nil {
		return nil, fmt.Errorf("service: re-reading configuration: %w", err)
	}

	list, err := workflowEnvListFor(cfg, backupSetID)
	if err != nil {
		return nil, err
	}

	kept := make([]config.EnvironmentVariable, 0, len(*list))
	found := false
	for _, e := range *list {
		if e.Name == name {
			found = true

			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrWorkflowEnvNotFound, name)
	}

	// nil rather than an empty slice, so a scope whose last variable is
	// removed loses the key entirely. config/workflows.go's own header
	// is explicit about this: a config file that never heard of
	// workflows must not come back from a save carrying "environment:
	// []", because an older binary reading it with KnownFields(true)
	// refuses the file outright.
	if len(kept) == 0 {
		kept = nil
	}
	*list = kept

	if err := b.persistConfig(cfg); err != nil {
		return nil, err
	}

	return b.ListWorkflowEnv(context.Background(), backupSetID)
}

// workflowEnvListFor returns a pointer to the list one scope's entries
// live in, so the two writes above share one lookup.
func workflowEnvListFor(cfg *config.Config, backupSetID string) (*[]config.EnvironmentVariable, error) {
	if strings.TrimSpace(backupSetID) == "" {
		return &cfg.Workflows.Environment, nil
	}

	bs, err := findBackupSetForWrite(cfg, backupSetID)
	if err != nil {
		return nil, err
	}

	return &bs.Environment, nil
}

// upsertEnvVar replaces an entry of the same name, or appends, keeping
// the list sorted by name.
//
// Sorted, because this list is re-marshalled into config.yaml on every
// save and an operator diffing two revisions of that file should see
// their own change rather than a reordering. It is also what makes two
// deployments configured the same way produce the same file.
func upsertEnvVar(list []config.EnvironmentVariable, v WorkflowEnvVar) []config.EnvironmentVariable {
	entry := toConfigEnvVar(v)

	for i := range list {
		if list[i].Name == v.Name {
			list[i] = entry

			return list
		}
	}

	list = append(list, entry)
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	return list
}

// validateWorkflowEnvVar refuses a request this package can see is wrong
// before it rewrites a file.
//
// The NAME rule is not re-implemented: workflow.ValidateEnvName is the one
// place it lives, and it is called rather than copied, so a spelling it
// learns later cannot leave this surface behind. What is checked here is
// the shape config.Validate genuinely cannot report well -- exactly one
// source -- because by the time the file is parsed, a request that named
// both a literal and a secret has already become two written keys and the
// refusal names a config path instead of the field the caller sent.
func validateWorkflowEnvVar(v WorkflowEnvVar) error {
	if err := workflow.ValidateEnvName(v.Name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}

	switch {
	case v.HasValue && !v.Secret.IsZero():
		return fmt.Errorf("%w: %s names a literal value and a secret reference, and this product will not choose between them: an operator who added a secret and left the old literal behind would get whichever one this happened to prefer", ErrInvalidRequest, v.Name)
	case !v.HasValue && v.Secret.IsZero():
		return fmt.Errorf("%w: %s names neither a value nor a secret reference. Pass an explicitly empty value to configure an empty variable, which is a real thing to want", ErrInvalidRequest, v.Name)
	case v.Secret.sources() > 1:
		return fmt.Errorf("%w: the secret reference for %s names more than one location; set exactly one of file, env or command", ErrInvalidRequest, v.Name)
	case strings.ContainsRune(v.Value, 0):
		return fmt.Errorf("%w: the value of %s contains a NUL byte, and execve would silently truncate it there rather than fail", ErrInvalidRequest, v.Name)
	}

	return nil
}

func toConfigEnvVar(v WorkflowEnvVar) config.EnvironmentVariable {
	out := config.EnvironmentVariable{Name: v.Name}

	if v.HasValue {
		value := v.Value
		out.Value = &value

		return out
	}

	out.FromSecret = &config.SecretSource{
		File:    v.Secret.File,
		Env:     v.Secret.Env,
		Command: append([]string(nil), v.Secret.Command...),
	}

	return out
}

func toWorkflowEnvVars(list []config.EnvironmentVariable) []WorkflowEnvVar {
	out := make([]WorkflowEnvVar, 0, len(list))
	for _, e := range list {
		v := WorkflowEnvVar{Name: e.Name}
		if e.Value != nil {
			v.Value = *e.Value
			v.HasValue = true
		}
		if e.FromSecret != nil {
			v.Secret = WorkflowSecretRef{
				File:    e.FromSecret.File,
				Env:     e.FromSecret.Env,
				Command: append([]string(nil), e.FromSecret.Command...),
			}
		}
		out = append(out, v)
	}

	return out
}

// lookupConfiguredBackupSet finds a set in the RUNNING configuration.
func lookupConfiguredBackupSet(cfg *config.Config, id string) (config.BackupSet, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return config.BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	for _, src := range cfg.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == setName {
				return bs, nil
			}
		}
	}

	return config.BackupSet{}, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
}

// findBackupSetForWrite finds a set in a freshly-loaded configuration and
// returns a POINTER into it, so a caller can edit the file's own copy.
func findBackupSetForWrite(cfg *config.Config, id string) (*config.BackupSet, error) {
	sourceName, setName, ok := splitBackupSetID(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
	}

	for i := range cfg.Sources {
		if cfg.Sources[i].Name != sourceName {
			continue
		}
		for j := range cfg.Sources[i].BackupSets {
			if cfg.Sources[i].BackupSets[j].Name == setName {
				return &cfg.Sources[i].BackupSets[j], nil
			}
		}
	}

	return nil, fmt.Errorf("%w: %s", ErrBackupSetNotFound, id)
}
