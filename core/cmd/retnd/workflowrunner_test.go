package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/retnd/retnd/core/internal/hostrunner"
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
// keeps the three host directories required rather than defaulted.
//
// A default would be a guess about where this deployment is installed,
// made by a process that has no way to know: the runner's paths are HOST
// paths, and the same config.yaml describes them differently from inside
// the container and from a shell beside it. A guess that was wrong would
// put a hook's working directory, and a socket the engine authenticates
// against, somewhere nobody declared.
//
// The last case is the one with teeth: --runtime-dir is the directory
// the engine's container mounts read-write, so a workspace nested inside
// it would be a working directory tree the engine can plant symbolic
// links in. That is a refusal at the command line rather than a rule the
// installer is trusted to follow.
func TestWorkflowRunner_RefusesAnIncompleteCommandLineBeforeItTouchesAnything(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no verb at all", nil},
		{"a verb this command does not have", []string{"exec"}},
		{"serve with no directories", []string{"serve"}},
		{"serve with no workspace directory", []string{"serve", "--runtime-dir", "/tmp/run", "--secrets-dir", "/tmp"}},
		{"serve with a relative runtime directory", []string{"serve", "--runtime-dir", "run", "--workspace-dir", "/tmp/ws", "--secrets-dir", "/tmp"}},
		{"status with no directories", []string{"status"}},
		{"serve with a surplus argument", []string{"serve", "--runtime-dir", "/tmp/run", "--workspace-dir", "/tmp/ws", "--secrets-dir", "/tmp", "extra"}},
		{"serve with the workspace inside the engine's mount", []string{"serve", "--runtime-dir", "/tmp/run", "--workspace-dir", "/tmp/run/workspace", "--secrets-dir", "/tmp"}},
		{"serve with the credential inside the engine's mount", []string{"serve", "--runtime-dir", "/tmp/run", "--workspace-dir", "/tmp/ws", "--secrets-dir", "/tmp/run/secrets"}},
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
	workspaceDir := filepath.Join(root, "workspace")
	secretsDir := filepath.Join(root, "secrets")
	for _, dir := range []string{runtimeDir, workspaceDir, secretsDir} {
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
		serving <- workflowRunnerServe(append([]string{
			"--runtime-dir", runtimeDir,
			"--workspace-dir", workspaceDir,
			"--secrets-dir", secretsDir,
			"--config", filepath.Join(root, "no-such-config.yaml"),
		}, standInContainerFlags(t, root)...))
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

	code := workflowRunnerStatus([]string{"--runtime-dir", runtimeDir, "--workspace-dir", workspaceDir, "--secrets-dir", secretsDir})
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
	if !strings.Contains(out, "hook bash /") {
		t.Errorf("status does not name the absolute bash inside the hook image, which is the interpreter a hook actually gets:\n%s", out)
	}
	// The container facts #865 added. An operator whose hook cannot see
	// a directory, or who needs to know which image it ran in, has
	// nowhere else to read them.
	if !strings.Contains(out, "hook image ") || !strings.Contains(out, "docker ") {
		t.Errorf("status does not report the container this deployment runs hooks in:\n%s", out)
	}
	if !strings.Contains(out, "hook network none") {
		t.Errorf("status does not report the hook network, so an operator cannot tell whether their hook has one:\n%s", out)
	}
	if strings.Contains(out, "uid 0)") {
		t.Errorf("the runner reports that it executes hooks as root:\n%s", out)
	}
	if !strings.Contains(out, "running nothing\n") {
		t.Errorf("status does not say what the runner is running:\n%s", out)
	}

	// The working directories are somewhere the engine's container
	// mount does not reach, and `status` is where an operator can see
	// that rather than take it on trust.
	if !strings.Contains(out, "workspace "+workspaceDir+"\n") {
		t.Errorf("status does not report the runner-private workspace (%s):\n%s", workspaceDir, out)
	}
	if strings.HasPrefix(workspaceDir, runtimeDir+"/") {
		t.Errorf("the workspace %s is inside the directory the engine container mounts (%s)", workspaceDir, runtimeDir)
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
		code = cmdWorkflowRunner([]string{"status", "--runtime-dir", filepath.Join(root, "run"), "--workspace-dir", filepath.Join(root, "workspace"), "--secrets-dir", root})
	})
	if code != exitFailure {
		t.Fatalf("exit %d rather than %d for a deployment with no workflow-runner credential", code, exitFailure)
	}
}

// The child process the listener test below inspects: this same binary,
// re-executed as `workflow-runner serve`, for daemon_signal_test.go's
// reasons. What is under assertion is a property of a PROCESS -- the
// sockets it is listening on -- and a process is the only place it can
// be observed.
const (
	runnerChildEnv       = "RETND_TEST_RUNNER_CHILD"
	runnerChildRuntime   = "RETND_TEST_RUNNER_RUNTIME"
	runnerChildWorkspace = "RETND_TEST_RUNNER_WORKSPACE"
	runnerChildSecrets   = "RETND_TEST_RUNNER_SECRETS"
	runnerChildDocker    = "RETND_TEST_RUNNER_DOCKER"
)

// TestWorkflowRunnerChildProcess is not a test. It is the entry point of
// that child, and it skips itself in an ordinary run.
func TestWorkflowRunnerChildProcess(t *testing.T) {
	if os.Getenv(runnerChildEnv) != "1" {
		t.Skip("child-process entry point: only runs when a parent test re-executes this binary")
	}
	os.Exit(run([]string{
		"workflow-runner", "serve",
		"--runtime-dir", os.Getenv(runnerChildRuntime),
		"--workspace-dir", os.Getenv(runnerChildWorkspace),
		"--secrets-dir", os.Getenv(runnerChildSecrets),
		"--config", filepath.Join(os.Getenv(runnerChildRuntime), "no-such-config.yaml"),
		"--docker", os.Getenv(runnerChildDocker),
		"--hook-image", "stand-in/hook:test",
		"--hook-bash", "/bin/bash",
	}))
}

// TestWorkflowRunnerServe_ListensOnAUnixSocketAndNothingElse holds the
// central deployment claim of this command to what the kernel says about
// the running process.
//
// "Unix domain only" is asserted in internal/hostrunner by the shape of
// the code -- there is no address flag to set. That is an argument about
// the source, and the thing an operator is exposed to is a process: one
// accidental net.Listen("tcp", ...) anywhere in the startup path, in this
// package or in something it imports, and a runner whose whole security
// model is "the socket is 0600 in a 0700 directory" is reachable from the
// network. So this starts the real command in a real process and reads
// its listening descriptors back out of the operating system.
func TestWorkflowRunnerServe_ListensOnAUnixSocketAndNothingElse(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the runner refuses to serve as root")
	}

	root, err := os.MkdirTemp("/tmp", "bdwr")
	if err != nil {
		t.Fatalf("preparing a short temporary root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	runtimeDir := filepath.Join(root, "run")
	workspaceDir := filepath.Join(root, "workspace")
	secretsDir := filepath.Join(root, "secrets")
	for _, dir := range []string{runtimeDir, workspaceDir, secretsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
	}
	token := strings.Repeat("b", hostrunner.MinTokenLength)
	if err := os.WriteFile(filepath.Join(secretsDir, hostrunner.TokenName), []byte(token), 0o600); err != nil {
		t.Fatalf("writing the credential: %v", err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestWorkflowRunnerChildProcess$")
	child.Env = append(os.Environ(),
		runnerChildEnv+"=1",
		runnerChildRuntime+"="+runtimeDir,
		runnerChildWorkspace+"="+workspaceDir,
		runnerChildSecrets+"="+secretsDir,
		runnerChildDocker+"="+standInDocker(t, root),
	)
	var output bytes.Buffer
	child.Stdout = &output
	child.Stderr = &output
	if err := child.Start(); err != nil {
		t.Fatalf("starting the runner: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	socket := filepath.Join(runtimeDir, hostrunner.SocketName)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the runner never created %s. It said:\n%s", socket, output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	unix, inet := processSockets(t, child.Process.Pid)
	if len(inet) != 0 {
		t.Errorf("the runner process holds %d internet socket(s): %v. What is on the other end of this runner is arbitrary code execution as the service account, and a port is reachable by anything that can route to this host", len(inet), inet)
	}
	found := false
	for _, path := range unix {
		if path == socket || strings.HasSuffix(path, string(filepath.Separator)+hostrunner.SocketName) {
			found = true
		}
	}
	if !found {
		t.Errorf("the runner process is not holding the Unix socket %s. Its sockets are %v", socket, unix)
	}
}

// processSockets reports the Unix socket paths and the internet sockets
// one process holds, by asking the operating system rather than the
// process.
//
// Two implementations because there is no portable one: Linux answers
// through /proc with no tools installed, and macOS answers through lsof,
// which it ships.
func processSockets(t *testing.T, pid int) (unix, inet []string) {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		return linuxProcessSockets(t, pid)
	case "darwin":
		return darwinProcessSockets(t, pid)
	default:
		t.Skipf("this test reads a process's sockets from the operating system and has no way to do that on %s", runtime.GOOS)
		return nil, nil
	}
}

func linuxProcessSockets(t *testing.T, pid int) (unix, inet []string) {
	t.Helper()
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatalf("reading %s: %v", fdDir, err)
	}
	inodes := map[string]bool{}
	for _, entry := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, entry.Name()))
		if err != nil {
			continue
		}
		if rest, ok := strings.CutPrefix(link, "socket:["); ok {
			inodes[strings.TrimSuffix(rest, "]")] = true
		}
	}
	if len(inodes) == 0 {
		t.Fatalf("the runner process holds no sockets at all, so it is not listening on anything")
	}

	for _, line := range procNetLines(t, "/proc/net/unix") {
		fields := strings.Fields(line)
		if len(fields) >= 7 && inodes[fields[6]] {
			path := ""
			if len(fields) >= 8 {
				path = fields[7]
			}
			unix = append(unix, path)
		}
	}
	for _, name := range []string{"tcp", "tcp6", "udp", "udp6"} {
		for _, line := range procNetLines(t, "/proc/net/"+name) {
			fields := strings.Fields(line)
			if len(fields) >= 10 && inodes[fields[9]] {
				inet = append(inet, name+" "+fields[1])
			}
		}
	}
	return unix, inet
}

// procNetLines reads one /proc/net table without its header row.
func procNetLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		// tcp6 and udp6 are absent on a kernel built without IPv6,
		// which is a table with no sockets in it rather than a failure.
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) <= 1 {
		return nil
	}
	return lines[1:]
}

func darwinProcessSockets(t *testing.T, pid int) (unix, inet []string) {
	t.Helper()
	return lsofNames(t, pid, "-U"), lsofNames(t, pid, "-i")
}

// lsofNames lists the names of one process's sockets of one family.
// lsof exits non-zero when nothing matches, which is an empty answer
// rather than an error.
func lsofNames(t *testing.T, pid int, family string) []string {
	t.Helper()
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-a", family, "-F", "n").Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, ok := strings.CutPrefix(line, "n"); ok {
			names = append(names, name)
		}
	}
	return names
}

// The stand-in docker client these tests hand `serve`.
//
// `workflow-runner serve` proves a container capability before it binds
// its socket (#865), and a unit-tier package may not need a docker daemon
// (core/internal/testtier). What these tests are about is the WIRING --
// the version this build pins, the credential, the layout, the flags
// reaching the preflight -- so the capability is satisfied by a client
// that answers the three questions the preflight asks and runs nothing.
//
// It is deliberately not core/internal/hostrunner's richer stand-in: that
// one exists to run hooks, and no hook is executed here. What a real
// daemon does with these flags is asserted in core/tests/containerhooks.
func standInDocker(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "stand-in-docker")
	body := `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    --config|--host|--context) shift 2 ;;
    *) break ;;
  esac
done
case "${1:-}" in
  version) printf '27.0.0-stand-in\n' ;;
  image) printf 'sha256:feedfacefeedface\n' ;;
  run)
    printf 'backupd-container-probe-ok\n'
    printf 'bash_path=/usr/local/bin/bash\n'
    printf 'bash_version=5.2.37(1)-release\n'
    printf 'uid=1000\n'
    printf 'tty=no\n' ;;
  *) exit 125 ;;
esac
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("writing the stand-in docker client: %v", err)
	}
	return path
}

// standInContainerFlags is standInDocker plus the flags that name it.
func standInContainerFlags(t *testing.T, dir string) []string {
	t.Helper()
	return []string{
		"--docker", standInDocker(t, dir),
		"--hook-image", "stand-in/hook:test",
		"--hook-bash", "/usr/local/bin/bash",
	}
}
