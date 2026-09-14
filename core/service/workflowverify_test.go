package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/backupdproject/backupd/core/internal/config"
)

// The save gate (#906): a workflow configuration write is refused when a
// script it points at does not pass verification, and is NOT refused for
// anything the verification merely reported or could not look at.
//
// Every refusal here is checked by READING THE FILE BACK, not by
// believing the error. A gate that returns an error and writes anyway is
// exactly the bug this is for, and the returned error cannot see it.

func TestASaveIsRefusedWhenAScriptDoesNotParse(t *testing.T) {
	t.Parallel()

	svc, configPath, root := openWorkflowSaveService(t)
	writeHook(t, root, "global-before", "quiesce.local.sh",
		"#!/bin/bash",
		"if true",
		"then",
		"  echo hi",
	)

	before := readFileString(t, configPath)

	_, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		BeforeDir: new("global-before"),
	})

	if !errors.Is(err, ErrWorkflowScriptRefused) {
		t.Fatalf("UpdateWorkflowSettings = %v, want ErrWorkflowScriptRefused", err)
	}
	if !strings.Contains(err.Error(), "quiesce.local.sh") {
		t.Errorf("the refusal does not name the script: %v", err)
	}
	if !strings.Contains(err.Error(), "does not parse at 2:1") {
		t.Errorf("the refusal carries no position: %v", err)
	}
	if got := readFileString(t, configPath); got != before {
		t.Fatalf("the configuration file was rewritten by a refused save:\n%s", got)
	}

	// Read back through a second load rather than through the service's
	// own state: the file is what the next process will run, and a gate
	// that refused in memory and wrote to disk would pass every
	// in-process assertion.
	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.Global.BeforeDir != "" {
		t.Errorf("a refused save left before_dir = %q on disk", reloaded.Workflows.Global.BeforeDir)
	}
}

func TestASaveIsRefusedWhenAScriptCarriesAnErrorSeverityFinding(t *testing.T) {
	t.Parallel()

	svc, configPath, root := openWorkflowSaveService(t)
	writeHook(t, root, "global-before", "clean.local.sh",
		"#!/bin/bash",
		"rm -rf $STAGING/tmp",
	)

	_, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		BeforeDir: new("global-before"),
	})

	if !errors.Is(err, ErrWorkflowScriptRefused) {
		t.Fatalf("UpdateWorkflowSettings = %v, want ErrWorkflowScriptRefused", err)
	}

	var refusal *WorkflowScriptRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal does not carry its findings: %T", err)
	}
	if len(refusal.Scripts) != 1 {
		t.Fatalf("refusal covers %d script(s), want 1: %+v", len(refusal.Scripts), refusal.Scripts)
	}

	refused := refusal.Scripts[0]
	if refused.ScriptName != "clean.local.sh" || refused.Dir != "global-before" {
		t.Errorf("refusal names %q in %q, want clean.local.sh in global-before", refused.ScriptName, refused.Dir)
	}
	if len(refused.Findings) != 1 {
		t.Fatalf("refusal carries %d finding(s), want the one blocking finding: %+v", len(refused.Findings), refused.Findings)
	}
	if refused.Findings[0].Code != "BSH003" || refused.Findings[0].Line != 2 {
		t.Errorf("blocking finding = %+v, want BSH003 on line 2", refused.Findings[0])
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.Global.BeforeDir != "" {
		t.Errorf("a refused save left before_dir = %q on disk", reloaded.Workflows.Global.BeforeDir)
	}
}

// The other half of the threshold, and the half that decides whether
// anybody keeps this feature switched on: a warning does not stop a save.
func TestASaveSucceedsWithWarningAndStyleFindings(t *testing.T) {
	t.Parallel()

	svc, configPath, root := openWorkflowSaveService(t)
	writeHook(t, root, "global-before", "quiesce.local.sh",
		"cd /srv/data",
		"tar -cf out.tar $files",
		"if [ -n $reply ]; then echo yes; fi",
	)

	if _, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		BeforeDir: new("global-before"),
	}); err != nil {
		t.Fatalf("a script with only warning, info and style findings was refused: %v", err)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.Global.BeforeDir != "global-before" {
		t.Errorf("before_dir = %q on disk, want global-before", reloaded.Workflows.Global.BeforeDir)
	}
}

// A stage directory with nothing wrong in it saves, so the tests above
// are exercising the gate rather than a write that refuses everything.
func TestASaveSucceedsWithACleanScript(t *testing.T) {
	t.Parallel()

	svc, configPath, root := openWorkflowSaveService(t)
	writeHook(t, root, "global-before", "quiesce.local.sh",
		"#!/usr/bin/env bash",
		"set -euo pipefail",
		`echo "${BACKUPD_RUN_ID:-none}"`,
	)

	if _, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		BeforeDir: new("global-before"),
	}); err != nil {
		t.Fatalf("a clean script was refused: %v", err)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.Global.BeforeDir != "global-before" {
		t.Errorf("before_dir = %q on disk, want global-before", reloaded.Workflows.Global.BeforeDir)
	}
}

// The escape hatch, and it is the reason the gate verifies the RESULTING
// configuration: an operator whose stage directory holds a broken script
// has to be able to point the stage somewhere else or clear it. A gate
// that verified the current file instead would make that impossible.
func TestAStageDirectoryHoldingABrokenScriptCanStillBeCleared(t *testing.T) {
	t.Parallel()

	svc, configPath, root := openWorkflowSaveService(t)
	writeHook(t, root, "good", "ok.local.sh", "#!/bin/bash", "echo fine")

	if _, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		BeforeDir: new("good"),
	}); err != nil {
		t.Fatalf("configuring a clean stage: %v", err)
	}

	// The script is broken AFTER the save, which is the real sequence:
	// somebody edits a hook on disk and this product finds out at the
	// next write.
	writeHook(t, root, "good", "ok.local.sh", "#!/bin/bash", "rm -rf $STAGING/")

	if _, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		BeforeDir: new(""),
	}); err != nil {
		t.Fatalf("clearing the stage directory that holds the broken script was refused: %v", err)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.Global.BeforeDir != "" {
		t.Errorf("before_dir = %q on disk, want it cleared", reloaded.Workflows.Global.BeforeDir)
	}
}

func TestAPerSetSaveIsRefusedByItsOwnScripts(t *testing.T) {
	t.Parallel()

	svc, configPath, root := openWorkflowSaveService(t)
	writeHook(t, root, "alpha-before", "dump.remote.sh",
		"#!/bin/bash",
		"rm -rf ${STAGING}/*",
	)

	_, err := svc.UpdateBackupSetWorkflow(context.Background(), "production/alpha", UpdateBackupSetWorkflowRequest{
		BeforeDir: new("alpha-before"),
	})

	if !errors.Is(err, ErrWorkflowScriptRefused) {
		t.Fatalf("UpdateBackupSetWorkflow = %v, want ErrWorkflowScriptRefused", err)
	}
	if !strings.Contains(err.Error(), "dump.remote.sh") {
		t.Errorf("the refusal does not name the script: %v", err)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	for _, source := range reloaded.Sources {
		for _, bs := range source.BackupSets {
			if bs.Workflow != nil {
				t.Errorf("a refused per-set save left a workflow block on disk: %+v", bs.Workflow)
			}
		}
	}
}

// A per-set save is not refused by a GLOBAL script somebody else broke.
// The scope rule matters: the operator patching their own set may have no
// access to the deployment-wide hook directory at all.
func TestAPerSetSaveIsNotRefusedByABrokenGlobalScript(t *testing.T) {
	t.Parallel()

	svc, _, root := openWorkflowSaveService(t)
	writeHook(t, root, "global-before", "broken.local.sh", "#!/bin/bash", "rm -rf $STAGING/")
	writeHook(t, root, "alpha-before", "fine.local.sh", "#!/bin/bash", "echo fine")

	// The global stage is configured by hand, because configuring it
	// through the service is what the gate refuses.
	writeGlobalBeforeDir(t, svc, root)

	if _, err := svc.UpdateBackupSetWorkflow(context.Background(), "production/alpha", UpdateBackupSetWorkflowRequest{
		BeforeDir: new("alpha-before"),
	}); err != nil {
		t.Fatalf("a per-set save was refused by a broken global script: %v", err)
	}
}

// A configuration write that touches no stage directory at all is not
// made to depend on the hook tree: this is the ordinary case (pinning a
// timeout, choosing an exec connection) and the verification has nothing
// to say about it.
func TestASaveThatConfiguresNoStageIsNotGated(t *testing.T) {
	t.Parallel()

	svc, configPath, _ := openWorkflowSaveService(t)

	if _, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		MaxScriptSizeBytes: new(int64(2 << 20)),
	}); err != nil {
		t.Fatalf("a patch that configures no stage was refused: %v", err)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.MaxScriptSizeBytes != 2<<20 {
		t.Errorf("max_script_size_bytes = %d on disk, want %d", reloaded.Workflows.MaxScriptSizeBytes, 2<<20)
	}
}

// A stage directory that does not exist does not refuse the save. It is
// the state every deployment is in while somebody is setting hooks up,
// and `validate workflow` is where it is reported.
func TestASaveIsNotRefusedForADirectoryNobodyHasCreated(t *testing.T) {
	t.Parallel()

	svc, configPath, _ := openWorkflowSaveService(t)

	if _, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		BeforeDir: new("not-created-yet"),
	}); err != nil {
		t.Fatalf("a stage directory nobody has created refused the save: %v", err)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.Global.BeforeDir != "not-created-yet" {
		t.Errorf("before_dir = %q on disk, want not-created-yet", reloaded.Workflows.Global.BeforeDir)
	}
}

// The validation surface carries the same verdict the gate refused on,
// per script, with its position: this is what the CLI prints and what
// the Workflow tab draws, and a report that only said "invalid" would
// leave an operator with nowhere to look.
func TestTheValidationReportCarriesEachScriptsFindingsWithPositions(t *testing.T) {
	t.Parallel()

	svc, _, root := openWorkflowSaveService(t)
	writeHook(t, root, "global-before", "clean.local.sh", "#!/bin/bash", "rm -rf $STAGING/tmp")
	writeHook(t, root, "global-before", "warn.local.sh", "#!/bin/bash", "cd /srv/data", "echo there")
	writeGlobalBeforeDir(t, svc, root)

	report, err := svc.ValidateWorkflow(context.Background(), "production/alpha")
	if err != nil {
		t.Fatalf("ValidateWorkflow: %v", err)
	}

	if report.WorkflowValid {
		t.Error("a set whose hook carries an error-severity finding reports its workflows as valid")
	}

	byName := map[string]WorkflowValidatedScript{}
	for _, s := range report.Scripts {
		byName[s.ScriptName] = s
	}

	broken, ok := byName["clean.local.sh"]
	if !ok {
		t.Fatalf("the report does not carry clean.local.sh: %+v", report.Scripts)
	}
	if !broken.Lint.Examined || !broken.Lint.Parsed {
		t.Errorf("clean.local.sh parses and was examined, reported as %+v", broken.Lint)
	}
	blocking := broken.Lint.BlockingLintFindings()
	if len(blocking) != 1 || blocking[0].Code != "BSH003" || blocking[0].Line != 2 || blocking[0].Col == 0 {
		t.Errorf("blocking findings = %+v, want one BSH003 with a position on line 2", blocking)
	}

	warned, ok := byName["warn.local.sh"]
	if !ok {
		t.Fatalf("the report does not carry warn.local.sh: %+v", report.Scripts)
	}
	if len(warned.Lint.BlockingLintFindings()) != 0 {
		t.Errorf("a warning-only script reports blocking findings: %+v", warned.Lint.Findings)
	}
	if len(warned.Lint.Findings) == 0 {
		t.Error("a script with an unguarded cd reports no findings at all")
	}

	// And the findings list an operator reads names the script and the
	// position, under the syntax check id that has always meant "would a
	// shell accept this".
	var named bool
	for _, f := range report.Findings {
		if f.Check == WorkflowCheckLocalBashSyntax && f.Script == "clean.local.sh" && f.Severity == WorkflowSeverityError {
			named = true
			if !strings.Contains(f.Detail, "BSH003 at 2:") {
				t.Errorf("the finding does not carry the code and position: %q", f.Detail)
			}
		}
	}
	if !named {
		t.Errorf("no %s error finding names the broken script: %+v", WorkflowCheckLocalBashSyntax, report.Findings)
	}
}

// openWorkflowSaveService opens a file-backed service with a workflow
// root and one backup set, and returns the service, its configuration
// path and the root.
func openWorkflowSaveService(t *testing.T) (*BackupService, string, string) {
	t.Helper()

	// EvalSymlinks, because the spool's custody check refuses a
	// symlinked ancestor and macOS puts every temporary directory under
	// /var, which is a link to /private/var. Resolving it here is what a
	// real deployment's configuration would already be.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(TempDir): %v", err)
	}

	root := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll(workflows): %v", err)
	}

	remote := filepath.Join(dir, "remote")
	local := filepath.Join(dir, "local")
	for _, d := range []string{remote, local} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"workflows:\n" +
		"  root: " + root + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: alpha\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remote + "\n" +
		"        local_path: " + local + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n  timezone: UTC\n  week_starts_on: monday\n"

	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(config): %v", err)
	}

	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = svc.Close()
		_ = cleanup()
	})

	return svc, configPath, root
}

// writeHook puts one script in a stage directory under the root,
// creating the directory.
func writeHook(t *testing.T, root, stage, name string, lines ...string) {
	t.Helper()

	dir := filepath.Join(root, stage)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

// writeGlobalBeforeDir configures the deployment-wide before stage
// WITHOUT going through the gate, by editing the file and reloading.
//
// It exists for the one test that needs a broken global hook already
// configured, which is a state the gate refuses to create and a
// deployment can reach by somebody editing a script after the fact.
func writeGlobalBeforeDir(t *testing.T, svc *BackupService, root string) {
	t.Helper()

	cfg, err := config.Load(svc.configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Workflows.Root = root
	cfg.Workflows.Global.BeforeDir = "global-before"

	if err := svc.persistConfig(cfg); err != nil {
		t.Fatalf("persistConfig: %v", err)
	}
}

// Review BLOCKER 2: a deployment-wide patch that moves workflows.root
// re-resolves every set's stage directories underneath the NEW root, so
// it has to verify those too.
//
// Without it, moving the root re-points every backup set's hooks at
// scripts nobody verified, and the gate reports nothing because it only
// ever looked at the global stages. This drives the exact shape: the set
// keeps its own before_dir, and the patch moves the root so that name
// now resolves to a directory holding an unsafe script.
func TestARootChangeIsRefusedByAPerSetScriptUnderTheNewRoot(t *testing.T) {
	t.Parallel()

	svc, configPath, root := openWorkflowSaveService(t)

	// The set's own stage, clean under the current root, so configuring
	// it is allowed.
	writeHook(t, root, "alpha-before", "dump.remote.sh", "#!/bin/bash", "echo fine")
	if _, err := svc.UpdateBackupSetWorkflow(context.Background(), "production/alpha", UpdateBackupSetWorkflowRequest{
		BeforeDir: new("alpha-before"),
	}); err != nil {
		t.Fatalf("configuring a clean per-set stage: %v", err)
	}

	// A second tree, where the SAME relative name holds a script that
	// must never be saved. Nothing about the set's own block changes.
	moved := filepath.Join(filepath.Dir(root), "workflows-moved")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatalf("MkdirAll(moved root): %v", err)
	}
	writeHook(t, moved, "alpha-before", "dump.remote.sh", "#!/bin/bash", "rm -rf $STAGING/")

	_, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		Root: new(moved),
	})

	if !errors.Is(err, ErrWorkflowScriptRefused) {
		t.Fatalf("moving the root onto a tree holding an unsafe per-set hook = %v, want ErrWorkflowScriptRefused", err)
	}

	var refusal *WorkflowScriptRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the refusal carries no findings: %T", err)
	}
	if len(refusal.Scripts) != 1 {
		t.Fatalf("refusal covers %d script(s), want the one per-set script: %+v", len(refusal.Scripts), refusal.Scripts)
	}
	if got := refusal.Scripts[0]; got.BackupSetID != "production/alpha" || got.Scope != "set" {
		t.Errorf("refusal names %+v, want production/alpha's own set-scoped stage", got)
	}
	if !strings.Contains(err.Error(), "production/alpha") {
		t.Errorf("the message does not say whose set it is: %v", err)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.Root == moved {
		t.Error("a refused root change was written to disk")
	}
}

// And the same patch is not refused when the new tree is sound, so the
// test above is exercising the gate rather than a root change that
// always fails.
func TestARootChangeSucceedsWhenEverySetsHooksAreSoundUnderTheNewRoot(t *testing.T) {
	t.Parallel()

	svc, configPath, root := openWorkflowSaveService(t)
	writeHook(t, root, "alpha-before", "dump.remote.sh", "#!/bin/bash", "echo fine")
	if _, err := svc.UpdateBackupSetWorkflow(context.Background(), "production/alpha", UpdateBackupSetWorkflowRequest{
		BeforeDir: new("alpha-before"),
	}); err != nil {
		t.Fatalf("configuring a clean per-set stage: %v", err)
	}

	moved := filepath.Join(filepath.Dir(root), "workflows-sound")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatalf("MkdirAll(moved root): %v", err)
	}
	writeHook(t, moved, "alpha-before", "dump.remote.sh", "#!/usr/bin/env bash", "set -euo pipefail", "echo moved")

	if _, err := svc.UpdateWorkflowSettings(context.Background(), UpdateWorkflowSettingsRequest{
		Root: new(moved),
	}); err != nil {
		t.Fatalf("moving the root onto a sound tree was refused: %v", err)
	}

	reloaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if reloaded.Workflows.Root != moved {
		t.Errorf("root = %q on disk, want %q", reloaded.Workflows.Root, moved)
	}
}

// Review MAJOR 1: the two things that observe a script's syntax -- this
// product's parser, in process, and the executor's `bash -n` -- report
// under the same two check ids, and the report must carry ONE row per
// script at the worst severity rather than a pass beside a refusal.
func TestTheSyntaxChecksCarryOneRowPerScriptAtTheWorstSeverity(t *testing.T) {
	t.Parallel()

	svc, _, root := openWorkflowSaveService(t)
	writeHook(t, root, "global-before", "clean.local.sh", "#!/bin/bash", "rm -rf $STAGING/tmp")
	writeHook(t, root, "global-before", "fine.local.sh", "#!/usr/bin/env bash", "set -euo pipefail", "echo ok")
	writeGlobalBeforeDir(t, svc, root)

	report, err := svc.ValidateWorkflow(context.Background(), "production/alpha")
	if err != nil {
		t.Fatalf("ValidateWorkflow: %v", err)
	}

	seen := map[string][]WorkflowFinding{}
	for _, f := range report.Findings {
		if f.Check != WorkflowCheckLocalBashSyntax && f.Check != WorkflowCheckRemoteBashSyntax {
			continue
		}
		seen[f.Check+"|"+f.Script] = append(seen[f.Check+"|"+f.Script], f)
	}

	for key, findings := range seen {
		if len(findings) != 1 {
			t.Errorf("%s carries %d rows, want exactly one at the worst severity: %+v", key, len(findings), findings)
		}
	}

	// And the worst severity is the one that survives: the script with
	// the blocking finding must not also be reported as passing.
	var broken []WorkflowFinding
	for _, f := range report.Findings {
		if f.Check == WorkflowCheckLocalBashSyntax && f.Script == "clean.local.sh" {
			broken = append(broken, f)
		}
	}
	if len(broken) != 1 || broken[0].Severity != WorkflowSeverityError {
		t.Errorf("the blocking script's syntax rows = %+v, want exactly one error row", broken)
	}

	// The aggregate "N hook(s) parse" row is dropped once there are
	// per-script rows: it either repeats them or contradicts them.
	for _, f := range report.Findings {
		if f.Check == WorkflowCheckLocalBashSyntax && f.Script == "" && f.Severity == WorkflowSeverityOK {
			t.Errorf("a check-level pass survived beside per-script rows: %+v", f)
		}
	}
}

// The excerpt review MAJOR 2 asks for: a finding carries the script's own
// line, inert and bounded, from the bytes this validation hashed.
func TestEachFindingCarriesItsOwnSourceLine(t *testing.T) {
	t.Parallel()

	svc, _, root := openWorkflowSaveService(t)
	writeHook(t, root, "global-before", "clean.local.sh",
		"#!/bin/bash",
		"rm -rf $STAGING/tmp",
		"echo \x1b]0;pwned\x07done",
	)
	writeGlobalBeforeDir(t, svc, root)

	report, err := svc.ValidateWorkflow(context.Background(), "production/alpha")
	if err != nil {
		t.Fatalf("ValidateWorkflow: %v", err)
	}
	if len(report.Scripts) != 1 {
		t.Fatalf("scripts = %+v", report.Scripts)
	}

	var found bool
	for _, f := range report.Scripts[0].Lint.Findings {
		if f.Code != "BSH003" {
			continue
		}
		found = true

		if len(f.Excerpt.Lines) == 0 {
			t.Fatalf("BSH003 carries no source excerpt: %+v", f)
		}

		var onTheReportedLine string
		for _, line := range f.Excerpt.Lines {
			if line.Number == f.Line {
				onTheReportedLine = line.Text
			}
			for _, r := range line.Text {
				if r < 0x20 || r == 0x7f {
					t.Errorf("the excerpt carries a control character a terminal would act on: %q", line.Text)
				}
			}
		}
		if !strings.Contains(onTheReportedLine, "rm -rf") {
			t.Errorf("the excerpt's line %d = %q, want the line the finding is about", f.Line, onTheReportedLine)
		}
	}
	if !found {
		t.Fatalf("no BSH003 finding on the script: %+v", report.Scripts[0].Lint)
	}
}

// The captured scripts a validation reports on have to outlive the
// on-disk checks, because the pass is not over when those finish: the
// host runner's own `bash -n` runs afterwards, over the SAME captured
// bytes, and reads them out of the spool through plan.OpenScript.
//
// #816's rig found this from a browser. checkPlan deleted its spool on
// the way out, so every set with a NAME.local.sh hook came back as
//
//	local_bash_syntax  error  this workflow run plan cannot be built:
//	<state>/workflow-runs/validate-NNN cannot be opened: no such file
//	or directory
//
// once per local step -- and an error-severity finding is what makes
// WorkflowValid false, so a deployment whose hooks were sound reported
// "this backup set is sound and its hooks are not" on the CLI and in the
// UI's validation report. The probe itself needs a runner and a docker
// daemon; the lifetime that broke does not, which is what this holds.
func TestTheValidationSpoolOutlivesThePlanChecks(t *testing.T) {
	t.Parallel()

	svc, _, root := openWorkflowSaveService(t)
	writeHook(t, root, "alpha-before", "10-quiesce.local.sh",
		"#!/usr/bin/env bash",
		"echo quiescing",
	)

	cfg := svc.state.Load().inner.Config
	bs, err := lookupConfiguredBackupSet(cfg, "production/alpha")
	if err != nil {
		t.Fatalf("lookupConfiguredBackupSet: %v", err)
	}
	bs.Workflow = &config.SetWorkflow{BeforeDir: "alpha-before"}

	v := &workflowValidator{svc: svc, cfg: cfg, set: bs}
	v.checkPlan(context.Background(), cfg.WorkflowStagesFor(&bs))

	steps := v.plan.Steps()
	if len(steps) != 1 {
		t.Fatalf("the plan holds %d step(s), want the one hook written above: %+v", len(steps), steps)
	}

	// The assertion, and it is made where the runner's probe makes it.
	if _, err := v.plan.OpenScript(steps[0].ID); err != nil {
		t.Fatalf("the captured script cannot be read after the on-disk checks, so the runner's own bash -n would report a broken deployment: %v", err)
	}

	// And the other half of the same contract: it does not leak. The
	// spool is this validation's private copy under the state directory,
	// and a pass that left one behind would grow one per validation for
	// as long as the deployment ran.
	if v.releaseSpool == nil {
		t.Fatal("the validation captured a spool and kept no way to remove it")
	}
	v.releaseSpool()
	if _, err := v.plan.OpenScript(steps[0].ID); err == nil {
		t.Error("the spool survived the pass that owns it")
	}
}
