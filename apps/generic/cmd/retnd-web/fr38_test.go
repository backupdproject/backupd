package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/retnd/retnd/apps/common/auth/local"
)

// FR-38 at the level it actually bites: this process.
//
// EPIC R (#890) moved this binary's default configuration directory from
// /etc/backupd/config to /etc/retnd/config. An operator running the
// compose file this project published bind-mounts their host configuration
// directory onto the pre-rename path, so the upgraded binary resolves a
// directory that holds nothing — and every branch that follows reads that
// correctly as "this deployment has not been set up yet". The consequence
// on a live deployment is the first-run flow, which claims the
// administrator account and burns the enrollment token, and the operator's
// first instinct (completing the wizard it just handed them) is the action
// that makes it worse.
//
// core/legacypath decides the four cells of FR-38's table and
// core/tests/compat's cells 20, 21 and 22 pin the wording of the warning
// and the refusal against the real `retnd` binary. What THOSE cells
// cannot observe is anything about this process, because
// core/tests/compat is in the core module and apps/ is deleted by
// verify-core-without-apps.sh: whether an adopted start enters the
// first-run flow at all, whether it mints an enrollment token, whether it
// creates a second administrator record, and whether a genuinely empty one
// still first-runs. Those are properties of a running process, so they are
// observed here the same way serve_signal_test.go observes an exit status:
// by re-executing this binary as the engine and reading what it does.
//
// The child entry point, the environment variables and the read-until-a-
// sentinel-line loop are serve_signal_test.go's, deliberately reused
// rather than duplicated: one harness for "what does the engine process
// really do" is the point of having one.

// fr38Layout is one deployment shape: the four directories FR-38's table
// is about, under a throwaway root.
//
// The root is a temp directory rather than / because core/legacypath
// decides on a path COMPONENT equal to this product's name rather than on
// two absolute prefixes — see its own doc for why that is the stronger
// rule and not a concession to testability. <root>/etc/retnd/config and
// <root>/etc/backupd/config are the same pair of names the packaged
// deployment uses, one directory down.
type fr38Layout struct {
	root string

	// renamedConfig is the path this release resolves, and is what every
	// case below passes as --config: the failure this is all about
	// starts with an upgraded binary resolving exactly this path.
	renamedConfig string
	legacyConfig  string

	renamedStateDB string
	legacyStateDB  string

	authStore string
}

func fr38NewLayout(t *testing.T) fr38Layout {
	t.Helper()
	root := t.TempDir()
	l := fr38Layout{
		root:           root,
		renamedConfig:  filepath.Join(root, "etc", "retnd", "config", "config.yaml"),
		legacyConfig:   filepath.Join(root, "etc", "backupd", "config", "config.yaml"),
		renamedStateDB: filepath.Join(root, "var", "lib", "retnd", "state.db"),
		legacyStateDB:  filepath.Join(root, "var", "lib", "backupd", "state.db"),
		// Deliberately NOT under either renamed directory. The local-auth
		// store's packaged path is /data/state/local-auth.json, which
		// carries no brand and is therefore not moved by this rename:
		// putting it inside one of the two families under test would
		// make "no second administrator record" an accident of where the
		// file happened to live.
		authStore: filepath.Join(root, "auth", "local-auth.json"),
	}
	if err := os.MkdirAll(filepath.Dir(l.authStore), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return l
}

// writeConfigNaming writes the smallest configuration this binary starts
// from, pointing at the journal it is given.
func (l fr38Layout) writeConfigNaming(t *testing.T, configPath, dbPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + filepath.Join(l.root, "remote") + "\n" +
		"        local_path: " + filepath.Join(l.root, "local") + "\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.MkdirAll(filepath.Join(l.root, "remote"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// fr38Run starts the engine against a layout and returns what an operator
// would see, plus the exit status.
//
// It stops on the first line matching one of stopOn and then sends
// SIGTERM, so a start that comes up is observed as a running process
// rather than as a timeout, and a start that refuses is observed as an
// exit. Either way both streams are returned whole.
func fr38Run(t *testing.T, l fr38Layout, stopOn ...string) (code int, stdout, stderr string) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestServeChildProcess$")
	cmd.Env = append(os.Environ(),
		serveChildEnv+"=1",
		serveChildConfig+"="+l.renamedConfig,
		serveChildAuthStore+"="+l.authStore,
		serveChildStateDB+"="+l.renamedStateDB,
	)
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the serve child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	var outBuf, errBuf bytes.Buffer
	done := make(chan struct{}, 2)
	sawSentinel := make(chan struct{}, 2)
	scan := func(r *os.File, into *bytes.Buffer) {
		defer func() { done <- struct{}{} }()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			into.WriteString(line)
			into.WriteString("\n")
			for _, want := range stopOn {
				if strings.Contains(line, want) {
					select {
					case sawSentinel <- struct{}{}:
					default:
					}
				}
			}
		}
	}
	go scan(outPipe.(*os.File), &outBuf)
	go scan(errPipe.(*os.File), &errBuf)

	select {
	case <-sawSentinel:
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("sending SIGTERM: %v", err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the serve child printed none of %q within 60s\nstdout:\n%s\nstderr:\n%s",
			stopOn, outBuf.String(), errBuf.String())
	}

	<-done
	<-done
	code = 0
	if waitErr := cmd.Wait(); waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("waiting for the serve child: %v", waitErr)
		}
		code = exitErr.ExitCode()
	}
	return code, outBuf.String(), errBuf.String()
}

// fr38ClaimAdministrator provisions the one administrator an upgraded
// deployment already has.
//
// This is what makes "an adopted start mints no enrollment token" a real
// assertion rather than a coincidence. The token is minted by
// apps/common/auth/local when the store is UNCLAIMED, which is true of a
// fresh install whatever FR-38 decides; the deployment FR-38 protects has
// been running for years and has an administrator. So the shape under
// test is the real one.
func fr38ClaimAdministrator(t *testing.T, l fr38Layout) []byte {
	t.Helper()
	if _, err := local.CreateAdmin(local.CreateAdminConfig{
		StorePath: l.authStore,
		Username:  "operator",
		Password:  "a-long-enough-password",
	}); err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	before, err := os.ReadFile(l.authStore)
	if err != nil {
		t.Fatalf("reading the auth store: %v", err)
	}
	return before
}

const (
	// The two lines that say this process took the first-run branch.
	// Spelled out as literals rather than imported, exactly as
	// serve_signal_test.go pins its shutdown notice: what is under test
	// is what an operator finds in the container log.
	fr38FirstRunNotice   = "serving the first-run setup flow"
	fr38EnrollmentNotice = "Enrollment bootstrap token:"
	fr38StartupNotice    = "retnd-web: runtime profile"

	// fr38Serving is what a "this deployment came up" case waits for,
	// and it is the END of the engine's first scheduled cycle rather
	// than any line printed on the way in.
	//
	// The profile line above is printed several steps before the journal
	// is even opened, so signalling on it cuts the open short and reports
	// a context cancellation as a failed start — which is exactly what
	// the first version of this harness did. A completed cycle is the
	// strongest available statement that this process is really serving
	// THIS deployment: it opened the adopted journal, ran discovery
	// against the configured backup set and wrote the result. Nothing
	// weaker distinguishes "came up" from "got as far as printing a
	// banner".
	fr38Serving = `"event":"cycle_end"`
)

// TestServe_FR38_AdoptsAPreRenameConfigurationInsteadOfFirstRunning is the
// forbidden cell, asserted as the absence of the damage rather than as the
// presence of a message.
//
// The configuration and the journal are at the pre-rename paths and
// nothing is at the renamed ones, which is precisely an upgraded
// deployment whose compose file has not been edited. The start must serve
// that deployment, warn about it, and reach NONE of the first-run path:
// no setup flow, no enrollment token, no second administrator record, and
// no configuration file conjured at the renamed path.
func TestServe_FR38_AdoptsAPreRenameConfigurationInsteadOfFirstRunning(t *testing.T) {
	l := fr38NewLayout(t)
	l.writeConfigNaming(t, l.legacyConfig, l.legacyStateDB)
	storeBefore := fr38ClaimAdministrator(t, l)

	code, stdout, stderr := fr38Run(t, l, fr38Serving)

	if code != 0 {
		t.Errorf("serve exited %d on a deployment mounted at the pre-rename path, want 0: adoption is deliberately not a refusal, because refusing is a self-inflicted outage on a backup product\nstderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, fr38FirstRunNotice) {
		t.Fatalf("serve took the FIRST-RUN branch on a deployment whose configuration is at the pre-rename path. This is the one cell FR-38 exists to make unrepresentable: a live deployment has just been handed a setup wizard.\nstderr:\n%s", stderr)
	}
	if strings.Contains(stdout, fr38EnrollmentNotice) {
		t.Errorf("serve minted an enrollment token on an adopted deployment that already has an administrator; the token an operator is told to use is single-use, so minting one here invalidates the enrollment they already completed\nstdout:\n%s", stdout)
	}
	if _, err := os.Stat(l.renamedConfig); err == nil {
		t.Errorf("serve wrote a configuration at %s, the renamed path that was supposed to be empty: the adoption decided to serve the pre-rename path and then something wrote to the other one anyway", l.renamedConfig)
	}
	storeAfter, err := os.ReadFile(l.authStore)
	if err != nil {
		t.Fatalf("reading the auth store: %v", err)
	}
	if !bytes.Equal(storeBefore, storeAfter) {
		t.Errorf("the administrator store changed across an adopted start, so a record was created or rewritten on a deployment that already had one\nbefore:\n%s\nafter:\n%s", storeBefore, storeAfter)
	}

	// The warning itself: it has to name both paths, the compose line to
	// change and the command that changes it, because an operator meets
	// it once, in an incident, and a warning that only says "deprecated"
	// costs them an investigation.
	for _, want := range []string{
		"the pre-rename path",
		l.legacyConfig,
		l.renamedConfig,
		"migrate-identity",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the adoption warning never mentioned %q, so an operator reading this log cannot act on it\nstderr:\n%s", want, stderr)
		}
	}

	// EVERY start, not just the first. This is the whole difference
	// between a warning and a one-off notice: the deployment stays
	// adopted until somebody migrates it, and the start that finally
	// gets read is not the one that happened on upgrade day.
	_, stdoutAgain, stderrAgain := fr38Run(t, l, fr38Serving)
	if !strings.Contains(stderrAgain, "the pre-rename path") {
		t.Errorf("the second start did not warn, so the warning is a first-start notice rather than a standing one\nstderr:\n%s", stderrAgain)
	}
	if strings.Contains(stderrAgain, fr38FirstRunNotice) || strings.Contains(stdoutAgain, fr38EnrollmentNotice) {
		t.Errorf("the second start took the first-run branch\nstdout:\n%s\nstderr:\n%s", stdoutAgain, stderrAgain)
	}
}

// TestServe_FR38_AGenuinelyEmptyDeploymentStillFirstRuns is the positive
// control for the test above, and it is not optional.
//
// "It adopted the pre-rename path" is satisfiable by a build that adopts
// unconditionally, or by one that can no longer be installed fresh at
// all — and either of those would pass every assertion above. So a
// deployment with nothing at either path has to still reach the setup
// flow and still mint the token the operator needs to claim it.
func TestServe_FR38_AGenuinelyEmptyDeploymentStillFirstRuns(t *testing.T) {
	l := fr38NewLayout(t)
	if err := os.MkdirAll(filepath.Dir(l.renamedStateDB), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	code, stdout, stderr := fr38Run(t, l, fr38FirstRunNotice)

	if code != 0 {
		t.Errorf("serve exited %d on a genuinely empty deployment, want 0\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, fr38FirstRunNotice) {
		t.Fatalf("a deployment with nothing at either path did not serve the first-run setup flow, so this product can no longer be installed\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stdout, fr38EnrollmentNotice) {
		t.Errorf("no enrollment token was printed on a fresh install, so nobody can claim it\nstdout:\n%s", stdout)
	}
	if strings.Contains(stderr, "the pre-rename path") {
		t.Errorf("a fresh install was reported as an adoption; the pre-rename paths do not exist at all here\nstderr:\n%s", stderr)
	}
}

// TestServe_FR38_RefusesTwoDifferentPopulatedDirectories is the fourth
// row. Two journals are visible, choosing one silently is the worst
// option available, so nothing chooses.
func TestServe_FR38_RefusesTwoDifferentPopulatedDirectories(t *testing.T) {
	l := fr38NewLayout(t)
	l.writeConfigNaming(t, l.legacyConfig, l.legacyStateDB)
	l.writeConfigNaming(t, l.renamedConfig, l.renamedStateDB)

	code, stdout, stderr := fr38Run(t, l, "refusing to start")

	if code == 0 {
		t.Fatalf("serve started with two different populated configuration directories, want a refusal\nstderr:\n%s", stderr)
	}
	for _, want := range []string{l.renamedConfig, l.legacyConfig, "different device and inode"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal never mentioned %q; an operator cannot choose between two directories from a message that names one\nstderr:\n%s", want, stderr)
		}
	}
	// The refusal has to land BEFORE anything is minted or announced,
	// which is the difference between a refusal and a diagnosis printed
	// underneath the damage.
	if strings.Contains(stdout, fr38EnrollmentNotice) {
		t.Errorf("the refusal came after an enrollment token had already been minted\nstdout:\n%s", stdout)
	}
	if strings.Contains(stderr, fr38StartupNotice) {
		t.Errorf("the refusal came after the runtime profile had been resolved and the process had begun starting\nstderr:\n%s", stderr)
	}
}

// TestServe_FR38_DoesNotRefuseOneDirectoryReachableUnderBothNames is the
// control that keeps the refusal above from being satisfiable by a build
// that refuses whenever both paths are populated.
//
// The installer writes a compose override mounting ONE host directory at
// BOTH container paths, so a downgrade to the previous build finds its own
// paths for the length of the rollback window. Both spellings then hold a
// configuration and a journal — and both are the same device and inode,
// which is not ambiguity. A build without the same-device-and-inode test
// refuses here, which stops the supported downgrade path from starting at
// all.
func TestServe_FR38_DoesNotRefuseOneDirectoryReachableUnderBothNames(t *testing.T) {
	l := fr38NewLayout(t)

	// One host directory, two names each for the configuration and the
	// state families. Symlinks rather than bind mounts, for the property
	// that matters: os.Stat follows them, so both names report one
	// device and one inode, exactly as a doubled bind mount does.
	host := filepath.Join(l.root, "host")
	if err := os.MkdirAll(filepath.Join(host, "config"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(l.root, "etc"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(l.root, "var", "lib"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, link := range []string{
		filepath.Join(l.root, "etc", "retnd"),
		filepath.Join(l.root, "etc", "backupd"),
		filepath.Join(l.root, "var", "lib", "retnd"),
		filepath.Join(l.root, "var", "lib", "backupd"),
	} {
		if err := os.Symlink(host, link); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}
	l.writeConfigNaming(t, filepath.Join(host, "config", "config.yaml"), filepath.Join(host, "state.db"))
	fr38ClaimAdministrator(t, l)

	code, _, stderr := fr38Run(t, l, fr38Serving)

	if strings.Contains(stderr, "refusing to start") {
		t.Fatalf("serve refused a deployment whose two container paths are ONE host directory. That is the installer's own rollback-window compose file, so this build cannot be downgraded and the same-device-and-inode test is not doing its job.\nstderr:\n%s", stderr)
	}
	if code != 0 {
		t.Errorf("serve exited %d on the rollback-window layout, want 0\nstderr:\n%s", code, stderr)
	}
}
