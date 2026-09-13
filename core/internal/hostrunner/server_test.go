package hostrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestListen_PutsThePrivateSocketInAPrivateDirectory is the first of the
// two independent locks on this door.
//
// The kernel is the only enforcement that cannot be bypassed by a bug in
// this package, and what is on the other end of this socket is arbitrary
// code execution as the service account. The mode is set explicitly after
// the listen because the umask this process was started with decides what
// the kernel creates, and a service manager's umask is not this package's
// to assume.
func TestListen_PutsThePrivateSocketInAPrivateDirectory(t *testing.T) {
	client, layout, _ := serveTestRunner(t, nil)

	info, err := os.Lstat(client.SocketPath)
	if err != nil {
		t.Fatalf("the socket is not where the layout says it is: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket", client.SocketPath)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the socket is mode %#o rather than 0600, so another account on this host can connect and execute scripts as this one", perm)
	}

	dir, err := os.Stat(layout.RuntimeDir)
	if err != nil {
		t.Fatalf("stat of the runtime directory: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != RuntimeDirMode {
		t.Errorf("the runtime directory is mode %#o rather than %#o", perm, RuntimeDirMode)
	}

	// The socket path is the ONLY address this runner has. There is no
	// host, port or address field to set, which is what makes "Unix
	// domain only" a property of the type rather than of the default.
	if filepath.Base(client.SocketPath) != SocketName {
		t.Errorf("the runner is listening on %q, which is not the socket the installer and the compose mount name", client.SocketPath)
	}
}

// TestListen_RefusesASocketPathTheKernelCannotHold turns an EINVAL into
// a sentence.
//
// A Unix socket address is a fixed-size field, not a string. Bind a path
// past it and the kernel answers "invalid argument", which names neither
// the path nor the length and sends whoever is reading it looking at
// permissions, at SELinux and at the filesystem before they think of
// counting characters.
func TestListen_RefusesASocketPathTheKernelCannotHold(t *testing.T) {
	deep := filepath.Join("/tmp", strings.Repeat("d", 40), strings.Repeat("e", 40), strings.Repeat("f", 40))

	server, err := NewServer(Config{
		Layout:    Layout{RuntimeDir: deep, WorkspaceDir: "/tmp/ws", SecretsDir: "/tmp"},
		Version:   "test-1.2.3",
		Token:     []byte(testToken),
		Container: stubContainer(),
		EUID:      os.Geteuid() | 1,
		Username:  "test",
	})
	if err != nil {
		t.Fatalf("preparing the runner: %v", err)
	}
	err = server.Listen()
	if err == nil {
		t.Fatal("a socket path past the kernel's address field was bound")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(len(deep)+1+len(SocketName))) {
		t.Errorf("the refusal does not say how long the path is, which is the only fact that makes it actionable: %v", err)
	}
	if _, statErr := os.Stat(deep); statErr == nil {
		os.RemoveAll(filepath.Join("/tmp", strings.Repeat("d", 40)))
		t.Error("the runner created the runtime directory before finding out it could not listen in it")
	}
}

// TestServer_RefusesAClientWithoutTheInstallationCredential is the second
// lock.
//
// It matters on exactly the deployments this product ships to: a NAS
// package manager that runs everything as one account, an operator who
// chmod'ed a parent directory to debug something else, a restore of /etc
// with the wrong ownership. Any of those opens the first lock silently.
func TestServer_RefusesAClientWithoutTheInstallationCredential(t *testing.T) {
	client, _, _ := serveTestRunner(t, nil)

	for _, token := range []string{"", "wrong", strings.Repeat("f", len(testToken))} {
		conn := rawConn(t, client.SocketPath)
		msg := hello(t, conn, Hello{Protocol: Protocol, Version: client.Version, Token: token})

		if msg.Kind != KindFailure || msg.Failure == nil {
			t.Fatalf("a client presenting the token %q was welcomed", token)
		}
		if msg.Failure.Code != CodeUnauthorized {
			t.Errorf("a client with the wrong token was refused as %q rather than unauthorized", msg.Failure.Code)
		}
	}
}

// TestServer_RefusesAnEngineFromADifferentRelease is the version
// pairing.
//
// The engine and the runner are one program cut in half by a socket. An
// unpaired upgrade has to be a loud, immediate refusal naming both
// versions -- which is a five-minute fix -- rather than a hook running
// with an envelope the other half did not mean, which is not.
func TestServer_RefusesAnEngineFromADifferentRelease(t *testing.T) {
	client, _, _ := serveTestRunner(t, nil)

	conn := rawConn(t, client.SocketPath)
	msg := hello(t, conn, Hello{Protocol: Protocol, Version: "test-9.9.9", Token: testToken})

	if msg.Kind != KindFailure || msg.Failure == nil {
		t.Fatal("an engine from a different release was welcomed")
	}
	if msg.Failure.Code != CodeVersionMismatch {
		t.Fatalf("the refusal is %q rather than version_mismatch", msg.Failure.Code)
	}
	for _, want := range []string{"test-9.9.9", client.Version} {
		if !strings.Contains(msg.Failure.Message, want) {
			t.Errorf("the refusal does not name %q, so an operator cannot tell which half to upgrade: %q", want, msg.Failure.Message)
		}
	}
}

// TestServer_AuthenticatesBeforeItComparesVersions pins the ORDER of the
// two refusals.
//
// An unauthenticated caller learns only that it is unauthenticated. A
// legitimate engine of the wrong release still gets the specific sentence
// it needs, because it has the credential.
func TestServer_AuthenticatesBeforeItComparesVersions(t *testing.T) {
	client, _, _ := serveTestRunner(t, nil)

	conn := rawConn(t, client.SocketPath)
	msg := hello(t, conn, Hello{Protocol: Protocol, Version: "test-9.9.9", Token: "wrong"})

	if msg.Failure == nil || msg.Failure.Code != CodeUnauthorized {
		t.Fatalf("a caller that is both unauthorized and mismatched was told about the version: %+v", msg.Failure)
	}
}

// TestRefuseRoot_IsNotConfigurable is #809's unprivileged-by-default
// requirement.
//
// An administrator installed this product. That says nothing whatever
// about whether they meant every script in a hook directory to run as
// root -- including one that arrived by rsync from somewhere else. There
// is no override flag on purpose: a single switch that re-privileges
// every hook at once stays flipped forever on every deployment that ever
// flips it.
func TestRefuseRoot_IsNotConfigurable(t *testing.T) {
	if err := RefuseRoot(0); err == nil {
		t.Fatal("the runner would serve as uid 0")
	} else if !errors.Is(err, ErrRunningAsRoot) {
		t.Fatalf("the refusal is not an ErrRunningAsRoot: %v", err)
	}
	if err := RefuseRoot(1000); err != nil {
		t.Fatalf("an ordinary account was refused: %v", err)
	}

	_, err := NewServer(Config{
		Layout:    Layout{RuntimeDir: "/tmp/x", WorkspaceDir: "/tmp/w", SecretsDir: "/tmp/y"},
		Version:   "test-1.2.3",
		Token:     []byte(testToken),
		Container: stubContainer(),
		EUID:      0,
	})
	if !errors.Is(err, ErrRunningAsRoot) {
		t.Fatalf("a server was built for uid 0: %v", err)
	}
}

// TestLoadToken_RefusesACredentialOtherAccountsCanRead keeps the second
// lock meaningful.
//
// A credential every account on the host can read is not a credential,
// and the failure is silent: everything keeps working, and the door has
// been open since whoever ran the chmod.
func TestLoadToken_RefusesACredentialOtherAccountsCanRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TokenName)
	if err := os.WriteFile(path, []byte(testToken), 0o644); err != nil {
		t.Fatalf("writing the credential: %v", err)
	}

	if _, err := LoadToken(path); err == nil {
		t.Fatal("a world-readable credential was accepted")
	} else if !errors.Is(err, ErrServer) {
		t.Fatalf("the refusal is not an ErrServer: %v", err)
	}

	if err := os.Chmod(path, TokenFileMode); err != nil {
		t.Fatalf("tightening the credential: %v", err)
	}
	token, err := LoadToken(path)
	if err != nil {
		t.Fatalf("a 0600 credential was refused: %v", err)
	}
	if string(token) != testToken {
		t.Errorf("the credential read back as %q", token)
	}

	// A truncated or empty file must not authenticate anything either.
	if err := os.WriteFile(path, []byte("short"), TokenFileMode); err != nil {
		t.Fatalf("writing a truncated credential: %v", err)
	}
	if _, err := LoadToken(path); err == nil {
		t.Fatal("a five-byte credential was accepted, so a truncated file authenticates every client")
	}
}

// TestServer_RefusesARequestThatNamesAPath is the end-to-end form of the
// protocol's central claim: not that the decoder is strict, but that a
// client cannot reach execution with a path.
func TestServer_RefusesARequestThatNamesAPath(t *testing.T) {
	client, _, _ := serveTestRunner(t, nil)

	conn := rawConn(t, client.SocketPath)
	if msg := hello(t, conn, Hello{Protocol: Protocol, Version: client.Version, Token: testToken}); msg.Kind != KindWelcome {
		t.Fatalf("the handshake failed: %+v", msg.Failure)
	}

	// Hand-built, because the Request type has no field that could
	// carry this.
	frame := frameOf(t, `{"kind":"request","request":{"op":"execute","run_id":"run-1","step_id":"step-1","script_path":"/etc/cron.d/x"}}`)
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("writing the request: %v", err)
	}

	msg, err := ReadMessage(conn)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	if msg.Kind != KindFailure || msg.Failure == nil || msg.Failure.Code != CodeMalformed {
		t.Fatalf("a request naming a host path was not refused as malformed: %+v", msg)
	}
}

// TestServer_LeaseExpiryKillsARunawayHook is the failure this whole
// design exists to bound, and it is tested with a fake engine that
// simply stops existing.
//
// The scenario is real and is not rare: the engine container is recreated
// by a `docker compose up` during a `before` hook that has quiesced a
// database. Nothing upstream is left to time the hook out, nothing is
// left to run the matching `after` hook, and without a lease the
// quiesce lasts until somebody notices.
//
// The assertion is about the CHILD, not the hook: a lease that killed
// only the process it started would leave the pipeline holding whatever
// it held.
func TestServer_LeaseExpiryKillsARunawayHook(t *testing.T) {
	client, layout, _ := serveTestRunner(t, nil)
	evidence := t.TempDir()

	conn := rawConn(t, client.SocketPath)
	if msg := hello(t, conn, Hello{Protocol: Protocol, Version: client.Version, Token: testToken}); msg.Kind != KindWelcome {
		t.Fatalf("the handshake failed: %+v", msg.Failure)
	}

	body := fmt.Sprintf(`( sleep 4; touch %s/child-survived ) >/dev/null 2>&1 &
touch %s/started
sleep 60
`, evidence, evidence)
	req := scriptRequest("run-lease", "step-lease", body)
	req.TimeoutMS = 60_000
	if err := WriteMessage(conn, Message{Kind: KindRequest, Request: &req}); err != nil {
		t.Fatalf("sending the execute: %v", err)
	}

	waitForFile(t, filepath.Join(evidence, "started"), 10*time.Second)

	// The engine dies. No cancel, no goodbye: the socket simply closes,
	// which is what a killed process leaves behind.
	conn.Close()

	// The hook's own sleep is 60s and its timeout is 60s, so anything
	// that happens inside the next few seconds happened because the
	// lease expired.
	workDir, err := layout.StepWorkDir("run-lease", "step-lease")
	if err != nil {
		t.Fatalf("deriving the working directory: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(workDir); os.IsNotExist(err) {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("ten seconds after the engine vanished, %s is still there: the runner is holding a working directory for a hook nobody owns", workDir)
		}
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(5 * time.Second)
	if _, err := os.Stat(filepath.Join(evidence, "child-survived")); err == nil {
		t.Error("the process the hook started survived the engine's disappearance. A dead engine left a runaway script running on the host, which is precisely what the lease exists to make impossible.")
	}
}

// TestServer_CancelReachesAStepFromASecondConnection is why `cancel` is
// an operation rather than a flag on the execute connection.
//
// The connection running a step is busy being the lease, and an engine
// that needs to stop a step whose own connection has wedged needs a door
// that is not that connection.
func TestServer_CancelReachesAStepFromASecondConnection(t *testing.T) {
	client, _, _ := serveTestRunner(t, nil)
	evidence := t.TempDir()

	body := fmt.Sprintf("touch %s/started\nsleep 60\n", evidence)
	done := make(chan Result, 1)
	fail := make(chan error, 1)
	go func() {
		result, err := client.Execute(context.Background(), ExecuteRequest{
			RunID:   "run-cancel",
			StepID:  "step-cancel",
			Script:  []byte(body),
			Timeout: 60 * time.Second,
		}, &collector{})
		if err != nil {
			fail <- err
			return
		}
		done <- result
	}()

	waitForFile(t, filepath.Join(evidence, "started"), 10*time.Second)

	if err := client.Cancel(context.Background(), "run-cancel", "step-cancel"); err != nil {
		t.Fatalf("cancelling: %v", err)
	}

	select {
	case result := <-done:
		if result.State != StateCanceled {
			t.Errorf("the cancelled step reported %q", result.State)
		}
		if result.TerminationCertainty != CertaintyConfirmed {
			t.Errorf("the cancel could not prove the process group was gone: %q", result.TerminationCertainty)
		}
	case err := <-fail:
		t.Fatalf("the cancelled execution failed instead of reporting a cancellation: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the step was still running twenty seconds after it was cancelled")
	}

	if err := client.Cancel(context.Background(), "run-cancel", "step-cancel"); !IsCode(err, CodeNotFound) {
		t.Errorf("cancelling a step that is not running answered %v rather than not_found", err)
	}
}

// TestClient_StatusIsThePreflightAnswer covers the health surface #809
// requires `.local.sh` validation to consult BEFORE a backup starts.
func TestClient_StatusIsThePreflightAnswer(t *testing.T) {
	client, layout, _ := serveTestRunner(t, nil)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("asking the runner what it is: %v", err)
	}
	if status.Version != client.Version {
		t.Errorf("the runner reports version %q", status.Version)
	}
	if !filepath.IsAbs(status.BashPath) || status.BashVersion == "" {
		t.Errorf("the runner cannot say which bash it fixed on (%q / %q), so 'which shell ran my hook' is unanswerable", status.BashPath, status.BashVersion)
	}
	if status.UID == 0 {
		t.Error("the runner reports that it executes hooks as uid 0")
	}
	if status.SocketPath != layout.SocketPath() || status.RuntimeDir != layout.RuntimeDir {
		t.Errorf("the runner reports a socket or runtime directory that is not the one it is using: %+v", status)
	}
}

// TestClient_SyntaxCheckRunsNothing is the other half of preflight:
// validating a hook must not execute it, or "validate before the backup
// starts" would mean "run the hook before the backup starts".
func TestClient_SyntaxCheckRunsNothing(t *testing.T) {
	client, _, _ := serveTestRunner(t, nil)
	marker := filepath.Join(t.TempDir(), "ran")

	if err := client.SyntaxCheck(context.Background(), "run-1", "step-1", []byte("touch "+marker+"\n")); err != nil {
		t.Fatalf("checking a valid script: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the syntax check executed the script")
	}

	err := client.SyntaxCheck(context.Background(), "run-1", "step-1", []byte("if true\n"))
	if !IsCode(err, CodeSyntax) {
		t.Fatalf("a script that does not parse was not reported as a syntax error: %v", err)
	}
}

// TestClient_ExecuteStreamsOutputAndReportsTheHooksOwnExitStatus is the
// ordinary path, end to end over the socket.
//
// The non-zero exit is the part worth asserting: a hook that fails is a
// Result, not an error. Conflating "the hook failed" with "we never ran
// the hook" would take away the one distinction a workflow engine cannot
// do without.
func TestClient_ExecuteStreamsOutputAndReportsTheHooksOwnExitStatus(t *testing.T) {
	client, _, _ := serveTestRunner(t, nil)
	out := &collector{}

	result, err := client.Execute(context.Background(), ExecuteRequest{
		RunID:   "run-1",
		StepID:  "step-1",
		Script:  []byte("echo out\necho err >&2\nexit 7\n"),
		Env:     EnvSet{Vars: []EnvVar{{Name: "PGHOST", Value: "db.example"}}},
		Timeout: 30 * time.Second,
	}, out)
	if err != nil {
		t.Fatalf("executing: %v", err)
	}
	if result.State != StateExited {
		t.Fatalf("the step reported %q", result.State)
	}
	if result.ExitCode == nil || *result.ExitCode != 7 {
		t.Errorf("the hook's own exit status was not reported: %+v", result)
	}
	if got := strings.TrimSpace(out.text(StreamStdout)); got != "out" {
		t.Errorf("stdout arrived as %q", got)
	}
	if got := strings.TrimSpace(out.text(StreamStderr)); got != "err" {
		t.Errorf("stderr arrived as %q", got)
	}
	if !result.WorkDirRemoved {
		t.Error("the per-step working directory was not removed after a step that ended normally")
	}
}

// TestServer_ConcurrentExecutesOfOneStepLeaveTheWinnerAlone is the race
// the registry exists to lose safely.
//
// Two executes for the same step arriving at once is not exotic: an
// engine that retried after deciding a timeout locally produces it, and
// so does a deployment that has pointed two engines at one runner. The
// question is not whether the second one is refused -- it is what the
// refusal COSTS the first. A check and an insert in two separate lock
// sections let both callers past the check; the one that then fails on
// the script file's O_EXCL runs the same cleanup every failed step runs,
// which removes the working directory and the script of the hook that is
// still executing, and its deferred release deletes the registry entry
// holding the winner's cancel function -- leaving a live process group
// that `cancel` can no longer reach.
//
// Sixteen racers rather than two, because the window is small and a test
// that only sometimes enters it is a test that only sometimes means
// anything.
func TestServer_ConcurrentExecutesOfOneStepLeaveTheWinnerAlone(t *testing.T) {
	client, layout, _ := serveTestRunner(t, nil)
	evidence := t.TempDir()

	// The hook proves its own working directory outlived it: a cleanup
	// run by somebody else's failed execute takes this file with it.
	body := fmt.Sprintf(`echo mine > "$BACKUPD_WORK_DIR/mine"
touch %s/started
sleep 2
test -f "$BACKUPD_WORK_DIR/mine" || exit 9
echo survived
`, evidence)

	const racers = 16
	type outcome struct {
		result Result
		err    error
	}
	outcomes := make(chan outcome, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			result, err := client.Execute(context.Background(), ExecuteRequest{
				RunID:   "run-race",
				StepID:  "step-race",
				Script:  []byte(body),
				Timeout: 30 * time.Second,
			}, &collector{})
			outcomes <- outcome{result, err}
		}()
	}
	close(start)

	// While the winner is still running, the registry must hold exactly
	// one entry for it. A loser that registered and then released on its
	// way out would have deleted the winner's cancel function, and this
	// is where that is visible from outside.
	waitForFile(t, filepath.Join(evidence, "started"), 15*time.Second)
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("asking the runner what it is running: %v", err)
	}
	running := 0
	for _, key := range status.Active {
		if key == "run-race/step-race" {
			running++
		}
	}
	if running != 1 {
		t.Errorf("the runner reports %v as running. A step whose cancel function has been dropped from the registry is a hook nothing can stop", status.Active)
	}

	var winners, refusals int
	for range racers {
		got := <-outcomes
		switch {
		case got.err == nil && got.result.ExitCode != nil && *got.result.ExitCode == 0:
			winners++
		case got.err == nil:
			t.Errorf("an execute finished with exit code %v: the winner's own working directory was removed out from under it by somebody else's cleanup", got.result.ExitCode)
		case IsCode(got.err, CodeBusy):
			refusals++
		default:
			t.Errorf("a second execute of a running step failed with something other than a legible refusal: %v", got.err)
		}
	}
	if winners != 1 {
		t.Errorf("%d of %d concurrent executes of one step ran it, rather than exactly one", winners, racers)
	}
	if refusals != racers-1 {
		t.Errorf("%d of the %d losers were refused as busy", refusals, racers-1)
	}
	assertNothingLeftBehind(t, layout, "run-race", "step-race")
}

// TestServe_ACancelledContextStopsTheRunnerAndTheHooksItOwns is what
// makes SIGTERM a shutdown rather than a hang.
//
// Two connections are open when the context is cancelled, and the first
// is the one that used to wedge the whole process: a client that
// connected and said nothing leaves handleConn blocked in a read with no
// deadline, and a Serve that only stopped ACCEPTING would sit in its
// final Wait until systemd's stop timeout turned into SIGKILL. SIGKILL
// on this process is the worst possible ending, because every hook it
// was supervising is in a session of its own and simply carries on,
// orphaned, with nothing left on the host that knows the process group
// ids.
func TestServe_ACancelledContextStopsTheRunnerAndTheHooksItOwns(t *testing.T) {
	layout := testLayout(t)
	if err := os.WriteFile(layout.TokenPath(), []byte(testToken), TokenFileMode); err != nil {
		t.Fatalf("writing the credential: %v", err)
	}
	container, state := fakeCapability(t, fakeDocker{})

	server, err := NewServer(Config{
		Layout:    layout,
		Version:   "test-1.2.3",
		Token:     []byte(testToken),
		Container: container,
		Grace:     200 * time.Millisecond,
		EUID:      os.Geteuid(),
		Username:  CurrentUsername(os.Geteuid()),
	})
	if err != nil {
		t.Fatalf("preparing the runner: %v", err)
	}
	if err := server.Listen(); err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()

	client := Client{SocketPath: layout.SocketPath(), Version: "test-1.2.3", Token: testToken}
	evidence := t.TempDir()

	// A hook that would outlive any stop timeout.
	body := fmt.Sprintf("touch %s/started\nsleep 300\n", evidence)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = client.Execute(context.Background(), ExecuteRequest{
			RunID:   "run-stop",
			StepID:  "step-stop",
			Script:  []byte(body),
			Timeout: 300 * time.Second,
		}, &collector{})
	}()
	waitForFile(t, filepath.Join(evidence, "started"), 15*time.Second)
	if records := containerRecords(t, state); len(records) != 1 {
		t.Fatalf("the hook is not running in a container this test can watch: %v", records)
	}

	// And a connection that says nothing at all, which is where the
	// handshake read blocks forever.
	silent := rawConn(t, client.SocketPath)
	defer silent.Close()

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("a cancelled Serve returned an error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return twenty seconds after its context was cancelled. Under a service manager this is the window that ends in SIGKILL, and a SIGKILLed runner leaves every hook container it was supervising running with nothing left on the host that knows their names")
	}

	// And the container it owned went with it, rather than being left
	// behind. This is the lease guarantee at the process level: a runner
	// that stopped is a runner whose hooks stopped.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if len(containerRecords(t, state)) == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("a hook container is still there after the runner stopped: %v. It is now an orphan nothing on this host can account for", containerRecords(t, state))
		}
		time.Sleep(50 * time.Millisecond)
	}
	<-done
}

// stubContainer is a capability with nothing behind it, for the tests
// that are about the SERVER's own refusals -- a socket path the kernel
// cannot hold, a uid of 0 -- and must get past NewServer's requirement
// for one without starting anything.
func stubContainer() Container {
	return Container{
		Docker: "/usr/bin/docker",
		Image:  DefaultHookImage,
		Bash:   Bash{Path: DefaultHookBash, Version: "stub"},
	}
}
