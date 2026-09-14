package remoteexec

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/backupdproject/backupd/core/internal/transport"
	"github.com/backupdproject/backupd/core/internal/workflowexec"
)

// An SSH server this test binary runs, for the three behaviours a real
// server cannot be asked for on demand.
//
// core/tests/sshexecintegration is where this package's claims are measured
// against a real OpenSSH -- what a forced command does, what internal-sftp
// answers, what happens to a process group. None of that is restated here.
// What is here is the set of behaviours a server has to be BUILT to have:
// one that accepts a channel and then reads nothing for ever, one that
// drops the connection in the middle of a syntax check, and one that
// answers every request successfully while running none of the bytes it was
// sent. Each of them is a state a production server reaches by failing, and
// waiting for one to fail is not a test.

// fakeSession is one exec request the fixture server received.
type fakeSession struct {
	command string
	channel ssh.Channel
}

// ReadAll drains the channel's stdin, which is how the payload arrives.
func (s *fakeSession) ReadAll(t *testing.T) string {
	t.Helper()
	data, err := io.ReadAll(s.channel)
	if err != nil && !errors.Is(err, io.EOF) {
		return string(data)
	}

	return string(data)
}

func (s *fakeSession) Print(t *testing.T, text string) {
	t.Helper()
	if _, err := io.WriteString(s.channel, text); err != nil {
		t.Logf("fixture server: writing to the channel: %v", err)
	}
}

func (s *fakeSession) Exit(code uint32) {
	_, _ = s.channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
	_ = s.channel.Close()
}

// fakeSSHD is a server that authenticates any key and hands every exec
// request to a handler the test wrote.
type fakeSSHD struct {
	listener net.Listener
	hostKey  ssh.Signer

	// hook handles the exec requests that are not the preflight's. The
	// probe and the syntax check are answered by the fixture itself
	// (answerPreflight) so that a test about the RUN does not have to
	// reimplement a shell.
	hook func(t *testing.T, s *fakeSession)

	mu      sync.Mutex
	conns   []ssh.Conn
	handled []string
}

// commands is every command string the server was asked to run, which is
// how a test says "the hook never started" rather than inferring it from an
// error.
func (f *fakeSSHD) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.handled...)
}

// dropConnections kills every live connection, which is what a network that
// goes away looks like from the client's side.
func (f *fakeSSHD) dropConnections() {
	f.mu.Lock()
	conns := append([]ssh.Conn(nil), f.conns...)
	f.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

func startFakeSSHD(t *testing.T, hook func(t *testing.T, s *fakeSession)) *fakeSSHD {
	t.Helper()

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating the fixture host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("building the fixture host signer: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	server := &fakeSSHD{listener: listener, hostKey: signer, hook: hook}
	t.Cleanup(func() {
		_ = listener.Close()
		server.dropConnections()
	})

	go server.serve(t)

	return server
}

func (f *fakeSSHD) serve(t *testing.T) {
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(f.hostKey)

	for {
		tcp, err := f.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			conn, chans, reqs, err := ssh.NewServerConn(tcp, config)
			if err != nil {
				_ = tcp.Close()

				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, conn)
			f.mu.Unlock()

			go ssh.DiscardRequests(reqs)
			for newChannel := range chans {
				if newChannel.ChannelType() != "session" {
					_ = newChannel.Reject(ssh.UnknownChannelType, "sessions only")

					continue
				}
				channel, requests, err := newChannel.Accept()
				if err != nil {
					return
				}
				go f.handleChannel(t, channel, requests)
			}
		}()
	}
}

func (f *fakeSSHD) handleChannel(t *testing.T, channel ssh.Channel, requests <-chan *ssh.Request) {
	for req := range requests {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)

			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)

			continue
		}
		_ = req.Reply(true, nil)

		f.mu.Lock()
		f.handled = append(f.handled, payload.Command)
		f.mu.Unlock()

		f.hook(t, &fakeSession{command: payload.Command, channel: channel})
	}
}

// answerInternalSessions plays the part of a bash well enough for this
// package's own sessions -- the capability probe, the syntax check, the
// reaper and the process-group scan -- and hands the STEP's own channel to
// the test's handler.
//
// It dispatches on the command rather than on the payload, because the
// step's channel is the one a test may need to leave unread: reading it
// would unblock the very write a stalled-connection test is about. The
// command is what tells them apart, and it does so for the same reason the
// reaper works at all -- the step's token is on it and nothing else's is.
func answerInternalSessions(hook func(t *testing.T, s *fakeSession)) func(t *testing.T, s *fakeSession) {
	return func(t *testing.T, s *fakeSession) {
		t.Helper()
		switch {
		case isSyntaxCheck(s.command):
			s.ReadAll(t)
			s.Exit(0)
		case !strings.HasSuffix(s.command, "-s"):
			hook(t, s)
		default:
			if containsProbe(s.ReadAll(t)) {
				s.Print(t, probeAnswer)
				s.Exit(0)

				return
			}
			// The reaper and the group scan: nothing of the step was
			// found, which is all this fixture has to say about a
			// process table it does not have.
			s.Print(t, "groups=\nsurvivors=\n")
			s.Exit(0)
		}
	}
}

// probeAnswer is what an exec-capable host's probe prints.
const probeAnswer = probeMarker + "\nbash_version=5.2.15(1)-release\nuser=hookuser\ntty=no\nbash_env=\nenv=\nshellopts=braceexpand:hashall\nbashopts=checkwinsize\nenumerates_environment=yes\n"

func isSyntaxCheck(command string) bool { return strings.HasSuffix(command, "-n -s") }

func containsProbe(payload string) bool { return strings.Contains(payload, probeMarker) }

// connectTo dials the fixture server the way production does: the same
// Dial, the same host-key policy, the same credential resolution. The key
// travels in an environment variable so the test does not have to reproduce
// the transfer path's file-custody rules to say something about exec.
func connectTo(t *testing.T, server *fakeSSHD) *Client {
	t.Helper()

	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating the fixture client key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(clientKey, "")
	if err != nil {
		t.Fatalf("marshalling the fixture client key: %v", err)
	}
	t.Setenv("BACKUPD_TEST_EXEC_KEY", string(pem.EncodeToMemory(block)))

	addr := server.listener.Addr().(*net.TCPAddr)
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))}, server.hostKey.PublicKey())
	if err := os.WriteFile(knownHosts, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("writing known_hosts: %v", err)
	}

	client, err := Dial(t.Context(), Connection{
		Ref:  "fixture",
		Kind: KindDeclared,
		Source: transport.Source{
			ID:         "fixture",
			Type:       "sftp",
			Host:       "127.0.0.1",
			Port:       addr.Port,
			User:       "hookuser",
			KeyEnv:     "BACKUPD_TEST_EXEC_KEY",
			KnownHosts: knownHosts,
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	return client
}

type collectingSink struct {
	mu   sync.Mutex
	text string
}

func (s *collectingSink) Chunk(c workflowexec.Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text += string(c.Data)

	return nil
}

// TestARunOnAForcedCommandAccountIsRefusedRatherThanReportedAsSuccess is
// the silent success ADR 0021 forbids, asserted against a server that
// behaves exactly as a forced-command account does: it accepts every exec
// request, runs its own program, and exits 0.
//
// The assertion that matters is not only the error. It is that the hook's
// own command -- the one carrying the step token -- was never asked for at
// all, which is the difference between "the hook failed" and "the hook
// never ran and nobody said so".
func TestARunOnAForcedCommandAccountIsRefusedRatherThanReportedAsSuccess(t *testing.T) {
	forced := func(t *testing.T, s *fakeSession) {
		t.Helper()
		s.ReadAll(t)
		s.Print(t, "forced-command-only: this account runs its own program\n")
		s.Exit(0)
	}
	server := startFakeSSHD(t, forced)
	client := connectTo(t, server)

	sink := &collectingSink{}
	res, err := client.Run(t.Context(), Request{
		Token:   "backupd-exec-forced",
		Script:  []byte("printf 'the hook ran\\n'\n"),
		Sink:    sink,
		Timeout: 30 * time.Second,
	})

	if err == nil {
		t.Fatal("a forced-command account ran a step successfully; the hook was reported as having run while the account's own program ran instead")
	}
	if !errors.Is(err, ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability, so a caller cannot tell it from a broken connection: %v", err)
	}
	if res.ExitCode != nil {
		t.Errorf("a refused step reported exit code %d, which is the forced command's status and not the hook's", *res.ExitCode)
	}
	for _, command := range server.commands() {
		if strings.Contains(command, "backupd-exec-forced") {
			t.Errorf("the hook's own command was started on a connection that had not been proven exec-capable: %q", command)
		}
	}
}

// TestARunWithACapabilityForOtherBytesIsRefused is the other half of the
// gate. A proof is about a specific script on a specific connection: the
// syntax check that is part of it was run against those bytes, so honouring
// it for different ones would run a script nothing has parsed on that host.
func TestARunWithACapabilityForOtherBytesIsRefused(t *testing.T) {
	ranHook := false
	hook := func(t *testing.T, s *fakeSession) {
		t.Helper()
		ranHook = true
		s.ReadAll(t)
		s.Exit(0)
	}
	server := startFakeSSHD(t, answerInternalSessions(hook))
	client := connectTo(t, server)

	capability, err := client.Preflight(t.Context(), []byte("printf 'the proven script\\n'\n"))
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	sink := &collectingSink{}
	_, err = client.Run(t.Context(), Request{
		Token:      "backupd-exec-swapped",
		Script:     []byte("printf 'a different script entirely\\n'\n"),
		Capability: capability,
		Sink:       sink,
		Timeout:    30 * time.Second,
	})

	if err == nil {
		t.Fatal("a capability proved for one script was accepted for another")
	}
	if !errors.Is(err, ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability: %v", err)
	}
	if ranHook {
		t.Error("the hook ran on the strength of a proof taken against different bytes")
	}
}

// TestACapabilityFromAnotherConnectionIsRefused is the same rule for the
// other half of the binding. A capability that outlived its connection
// proves something about a connection that no longer exists, and the moment
// a connection is remade is exactly the moment an account's capability can
// have changed.
func TestACapabilityFromAnotherConnectionIsRefused(t *testing.T) {
	hook := func(t *testing.T, s *fakeSession) {
		t.Helper()
		s.ReadAll(t)
		s.Exit(0)
	}
	server := startFakeSSHD(t, answerInternalSessions(hook))
	client := connectTo(t, server)

	script := []byte("printf 'ok\\n'\n")
	capability, err := client.Preflight(t.Context(), script)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	// The same bytes, proven on a connection this client is not.
	other := connectTo(t, startFakeSSHD(t, answerInternalSessions(hook)))
	if err := other.honours(capability, script); err == nil {
		t.Fatal("a capability established on one connection was honoured on another")
	} else if !errors.Is(err, ErrExecCapability) {
		t.Errorf("the refusal is not an ErrExecCapability: %v", err)
	}

	// And nothing at all is never enough.
	if err := client.honours(nil, script); err == nil {
		t.Fatal("a step with no capability at all was allowed to run")
	}
}

// TestAStalledConnectionAbortsWithinTheStepsBound is the hang this package
// must not have. The server accepts the channel and then reads nothing and
// says nothing -- the shape of a peer whose machine is gone but whose TCP
// connection has not failed yet -- so the payload write blocks on the
// channel window with no deadline of its own.
//
// The step's own bound has to reach that, which means it has to start
// before the write rather than after it.
func TestAStalledConnectionAbortsWithinTheStepsBound(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })

	// The step's own channel is accepted and then neither read nor
	// answered. Everything else this package opens -- the probe, the
	// syntax check, the reaper -- is answered normally, so what the test
	// measures is the step's bound and not a server that went silent.
	hook := func(t *testing.T, s *fakeSession) {
		t.Helper()
		<-stall
	}
	server := startFakeSSHD(t, answerInternalSessions(hook))
	client := connectTo(t, server)

	// Big enough that the channel window cannot swallow it, which is what
	// makes the write block rather than complete into a buffer.
	script := make([]byte, 4<<20)
	for i := range script {
		script[i] = 'x'
	}
	script = append([]byte("# "), script...)
	script = append(script, '\n')

	sink := &collectingSink{}
	start := time.Now()
	res, err := client.Run(t.Context(), Request{
		Token:   "backupd-exec-stalled",
		Script:  script,
		Sink:    sink,
		Timeout: 2 * time.Second,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a step against a peer that reads nothing and answers nothing was reported as having finished")
	}
	if !errors.Is(err, ErrStepTimeout) {
		t.Errorf("error %v is not an ErrStepTimeout", err)
	}
	if res.ExitCode != nil {
		t.Errorf("a stalled step produced exit code %d", *res.ExitCode)
	}
	// The bound is 2s; termination adds its own reap and signal graces,
	// neither of which the stalled channel can satisfy.
	if limit := 2*time.Second + reapGrace + signalGrace + 5*time.Second; elapsed > limit {
		t.Errorf("the step took %s to give up on a stalled connection, want no more than %s: the bound has to cover the payload write, not start after it", elapsed, limit)
	}
}

// TestASyntaxCheckThatLosesTheConnectionIsNotASyntaxRefusal is the
// difference between "your script is broken" and "the link is broken". The
// first sends an operator to look at a script that is fine; the second is
// the retryable infrastructure failure it actually was.
func TestASyntaxCheckThatLosesTheConnectionIsNotASyntaxRefusal(t *testing.T) {
	var server *fakeSSHD
	dropDuringCheck := func(t *testing.T, s *fakeSession) {
		t.Helper()
		if isSyntaxCheck(s.command) {
			go server.dropConnections()

			return
		}
		payload := s.ReadAll(t)
		if containsProbe(payload) {
			s.Print(t, probeMarker+"\nbash_version=5.2.15(1)-release\nuser=hookuser\ntty=no\nenumerates_environment=yes\n")
			s.Exit(0)

			return
		}
		s.Exit(0)
	}
	server = startFakeSSHD(t, dropDuringCheck)
	client := connectTo(t, server)

	_, err := client.Preflight(t.Context(), []byte("printf 'a perfectly good script\\n'\n"))
	if err == nil {
		t.Fatal("a preflight whose connection died mid-check reported success")
	}
	if errors.Is(err, ErrExecCapability) {
		t.Errorf("a lost connection was reported as a capability refusal, which tells an operator to go and fix a script that parses: %v", err)
	}
	if !errors.Is(err, ErrTransportLoss) {
		t.Errorf("error %v is not an ErrTransportLoss", err)
	}
}

// TestASyntaxErrorIsRefusedWithTheTargetsOwnWords is the control for the
// test above: a genuine non-zero from bash -n still has to be a capability
// refusal, or the distinction above would have been bought by never
// refusing anything.
func TestASyntaxErrorIsRefusedWithTheTargetsOwnWords(t *testing.T) {
	refuse := func(t *testing.T, s *fakeSession) {
		t.Helper()
		if isSyntaxCheck(s.command) {
			s.ReadAll(t)
			_, _ = io.WriteString(s.channel.Stderr(), "bash: line 2: syntax error: unexpected end of file\n")
			s.Exit(2)

			return
		}
		payload := s.ReadAll(t)
		if containsProbe(payload) {
			s.Print(t, probeMarker+"\nbash_version=5.2.15(1)-release\nuser=hookuser\ntty=no\nenumerates_environment=yes\n")
		}
		s.Exit(0)
	}
	server := startFakeSSHD(t, refuse)
	client := connectTo(t, server)

	_, err := client.Preflight(t.Context(), []byte("if true; then\n"))
	if err == nil {
		t.Fatal("a script the target's own bash refused to parse passed the preflight")
	}
	if !errors.Is(err, ErrExecCapability) {
		t.Errorf("error %v is not an ErrExecCapability", err)
	}
	if !strings.Contains(err.Error(), "unexpected end of file") {
		t.Errorf("the refusal does not carry what that host's bash actually said: %v", err)
	}
}

// TestAHostThatCannotEnumerateItsEnvironmentIsRefused covers the one thing
// the envelope's environment clearing depends on. A bash that cannot list
// its own exported variables would run the hook in the account's
// environment while the payload looked like it had cleared it.
func TestAHostThatCannotEnumerateItsEnvironmentIsRefused(t *testing.T) {
	noCompgen := func(t *testing.T, s *fakeSession) {
		t.Helper()
		if isSyntaxCheck(s.command) {
			s.ReadAll(t)
			s.Exit(0)

			return
		}
		payload := s.ReadAll(t)
		if containsProbe(payload) {
			s.Print(t, probeMarker+"\nbash_version=5.2.15(1)-release\nuser=hookuser\ntty=no\nenumerates_environment=no\n")
		}
		s.Exit(0)
	}
	server := startFakeSSHD(t, noCompgen)
	client := connectTo(t, server)

	_, err := client.Preflight(t.Context(), []byte("printf 'ok\\n'\n"))
	if err == nil {
		t.Fatal("a shell that cannot enumerate its exported variables passed the preflight")
	}
	if !errors.Is(err, ErrExecCapability) {
		t.Errorf("error %v is not an ErrExecCapability: %v", err, err)
	}
}

// oversizedScript is a script too big for the exec channel's window, which
// is what makes the payload write to a server that is not reading it still
// be in flight when the step ends. A script that fitted would be written
// into the window and reported as delivered, and the race these tests are
// about would never happen.
func oversizedScript() []byte {
	script := make([]byte, 0, 4<<20+4)
	script = append(script, "# "...)
	for len(script) < 4<<20 {
		script = append(script, 'x')
	}

	return append(script, '\n')
}

// TestAHookThatEndsUnderItsOwnPayloadWriteReportsItsExitStatus is #915: a
// remote hook that ran, and said how it ended, reported as an outcome
// nobody observed.
//
// A hook stops reading its payload the moment it exits -- an early
// "exit 0", a script shorter than the stdin behind it, a hook killed on the
// far side -- so the remote closes the channel's stdin while this side is
// still writing and the write fails ON THE HOOK'S OWN ENDING. The server
// here does exactly that: it reads nothing and exits, leaving a four
// mebibyte payload parked on a two mebibyte window.
//
// The exit status is the step's outcome. Returning the write's failure
// instead made every such step transport_lost, which is a hook whose side
// effects may be half applied and whose exit code is nil -- reported for a
// hook that in fact ran to completion and returned a status.
func TestAHookThatEndsUnderItsOwnPayloadWriteReportsItsExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		code uint32
	}{
		{name: "a hook that succeeded", code: 0},
		{name: "a hook that failed", code: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The step's own channel: not one byte of the payload is
			// read before the status is sent and the channel closed.
			exitAtOnce := func(t *testing.T, s *fakeSession) {
				t.Helper()
				s.Exit(tc.code)
			}
			server := startFakeSSHD(t, answerInternalSessions(exitAtOnce))
			client := connectTo(t, server)

			sink := &collectingSink{}
			res, err := client.Run(t.Context(), Request{
				Token:   "backupd-exec-earlyexit",
				Script:  oversizedScript(),
				Sink:    sink,
				Timeout: 30 * time.Second,
			})

			if err != nil {
				t.Fatalf("a hook that exited %d was reported as a failure: %v", tc.code, err)
			}
			if res.ExitCode == nil {
				t.Fatalf("a hook that exited %d left no exit code, which is how this product spells \"nobody saw a status\"", tc.code)
			}
			if *res.ExitCode != int(tc.code) {
				t.Errorf("exit code %d, want %d", *res.ExitCode, tc.code)
			}
		})
	}
}

// TestAnExecChannelThatClosesWithoutAStatusIsStillTransportLoss is the
// control for the test above, and the thing it must not have cost. The
// write fails in exactly the same way here -- same unread oversized payload
// -- but the channel closes with NO exit status, so nobody observed how the
// hook ended, or whether it ran at all.
//
// That is transport loss, and it stays transport loss with a nil exit code:
// the difference between the two tests is the status, which is the only
// thing that may decide it.
func TestAnExecChannelThatClosesWithoutAStatusIsStillTransportLoss(t *testing.T) {
	vanish := func(t *testing.T, s *fakeSession) {
		t.Helper()
		_ = s.channel.Close()
	}
	server := startFakeSSHD(t, answerInternalSessions(vanish))
	client := connectTo(t, server)

	sink := &collectingSink{}
	res, err := client.Run(t.Context(), Request{
		Token:   "backupd-exec-nostatus",
		Script:  oversizedScript(),
		Sink:    sink,
		Timeout: 30 * time.Second,
	})

	if err == nil {
		t.Fatal("a channel that closed without an exit status was reported as a step that finished")
	}
	if !errors.Is(err, ErrTransportLoss) {
		t.Errorf("error %v is not an ErrTransportLoss", err)
	}
	if res.ExitCode != nil {
		t.Errorf("a step whose channel reported no status produced exit code %d", *res.ExitCode)
	}
}

func init() {
	// A guard against this file's fixture drifting from the command the
	// client actually sends: isSyntaxCheck reads the tail of the command
	// string, so a change to the syntax-check invocation has to be
	// reflected here or every test above would silently take the hook
	// path.
	if !isSyntaxCheck("exec /bin/bash --noprofile --norc -n -s") {
		panic("the fixture no longer recognises the syntax-check command")
	}
}
