package machines

import (
	"strings"
	"testing"
	"time"
)

// The tier's docker access for a test whose subject LAUNCHES containers
// itself (#865), rather than for a fixture machine this package stands up.
//
// core/internal/hostrunner runs every local workflow hook in an ephemeral
// container. Proving that against a real daemon needs three things no
// fixture machine provides: the hook image present, a way to ask whether
// a container this package did not create still exists, and a way to
// clear away what a killed run left behind.
//
// They live here rather than in the suite that uses them for the reason
// ExecHost.Inside does: core/internal/testtier's bypasses-harness rule
// says nothing under core/tests may exec docker itself, because
// everything this package learned the hard way -- every call bounded,
// a timeout told apart from a non-zero exit, image presence checked
// before a pull -- protects only the callers that come through it.
//
// Nothing here asserts. A test decides what an answer means.

// EnsureHookImage makes sure one image is on this daemon FOR THIS
// DAEMON'S PLATFORM, pulling it if it is absent or built for another
// architecture, and FAILS rather than skipping when it cannot be
// obtained.
//
// The presence half is ensureImageStaged's policy, shared rather than
// restated: a second copy would drift into being more obliging, and an
// obliging version converts a loud environmental failure into silently
// missing coverage with the gate still printing ok (#160).
//
// The PLATFORM half is here because the code under test refuses a
// mismatch, by design (core/internal/hostrunner's
// refusePlatformMismatch): an amd64 hook image on an arm64 host runs
// under emulation and makes the docker client print a warning into every
// hook's stderr. A developer machine routinely has the wrong-arch copy of
// a multi-arch tag lying around from something else, and a suite that
// simply failed on it would be reporting on the machine rather than on
// the product.
func EnsureHookImage(t *testing.T, ref string) string {
	t.Helper()

	platform := daemonPlatform(t)
	if imagePlatform(t, ref) == platform {
		return ref
	}
	// --platform, so a multi-arch tag resolves to this daemon's own
	// architecture rather than to whatever the local copy happened to
	// be.
	if _, stderr, err := dockerRun(dockerPullTimeout, "pull", "--platform", platform, ref); err != nil {
		t.Fatalf("machines: %s is not on this daemon for %s and it could not be pulled, so this suite cannot run here.\n"+
			"That is a FAILURE and deliberately not a skip: skipping would take #865's container evidence out of the gate while the gate went on reporting ok (#160).\nlast error: %v\n%s", ref, platform, err, stderr)
	}
	if got := imagePlatform(t, ref); got != platform {
		t.Fatalf("machines: %s is on this daemon as %s and this daemon runs %s. The code under test refuses that mismatch by design, so there is nothing to measure here", ref, got, platform)
	}
	return ref
}

// daemonPlatform is the os/arch this daemon runs containers for.
func daemonPlatform(t *testing.T) string {
	t.Helper()
	stdout, stderr, err := dockerRun(30*time.Second, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		t.Fatalf("machines: this daemon would not say what platform it runs: %v\n%s", err, stderr)
	}
	return strings.TrimSpace(stdout)
}

// imagePlatform is one image's os/arch, or "" when the image is absent.
func imagePlatform(t *testing.T, ref string) string {
	t.Helper()
	stdout, _, err := dockerRun(imageInspectTimeout, "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(stdout)
}

// ContainerExists reports whether the daemon still knows about a
// container, by name.
//
// `ps --all --quiet --filter name=` rather than `inspect`, for the reason
// the code under test uses the same call: inspect exits non-zero both for
// "no such container" and for "no daemon", so it cannot tell a removed
// container from an unanswerable question. A daemon that cannot be
// reached is a FAILURE here, because a test that read it as "gone" would
// pass while proving nothing.
func ContainerExists(t *testing.T, name string) bool {
	t.Helper()
	stdout, stderr, err := dockerRun(30*time.Second, "ps", "--all", "--quiet", "--filter", "name=^"+name+"$")
	if err != nil {
		t.Fatalf("machines: asking this daemon whether the container %s exists failed: %v\n%s", name, err, stderr)
	}
	return strings.TrimSpace(stdout) != ""
}

// ContainersWithLabel lists the ids of every container carrying a label,
// running or not.
//
// It is how a test observes a container it did not name: the host runner
// mints its own container names, and labels them with the run and step
// they belong to, so "is this step's container there" and "did this step
// leave anything behind" are both label questions.
func ContainersWithLabel(t *testing.T, label string) []string {
	t.Helper()
	stdout, stderr, err := dockerRun(30*time.Second, "ps", "--all", "--quiet", "--filter", "label="+label)
	if err != nil {
		t.Fatalf("machines: listing containers labelled %s failed: %v\n%s", label, err, stderr)
	}
	return strings.Fields(stdout)
}

// RemoveContainersWithLabel force-removes every container carrying a
// label, and reports how many it removed.
//
// This is the hermetic half of a suite that drives a container-launching
// product: on the way IN it clears what a killed run left behind, and on
// the way out it clears what a failed assertion left running. Both
// matter, and the way-in call is the one that cannot be replaced by a
// t.Cleanup -- a SIGKILLed test binary takes its cleanups with it, which
// is the whole argument in core/tests/dockerlease.
//
// The label is the safety boundary: an unlabelled container is never
// touched.
func RemoveContainersWithLabel(t *testing.T, label string) int {
	t.Helper()
	ids := ContainersWithLabel(t, label)
	if len(ids) == 0 {
		return 0
	}
	// Best effort, and deliberately not fatal: several worktrees share
	// one daemon on a developer's machine, so a container listed a
	// moment ago is routinely removed by somebody else's cleanup before
	// this call gets to it.
	_, _, _ = dockerRun(60*time.Second, append([]string{"rm", "--force", "--volumes"}, ids...)...)
	return len(ids)
}
