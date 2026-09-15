// The canonical runtime contract, asked again with local workflow hooks
// in use (EPIC L #812).
//
// The engine contract itself is checked by contract_test.go: every field
// the contract names is declared, and no definition needs a prohibited
// host privilege. What no check in this package could say before #865 is
// whether the feature that runs a shell script on the NAS bought its
// shell by weakening any of that -- because the two mounts EPIC L added to
// the canonical definition are not contract fields and nothing asserted
// their write modes, their host paths, or that they are the only two.
//
// That is the whole security argument of the host runner stated as a
// document check: the engine gains a READ-ONLY view of the script tree
// and a WRITABLE directory containing one Unix socket, and it gains no
// capability, no privilege, no device and no Docker socket. Every one of
// those is a line somebody could add to container/compose.yaml in an
// afternoon while debugging a hook that will not start, and the whole
// point of #865 is that such a line has to fail a gate rather than ship.
//
// Each rule below is also run against a deliberately broken document, for
// contract_test.go's reason: these are negative claims, and a negative
// claim nobody has watched refuse anything is a comment.
package compose_test

import (
	"strings"
	"testing"

	"github.com/retnd/retnd/distribution/compose"
)

// The two container paths EPIC L added to the canonical definition, and
// the write mode each one is allowed.
const (
	// hookScriptsPath is the operator's script tree. Read-only: the
	// engine reads each script once, hashes it and copies it into its own
	// spool, so write access here would only let a compromised engine
	// edit what the host is about to execute.
	hookScriptsPath = "/workflows"

	// runnerDoorPath holds the host runner's socket and nothing else.
	// Writable, because connecting to a Unix socket is a write.
	runnerDoorPath = "/data/run"
)

// daemonSocketDirectories are the host directories a runtime mount must
// never be, because each of them CONTAINS the Docker socket on a normal
// Linux host. The prohibition list in runtime-contract.json names the
// socket itself (and /var, which covers /var/run); these are the
// directory forms of the same fault, asked here because the runtime
// directory is the one mount EPIC L made writable and therefore the one
// somebody would be tempted to point at /run while chasing a hook.
var daemonSocketDirectories = []string{"/run", "/var/run", "/"}

// engine returns the canonical definition's engine service, by role
// rather than by name.
func engine(t *testing.T) (compose.Document, map[string]any) {
	t.Helper()

	doc := canonical(t)
	for name, role := range doc.Roles() {
		if role == compose.RoleEngine {
			return doc, doc.Service(name)
		}
	}
	t.Fatal("the canonical definition declares no engine service, so nothing below is checking the container local hooks run beside")

	return doc, nil
}

// TestTheHookScriptTreeIsMountedReadOnly is the first half of the host
// runner's security argument: the engine may READ what the host is about
// to execute and may not write it.
func TestTheHookScriptTreeIsMountedReadOnly(t *testing.T) {
	t.Parallel()

	doc, svc := engine(t)

	if refused := doc.UnparseableMounts(svc); len(refused) != 0 {
		t.Fatalf("the engine declares volume entries this parser could not resolve, so every mount rule below is checking less than it looks: %v", refused)
	}

	mount, found := mountAt(doc.Mounts(svc), hookScriptsPath)
	if !found {
		t.Fatalf("the engine declares no %s mount, so a deployment built from the canonical definition has no hook scripts at all", hookScriptsPath)
	}
	if !mount.ReadOnly {
		t.Errorf("%s is mounted writable from %s; a compromised engine could then edit the script the host runner is about to execute, which is the one thing the read-only mount exists to prevent",
			hookScriptsPath, mount.HostPath)
	}
}

// TestTheRunnerDoorIsWritableAndIsNotTheDaemonSocketsDirectory is the
// second half. The runtime directory has to be writable -- connecting to a
// Unix socket is a write -- and that makes it the one mount where pointing
// at the wrong host path hands the engine the Docker daemon.
func TestTheRunnerDoorIsWritableAndIsNotTheDaemonSocketsDirectory(t *testing.T) {
	t.Parallel()

	doc, svc := engine(t)

	mount, found := mountAt(doc.Mounts(svc), runnerDoorPath)
	if !found {
		t.Fatalf("the engine declares no %s mount, so there is no socket to reach the host runner through and no local hook can run", runnerDoorPath)
	}
	if mount.ReadOnly {
		t.Errorf("%s is mounted read-only, so the engine cannot connect to the runner's socket at all", runnerDoorPath)
	}
	for _, dir := range daemonSocketDirectories {
		if equalHostPath(mount.HostPath, dir) {
			t.Errorf("the runner's runtime directory is host path %s, which contains the Docker daemon socket on a normal host: that is root on the NAS with extra steps, handed to the one container this contract keeps unprivileged",
				mount.HostPath)
		}
	}
}

// TestNoWorkflowMountReachesTheRunnersWorkspaceOrIsWritable is the rule
// that keeps the writable surface one directory.
//
// It is not a frozen list of mounts, deliberately. A read-only single-FILE
// credential mount is the shape this deployment already uses for the SSH
// key and for known_hosts, and the engine needs the runner's token the
// same way, so a rule that refused a third one would refuse the correct
// change. What is refused is the two shapes that would actually cost
// something:
//
//   - a WRITABLE mount anywhere in the workflow surface other than the
//     runner's door. Read-only is the whole argument for the script tree
//     and for any credential beside it.
//   - anything nested UNDER the runner's door, which is how the runner's
//     own workspace would become reachable. That workspace is where the
//     runner writes its private copy of a script and the per-step working
//     directory it later removes recursively: a symbolic link planted at a
//     run directory would let a compromised engine choose which host path
//     the runner writes an executable file into, and which one it then
//     deletes. The compose file says the same thing in its own words and
//     names no mount for it.
func TestNoWorkflowMountReachesTheRunnersWorkspaceOrIsWritable(t *testing.T) {
	t.Parallel()

	doc, svc := engine(t)

	for _, finding := range workflowMountFindings(doc, svc) {
		t.Error(finding)
	}
}

// workflowMountFindings is the rule above as a function, so the mutation
// controls below can apply the same rule to a broken document rather than
// restating it.
func workflowMountFindings(doc compose.Document, svc map[string]any) []string {
	var findings []string

	for _, mount := range doc.Mounts(svc) {
		nested := strings.HasPrefix(mount.ContainerPath, runnerDoorPath+"/")
		if nested {
			findings = append(findings, "the engine mounts "+mount.ContainerPath+" from "+mount.HostPath+
				", which is inside the host runner's own runtime directory: the runner's workspace must not be reachable from the container, because a link planted at a run directory chooses which host path the runner writes an executable into")

			continue
		}
		if mount.ContainerPath == runnerDoorPath || mount.ReadOnly {
			continue
		}
		if strings.Contains(mount.ContainerPath, "workflow") || strings.Contains(mount.ContainerPath, "workspace") {
			findings = append(findings, "the engine mounts "+mount.ContainerPath+" from "+mount.HostPath+
				" WRITABLE; the only writable door local hooks need is "+runnerDoorPath+", which holds a socket, and everything else in the workflow surface is read-only on purpose")
		}
	}

	return findings
}

// TestLocalHooksBoughtTheirShellWithoutWeakeningTheContainer is the claim
// #807's consensus table makes in one line: a dedicated host runner,
// rather than a weaker container to gain a shell.
//
// It asserts the posture keys DIRECTLY, in the presence of the two hook
// mounts, rather than relying on the field list: runtime-contract.json
// requires `user` and `read_only` as fields and prohibits cap_add,
// privileged, host namespaces and unconfined profiles -- but nothing
// requires cap_drop or no-new-privileges to be PRESENT, so a definition
// that dropped the shared x-security anchor while adding a hook mount
// would pass every existing check.
func TestLocalHooksBoughtTheirShellWithoutWeakeningTheContainer(t *testing.T) {
	t.Parallel()

	doc := canonical(t)

	// The mounts have to actually be there, or this test is about a
	// deployment that runs no local hooks and proves nothing about one
	// that does.
	_, svc := engine(t)
	if _, found := mountAt(doc.Mounts(svc), runnerDoorPath); !found {
		t.Fatalf("the canonical definition has no %s mount, so this test is not looking at a deployment with local hooks enabled", runnerDoorPath)
	}

	for _, name := range doc.ServiceNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service := doc.Service(name)

			if dropped := stringsOf(service["cap_drop"]); !containsString(dropped, "ALL") {
				t.Errorf("service %s declares cap_drop %v; every capability has to be dropped, and the host runner exists precisely so that gaining a shell does not need one", name, dropped)
			}
			if opts := stringsOf(service["security_opt"]); !containsString(opts, "no-new-privileges:true") {
				t.Errorf("service %s declares security_opt %v, which does not set no-new-privileges:true; a setuid binary inside the image could then re-privilege whatever a hook's path reaches", name, opts)
			}
			if privileged, ok := service["privileged"].(bool); ok && privileged {
				t.Errorf("service %s is privileged, which removes the container boundary this whole runner design exists to keep", name)
			}
			if ro, ok := service["read_only"].(bool); !ok || !ro {
				t.Errorf("service %s does not set read_only: true, so the image's own filesystem is writable at runtime", name)
			}
			user, _ := service["user"].(string)
			if user == "" {
				t.Errorf("service %s names no user, so it runs as root in the image's default account", name)
			}
			if rootUser(user) {
				t.Errorf("service %s runs as %q, which is root", name, user)
			}
		})
	}

	// And the generic prohibition list still passes with the hook mounts
	// in place. This is deliberately the contract's own check rather than
	// a second copy of its rules: what is new here is the CONDITION it is
	// asked under.
	if findings := doc.CheckProhibited(compose.MustLoadContract()); len(findings) != 0 {
		for _, f := range findings {
			t.Errorf("with local hooks enabled the canonical definition needs a prohibited host privilege: %s: %s", f.Rule, f.Detail)
		}
	}
}

// TestTheWorkflowMountRulesFireOnTheMistakesTheyExistFor is the positive
// control for every rule above. Each mutation is one line an operator or a
// maintainer could plausibly add to the canonical definition, and each one
// has to be caught by the rule named beside it.
func TestTheWorkflowMountRulesFireOnTheMistakesTheyExistFor(t *testing.T) {
	t.Parallel()

	for _, mutation := range []struct {
		name string

		// volume is the entry added to every service.
		volume string

		// caught reports whether the rules above see this document as
		// broken. It is the rule under test, applied to the mutated
		// document rather than to the real one.
		caught func(doc compose.Document, svc map[string]any) bool
	}{
		{
			name:   "the script tree becomes writable",
			volume: "/srv/backupd/workflows:" + hookScriptsPath,
			caught: func(doc compose.Document, svc map[string]any) bool {
				// The last entry wins in this parser's view, so the
				// writable duplicate is what a check would read.
				for _, m := range doc.Mounts(svc) {
					if m.ContainerPath == hookScriptsPath && !m.ReadOnly {
						return true
					}
				}

				return false
			},
		},
		{
			name:   "the runner door is pointed at the daemon socket's directory",
			volume: "/run:" + runnerDoorPath,
			caught: func(doc compose.Document, svc map[string]any) bool {
				for _, m := range doc.Mounts(svc) {
					if m.ContainerPath != runnerDoorPath {
						continue
					}
					for _, dir := range daemonSocketDirectories {
						if equalHostPath(m.HostPath, dir) {
							return true
						}
					}
				}

				return false
			},
		},
		{
			name:   "the runner's own workspace is mounted into the engine",
			volume: "/srv/backupd/run/workspace:/data/run/workspace",
			caught: func(doc compose.Document, svc map[string]any) bool {
				return len(workflowMountFindings(doc, svc)) != 0
			},
		},
		{
			name:   "a workflow directory beside the script tree is writable",
			volume: "/srv/backupd/workflow-extra:/etc/backupd/workflow-extra",
			caught: func(doc compose.Document, svc map[string]any) bool {
				return len(workflowMountFindings(doc, svc)) != 0
			},
		},
		{
			name:   "the Docker socket is handed to the engine to run hooks with",
			volume: "/var/run/docker.sock:/var/run/docker.sock",
			caught: func(doc compose.Document, _ map[string]any) bool {
				return len(doc.CheckProhibited(compose.MustLoadContract())) != 0
			},
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()

			doc, describe := canonical(t).WithVolume(mutation.volume)
			if describe == "" {
				t.Fatal("the mutation helper injected nothing, so this control is watching an unmutated document")
			}

			_, svc := engineOf(t, doc)
			if !mutation.caught(doc, svc) {
				t.Errorf("the rule did not notice %q, so it would not notice it in container/compose.yaml either", describe)
			}
		})
	}
}

// TestAReadOnlyCredentialFileMountIsNotRefused is the negative control for
// the rule above, and it is the one that stops this file from being a
// frozen mount list.
//
// The engine reads the host runner's token to authenticate to it, the same
// way it reads the SSH private key and known_hosts: a read-only single
// file, from a host path an operator named. A rule that refused a third
// such mount because it had the word "workflow" in its path would refuse
// the correct change and teach the next person to delete the rule.
func TestAReadOnlyCredentialFileMountIsNotRefused(t *testing.T) {
	t.Parallel()

	doc, describe := canonical(t).WithVolume("/srv/backupd/secrets/workflow-runner.token:/etc/backupd/workflow-runner.token:ro")
	if describe == "" {
		t.Fatal("the mutation helper injected nothing, so this control is watching an unmutated document")
	}

	_, svc := engineOf(t, doc)

	// It really is there, or this control is about a document with no
	// credential mount in it.
	if _, found := mountAt(doc.Mounts(svc), "/etc/backupd/workflow-runner.token"); !found {
		t.Fatalf("the injected credential mount is not in the document: %s", describe)
	}
	if findings := workflowMountFindings(doc, svc); len(findings) != 0 {
		t.Errorf("a read-only credential file mount was refused, which would refuse the engine's own access to the runner's token:\n  %s", strings.Join(findings, "\n  "))
	}
	if prohibited := doc.CheckProhibited(compose.MustLoadContract()); len(prohibited) != 0 {
		for _, f := range prohibited {
			t.Errorf("a read-only credential file mount tripped the prohibition list: %s: %s", f.Rule, f.Detail)
		}
	}
}

// engineOf is engine() for a document a test has already built.
func engineOf(t *testing.T, doc compose.Document) (compose.Document, map[string]any) {
	t.Helper()

	for name, role := range doc.Roles() {
		if role == compose.RoleEngine {
			return doc, doc.Service(name)
		}
	}
	t.Fatal("the document declares no engine service")

	return doc, nil
}

func mountAt(mounts []compose.Mount, containerPath string) (compose.Mount, bool) {
	for _, m := range mounts {
		if m.ContainerPath == containerPath {
			return m, true
		}
	}

	return compose.Mount{}, false
}

// equalHostPath compares two host paths as paths rather than as strings,
// so a trailing slash or a doubled separator is the same directory.
func equalHostPath(a, b string) bool {
	trim := func(s string) string {
		for len(s) > 1 && strings.HasSuffix(s, "/") {
			s = strings.TrimSuffix(s, "/")
		}

		return strings.ReplaceAll(s, "//", "/")
	}

	return trim(a) == trim(b)
}

func stringsOf(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}

	return out
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}

	return false
}

// rootUser reports whether a compose `user:` value names root. The
// canonical definition writes ${PUID:-1000}:${PGID:-1000}, so what is
// being read here is the resolved value, and "0", "0:0" and "root" all
// have to be refused rather than only the first of them.
func rootUser(user string) bool {
	uid, _, _ := strings.Cut(user, ":")
	switch strings.TrimSpace(uid) {
	case "root":
		return true
	}
	trimmed := strings.TrimLeft(strings.TrimSpace(uid), "0")

	return trimmed == "" && strings.TrimSpace(uid) != ""
}
