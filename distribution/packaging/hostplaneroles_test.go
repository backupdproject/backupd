package packaging

import (
	"slices"
	"strings"
	"testing"
)

// The workflow runner's two paths are KNOWN to the canonical contract and
// NOT REQUIRED of every platform, and this file is what keeps those two
// halves from collapsing into each other. Both directions have already
// shipped as bugs.
//
// Leave the paths out of the contract entirely, which is how they arrived,
// and roleForContainerPath cannot name them: CheckRequiredMounts reads each
// one as "a mount the binaries never read", and Dockge and Generic Docker,
// the two profiles that deploy the canonical stack unmodified, go NOT_READY
// for mounting exactly what that stack mounts.
//
// Put them in Roles instead and the opposite happens, silently and to
// everybody: every NAS store profile is told it is missing storage for a
// workflow runner it never deploys.
//
// So each test below is paired with the mutation it is meant to survive.

// A host-plane path has to resolve to a role. This is the assertion that
// was false before the contract carried them.
func TestHostPlanePathsResolveToARole(t *testing.T) {
	c := MustLoad()

	for _, role := range HostPlaneRoles {
		p, ok := c.ContainerPaths.ByRole(role)
		if !ok || p == "" {
			t.Fatalf("canonical.json declares no container path for the %q role, so nothing in the product can name it", role)
		}
		if got := roleForContainerPath(c, p); got != role {
			t.Errorf("roleForContainerPath(%s) = %q, want %q. An unnamed path is reported as one the binaries never read, which is how the canonical stack's own mounts became a drift violation", p, got, role)
		}
	}
}

// The half that must NOT hold: a host-plane role is not a required one.
// This is the guard against somebody folding HostPlaneRoles into Roles to
// simplify the two lists into one.
func TestHostPlaneRolesAreNotRequiredOfEveryPlatform(t *testing.T) {
	for _, role := range HostPlaneRoles {
		if slices.Contains(Roles, role) {
			t.Fatalf("%q is in both Roles and HostPlaneRoles. Roles is what CheckRequiredMounts demands of every profile, so this makes a workflow-runner directory mandatory for every NAS store package that will never run one", role)
		}
	}

	// Driven, not just asserted: a profile that mounts the five required
	// roles and nothing else is not asked about the host-plane ones.
	c := MustLoad()
	var mounts []Mount
	for _, role := range Roles {
		p, _ := c.ContainerPaths.ByRole(role)
		mounts = append(mounts, Mount{
			Role:          role,
			HostPath:      "/mnt/tank/backupd/" + role,
			ContainerPath: p,
			ReadOnly:      c.WriteModeFor(p) == WriteModeReadOnly,
		})
	}
	svc := Service{Name: "backupd", Source: "a store profile that deploys no runner", Mounts: mounts}

	if v := CheckRequiredMounts(svc, c); len(v) != 0 {
		t.Errorf("a profile mounting exactly the required roles was refused: %s.\nThat profile is every NAS store package this product ships", format(v))
	}
}

// Optional to mount, and not optional to mount correctly. The workflows
// directory is the one that matters: the engine executes what it reads out
// of it.
func TestAMountedHostPlanePathIsHeldToItsWriteMode(t *testing.T) {
	c := MustLoad()

	workflows, ok := c.ContainerPaths.ByRole("workflows")
	if !ok {
		t.Fatal("no container path for the workflows role")
	}
	if c.WriteModeFor(workflows) != WriteModeReadOnly {
		t.Fatalf("canonical.json declares %s %v, and this test is about it being read-only; a writable scripts directory is a process one compromise away from running anything on the host", workflows, c.WriteModeFor(workflows))
	}

	svc := Service{
		Name:   "backupd",
		Source: "positive control: the scripts directory mounted writable",
		Mounts: []Mount{{
			Role:          "workflows",
			HostPath:      "/opt/backupd/workflows",
			ContainerPath: workflows,
			ReadOnly:      false,
		}},
	}

	v := CheckRequiredMounts(svc, c)
	if len(v) == 0 {
		t.Fatal("mounting the workflows directory writable produced no violation, so nothing stops a profile shipping it that way")
	}
	if !hasRule(v, RuleContractDrift) {
		t.Errorf("the writable mount was refused, but not as %q: %s", RuleContractDrift, format(v))
	}
	if joined := format(v); !strings.Contains(joined, workflows) {
		t.Errorf("the refusal never names %s, so it does not say which mount to fix:\n%s", workflows, joined)
	}
}

// Every known role answers the write-mode question. Without this a path
// could be added to the contract with no answer to "may the engine write
// this", which CheckCanonicalWriteModes only asks about roles it iterates.
func TestEveryKnownRoleDeclaresAWriteMode(t *testing.T) {
	c := MustLoad()
	for _, role := range KnownRoles() {
		p, ok := c.ContainerPaths.ByRole(role)
		if !ok {
			t.Errorf("KnownRoles names %q and ContainerPaths.ByRole does not answer for it", role)
			continue
		}
		if c.WriteModeFor(p) == WriteModeUndeclared {
			t.Errorf("the %q role's container path %s is in neither readOnlyContainerPaths nor writableContainerPaths", role, p)
		}
	}
}
