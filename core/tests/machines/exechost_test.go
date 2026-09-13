package machines

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The exec host has its own container, its own teardown and its own
// watchdog (exechost.go), reached by a path no test of Source ever runs:
// ExecHost registers finish as a t.Cleanup and removes the container from
// there. #161 is the record of what a fixture that fails to do that costs
// -- orphaned containers found running for hours, each one competing with
// the next run for a Docker VM with roughly 4 GB to give -- and a second
// fixture with a second teardown is a second place to lose one.
//
// So this is the same evidence source_test.go carries for Source, against
// this machine: a test that dies the hardest way a Go test can must leave
// no container behind. Everything it uses (the helper-process shape, the
// container assertions, the live control) is that file's, shared rather
// than restated.

// TestExecHostRemovesItsContainerWhenTheTestPanics drives a child test
// process that stands the exec host up and then panics, and asserts from
// out here -- after the child is genuinely gone -- that its container is
// gone too.
//
// It has to be a child process for the reason the Source tests state: a
// test cannot assert its own hard failure from the inside, and the leak
// assertion is only meaningful once the fixture's owner has exited.
func TestExecHostRemovesItsContainerWhenTheTestPanics(t *testing.T) {
	requireDocker(t)

	// Building the exec host image on a cold daemon is the slow part, and
	// it happens inside the child.
	const window = 5 * time.Minute
	out, exited, code := runSourceHelper(t, "TestHelperExecHostPanicsMidTest", window)
	if !exited {
		t.Fatalf("a panicking test never finished within %s.\nhelper output:\n%s", window, out)
	}
	if code == 0 {
		t.Fatalf("a panicking test reported success.\nhelper output:\n%s", out)
	}
	if !strings.Contains(out, "deliberate panic") {
		t.Fatalf("the helper did not reach its panic, so it never got as far as owning a container.\nhelper output:\n%s", out)
	}

	id := containerIDFrom(t, out)
	removeAfterwards(t, id)
	control := liveControlContainer(t)
	if !containerExists(t, control) {
		t.Fatal("containerExists says no to a container that was just created, so it cannot answer yes to anything; the leak assertion below would pass whatever happened")
	}
	if containerExists(t, id) {
		t.Fatalf("the exec host container %s outlived a panicking test", id)
	}
}

func TestHelperExecHostPanicsMidTest(t *testing.T) {
	skipUnlessSourceHelper(t)
	h := Start(t).ExecHost(t)
	fmt.Println(containerMarker + h.ContainerID())
	panic("deliberate panic, standing in for any hard failure mid-test")
}
