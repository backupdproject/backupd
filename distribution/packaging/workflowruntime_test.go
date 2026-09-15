package packaging

import (
	"os"
	"strings"
	"testing"
)

// This file decides the cross-provider matrix's `local-workflow-hooks`
// capability (issue #877) and holds the three declarations that have to
// agree about it to each other: the capability contract in
// apps/common/platform/capabilities, conformance.json's own
// workflowRunner blocks, and the document each provider's operator
// actually reads.
//
// Two of the tests here run OUTSIDE the declaration machinery, and that
// is deliberate rather than redundant. The matrix's staleness guard
// requires a check to FAIL for a cell declared unsupported or
// not-applicable, which means the cell check cannot also be the thing
// that holds an appliance's document to saying so — the moment it did,
// the document requirement would only be enforced on the three providers
// that support the feature. So the "did anyone tell the operator" half is
// asserted separately, over all eleven columns, the same way
// TestReleaseManifestPinsACommitThisHistoryCanReach asks its question a
// second time so that re-declaring a cell cannot make a fact go away.

// checkLocalWorkflowHooks is the capability's cell check. See
// LocalHookCell for what it decides and why a NOT_APPLICABLE provider
// legitimately fails it.
func checkLocalWorkflowHooks(p providerUnderTest) (bool, string) {
	contract, err := ReadLocalHookContract()
	if err != nil {
		return false, err.Error()
	}
	return LocalHookCell(p.spec.WorkflowRunner, contract)
}

// ---------------------------------------------------------------------
// The declarations agree with each other
// ---------------------------------------------------------------------

// TestEveryProviderDeclaresAWorkflowRunnerAnswer is the completeness
// guard for the new block. A provider that simply omits it would get the
// zero WorkflowRunner, which reads as "no answer" — and the whole point
// of #877 is that there is no such thing as no answer: a platform that
// cannot run the runner has to say so.
func TestEveryProviderDeclaresAWorkflowRunnerAnswer(t *testing.T) {
	c := MustLoadConformance()
	contract, err := ReadLocalHookContract()
	if err != nil {
		t.Fatal(err)
	}

	for _, pid := range c.ProviderIDs() {
		wr := c.Providers[pid].WorkflowRunner
		switch wr.LocalHooks {
		case LocalHooksAvailable, LocalHooksUnavailable:
		default:
			t.Errorf("%s: workflowRunner.localHooks is %q, want %q or %q", pid, wr.LocalHooks, LocalHooksAvailable, LocalHooksUnavailable)
		}
		if wr.Doc == "" {
			t.Errorf("%s: workflowRunner names no operator-visible document; the document is the only place an operator on this platform finds out whether local hooks run", pid)
			continue
		}
		if _, err := os.Stat(Path(wr.Doc)); err != nil {
			t.Errorf("%s: workflowRunner names document %s, which is not in the tree: %v", pid, wr.Doc, err)
		}
		if wr.Platform == "" {
			continue
		}
		if _, ok := contract[wr.Platform]; !ok {
			t.Errorf("%s: workflowRunner names platform %q, which %s declares no local-hook answer for", pid, wr.Platform, LocalHookContractFile)
		}
	}
}

// TestTheCapabilityContractAndTheMatrixAgreeOnLocalHooks is the
// cross-layer pin, in both directions.
//
// One direction stops the matrix drifting away from the contract: a
// column that claimed local hooks the running process refuses would put
// a green cell in front of an operator whose hooks never run. The other
// stops the contract growing a platform no column reports, which is how a
// capability ends up decided in Go and invisible in the matrix an epic's
// gate is computed over.
func TestTheCapabilityContractAndTheMatrixAgreeOnLocalHooks(t *testing.T) {
	c := MustLoadConformance()
	contract, err := ReadLocalHookContract()
	if err != nil {
		t.Fatal(err)
	}
	if len(contract) == 0 {
		t.Fatal("the capability contract declares no local-hook answers, so every comparison below would be vacuous")
	}

	declaredPlatforms := map[string]string{}
	for _, pid := range c.ProviderIDs() {
		wr := c.Providers[pid].WorkflowRunner
		if wr.Platform == "" {
			continue
		}
		declaredPlatforms[wr.Platform] = pid
		want, ok := contract[wr.Platform]
		if !ok {
			continue // TestEveryProviderDeclaresAWorkflowRunnerAnswer reports this.
		}
		if wr.LocalHooks != want {
			t.Errorf("provider %s says local hooks are %q for platform %q, and %s says %q.\n\nThe contract is the authority: it is what a running process resolves. Move the matrix, or change the contract and this cell together.",
				pid, wr.LocalHooks, wr.Platform, LocalHookContractFile, want)
		}
	}
	for platform := range contract {
		if _, ok := declaredPlatforms[platform]; !ok {
			t.Errorf("%s answers the local-hook question for platform %q and no provider column claims that platform, so the answer is decided in Go and reported nowhere",
				LocalHookContractFile, platform)
		}
	}
}

// ---------------------------------------------------------------------
// Somebody told the operator (all eleven columns)
// ---------------------------------------------------------------------

// TestEveryProviderStatesItsLocalHookAnswerWhereItsOperatorWillRead is
// the acceptance criterion of #877 that no cell check can carry, for the
// reason given at the top of this file. It runs over all eleven columns,
// including the four that select the generic runtime profile and the one
// whose gate belongs to another epic.
//
// It is what the not-applicable and unsupported cells point their
// verifiedBy at, so declaring a cell exempt from the matrix does not
// exempt the platform from having told anyone.
func TestEveryProviderStatesItsLocalHookAnswerWhereItsOperatorWillRead(t *testing.T) {
	c := MustLoadConformance()

	for _, pid := range c.ProviderIDs() {
		t.Run(pid, func(t *testing.T) {
			wr := c.Providers[pid].WorkflowRunner
			if wr.Doc == "" {
				t.Fatal("no operator-visible document declared")
			}
			data, err := os.ReadFile(Path(wr.Doc))
			if err != nil {
				t.Fatalf("read %s: %v", wr.Doc, err)
			}
			ok, detail := LocalHookDocStates(wr.Doc, string(data), wr.LocalHooks)
			if !ok {
				t.Errorf("%s\n\nThis is the failure #877 exists to prevent: an operator of a %s deployment with no way to find out whether a local workflow hook will run.", detail, c.Providers[pid].DisplayName)
			}
		})
	}
}

// TestEveryProviderThatAdvertisesLocalHooksCanActuallyReachTheRunner is
// issue #921's gate, and the sweep above is exactly why it is a separate
// one: that test asks whether the operator was TOLD, this one asks
// whether the deployment they were told about can do it.
//
// Four profiles passed every declaration check while mounting none of
// the three paths the engine reaches the runner through, so the answer an
// operator read was true about the platform and false about the stack
// they had just installed.
func TestEveryProviderThatAdvertisesLocalHooksCanActuallyReachTheRunner(t *testing.T) {
	c := MustLoadConformance()
	canonical := MustLoad()
	asked := 0

	for _, pid := range c.ProviderIDs() {
		t.Run(pid, func(t *testing.T) {
			p := providerUnderTest{id: pid, spec: c.Providers[pid], canonical: canonical}
			wr := p.spec.WorkflowRunner
			kind := p.spec.Metadata.Kind
			if wr.LocalHooks != LocalHooksAvailable || kind == "canonical-compose" {
				return
			}
			asked++
			svcs, err := p.services()
			if err != nil {
				t.Fatalf("read this provider's runtime definition: %v", err)
			}
			rt, drift := ReduceToRoles(pid, svcs, canonical)
			if len(drift) > 0 {
				t.Fatalf("could not reduce this provider to roles:\n%s", FormatDrift(drift))
			}
			if v := CheckLocalHookMounts(p.spec.Metadata.Compose, wr, kind, rt.Engine, canonical); len(v) > 0 {
				t.Errorf("this provider advertises local workflow hooks it cannot run:\n%s", format(v))
			}
		})
	}

	// A sweep that asked nobody is a sweep that proves nothing, and the
	// way that happens is a declaration edit rather than a deleted test.
	// Five providers ship a profile of their own and advertise the
	// capability: Generic Docker, which always carried the three mounts,
	// and the four #921 found without them.
	if asked < 5 {
		t.Errorf("this gate ran over %d provider(s) and there are five: a provider that stopped advertising local hooks, or stopped shipping a profile of its own, is a declaration change and not a smaller gate", asked)
	}
}

// TestTheLocalHookMountRuleFiresOnTheCombinationNothingElseReads is the
// control, and the first case is the one that matters: it is #921's own
// shape, the state four shipped profiles were in, and the rule has to
// redden on it or this whole gate is decoration.
//
// The last two cases are the optionality this must not break. Host-plane
// mounts stay optional for a provider that does not advertise the
// capability and for one that ships no profile of its own, which is the
// distinction HostPlaneRoles exists to draw.
func TestTheLocalHookMountRuleFiresOnTheCombinationNothingElseReads(t *testing.T) {
	c := MustLoad()
	available := WorkflowRunner{LocalHooks: LocalHooksAvailable, Platform: "openmediavault", Doc: "docs/acceptance/openmediavault-provider-acceptance.md"}

	mount := func(role string, readOnly bool) Mount {
		path, ok := c.ContainerPaths.ByRole(role)
		if !ok {
			t.Fatalf("canonical.json declares no container path for the %q role", role)
		}
		return Mount{Role: role, HostPath: "/srv/backupd/" + role, ContainerPath: path, ReadOnly: readOnly}
	}
	engine := func(mounts ...Mount) *Service {
		return &Service{Name: "backupd", Source: "fixture.yml", Mounts: mounts}
	}
	// The five storage roles every profile already carries. The rule must
	// decide on the runner's three and on nothing else.
	storage := []Mount{
		mount("state", false), mount("backups", false), mount("config", false),
		mount("sshKey", true), mount("knownHosts", true),
	}
	with := func(extra ...Mount) *Service {
		return engine(append(append([]Mount(nil), storage...), extra...)...)
	}

	runner := []Mount{mount("workflows", true), mount("runtime", false), mount("runnerToken", true)}

	t.Run("advertised and unreachable, which is #921", func(t *testing.T) {
		v := CheckLocalHookMounts("fixture.yml", available, "compose", with(), c)
		if len(v) != len(HostPlaneRoles) {
			t.Fatalf("want one violation per unmounted runner path (%d), got %d: %s", len(HostPlaneRoles), len(v), oneLine(v))
		}
	})

	t.Run("advertised and reachable", func(t *testing.T) {
		if v := CheckLocalHookMounts("fixture.yml", available, "compose", with(runner...), c); len(v) > 0 {
			t.Errorf("a profile carrying all three runner paths was refused: %s", oneLine(v))
		}
	})

	t.Run("the scripts directory handed over writable", func(t *testing.T) {
		bad := []Mount{mount("workflows", false), mount("runtime", false), mount("runnerToken", true)}
		if v := CheckLocalHookMounts("fixture.yml", available, "compose", with(bad...), c); len(v) == 0 {
			t.Error("a writable /workflows was accepted; the engine executes what it reads out of there")
		}
	})

	t.Run("the socket directory handed over read-only", func(t *testing.T) {
		bad := []Mount{mount("workflows", true), mount("runtime", true), mount("runnerToken", true)}
		if v := CheckLocalHookMounts("fixture.yml", available, "compose", with(bad...), c); len(v) == 0 {
			t.Error("a read-only /data/run was accepted, and the runner's socket has to appear in it")
		}
	})

	t.Run("not advertised, so not asked", func(t *testing.T) {
		unavailable := WorkflowRunner{LocalHooks: LocalHooksUnavailable, Platform: "zimaos", Doc: available.Doc}
		if v := CheckLocalHookMounts("fixture.yml", unavailable, "compose", with(), c); len(v) > 0 {
			t.Errorf("a profile that declares local hooks unavailable was told it is missing the runner's storage: %s", oneLine(v))
		}
	})

	t.Run("deploys the canonical stack, so not asked", func(t *testing.T) {
		inherits := WorkflowRunner{LocalHooks: LocalHooksAvailable, Doc: available.Doc}
		if v := CheckLocalHookMounts("fixture.yml", inherits, "canonical-compose", with(), c); len(v) > 0 {
			t.Errorf("a provider that deploys container/compose.yaml itself was held to a profile it does not ship: %s", oneLine(v))
		}
	})

	// The boundary that decides whether this rule catches all four of
	// #921 or only half of them. An empty Platform means "no row in the
	// capability contract, inherits generic's answer", which is CasaOS
	// and Portainer -- and both ship a compose file. Keying the rule on
	// Platform instead of on whose runtime definition it is would let
	// exactly those two through.
	t.Run("no capability-contract row but a profile of its own, so asked", func(t *testing.T) {
		inherited := WorkflowRunner{LocalHooks: LocalHooksAvailable, Doc: available.Doc}
		if v := CheckLocalHookMounts("fixture.yml", inherited, "compose", with(), c); len(v) != len(HostPlaneRoles) {
			t.Errorf("a provider that inherits the available answer and ships its own stack was not held to it: %s", oneLine(v))
		}
	})
}

// The positive control for the test above, and it is the one that matters
// most: the requirement is a set of regexes over a markdown document, and
// a regex set that cannot fail is decoration. Both branches are proved,
// because they ask for opposite things.
func TestTheLocalHookDocRequirementWouldNoticeASilentDocument(t *testing.T) {
	t.Run("available", func(t *testing.T) {
		full := "Install it with backupd-workflow-runner.service, run `sudo usermod -aG docker backupd`, and set WORKFLOW_RUNNER_HOOK_IMAGE."
		if ok, detail := LocalHookDocStates("fixture.md", full, LocalHooksAvailable); !ok {
			t.Fatalf("a document stating all three facts was refused: %s", detail)
		}
		for _, missing := range []string{
			"run `sudo usermod -aG docker backupd` and set WORKFLOW_RUNNER_HOOK_IMAGE.",
			"Install backupd-workflow-runner.service and set WORKFLOW_RUNNER_HOOK_IMAGE.",
			"Install backupd-workflow-runner.service, then `sudo usermod -aG docker backupd`.",
		} {
			if ok, _ := LocalHookDocStates("fixture.md", missing, LocalHooksAvailable); ok {
				t.Errorf("a document that omits one of the three prerequisites passed:\n%s", missing)
			}
		}

		// The other direction, and the review finding that put it here.
		// The three install facts sit perfectly happily in a document
		// that goes on to tell the operator local hooks are unavailable
		// here: nothing about the requirements above is contradicted by
		// that sentence, so a contract row flipped to `available` over a
		// stale procedure was green. The one reader who matters would
		// have been told the opposite of the truth.
		stale := full + " Note that local workflow hooks are unavailable on this platform."
		ok, detail := LocalHookDocStates("fixture.md", stale, LocalHooksAvailable)
		if ok {
			t.Error("a document that says local hooks are unavailable passed as an `available` provider's")
		}
		if !strings.Contains(detail, "opposite of what the capability contract answers") {
			t.Errorf("the refusal does not say what is wrong with it: %s", detail)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		full := "Local workflow hooks are unavailable on this platform: the capability contract answers so, and the engine refuses a step with no host workflow runner behind it. Use a remote workflow step instead."
		if ok, detail := LocalHookDocStates("fixture.md", full, LocalHooksUnavailable); !ok {
			t.Fatalf("a document stating all three facts was refused: %s", detail)
		}
		for _, missing := range []string{
			// Says nothing about availability at all: the silent case.
			"The capability contract answers so. Use a remote workflow step instead.",
			// Says it is unavailable and attributes the refusal to
			// nothing, which is the difference between a told operator
			// and a hook that quietly never ran.
			"Local workflow hooks are unavailable here. Use a remote workflow step instead.",
			// Attributes it to a mechanism that does not exist. This is
			// the exact sentence five procedures carried until the
			// review: the installer has no platform gate, so nothing
			// there refuses on these grounds.
			"Local workflow hooks are unavailable here: the installer's preflight refuses to provision a runner. Use a remote workflow step instead.",
			// Says no and names no alternative (§22).
			"Local workflow hooks are unavailable on this platform: the capability contract answers so.",
		} {
			if ok, _ := LocalHookDocStates("fixture.md", missing, LocalHooksUnavailable); ok {
				t.Errorf("a document that omits one of the three statements passed:\n%s", missing)
			}
		}
	})

	t.Run("an answer nobody declared", func(t *testing.T) {
		if ok, _ := LocalHookDocStates("fixture.md", "anything at all", "maybe"); ok {
			t.Error("an unrecognised answer was accepted; the two answers are a closed set")
		}
	})
}

// ---------------------------------------------------------------------
// The cell check refuses the claims it exists to refuse
// ---------------------------------------------------------------------

// The issue's own TDD requirement, stated as a test: "a provider that
// claims the capability without a Docker path fails". Every way of
// claiming it without one is here, because each of them is a different
// file somebody would have edited.
func TestAProviderCannotClaimLocalHooksWithoutADockerPath(t *testing.T) {
	contract := map[string]string{
		"generic":  LocalHooksAvailable,
		"synology": LocalHooksUnavailable,
	}

	for _, tc := range []struct {
		name string
		wr   WorkflowRunner
		want string
	}{
		{
			name: "an appliance whose column claims the capability anyway",
			wr:   WorkflowRunner{LocalHooks: LocalHooksAvailable, Platform: "synology", Doc: "docs/install.md"},
			want: "the capability contract says",
		},
		{
			name: "a platform the contract has never heard of",
			wr:   WorkflowRunner{LocalHooks: LocalHooksAvailable, Platform: "a-platform-nobody-declared", Doc: "docs/install.md"},
			want: "declares no local-hook answer",
		},
		{
			name: "no answer at all",
			wr:   WorkflowRunner{Platform: "generic", Doc: "docs/install.md"},
			want: "declares no workflowRunner block",
		},
		{
			name: "an answer with nowhere for an operator to read it",
			wr:   WorkflowRunner{LocalHooks: LocalHooksAvailable, Platform: "generic"},
			want: "names no operator-visible document",
		},
		{
			name: "a document that is not in the tree",
			wr:   WorkflowRunner{LocalHooks: LocalHooksAvailable, Platform: "generic", Doc: "docs/a-document-nobody-wrote.md"},
			want: "cannot read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, detail := LocalHookCell(tc.wr, contract)
			if ok {
				t.Fatalf("the cell passed; %s", detail)
			}
			if !strings.Contains(detail, tc.want) {
				t.Errorf("refusal does not say why in the expected terms.\n got: %s\nwant it to contain: %s", detail, tc.want)
			}
		})
	}
}

// A provider with no runtime profile of its own must not pass this cell
// by borrowing generic's answer — that is what NOT_APPLICABLE is for, and
// the reason has to say whose answer it actually inherits so a reader is
// not left thinking nobody decided.
func TestAProviderWithNoRuntimeProfileInheritsRatherThanPasses(t *testing.T) {
	contract := map[string]string{"generic": LocalHooksAvailable}
	ok, detail := LocalHookCell(WorkflowRunner{LocalHooks: LocalHooksAvailable, Doc: "docs/install.md"}, contract)
	if ok {
		t.Fatal("a provider with no platform of its own passed the cell on generic's evidence")
	}
	for _, want := range []string{"no runtime profile of its own", LocalHooksAvailable, "docs/install.md"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal does not mention %q: %s", want, detail)
		}
	}
}

// ---------------------------------------------------------------------
// The reader itself
// ---------------------------------------------------------------------

// The contract reader is a regex over somebody else's Go file, so the
// failure that matters is the one where that file moves and the reader
// quietly returns nothing: an empty contract makes every comparison above
// vacuous and every cell fail for the wrong reason. It has to refuse
// instead.
func TestTheContractReaderRefusesRatherThanReturningNothing(t *testing.T) {
	if _, err := ReadLocalHookContract(); err != nil {
		t.Fatalf("reading the real contract failed: %v", err)
	}

	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{"no table", "package capabilities\n\nvar something = 1\n", "no longer declares"},
		{"an empty table", "package capabilities\n\nvar localHookSupport = map[PlatformID]LocalHookSupport{\n}\n", "declares no rows"},
		{"a row naming a constant that does not exist", "package capabilities\n\nvar localHookSupport = map[PlatformID]LocalHookSupport{\n\tPlatformAtlantis: {Available: true},\n}\n", "not a declared PlatformID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(dir+"/localhooks.go", []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := readLocalHookContractFrom(dir+"/localhooks.go", Path(PlatformIDConstFile))
			if err == nil {
				t.Fatal("the reader returned no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say why in the expected terms.\n got: %v\nwant it to contain: %s", err, tc.want)
			}
		})
	}
}

// And the same for the identifier set the rows resolve against: a table
// that parses perfectly against a constant list nobody could read is
// still an answer about nothing.
func TestTheContractReaderRefusesAMissingIdentifierSet(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/capabilities.go", []byte("package capabilities\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readLocalHookContractFrom(Path(LocalHookContractFile), dir+"/capabilities.go")
	if err == nil || !strings.Contains(err.Error(), "declares no") {
		t.Fatalf("reader error = %v, want a refusal naming the missing PlatformID constants", err)
	}
}

// ---------------------------------------------------------------------
// canonical.json's host-side runner contract
// ---------------------------------------------------------------------

// Every field of canonical.json's workflowRunner block is pinned to
// whatever actually decides it. The failure this prevents is specific and
// has already happened once for the image reference
// (TestTheInstallerDefaultsToTheCanonicalImage exists because a 0.2.0
// install pulled 0.1.0 and reported success): a version pinned in several
// files drifts in one of them, and the copy a document quotes is the copy
// an operator acts on.
func TestTheRunnerContractIsPinnedToWhateverDecidesEachField(t *testing.T) {
	c := MustLoad()
	wr := c.WorkflowRunner

	image, err := ReadHookImageAuthority()
	if err != nil {
		t.Fatal(err)
	}
	if wr.HookImage != image {
		t.Errorf("canonical.json pins the hook image at %q and %s defaults to %q.\n\nThe runner is the authority: it is the process that refuses when the image is not there. Move them together, and with them the acceptance procedures that quote the reference.",
			wr.HookImage, HookImageAuthorityFile, image)
	}

	consts, err := ReadInstallerConstants()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field, constName, got string
	}{
		{"unit", "WORKFLOW_RUNNER_UNIT", wr.Unit},
		{"hookImageEnvKey", "WORKFLOW_RUNNER_HOOK_IMAGE_ENV_KEY", wr.HookImageEnvKey},
		{"modeEnvKey", "WORKFLOW_RUNNER_ENV_KEY", wr.ModeEnvKey},
	} {
		want, ok := consts[tc.constName]
		if !ok {
			t.Errorf("%s no longer declares %s, which canonical.json's workflowRunner.%s is pinned to", RunnerInstallerFile, tc.constName, tc.field)
			continue
		}
		if tc.got != want {
			t.Errorf("canonical.json's workflowRunner.%s is %q and %s's %s is %q", tc.field, tc.got, RunnerInstallerFile, tc.constName, want)
		}
	}

	if _, err := os.Stat(Path(wr.Installer)); err != nil {
		t.Errorf("canonical.json names installer %s, which is not in the tree: %v", wr.Installer, err)
	}
}

// engineDockerAccess is the one field that is a promise rather than a
// pin, so what it is held to is the rules that keep it true. A
// declaration whose enforcement has been deleted is worse than no
// declaration: it reads as a checked fact.
func TestTheEngineGainsNothingAndTwoRulesSaySo(t *testing.T) {
	if MustLoad().WorkflowRunner.EngineDockerAccess {
		t.Fatal("canonical.json now claims the engine container has Docker access.\n\nThat is not a manifest edit. docs/runtime-contract.md, ADR 0020 Decision 9 and the whole containment argument for #865's one new privilege rest on the opposite, and every provider's permission rationale tells a store reviewer so.")
	}

	// Rule one: the mount. Both spellings, because a rule that reads one
	// is a rule that misses the other.
	for _, socket := range []string{"/var/run/docker.sock", "/run/docker.sock"} {
		svc := Service{Name: "backupd", Source: "fixture.yml", Mounts: []Mount{{HostPath: socket, ContainerPath: socket}}}
		if v := CheckMountedHostPaths([]Service{svc}); len(v) == 0 {
			t.Errorf("mounting %s is no longer a violation, so canonical.json's engineDockerAccess:false is a claim nothing enforces", socket)
		}
	}

	// Rule two: the two ways of handing over a daemon that need no mount.
	for _, artifact := range []string{
		"services:\n  backupd:\n    group_add:\n      - docker\n",
		"services:\n  backupd:\n    environment:\n      DOCKER_HOST: tcp://127.0.0.1:2375\n",
	} {
		if v := CheckNoContainerDockerAccess("fixture.yml", artifact); len(v) == 0 {
			t.Errorf("this artifact hands a container the Docker daemon and no rule refuses it:\n%s", artifact)
		}
	}

	// And the control: the rule must not fire on the shipped artifacts,
	// or every target's cell would be failing for a reason that has
	// nothing to do with what it checks.
	if v := CheckNoContainerDockerAccess("fixture.yml", "services:\n  backupd:\n    read_only: true\n    cap_drop:\n      - ALL\n"); len(v) != 0 {
		t.Errorf("a hardened service with no Docker access was refused: %s", oneLine(v))
	}
}

// ---------------------------------------------------------------------
// A credential mount with no storage role (#877 / #813's token)
// ---------------------------------------------------------------------

// EPIC L adds mounts that are not one of the five storage roles, and the
// runner's token is a CREDENTIAL among them: read-only for the same
// reason the SSH key and known_hosts are, because a writable credential
// file is one compromised process away from being replaced.
//
// CheckStorageShapes skipped every role-less mount outright, so adding
// the path to canonical.json's readOnlyContainerPaths declared a write
// mode that nothing enforced — the declaration read as a checked fact and
// was not one. Putting it in Roles would have been the wrong fix:
// CheckRequiredMounts demands every role of every adapter, and no
// provider package carries the runner's token, so all eleven columns
// would have started failing for a mount only the canonical stack has.
func TestADeclaredCredentialMountIsHeldToItsWriteModeWithoutAStorageRole(t *testing.T) {
	c := MustLoad()
	const token = "/etc/retnd/workflow-runner.token"

	mount := func(containerPath string, readOnly bool) []Service {
		return []Service{{
			Name:   "backupd",
			Source: "fixture.yaml",
			Mounts: []Mount{{HostPath: "./secrets/tok", ContainerPath: containerPath, ReadOnly: readOnly}},
		}}
	}

	// The SSH key stands in for the token until #813's canonical.json
	// entry lands: both are role-less credential files, and picking one
	// canonical.json already declares read-only keeps this test honest
	// rather than pending. Once the token is declared, the same assertion
	// covers it with no edit here.
	credential := c.ReadOnlyContainerPaths
	if len(credential) == 0 {
		t.Fatal("canonical.json declares no read-only container paths, so there is nothing to enforce a write mode against")
	}
	declared := credential[0]
	if contains(c.ReadOnlyContainerPaths, token) {
		declared = token
	}

	if v := CheckStorageShapes(mount(declared, false), c); len(v) == 0 {
		t.Errorf("%s is declared read-only and a writable role-less mount of it was accepted; a credential arriving without a storage role is exactly the mount that gets added and never checked", declared)
	}
	if v := CheckStorageShapes(mount(declared, true), c); len(v) != 0 {
		t.Errorf("a correct read-only mount of %s was refused: %s", declared, oneLine(v))
	}

	// And the other half: a role-less path canonical.json says nothing
	// about stays nobody's business here. /workflows was the live example
	// until #868 declared it, so the example is now a path the contract
	// genuinely never names; turning one of those into a finding would be
	// this check answering a question
	// TestEveryPlatformMapsEveryStorageRoleTheSameWay owns.
	for _, readOnly := range []bool{true, false} {
		if v := CheckStorageShapes(mount("/a/path/canonical/json/never/names", readOnly), c); len(v) != 0 {
			t.Errorf("an undeclared role-less mount (readOnly=%v) was refused: %s", readOnly, oneLine(v))
		}
	}
}
