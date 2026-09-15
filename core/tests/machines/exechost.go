// The exec host: a disposable sshd whose accounts differ in WHAT THE
// SERVER WILL LET THEM RUN, so #810's central claim can be proven instead
// of asserted.
//
// That claim is that an SFTP transfer credential must not be assumed to
// grant shell exec. A hardened backup source legitimately uses an account
// forced into internal-sftp, or one pinned behind a forced command, and
// either one authenticates perfectly, transfers files perfectly, and cannot
// run a hook script at all. Source (source.go) is exactly such a machine --
// atmoz/sftp is chrooted and internal-sftp-forced, which is why it can
// never prove the positive half.
//
// So this machine carries three accounts on one sshd, authenticating with
// ONE client key: an ordinary shell account, an internal-sftp-forced
// account, and a forced-command account. One key across all three is what
// makes "this credential may not exec" distinguishable from "this
// credential does not authenticate".
//
// scripts/e2e/exec-host.Dockerfile is the definition, read rather than
// restated here for sourceDockerfile's reason.
package machines

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/retnd/retnd/core/tests/dockerlease"
)

// The three accounts, by name. A test names the capability it wants rather
// than a username, which is what keeps the fixture's own arrangement out of
// the assertions.
const (
	// ExecUser is the ordinary shell account: the exec-capable execution
	// connection a remote hook legitimately runs over.
	ExecUser = "hookuser"
	// SFTPOnlyUser is forced into internal-sftp, the atmoz/sftp posture
	// docs/ssh-setup.md recommends for a backup source. It authenticates
	// and transfers; it cannot run a command.
	SFTPOnlyUser = "sftponly"
	// ForcedCommandUser runs somebody else's program whatever the client
	// asks for, the rrsync/backup-shell shape.
	ForcedCommandUser = "forcedcmd"
	// ContaminatedUser is an ordinary shell account whose SERVER sets
	// BASH_ENV for it (sshd SetEnv). bash reads BASH_ENV at startup, so
	// this is the one contamination --noprofile --norc cannot defend
	// against and a capability preflight has to detect instead.
	ContaminatedUser = "contaminated"
)

// ForcedCommandOutput is what ForcedCommandUser's wrapper prints instead of
// whatever the client asked for, verbatim from
// scripts/e2e/exec-host.Dockerfile.
//
// It is here so a test can assert the refusal carries THIS account's own
// evidence rather than the generic "no marker came back" wording, which
// any broken account produces. Reading it from the fixture is what makes
// deleting the fixture's ForceCommand a test failure instead of a quieter
// test.
const ForcedCommandOutput = "forced-command-only: this account runs "

// RemoteBashPath is where bash is on this machine. Tests that prove the
// "bash is not there" refusal point at a path that is not this one.
const RemoteBashPath = "/bin/bash"

// execHostImageOnce builds the image at most once per test binary, for
// ensureSourceImage's reason.
var (
	execHostImageOnce sync.Once
	execHostImageRef  string
	execHostImageErr  error
)

// ExecHost is a running sshd plus everything a test needs to open a real
// exec session against it.
type ExecHost struct {
	Host string
	Port int

	// KeyFile is the private client key authorized for all three accounts.
	KeyFile string

	// KnownHostsFile pins this machine's real host keys.
	// BadKnownHostsFile pins a different key for the same address, so a
	// host-key policy violation can be proven to refuse rather than only
	// the happy path proven to pass.
	KnownHostsFile    string
	BadKnownHostsFile string

	// Alias is the name other containers on the network reach it by.
	Alias string

	containerID   string
	containerName string
	runDir        string

	ctx    context.Context
	cancel context.CancelCauseFunc

	mu       sync.Mutex
	done     chan struct{}
	finished bool
	stage    string

	teardownOnce sync.Once
}

// ExecHost starts the exec host the first time it is called and returns the
// same one after that, so a suite pays for one container however many tests
// use it.
func (m *Machines) ExecHost(t *testing.T) *ExecHost {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.execHost != nil {
		return m.execHost
	}
	h := startExecHost(t, m.Network, "exechost-"+shortID(t), m.inNetwork)
	m.execHost = h
	return h
}

// Context is the fail-fast channel, exactly as Source.Context is: it is
// cancelled with a cause the moment the fixture knows its container has
// gone or the test has outrun its budget, so an operation taking it unwinds
// with a legible reason instead of retrying against a corpse.
func (h *ExecHost) Context() context.Context { return h.ctx }

// Addr is host:port as the test process reaches this machine.
func (h *ExecHost) Addr() string { return h.Host + ":" + strconv.Itoa(h.Port) }

// ContainerID is docker's id for the container, for a test that has to
// address it. Nothing under core/tests may exec docker itself (the
// bypasses-harness rule), which is why Inside below exists.
func (h *ExecHost) ContainerID() string { return h.containerID }

// Inside runs a command in the container and returns its combined view, for
// the assertions that are about the REMOTE machine's state rather than
// about what the code under test returned: whether a script file was left
// behind, and whether a process is still running.
//
// It is a harness capability rather than a per-test helper for the reason
// EstablishedConnections is one: a test that reached it by exec'ing docker
// would be the exact bypass core/internal/testtier exists to stop.
//
// It never asserts. The test decides what the answer means.
func (h *ExecHost) Inside(t *testing.T, argv ...string) string {
	t.Helper()
	args := append([]string{"exec", h.containerID}, argv...)
	stdout, stderr, err := dockerRun(dockerProbeTimeout, args...)
	if err != nil {
		// A non-zero exit is an ANSWER here ("no such file", "no
		// matching process"), not an infrastructure failure, so the
		// output is returned rather than the test failed. A docker that
		// did not answer at all is different, and says so.
		if errors.Is(err, errDockerTimedOut) {
			t.Fatalf("%s machines: `docker exec` on the exec host did not answer within %s: %v", infraMarker, dockerProbeTimeout, err)
		}
		return stdout + stderr
	}
	return stdout + stderr
}

// --- standing it up -------------------------------------------------------

func startExecHost(t *testing.T, network, alias string, inNetwork bool) *ExecHost {
	t.Helper()

	h := &ExecHost{Host: "127.0.0.1", done: make(chan struct{})}
	h.ctx, h.cancel = context.WithCancelCause(context.Background())
	h.setStage("creating the exec host's run directory")
	t.Cleanup(h.finish)
	h.watch(t)

	if inNetwork {
		h.Host = alias
		h.Port = 22
	}
	h.Alias = alias

	runDir := filepath.Join(testsRoot(t), ".run", fmt.Sprintf("exec-%s-%d", sanitize(t.Name()), time.Now().UnixNano()))
	must(t, os.MkdirAll(runDir, 0o700), "create exec host run dir")
	h.mu.Lock()
	h.runDir = runDir
	h.mu.Unlock()

	// Both host key types, for the reason startSourceOn states: x/crypto/ssh
	// negotiates a host-key algorithm by its own preference order rather
	// than by what known_hosts happens to hold, so pinning one type is not
	// pinning the connection.
	h.setStage("generating the exec host's host and client keys")
	hostKeyEd25519 := filepath.Join(runDir, "ssh_host_ed25519_key")
	keygenType(t, hostKeyEd25519, "ed25519", "")
	hostKeyRSA := filepath.Join(runDir, "ssh_host_rsa_key")
	keygenType(t, hostKeyRSA, "rsa", "2048")

	clientKey := filepath.Join(runDir, "id_ed25519")
	keygenType(t, clientKey, "ed25519", "")
	h.KeyFile = clientKey

	authorizedDir := filepath.Join(runDir, "authorized")
	must(t, os.MkdirAll(authorizedDir, 0o755), "create authorized dir")
	copyFile(t, clientKey+".pub", filepath.Join(authorizedDir, "backupd.pub"))

	h.setStage("dockerlease.Sweep (reclaiming containers a killed run left behind)")
	dockerlease.Sweep()

	image := h.ensureExecHostImage(t)

	name := fmt.Sprintf("backupd-gate-exec-%d", time.Now().UnixNano())
	h.mu.Lock()
	h.containerName = name
	h.mu.Unlock()

	args := []string{"run", "-d", "--name", name, dockerlease.LabelFlag, dockerlease.LabelSpec}
	if network != "" {
		args = append(args, "--network", network, "--network-alias", alias)
	}
	if !inNetwork {
		args = append(args, "-p", "127.0.0.1::22")
	}
	args = append(args,
		"-v", hostKeyEd25519+":/etc/ssh/ssh_host_ed25519_key:ro",
		"-v", hostKeyEd25519+".pub:/etc/ssh/ssh_host_ed25519_key.pub:ro",
		"-v", hostKeyRSA+":/etc/ssh/ssh_host_rsa_key:ro",
		"-v", hostKeyRSA+".pub:/etc/ssh/ssh_host_rsa_key.pub:ro",
		"-v", authorizedDir+":/etc/ssh/authorized:ro",
		image,
	)
	h.setStage("docker run " + image)
	containerID, err := dockerCapture(t, dockerRunTimeout, args...)
	if err != nil {
		t.Fatalf("machines: docker run (exec host): %v", err)
	}
	h.mu.Lock()
	h.containerID = containerID
	h.mu.Unlock()

	if !inNetwork {
		h.setStage("waiting for the exec host to publish its ssh port")
		h.Port = waitForPublishedPort(t, containerID)
	}

	h.setStage("ssh-keyscan for the exec host's host keys")
	h.KnownHostsFile = filepath.Join(runDir, "known_hosts")
	keyscan(t, h.Host, h.Port, h.KnownHostsFile)

	decoyKey := filepath.Join(runDir, "decoy_ed25519")
	keygen(t, decoyKey)
	h.BadKnownHostsFile = filepath.Join(runDir, "known_hosts_bad")
	writeSubstituteKnownHosts(t, h.BadKnownHostsFile, h.Host, h.Port, decoyKey+".pub")

	// Readiness is proven by AUTHENTICATION, not by a TCP connect, for
	// waitForSSHAuth's reason: a published docker port accepts connections
	// before sshd inside is answering, and a server can complete the
	// transport handshake and still turn the key away.
	h.setStage("waiting for sshd to authenticate the exec account")
	if err := waitForSSHAuth(h.Addr(), clientConfigFor(t, clientKey, ExecUser), sshReadyWindow); err != nil {
		dumpContainerLogs(t, containerID)
		t.Fatalf("machines: the exec host did not authenticate %s within %s: %v", ExecUser, sshReadyWindow, err)
	}

	h.setStage("running the test body")
	return h
}

// ensureExecHostImage builds the exec host image once per daemon, keyed by a
// digest of the Dockerfile so a changed definition is a new tag rather than
// a stale one.
func (h *ExecHost) ensureExecHostImage(t *testing.T) string {
	t.Helper()
	execHostImageOnce.Do(func() {
		text := execHostDockerfile(t)
		sum := sha256.Sum256([]byte(text))
		tag := "backupd-machines-exechost:" + hex.EncodeToString(sum[:6])
		h.setStage("docker image inspect " + tag)
		if _, _, err := dockerRun(imageInspectTimeout, "image", "inspect", tag); err == nil {
			execHostImageRef = tag
			return
		}
		// The base first, through the presence-check-then-pull path #243
		// asked for, then the local build on top of it. The policy is
		// Source's own, shared rather than restated, so a second copy
		// cannot drift into being more obliging than the first.
		ensureImageStaged(t, h.setStage, serverImage)

		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(text), 0o644); err != nil {
			execHostImageErr = fmt.Errorf("staging the exec host Dockerfile: %w", err)
			return
		}
		h.setStage("docker build " + tag + " (the exec host, once per daemon)")
		out, err := runDockerBuildWatched(context.Background(), defaultDockerBuildBounds, dockerBuildPoll, tag, dir)
		if err != nil {
			execHostImageErr = fmt.Errorf("building the exec host image %s: %w\n%s", tag, err, out)
			return
		}
		execHostImageRef = tag
	})
	if execHostImageErr != nil {
		t.Fatalf("machines: %v\nThat is a FAILURE and deliberately not a skip: skipping would take #810's exec-capability evidence out of the gate while the gate went on reporting ok (#160).", execHostImageErr)
	}
	return execHostImageRef
}

func execHostDockerfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "scripts", "e2e", "exec-host.Dockerfile")
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s machines: the exec host Dockerfile is not readable at %s: %v\nWithout it there is no machine that can run a hook script, so #810 has no evidence.", infraMarker, path, err)
	}
	return string(text)
}

// --- the watchdog and teardown -------------------------------------------

func (h *ExecHost) setStage(stage string) {
	h.mu.Lock()
	h.stage = stage
	h.mu.Unlock()
}

func (h *ExecHost) finish() {
	h.mu.Lock()
	if !h.finished {
		h.finished = true
		close(h.done)
	}
	h.mu.Unlock()
	h.cancel(errTestFinished)
	h.teardown()
}

// teardown removes the container and the run directory, once, however the
// test ended. Every error is dropped for the reason Source.teardown states.
func (h *ExecHost) teardown() {
	h.teardownOnce.Do(func() {
		h.mu.Lock()
		id, dir := h.containerID, h.runDir
		h.mu.Unlock()
		if id != "" {
			_, _, _ = dockerRun(dockerRemoveTimeout, "rm", "-f", "-v", id)
		}
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	})
}

// watch is Source.watch's shape for this machine: it answers the one
// question a hung machine-tier test cannot answer for itself -- is the
// fixture dead, or is the code under test stuck?
func (h *ExecHost) watch(t *testing.T) {
	budget := durationFromEnv(budgetEnv, defaultTestBudget)
	started := time.Now()
	go func() {
		ticker := time.NewTicker(probeInterval)
		defer ticker.Stop()
		missing := 0
		for {
			select {
			case <-h.done:
				return
			case <-ticker.C:
			}

			h.mu.Lock()
			id, stage := h.containerID, h.stage
			h.mu.Unlock()

			if time.Since(started) > budget {
				h.cancel(fmt.Errorf("the exec host's test budget of %s expired while %q", budget, stage))
				t.Errorf("%s machines: this test outran its %s budget while %q. Nothing died, so this points at a hang in the code under test rather than at the fixture.", infraMarker, budget, stage)
				h.teardown()
				return
			}
			if id == "" {
				continue
			}

			st, err := inspectContainer(id)
			switch {
			case errors.Is(err, errNoSuchContainer):
				missing++
			case err != nil:
				// The daemon said nothing about the container, which is
				// no evidence either way.
				continue
			case !st.Running:
				missing++
			default:
				missing = 0
				continue
			}
			if missing < probesBeforeDeclaringDeath {
				continue
			}
			h.mu.Lock()
			name := h.containerName
			h.mu.Unlock()
			logs := containerLogTail(id)
			h.cancel(&ContainerDiedError{
				Name:      name,
				ID:        id,
				Removed:   errors.Is(err, errNoSuchContainer),
				ExitCode:  st.ExitCode,
				OOMKilled: st.OOMKilled,
				Status:    st.Status,
				Logs:      logs,
			})
			t.Errorf("%s machines: the exec host container %s is gone or stopped while %q, so whatever this test was doing was never going to succeed.\n%s",
				infraMarker, id, stage, logs)
			h.teardown()
			return
		}
	}()
}
