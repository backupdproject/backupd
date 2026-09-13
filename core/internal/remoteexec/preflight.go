package remoteexec

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// preflightTimeout bounds each probe session. The probe runs one shell and
// prints a dozen lines, so this is generous for a loaded host and still
// short enough that a connection which authenticates and then hangs fails
// the step rather than the backup window.
const preflightTimeout = 45 * time.Second

// probeMarker is the line the probe prints first and the reaper prints
// nothing like.
//
// It exists because an exec-incapable account does not necessarily FAIL:
// internal-sftp answers "This service allows sftp connections only" and
// exits 1, which a client could mistake for a hook that exited 1, and a
// forced-command account exits 0 having run somebody else's program
// entirely. Neither can produce this marker, because producing it requires
// having executed the bytes this product sent. So the capability test is
// "did my script run", asked in a way that cannot be answered by accident.
const probeMarker = "backupd-exec-probe-ok"

// Report is what a preflight established about a connection, for the audit
// line and for the refusal wording.
//
// Every field is a fact about the far side that an operator may need and
// that carries no credential: the host identity that was actually
// presented, who the hook will run as, which bash and which version, and
// whether the account's own startup files have left anything behind that
// would change what a hook means.
type Report struct {
	// HostKeyFingerprint is the identity the server presented, already
	// verified against the pinned policy by the time this exists.
	HostKeyFingerprint string

	// User is who the far side says the session authenticated as. It is
	// read from the remote rather than copied from the configuration, so
	// a server-side Match block that switched accounts is visible.
	User string

	// BashPath and BashVersion are the shell that will run the hook.
	BashPath    string
	BashVersion string

	// PTY records whether the session had a terminal. It must be false:
	// the envelope never asks for one, and a server that provided one
	// anyway would have merged the hook's stdout and stderr into a single
	// stream, which is the one thing the capture contract cannot recover
	// from.
	PTY bool

	// Contamination names what the remote shell arrived carrying that
	// would change what a hook means: BASH_ENV or ENV set at all, or a
	// meaning-changing shell option already applied. Empty is the healthy
	// answer.
	Contamination []string

	// ScriptSyntaxChecked is true when the captured bytes were parsed by
	// the target's own bash with -n. It is a separate fact from "the
	// syntax was fine", because a preflight that could not run the check
	// must not be read as having run it.
	ScriptSyntaxChecked bool
}

// Preflight proves, against the server, that this connection can run this
// script -- before a byte of the hook is sent.
//
// The five things it establishes are #810's own list, and each of them is
// something that cannot be assumed:
//
//   - the host identity matches the pinned policy. That is settled before
//     this function is reached, by Dial, and is recorded here because an
//     audit line that says which host a hook ran on is only worth reading
//     if the identity was checked.
//   - the exec channel is permitted. An internal-sftp-forced account
//     refuses it; a forced-command account accepts it and runs something
//     else. Both are caught by requiring the probe's own marker.
//   - bash is present and executable at the configured path. A missing
//     path is the login shell's 127, a non-executable one its 126, and
//     both are reported as capability refusals naming the path rather than
//     as hook failures.
//   - the invocation works with no PTY. Asserted from inside the probe,
//     because "we did not request one" is a statement about this client
//     and not about what the session got.
//   - the captured bytes parse on the TARGET, with the target's own bash.
//     A script written for bash 5 can fail to parse on bash 3.2, and the
//     honest place to discover that is before the hook has half run.
//
// A refusal is ErrExecCapability, which the caller turns into a failed
// remote-hook validation. Artifact backup over the same credential is
// untouched: nothing here changes the transfer path.
func (c *Client) Preflight(ctx context.Context, script []byte) (Report, error) {
	report := Report{
		HostKeyFingerprint: c.HostKeyFingerprint(),
		BashPath:           c.conn.Bash(),
	}

	if err := workflowexec.ValidateScript(script); err != nil {
		return report, err
	}

	probe, err := c.runProbe(ctx)
	if err != nil {
		return report, err
	}

	report.User = probe["user"]
	report.BashVersion = probe["bash_version"]
	report.PTY = probe["tty"] == "yes"
	report.Contamination = contaminationOf(probe)

	switch {
	case report.BashVersion == "":
		return report, fmt.Errorf("%w: %s ran a shell at %s that reports no BASH_VERSION, so it is not bash. This product never substitutes another shell for a hook: configure the connection's bash path, or install bash on that host",
			ErrExecCapability, c.describeEndpoint(), report.BashPath)
	case report.PTY:
		return report, fmt.Errorf("%w: %s allocated a terminal for a session that never asked for one, which merges a hook's stdout and stderr into one stream that cannot be separated again",
			ErrExecCapability, c.describeEndpoint())
	case len(report.Contamination) != 0:
		return report, fmt.Errorf("%w: the account's own shell startup left %s set on %s, and those change what a hook script means before its first line runs. Clear them for this account, or use a separate execution connection",
			ErrExecCapability, strings.Join(report.Contamination, ", "), c.describeEndpoint())
	}

	if err := c.checkSyntax(ctx, script); err != nil {
		return report, err
	}
	report.ScriptSyntaxChecked = true

	return report, nil
}

// runProbe runs the fixed probe script and parses its key=value lines.
//
// It goes over the same channel, with the same fixed remote command and the
// same stdin delivery a hook uses, so what it proves is the capability a
// hook needs. The one thing it does NOT use is the payload's environment
// prologue, and that is the point of the probe rather than an omission:
// the prologue's first act is to unset BASH_ENV, ENV, SHELLOPTS and
// BASHOPTS, and this probe exists to report what those were BEFORE anything
// cleared them. A probe run through the prologue would report a clean shell
// on a contaminated host, every time.
func (c *Client) runProbe(ctx context.Context) (map[string]string, error) {
	if err := workflowexec.ValidateScript([]byte(probeScript)); err != nil {
		return nil, err
	}

	var stdout, stderr bytes.Buffer
	exit, err := c.runOnce(ctx, preflightTimeout, "", []byte(probeScript), &stdout, &stderr)
	if err != nil {
		return nil, err
	}

	out := stdout.String()
	if !strings.Contains(out, probeMarker) {
		return nil, c.capabilityRefusal(exit, out, stderr.String())
	}

	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if name, value, found := strings.Cut(strings.TrimSpace(line), "="); found {
			values[name] = value
		}
	}

	return values, nil
}

// capabilityRefusal turns "the probe did not come back" into a sentence
// naming the capability that is missing, because the three ways it happens
// look nothing like each other from the outside and have different fixes.
func (c *Client) capabilityRefusal(exit int, stdout, stderr string) error {
	evidence := strings.TrimSpace(stdout + " " + stderr)
	if len(evidence) > 400 {
		evidence = evidence[:400] + "..."
	}

	switch {
	case strings.Contains(stdout, "sftp connections only") || strings.Contains(stderr, "sftp connections only"):
		return fmt.Errorf("%w: %s is an SFTP-only account -- the server answered the exec request with %q. That is a valid backup transport and an invalid workflow executor: artifact backup over this credential keeps working, and a NAME.remote.sh needs a separate exec-capable execution connection (workflows.exec_connections)",
			ErrExecCapability, c.describeEndpoint(), evidence)
	case exit == 127:
		return fmt.Errorf("%w: %s has no executable at %s -- the server answered %q. This product never substitutes /bin/sh or another shell for a hook, so configure the connection's bash path or install bash there",
			ErrExecCapability, c.describeEndpoint(), c.conn.Bash(), evidence)
	case exit == 126:
		return fmt.Errorf("%w: %s has something at %s that is not executable by %s -- the server answered %q",
			ErrExecCapability, c.describeEndpoint(), c.conn.Bash(), c.conn.Source.User, evidence)
	default:
		return fmt.Errorf("%w: %s accepted the exec request and ran something else -- the session exited %d having produced %q instead of this product's own probe output, which is what a forced command (authorized_keys command=, sshd ForceCommand) does. A forced-command account cannot run a hook: give the backup set a separate exec-capable execution connection",
			ErrExecCapability, c.describeEndpoint(), exit, evidence)
	}
}

// checkSyntax parses the captured bytes with the TARGET's bash -n.
//
// The bytes go to bash's stdin as the script itself rather than through the
// envelope's payload, because what has to be parsed is the hook, on its own
// lines: a syntax error then reports the script's OWN line numbers, which
// is the whole value of doing this before the run. The envelope's runtime
// path carries a constant offset (see workflowexec.StdinPayload), and this
// is why that offset costs nothing in practice.
func (c *Client) checkSyntax(ctx context.Context, script []byte) error {
	session, err := c.session()
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()

	var stderr bytes.Buffer
	session.Stderr = &stderr
	session.Stdout = &bytes.Buffer{}

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("%w: opening stdin on the syntax-check channel: %v", ErrTransportLoss, err)
	}
	// -n before -s: parse, never execute. The remote command is otherwise
	// the same fixed string, so a server that refuses exec refuses this
	// too and the capability refusal has already been reported.
	command := "exec " + c.conn.Bash() + " --noprofile --norc -n -s"
	if err := session.Start(command); err != nil {
		return fmt.Errorf("%w: starting the syntax check: %v", ErrTransportLoss, err)
	}
	_, _ = stdin.Write(script)
	_ = stdin.Close()

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	select {
	case waitErr := <-done:
		if waitErr == nil {
			return nil
		}
		return fmt.Errorf("%w: the captured script does not parse on %s -- that host's own bash said: %s. It is refused before it runs rather than half-executed",
			ErrExecCapability, c.describeEndpoint(), strings.TrimSpace(stderr.String()))
	case <-ctx.Done():
		return fmt.Errorf("%w: the syntax check on %s did not finish within %s", ErrTransportLoss, c.describeEndpoint(), preflightTimeout)
	}
}

// describeEndpoint names the far side the way an audit line may: the user
// and the host, and the reference that chose them. Never a key, never a
// passphrase, never an environment value.
func (c *Client) describeEndpoint() string {
	return fmt.Sprintf("%s@%s (execution connection %q)", c.conn.Source.User, c.addr, c.conn.Ref)
}

// contaminationOf reads the probe's report of the remote shell's startup
// state.
//
// BASH_ENV and ENV are reported when set because bash sources them at
// STARTUP -- before the payload's own unset can run -- so a value there has
// already had its chance to redirect the shell. SHELLOPTS and BASHOPTS are
// reported only for the options that change what a script MEANS: bash sets
// braceexpand and hashall on every non-interactive shell, and
// interactive-comments comes along with them, so treating a non-empty
// SHELLOPTS as contamination would refuse every host on earth.
func contaminationOf(probe map[string]string) []string {
	var found []string

	for _, name := range workflowexec.StartupVariables() {
		value := probe[strings.ToLower(name)]
		if value == "" {
			continue
		}

		switch name {
		case "BASH_ENV", "ENV":
			// Set is enough. bash sources these at STARTUP, before the
			// payload's own unset can run, so by the time a hook's first
			// line is reached a file of somebody else's choosing has
			// already executed.
			found = append(found, name+" is set")
		default:
			// SHELLOPTS and BASHOPTS arrive non-empty on every host:
			// bash sets braceexpand and hashall on any non-interactive
			// shell, and interactive-comments comes with them. So only
			// the options that change what a script MEANS are refused,
			// which is the same list this product refuses to inject.
			for _, opt := range strings.Split(value, ":") {
				if meaningChangingShellOptions[opt] {
					found = append(found, name+" carries "+opt)
				}
			}
		}
	}

	return found
}

// meaningChangingShellOptions are the shell options whose presence at
// startup makes a hook script mean something other than what it says.
//
// It is the mirror image of the envelope's own rule: this product injects
// none of them (internal/workflowexec's package doc), so inheriting one
// from the remote account's environment would reintroduce exactly what
// that rule exists to prevent, silently, on one host.
var meaningChangingShellOptions = map[string]bool{
	"errexit":    true,
	"nounset":    true,
	"pipefail":   true,
	"xtrace":     true,
	"verbose":    true,
	"posix":      true,
	"noexec":     true,
	"noglob":     true,
	"allexport":  true,
	"privileged": true,
}

// probeScript is the fixed capability probe. It prints the marker first, so
// a truncated answer is still recognisable, and everything else as
// key=value lines.
//
// It runs no external command except `id`, and tolerates that one being
// absent: the point is to establish what the SHELL is and what state it
// started in, and a host with no `id` on PATH is still a host that can run
// a hook.
const probeScript = `printf '%s\n' ` + probeMarker + `
printf 'bash_version=%s\n' "${BASH_VERSION-}"
printf 'user=%s\n' "$(id -un 2>/dev/null || printf unknown)"
if [ -t 0 ] || [ -t 1 ]; then printf 'tty=yes\n'; else printf 'tty=no\n'; fi
printf 'bash_env=%s\n' "${BASH_ENV-}"
printf 'env=%s\n' "${ENV-}"
printf 'shellopts=%s\n' "${SHELLOPTS-}"
printf 'bashopts=%s\n' "${BASHOPTS-}"
`
