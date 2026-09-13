package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/secretref"
	"github.com/backupdproject/backupd/core/internal/workflow"
)

// EPIC L's configuration surface (#807, this file is #808): where hook
// scripts live, which directories are stages, how long a hook may run, and
// what a hook's environment contains.
//
// # What this file does not decide
//
// Nothing here opens a directory, stats a file or resolves a secret. That
// is validate.go's standing rule for the whole package -- "everything here
// is shape" -- and it has a specific consequence worth stating because it
// looks like a gap: a hook directory that is CONFIGURED AND MISSING is not
// a config error. It cannot be. The daemon on one host would refuse to
// start while the same file on another, whose /workflows mount happens to
// be up, would boot; and a mount that arrives a second after this process
// does would make a perfectly good configuration unbootable.
//
// So the split is: this file refuses configurations that cannot be right
// anywhere (a relative root, a "..", a reserved variable name, a workflow
// block naming no directory), and internal/workflow's Snapshot refuses the
// ones that depend on what is actually on disk at the moment a run starts
// (missing, symlinked, group-writable, oversized). #808's "configured
// missing dir = validation error" lands there, at run start, where the
// answer is a failed backup somebody can see rather than a daemon that
// will not come up.
//
// # Why the per-set block is a pointer and the global one is not
//
// Workflows is a struct on Config because "is this deployment running
// workflows at all" is answered by whether its Root is set, and one more
// nil check on the hot path buys nothing. BackupSet.Workflow is a POINTER
// because a set that says nothing and a set that says
// "workflow: {}" are different operator statements: the first inherits
// nothing and runs no per-set hooks, and the second is a block that
// configures no directory, which is a mistake this file refuses in words
// rather than accepting as an elaborate way of writing nothing.
//
// # omitempty, everywhere, without exception
//
// core/service re-marshals the whole Config on every settings save
// (FR-35), and Load runs KnownFields(true). So a config file that never
// heard of workflows must not come back from a save carrying
// "workflows: {}" or "environment: []", because an older binary refuses
// that file outright. TestMarshal_ANoWorkflowConfigGainsNoWorkflowKeys is
// the gate, and it checks the second, harder half too: a settings form
// that renders a workflow section and is submitted unchanged hands this
// struct empty strings and empty slices, and the file must be unmoved.

// Workflows is the deployment-wide workflow configuration.
type Workflows struct {
	// Root is the directory hook scripts live in, normally the read-only
	// /workflows mount. It is the trust boundary: every stage directory
	// must resolve inside it, and a deployment with no root configured
	// runs no workflows at all.
	//
	// It must be absolute and free of "..". Whether it EXISTS is
	// internal/workflow's question, at run start; see this file's doc for
	// why it cannot be this one.
	Root string `yaml:"root,omitempty"`

	// Global is the pair of hook directories that run for every backup
	// set in the deployment.
	Global WorkflowStageDirs `yaml:"global,omitempty"`

	// Environment is the deployment-wide environment every hook gets, on
	// top of the sanitized baseline and under each backup set's own.
	Environment []EnvironmentVariable `yaml:"environment,omitempty"`

	// ScriptTimeout is how long one hook may run when the backup set does
	// not say. Zero means workflow.DefaultStepTimeout, which is where
	// that number is declared and argued; this schema does not carry a
	// second copy of it.
	ScriptTimeout Duration `yaml:"script_timeout,omitempty"`

	// MaxScriptSizeBytes bounds one hook script. Zero means
	// workflow.DefaultMaxScriptSize. It is configurable because one
	// mebibyte is a judgement rather than a law, and it is capped at
	// workflow.MaxConfigurableScriptSize because "configurable" with no
	// limit means a deployment can configure the protection away, usually
	// while debugging something else, and then keep running that way.
	MaxScriptSizeBytes int64 `yaml:"max_script_size_bytes,omitempty"`
}

// WorkflowStageDirs is one scope's pair of hook directories.
//
// An unset directory means that stage is DISABLED, and that is a different
// fact from a directory that exists and is empty (zero steps) or one that
// is configured and missing (a run-start refusal). All three are real
// states and #808's technical requirements name them separately, which is
// why neither field has a default: there is no directory this product
// would guess at on an operator's behalf.
type WorkflowStageDirs struct {
	BeforeDir string `yaml:"before_dir,omitempty"`
	AfterDir  string `yaml:"after_dir,omitempty"`
}

func (d WorkflowStageDirs) isZero() bool { return d.BeforeDir == "" && d.AfterDir == "" }

// SetWorkflow is one backup set's own workflow configuration.
type SetWorkflow struct {
	// BeforeDir and AfterDir are this set's hook directories, resolved
	// inside Workflows.Root exactly as the global ones are.
	BeforeDir string `yaml:"before_dir,omitempty"`
	AfterDir  string `yaml:"after_dir,omitempty"`

	// ScriptTimeout overrides the deployment's bound for this set's
	// hooks. Zero means inherit, and inherit is not a copy of today's
	// global value: a set that says nothing here follows the deployment
	// wherever an operator later moves it, which is BackupSet.PollInterval's
	// reasoning for the same shape.
	ScriptTimeout Duration `yaml:"script_timeout,omitempty"`

	// RemoteExecConnectionRef names the connection this set's
	// NAME.remote.sh hooks run over.
	//
	// It is required if and only if a remote hook is actually discovered,
	// which is a fact about the directory rather than about the file, so
	// the refusal is internal/workflow's at run start. A set whose hook
	// directories contain only local scripts legitimately configures none.
	RemoteExecConnectionRef string `yaml:"remote_exec_connection_ref,omitempty"`
}

// EnvironmentVariable is one entry in a workflow environment: a name, and
// either a literal value or a reference to a secret.
//
// # Why both fields are pointers
//
// The two are mutually exclusive, and deciding WHICH ONE an operator meant
// requires knowing whether they wrote the key at all -- which a value type
// cannot express. With Value a string and FromSecret a SecretSource, the
// left column below decodes to the same struct as the right one (the
// leading list dash of each environment entry is elided, because a Go doc
// comment reads one as a list of its own):
//
//	name: PGPASSWORD                name: PGPASSWORD
//	value: ""                       from_secret:
//	from_secret:                      file: /run/secrets/db.pw
//	  file: /run/secrets/db.pw
//
//	name: PGPASSWORD                name: PGPASSWORD
//	from_secret: {}
//
// The first pair is a contradiction an operator should be told about: they
// have written an empty literal AND a secret, and a schema that resolves
// it silently picks one -- the mistake being that whichever it picks keeps
// working, so the wrong one is never noticed. The second pair is worse: a
// from_secret with nothing in it decoded to the zero SecretSource, which
// is indistinguishable from "no secret configured", so the variable became
// a silently EMPTY literal. A hook then ran with an empty credential and
// failed somewhere far away from the reason.
//
// Pointers make presence representable, so both are refusals in words.
// `value: ""` on its own remains exactly what it looks like: an empty
// variable, deliberately configured, which is a thing operators do.
type EnvironmentVariable struct {
	// Name must match workflow.EnvNamePattern and must not be in the
	// BACKUPD_ namespace. See workflow.ValidateEnvName, which is the one
	// place that rule lives: a copy here would be a second rule, and two
	// rules disagree the first time one of them learns a new spelling.
	Name string `yaml:"name"`

	// Value is the literal, passed to the hook with no shell evaluation
	// of any kind. "$HOME" is five characters.
	//
	// Nil means the key was not written. A non-nil pointer to "" means it
	// was written empty, which is a valid configuration.
	Value *string `yaml:"value,omitempty"`

	// FromSecret names where the value comes from when it is not a
	// literal. Nil means the key was not written; a non-nil one must name
	// exactly one location.
	FromSecret *SecretSource `yaml:"from_secret,omitempty"`
}

// Literal is the configured literal, and "" when none was written. It
// exists so that no caller dereferences the pointer, and so that "was the
// key written" and "what does it say" stay two different questions.
func (e EnvironmentVariable) Literal() string {
	if e.Value == nil {
		return ""
	}

	return *e.Value
}

// SecretSource names WHERE a secret comes from: the same file/env/command
// triple as Passphrase, KeyEncryption and MediumCredentials, spelled the
// same way.
//
// There is no fourth field, and there will not be. A key an operator could
// type directly into config.yaml would be a credential sitting in the
// clear beside the hostnames it protects, which is the exposure #298
// exists to close for the SSH key; secretref's package doc argues the same
// point for the repository passphrase. secretref's own
// TestRefFieldSetMatchesTheConfiguredOnes pins this type against
// secretref.Ref so a field on one and not the other cannot ship.
type SecretSource struct {
	File    string   `yaml:"file,omitempty"`
	Env     string   `yaml:"env,omitempty"`
	Command []string `yaml:"command,omitempty"`
}

// validateExactlyOneSource refuses a from_secret block that names no
// location or more than one.
//
// secretref.Ref.Validate already refuses both, and this is not a second
// copy of that rule: it is the same rule asked EARLIER, because a
// from_secret written as `{}` reaches secretref as the zero Ref, which is
// what "this variable has no secret" also looks like. The config layer is
// the only place that can tell the difference, so it is the only place
// that can say "you wrote from_secret and put nothing in it".
func (s SecretSource) validateExactlyOneSource() error {
	named := 0
	for _, set := range []bool{s.File != "", s.Env != "", len(s.Command) != 0} {
		if set {
			named++
		}
	}

	switch named {
	case 1:
		return nil
	case 0:
		return errors.New("names no location: set exactly one of file, env or command. A from_secret with nothing in it reads as an empty variable, and a hook handed an empty credential fails somewhere far away from the reason")
	default:
		return errors.New("names more than one location: set exactly one of file, env or command, because choosing between two would mean an operator who added a second while leaving the first behind gets whichever this product happened to prefer")
	}
}

// secretRef is this source as the engine-facing reference. It is a
// conversion and not a second declaration, for Passphrase.secretRef's
// reason: internal/secretref sits below this package and cannot import it,
// so the translation happens here, once.
func (s SecretSource) secretRef() secretref.Ref {
	return secretref.Ref{File: s.File, Env: s.Env, Command: s.Command}
}

// envVar is this entry as the domain type internal/workflow merges and
// hashes. It does not validate; validateEnvironment reports every problem
// with the config path in the sentence, which a conversion could not do.
func (e EnvironmentVariable) envVar() workflow.EnvVar {
	v := workflow.EnvVar{Name: e.Name, Value: e.Literal()}
	if e.FromSecret != nil {
		v.Secret = e.FromSecret.secretRef()
	}

	return v
}

// WorkflowsConfigured reports whether this deployment runs workflows at
// all: one question, answered in one place, so no surface re-derives it
// from a different combination of fields.
func (c *Config) WorkflowsConfigured() bool { return c.Workflows.Root != "" }

// WorkflowStagesFor returns the stages one backup set runs, in execution
// order, or nothing when this deployment or this set configures none.
//
// The ORDER is workflow.PlanStages's, not this function's: the unwinding
// rule is a product decision and a second opinion about it here is how the
// config layer and the run layer would come to disagree about which hook
// runs first.
func (c *Config) WorkflowStagesFor(bs *BackupSet) []workflow.StageSpec {
	if !c.WorkflowsConfigured() {
		return nil
	}

	var set workflow.StageDirs
	if bs != nil && bs.Workflow != nil {
		set = workflow.StageDirs{Before: bs.Workflow.BeforeDir, After: bs.Workflow.AfterDir}
	}

	return workflow.PlanStages(
		workflow.StageDirs{Before: c.Workflows.Global.BeforeDir, After: c.Workflows.Global.AfterDir},
		set,
	)
}

// EffectiveScriptTimeout is how long one of this set's hooks may run: the
// set's own bound, else the deployment's, else the domain's documented
// default.
//
// It is the one place those three are combined, so no caller decides it
// twice, which is Config.EffectivePollInterval's reasoning for the same
// shape. The default is read from internal/workflow rather than restated
// here, so changing it there cannot leave this schema quietly
// contradicting it.
func (c *Config) EffectiveScriptTimeout(bs *BackupSet) time.Duration {
	if bs != nil && bs.Workflow != nil && bs.Workflow.ScriptTimeout.Duration() > 0 {
		return bs.Workflow.ScriptTimeout.Duration()
	}

	if c.Workflows.ScriptTimeout.Duration() > 0 {
		return c.Workflows.ScriptTimeout.Duration()
	}

	return workflow.DefaultStepTimeout
}

// EffectiveMaxScriptSize is the size bound one hook script is held to:
// the deployment's, else the domain's default.
func (c *Config) EffectiveMaxScriptSize() int64 {
	if c.Workflows.MaxScriptSizeBytes > 0 {
		return c.Workflows.MaxScriptSizeBytes
	}

	return workflow.DefaultMaxScriptSize
}

// workflowSpoolDirName is the directory, under the state database's own
// directory, that holds run-scoped script spools.
const workflowSpoolDirName = "workflow-runs"

// WorkflowSpoolDir is where captured scripts are written: a sibling of the
// state database, exactly as core/service's application-validator script
// directory is.
//
// It is DERIVED rather than configured, deliberately. A configurable spool
// is a second path an operator can point at an SMB export, and the spool's
// whole value is that its contents are 0600 files in a 0700 directory that
// nothing but this process can reach -- a guarantee that evaporates the
// moment it lives inside a share. The state database's directory is
// already the place this product keeps things only it may read.
//
// It returns "" when no state database is configured, so a caller can tell
// that apart from a relative path it would then create in whatever working
// directory the init system chose.
func (c *Config) WorkflowSpoolDir() string {
	if c.State.Database == "" {
		return ""
	}

	return filepath.Join(filepath.Dir(c.State.Database), workflowSpoolDirName)
}

// validateWorkflows checks the deployment-wide block. Per-set blocks are
// checked by validateBackupSetWorkflow during the ordinary backup-set
// pass, and the resolved per-set environment is assembled in phase 2,
// because merging needs the global layer to have been validated first.
func (v *validator) validateWorkflows(c *Config) {
	w := &c.Workflows

	if w.Root != "" {
		if err := validAbsolutePath(w.Root); err != nil {
			v.addf("workflows.root: %v", err)
		}
	}

	v.validateStageDir("workflows.global.before_dir", w.Global.BeforeDir)
	v.validateStageDir("workflows.global.after_dir", w.Global.AfterDir)

	if w.ScriptTimeout != 0 && w.ScriptTimeout.Duration() <= 0 {
		v.addf("workflows.script_timeout: must be a positive duration when set (got %s); a hook with no bound can hold a backup window open indefinitely, so there is no spelling of \"wait forever\"", w.ScriptTimeout)
	}

	switch {
	case w.MaxScriptSizeBytes < 0:
		v.addf("workflows.max_script_size_bytes: must not be negative (got %d)", w.MaxScriptSizeBytes)
	case w.MaxScriptSizeBytes > workflow.MaxConfigurableScriptSize:
		v.addf("workflows.max_script_size_bytes: must not exceed %d (got %d); the bound is adjustable so a legitimately large hook is not refused, and capped so a deployment cannot configure the protection away",
			workflow.MaxConfigurableScriptSize, w.MaxScriptSizeBytes)
	}

	v.validateEnvironment("workflows.environment", w.Environment)

	// The root is required by anything that would use it, and refused
	// nowhere else: a deployment that configures no workflows at all is
	// every config file written before EPIC L and must stay valid.
	if w.Root == "" && !w.Global.isZero() {
		v.addf("workflows.root: must be set when workflows.global names a hook directory; the root is the approved tree every hook script must live inside, and there is no default for it")
	}

	if w.Root == "" && len(w.Environment) != 0 {
		v.addf("workflows.root: must be set when workflows.environment is configured; a workflow environment with no hook directories to run in is configuration that does nothing")
	}
}

// validateStageDir holds a configured hook directory to the rules that are
// true on every host: not empty when written, and free of "..".
//
// Containment inside the root is checked at run start, against the
// resolved filesystem, because a symbolic link inside the tree can escape
// a root no lexical rule would object to (internal/workflow's
// Root.ResolveStage). The lexical ".." is refused HERE anyway, rather than
// only there, because it is the one escape an operator can see in their
// own file and the cheapest moment to tell them is the restart that
// introduced it.
func (v *validator) validateStageDir(path, dir string) {
	if dir == "" {
		return
	}

	if strings.TrimSpace(dir) == "" {
		v.addf("%s: must not be whitespace; a stage that should not run is one with no directory configured at all", path)

		return
	}

	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		if seg == ".." {
			v.addf("%s: must not contain \"..\" (got %q); every hook directory resolves inside workflows.root, and that declaration is the only thing that makes the tree trusted", path, dir)

			return
		}
	}
}

// validateEnvironment checks one environment block: every name, every
// value, and no name twice.
//
// The rules themselves are workflow.ValidateEnvName's and
// workflow.EnvVar.Validate's, called rather than restated, so the config
// file and the runtime cannot drift into two vocabularies. What this adds
// is the config PATH in the sentence, which is the thing an operator needs
// and which the domain types cannot know.
func (v *validator) validateEnvironment(path string, vars []EnvironmentVariable) {
	seen := map[string]int{}

	for i, entry := range vars {
		where := fmt.Sprintf("%s[%d]", path, i)

		// The two PRESENCE rules, which the domain type cannot make
		// because presence is not something it can see: by the time an
		// entry is a workflow.EnvVar, `value: ""` with a secret and a
		// secret on its own are the same value. See
		// EnvironmentVariable's doc for what each of them silently did.
		if entry.Value != nil && entry.FromSecret != nil {
			v.addf("%s: %s declares both value and from_secret, and they are alternatives. An empty literal is a real configuration, so this is not read as \"the secret wins\": remove whichever one is not meant, because as long as both are here the variable works and only one of them is being used",
				where, entry.Name)

			continue
		}

		if entry.FromSecret != nil {
			if err := entry.FromSecret.validateExactlyOneSource(); err != nil {
				v.addf("%s: the secret reference for %s %v", where, entry.Name, err)

				continue
			}
		}

		if err := entry.envVar().Validate(); err != nil {
			v.addf("%s: %v", where, err)

			continue
		}

		if first, already := seen[entry.Name]; already {
			v.addf("%s: %s is declared twice in the same environment block (also at %s[%d]), and there is no rule for choosing between them",
				where, entry.Name, path, first)

			continue
		}
		seen[entry.Name] = i
	}
}

// validateBackupSetWorkflows checks every set's own block.
//
// It is a pass of its own rather than a call inside validateBackupSet
// because every rule it applies reads the DEPLOYMENT's block -- may this
// set configure a hook directory at all, is there a root for its
// environment to run in -- and validateBackupSet deliberately holds one
// set and its source, not the whole file. resolveBackupSetRetentions and
// validateEngineReferences are the same shape for the same reason.
func (v *validator) validateBackupSetWorkflows(c *Config) {
	for i := range c.Sources {
		for j := range c.Sources[i].BackupSets {
			v.validateBackupSetWorkflow(
				fmt.Sprintf("sources[%d].backup_sets[%d]", i, j),
				c,
				&c.Sources[i].BackupSets[j],
			)
		}
	}
}

// validateBackupSetWorkflow checks one set's own block.
//
// It is reached from validateBackupSetWorkflows rather than from
// validateBackupSet, so it can see the deployment block this set's rules
// depend on; the refusals still carry the set's own config path, which is
// what the operator has to go and edit.
func (v *validator) validateBackupSetWorkflow(path string, c *Config, bs *BackupSet) {
	v.validateEnvironment(path+".environment", bs.Environment)

	if w := bs.Workflow; w != nil {
		// A block that configures nothing is refused rather than treated
		// as an elaborate way of writing nothing at all. An operator who
		// wrote it meant something, and this is the one moment they are
		// watching.
		//
		// "Nothing", though, is not the same as "no hook directory", and
		// the first version of this rule got that wrong in a way that
		// refused a correct configuration. A set that declares only
		// remote_exec_connection_ref (or only script_timeout) is
		// configuring the GLOBAL stages as they apply to this set: the
		// deployment's global hook directory holds a NAME.remote.sh, and
		// the connection it runs over is necessarily per-set, because it
		// is that set's own source host. Refusing that block leaves an
		// operator with a global remote hook and nowhere to declare where
		// it runs.
		//
		// So the refusal is for a block that says nothing at all, and the
		// connection-and-timeout case is accepted exactly when some stage
		// actually runs for this set.
		configuresNothing := w.BeforeDir == "" && w.AfterDir == "" &&
			w.RemoteExecConnectionRef == "" && w.ScriptTimeout == 0

		switch {
		case configuresNothing:
			v.addf("%s.workflow: configures nothing at all; set before_dir, after_dir, script_timeout or remote_exec_connection_ref, or remove the block", path)
		case w.BeforeDir == "" && w.AfterDir == "" && len(c.WorkflowStagesFor(bs)) == 0:
			v.addf("%s.workflow: configures no hook directory of its own, and no global hook directory runs for this set either, so nothing would ever read its %s. Set workflows.global.before_dir/after_dir, or this set's own before_dir/after_dir, or remove the block",
				path, describeSetWorkflowSettings(w))
		}

		v.validateStageDir(path+".workflow.before_dir", w.BeforeDir)
		v.validateStageDir(path+".workflow.after_dir", w.AfterDir)

		if w.ScriptTimeout != 0 && w.ScriptTimeout.Duration() <= 0 {
			v.addf("%s.workflow.script_timeout: must be a positive duration when set (got %s)", path, w.ScriptTimeout)
		}

		if !c.WorkflowsConfigured() {
			v.addf("%s.workflow: workflows.root must be set before a backup set can configure hook directories; the root is the approved tree every hook script must live inside", path)
		}
	}

	// An environment nothing will ever run with is configuration that
	// does nothing, and it is refused for the reason every other
	// dead-configuration refusal in this package exists: an operator who
	// wrote a variable and sees a green backup concludes the variable
	// arrived. There are two ways to get here and they have different
	// fixes, so they get different sentences.
	if len(bs.Environment) == 0 || len(c.WorkflowStagesFor(bs)) != 0 {
		return
	}

	if !c.WorkflowsConfigured() {
		v.addf("%s: workflows.root must be set when a backup set configures an environment; a workflow environment with no hook directories to run in is configuration that does nothing", path)

		return
	}

	v.addf("%s: configures an environment and no hook directory runs for this set, so nothing would ever read it; set workflows.global.before_dir/after_dir, or this set's own workflow.before_dir/after_dir", path)
}

// describeSetWorkflowSettings names what a directory-less set block
// actually configured, so the refusal quotes the key the operator wrote
// rather than a generic "settings".
func describeSetWorkflowSettings(w *SetWorkflow) string {
	switch {
	case w.RemoteExecConnectionRef != "" && w.ScriptTimeout != 0:
		return "remote_exec_connection_ref and script_timeout"
	case w.RemoteExecConnectionRef != "":
		return "remote_exec_connection_ref"
	default:
		return "script_timeout"
	}
}

// resolveBackupSetWorkflowEnvironments merges each set's environment with
// the deployment's and the sanitized baseline, and stores the result on the
// set.
//
// Phase 2, for resolveBackupSetRetentions' reason: it needs phase 1 to
// have finished on the thing being inherited FROM. A set resolved during
// phase 1 would merge over an unvalidated global block, and an invalid
// global entry would then reach a set's resolved environment.
//
// A set with nothing configured anywhere gets the ZERO Environment, not
// the baseline. That is the difference between "this set runs no hooks"
// and "this set runs hooks with a minimal environment", and #808's first
// acceptance criterion is that the former behaves exactly as it did before
// this package had a workflow at all.
func (c *Config) resolveBackupSetWorkflowEnvironments(v *validator) {
	if !c.WorkflowsConfigured() {
		return
	}

	global := make([]workflow.EnvVar, 0, len(c.Workflows.Environment))
	for _, entry := range c.Workflows.Environment {
		global = append(global, entry.envVar())
	}

	for i := range c.Sources {
		for j := range c.Sources[i].BackupSets {
			bs := &c.Sources[i].BackupSets[j]

			stages := c.WorkflowStagesFor(bs)
			if len(stages) == 0 {
				bs.WorkflowEnvironment = workflow.Environment{}

				continue
			}

			own := make([]workflow.EnvVar, 0, len(bs.Environment))
			for _, entry := range bs.Environment {
				own = append(own, entry.envVar())
			}

			env, err := workflow.NewEnvironment(workflow.SanitizedBaseline(), global, own)
			if err != nil {
				// Every rule this can break has already been reported
				// against the exact config path that broke it, so this
				// is here to be complete rather than to be read: a
				// merge failure with no earlier problem would be a bug
				// in this package, and swallowing it would hide it.
				v.addf("sources[%d].backup_sets[%d]: resolving the workflow environment: %v", i, j, err)

				continue
			}

			bs.WorkflowEnvironment = env
		}
	}
}
