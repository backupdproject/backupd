package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/hostrunner"
)

// TestUsage_NamesEveryWorkflowRunnerVerb closes the level
// TestUsage_EveryRegisteredCommandIsPinned cannot reach, for
// `workflow-runner`.
//
// main.go's dispatch map has one entry for the whole command, so
// everything over there is satisfied the moment one line mentions it and
// stays satisfied forever after. A third verb added tomorrow would be
// dispatchable, absent from usage(), invisible to the black-box verb
// guard and pinned by nothing -- which is the exact shape of failure
// #549 was filed about. The verbs come off the dispatch table rather than
// a list typed here, so adding one is checked without anybody remembering
// this test exists.
func TestUsage_NamesEveryWorkflowRunnerVerb(t *testing.T) {
	verbs := workflowRunnerVerbNames()
	if len(verbs) == 0 {
		t.Fatal("workflowRunnerVerbNames() is empty, so this test would check nothing and pass")
	}

	out := captureStderr(t, usage)
	for _, verb := range verbs {
		if !strings.Contains(out, "workflow-runner "+verb+" ") {
			t.Errorf("usage() does not list \"workflow-runner %s\"; an operator cannot discover it and nothing pins a word of what it prints", verb)
		}
	}
}

// TestWorkflowRunner_RefusesAnIncompleteCommandLineBeforeItTouchesAnything
// keeps the two host directories required rather than defaulted.
//
// A default would be a guess about where this deployment is installed,
// made by a process that has no way to know: the runner's paths are HOST
// paths, and the same config.yaml describes them differently from inside
// the container and from a shell beside it. A guess that was wrong would
// put a hook's working directory, and a socket the engine authenticates
// against, somewhere nobody declared.
func TestWorkflowRunner_RefusesAnIncompleteCommandLineBeforeItTouchesAnything(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no verb at all", nil},
		{"a verb this command does not have", []string{"exec"}},
		{"serve with no directories", []string{"serve"}},
		{"serve with a relative runtime directory", []string{"serve", "--runtime-dir", "run", "--secrets-dir", "/tmp"}},
		{"status with no directories", []string{"status"}},
		{"serve with a surplus argument", []string{"serve", "--runtime-dir", "/tmp", "--secrets-dir", "/tmp", "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			captureStderr(t, func() { code = cmdWorkflowRunner(tc.args) })
			if code != exitUsage {
				t.Errorf("exit %d rather than %d: a command line this wrong must mean nothing ran", code, exitUsage)
			}
		})
	}
}

// TestWorkflowRunner_ServeThenStatusOverARealSocket is the wiring test:
// the command in this binary, a real Unix socket, and the same binary
// asking it what it is.
//
// It is worth having as an end-to-end rather than leaving it to
// internal/hostrunner's own suite because everything it covers is the
// part that lives HERE: the version this build pins, the credential read
// out of the secrets directory, the layout derived from two flags, and
// the fact that `status` and `serve` agree about all three. Every one of
// those is a place where two halves of one command can be wired to
// different values and every package-level test still passes.
func TestWorkflowRunner_ServeThenStatusOverARealSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the runner refuses to serve as root, which this test would otherwise be asserting about the test environment rather than about the runner")
	}

	// A short root, not t.TempDir(): a Unix socket address is a fixed
	// 104-byte field on Darwin and Go names its temporary directories
	// after the test.
	root, err := os.MkdirTemp("/tmp", "bdwr")
	if err != nil {
		t.Fatalf("preparing a short temporary root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	runtimeDir := filepath.Join(root, "run")
	secretsDir := filepath.Join(root, "secrets")
	for _, dir := range []string{runtimeDir, secretsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
	}
	token := strings.Repeat("a", hostrunner.MinTokenLength)
	if err := os.WriteFile(filepath.Join(secretsDir, hostrunner.TokenName), []byte(token), 0o600); err != nil {
		t.Fatalf("writing the credential: %v", err)
	}

	// os.Stdout is swapped for a FILE rather than nested pipe captures.
	// serve prints its banner and then blocks, so a second capture
	// running concurrently would be swapping a global out from under a
	// live goroutine -- a data race, and one whose symptom is the
	// banner turning up inside the output under assertion.
	stdoutFile, err := os.CreateTemp(root, "stdout")
	if err != nil {
		t.Fatalf("preparing a stdout file: %v", err)
	}
	realStdout := os.Stdout
	os.Stdout = stdoutFile

	serving := make(chan int, 1)
	go func() {
		serving <- workflowRunnerServe([]string{
			"--runtime-dir", runtimeDir,
			"--secrets-dir", secretsDir,
			"--config", filepath.Join(root, "no-such-config.yaml"),
		})
	}()
	t.Cleanup(func() {
		// The serve loop ends when the process is signalled, which a
		// test cannot do to itself without taking the suite with it.
		// Closing the socket's directory out from under it is the
		// honest alternative: Accept fails, Serve returns, and the
		// goroutine ends.
		os.RemoveAll(root)
		select {
		case <-serving:
		case <-time.After(5 * time.Second):
		}
		os.Stdout = realStdout
	})

	socket := filepath.Join(runtimeDir, hostrunner.SocketName)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the runner never created %s", socket)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Everything serve printed is behind this offset, so what is read
	// afterwards is exactly what `status` printed and nothing else.
	banner, err := os.ReadFile(stdoutFile.Name())
	if err != nil {
		t.Fatalf("reading what serve printed: %v", err)
	}
	if !strings.Contains(string(banner), "socket "+socket+"\n") {
		t.Errorf("serve does not announce the socket it is listening on, which is the first thing an operator reads out of a supervisor log:\n%s", banner)
	}

	code := workflowRunnerStatus([]string{"--runtime-dir", runtimeDir, "--secrets-dir", secretsDir})
	whole, err := os.ReadFile(stdoutFile.Name())
	if err != nil {
		t.Fatalf("reading what status printed: %v", err)
	}
	out := string(whole[len(banner):])
	if code != exitOK {
		t.Fatalf("status exited %d: %s", code, out)
	}

	// The version is the one this build pins, which is what makes an
	// engine of a different release a refusal rather than a surprise.
	if !strings.Contains(out, "runner "+version+"\n") {
		t.Errorf("status does not report this build's version (%s):\n%s", version, out)
	}
	if !strings.Contains(out, "socket "+socket+"\n") {
		t.Errorf("status reports a socket other than the one serve created (%s):\n%s", socket, out)
	}
	if !strings.Contains(out, "bash /") {
		t.Errorf("status does not name the absolute bash the runner fixed on:\n%s", out)
	}
	if strings.Contains(out, "uid 0)") {
		t.Errorf("the runner reports that it executes hooks as root:\n%s", out)
	}
	if !strings.Contains(out, "running nothing\n") {
		t.Errorf("status does not say what the runner is running:\n%s", out)
	}
}

// TestWorkflowRunner_StatusRefusesWithoutTheInstallationCredential keeps
// the credential load-bearing at THIS level too.
//
// The socket's permissions are the kernel's half of the door. This is the
// other half, and the failure it prevents is the one that is silent: a
// deployment where the secrets directory was never provisioned would
// otherwise get a runner that authenticates nothing.
func TestWorkflowRunner_StatusRefusesWithoutTheInstallationCredential(t *testing.T) {
	root := t.TempDir()

	var code int
	captureStderr(t, func() {
		code = cmdWorkflowRunner([]string{"status", "--runtime-dir", filepath.Join(root, "run"), "--secrets-dir", root})
	})
	if code != exitFailure {
		t.Fatalf("exit %d rather than %d for a deployment with no workflow-runner credential", code, exitFailure)
	}
}
