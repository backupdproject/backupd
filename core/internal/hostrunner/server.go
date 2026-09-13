package hostrunner

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The server: one socket, two locks on it, and a lease that outlives
// nothing.
//
// # Unix domain only
//
// There is no address field, no --listen flag, no TCP fallback and no
// code path in this package that calls net.Listen with anything but
// "unix". That is a decision rather than a default: a TCP listener would
// make the runner reachable from the network the moment somebody set it
// to 0.0.0.0 while debugging, and what is on the other end of this socket
// is arbitrary code execution as the service account. A socket also gives
// the kernel a say -- directory and file permissions -- which a port
// never can.
//
// # Two independent locks
//
// The socket is 0600 in a 0700 directory. And every connection must
// present the installation's credential. See the package doc for why one
// of those is not enough on the deployments this product ships to.
//
// # The lease
//
// A connection that is running a step owns that step. When the read side
// of that connection fails -- the engine crashed, its container was
// recreated, the machine it was on lost power -- the step's context is
// cancelled with errLeaseExpired, and Executor.Execute terminates the
// process group and removes the working directory.
//
// The alternative is not "the hook finishes on its own". It is a hook
// that has quiesced a database, an engine that is no longer there to run
// the matching `after` hook, and nothing anywhere that will ever notice.
// A lease is the only structure that turns an engine crash into a bounded
// event.

// ErrServer is a refusal about serving itself: running as root, a socket
// another runner already holds, a credential file that is not private.
var ErrServer = errors.New("hostrunner: this workflow runner cannot serve")

// ErrRunningAsRoot is the refusal that cannot be configured away.
//
// #809 states it as a requirement and the reasoning is worth restating
// where the check is: an operator installed this product as an
// administrator, which says nothing whatever about whether they meant
// every hook script in a directory to run as root. A runner that executed
// as root by default would turn "drop a file in /workflows" into "drop a
// file in /workflows and own the machine", and would do it for a file
// that arrived by rsync from somewhere else.
//
// There is deliberately NO override flag. An operator who genuinely needs
// a privileged action in a hook has a tool for it that is auditable,
// per-command, and already installed: sudoers. A flag here would be a
// single switch that silently re-privileges every hook at once, and every
// deployment that ever flips it stays flipped.
var ErrRunningAsRoot = errors.New("hostrunner: this workflow runner will not run as root")

// Config is everything a server needs.
type Config struct {
	// Layout is where the socket, the working directories and the
	// credential live.
	Layout Layout

	// Version is this build's product version. A client's hello must
	// match it exactly; see Hello.Version.
	Version string

	// Token is the installation-scoped credential, already read from
	// the secrets area (LoadToken).
	Token []byte

	// Bash is the interpreter fixed by preflight.
	Bash Bash

	// Grace is the SIGTERM-to-SIGKILL window. Zero takes
	// DefaultGracePeriod.
	Grace time.Duration

	// MaxScriptSize bounds one script.
	MaxScriptSize int64

	// EUID and Username describe the account this process runs as. They
	// are parameters rather than calls to os.Geteuid so that the refusal
	// to run as root is testable without a root test.
	EUID     int
	Username string
}

// Server answers the runner protocol on one Unix socket.
type Server struct {
	cfg      Config
	exec     *Executor
	listener net.Listener

	mu     sync.Mutex
	active map[string]context.CancelCauseFunc
}

// RefuseRoot is the unprivileged-by-default rule, as a function, so that
// it is testable and so the sentence lives in one place.
func RefuseRoot(euid int) error {
	if euid == 0 {
		return fmt.Errorf("%w: it is running as uid 0, and a hook script is an operator's file rather than an administrator's decision. Run it as the dedicated service account the installer creates, and give that account the specific privileged commands it needs through sudoers", ErrRunningAsRoot)
	}
	return nil
}

// LoadToken reads the installation-scoped credential and holds the file
// to its mode.
//
// The mode check is not decoration. A credential every account on the
// host can read is not a credential, and the failure is silent: the
// runner works, the engine works, and the second lock this design
// documents has quietly been open since whenever somebody ran a chmod.
func LoadToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: the installation credential %s cannot be read: %v. The installer writes it; a deployment that has never provisioned the workflow runner has none", ErrServer, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: the installation credential %s is a symbolic link, and this process will not follow one to a secret", ErrServer, path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: the installation credential %s is not a regular file", ErrServer, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: the installation credential %s is mode %#o, so an account other than this one can read it. It must be %#o", ErrServer, path, info.Mode().Perm(), TokenFileMode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: the installation credential %s cannot be read: %v", ErrServer, path, err)
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < MinTokenLength {
		return nil, fmt.Errorf("%w: the installation credential %s holds %d bytes, and anything under %d is not a credential", ErrServer, path, len(token), MinTokenLength)
	}
	return []byte(token), nil
}

// MinTokenLength is the shortest credential this runner will accept.
//
// 32 characters, which is what the installer's 32 hex bytes produce. It
// is a floor on what may be PROVISIONED rather than a strength estimate:
// the point is that a deployment cannot end up with a one-character token
// because a file got truncated, or an empty one because a here-document
// went wrong, and then authenticate happily against it.
const MinTokenLength = 32

// NewServer prepares a server, refusing everything that must be refused
// before a socket exists.
func NewServer(cfg Config) (*Server, error) {
	if err := cfg.Layout.Validate(); err != nil {
		return nil, err
	}
	if err := RefuseRoot(cfg.EUID); err != nil {
		return nil, err
	}
	if cfg.Version == "" {
		return nil, fmt.Errorf("%w: it was given no version to pair an engine against, and a runner that accepted every version would be the mismatch this refusal exists to prevent", ErrServer)
	}
	if len(cfg.Token) < MinTokenLength {
		return nil, fmt.Errorf("%w: it was given no installation credential", ErrServer)
	}
	if cfg.Bash.Path == "" {
		return nil, fmt.Errorf("%w: no bash was fixed by preflight, so there is nothing to run a hook with", ErrServer)
	}
	return &Server{
		cfg: cfg,
		exec: &Executor{
			Layout:        cfg.Layout,
			Bash:          cfg.Bash,
			Grace:         cfg.Grace,
			MaxScriptSize: cfg.MaxScriptSize,
		},
		active: map[string]context.CancelCauseFunc{},
	}, nil
}

// MaxSocketPathLength is the longest socket path this runner will try to
// bind.
//
// A Unix socket address is a fixed-size field in a struct the kernel
// copies, not a string: 104 bytes on Darwin, 108 on Linux, INCLUDING the
// terminating NUL. Exceed it and bind(2) answers EINVAL -- "invalid
// argument" -- which says nothing whatsoever about the actual problem and
// has cost more than one afternoon on a deployment whose prefix was a
// few directories deeper than the installer's default.
//
// 103 is the smaller platform's usable length, applied on both so the
// refusal does not depend on where the deployment happens to be running.
// It is checked HERE, with a sentence naming the length, rather than
// left to the kernel.
const MaxSocketPathLength = 103

// Listen creates the socket with the permissions this design depends on.
//
// The order matters and is the reason this is not three lines inline. The
// socket is created inside a directory that is ALREADY 0700, and is
// chmod'ed 0600 immediately afterwards; a socket created world-accessible
// in a world-accessible directory and narrowed a microsecond later is a
// microsecond during which anything on the host could connect.
//
// A stale socket file is removed, but only after this process has proved
// nothing is listening on it: removing a live runner's socket would leave
// that runner running with no name, holding process groups nobody can
// reach, while this one takes over the path.
func (s *Server) Listen() error {
	path := s.cfg.Layout.SocketPath()
	if len(path) > MaxSocketPathLength {
		return fmt.Errorf("%w: the socket path %s is %d bytes, and a Unix socket address is a fixed %d-byte field in the kernel. Install this deployment under a shorter prefix, or point the runtime directory somewhere shallower", ErrServer, path, len(path), MaxSocketPathLength+1)
	}
	if err := EnsureDir(s.cfg.Layout.RuntimeDir); err != nil {
		return err
	}
	if err := EnsureDir(s.cfg.Layout.WorkflowRoot()); err != nil {
		return err
	}

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%w: %s exists and is not a socket, so this runner will not remove it", ErrServer, path)
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			conn.Close()
			return fmt.Errorf("%w: another workflow runner is already listening on %s. One runner per installation: two would each own process groups the other cannot see", ErrServer, path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("%w: the stale socket %s cannot be removed: %v", ErrServer, path, err)
		}
	}

	// The umask decides the mode of a socket the kernel creates, and
	// this process does not control the umask it was started with. So
	// the mode is set explicitly afterwards rather than hoped for.
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("%w: %s cannot be listened on: %v", ErrServer, path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("%w: the socket %s cannot be made private: %v", ErrServer, path, err)
	}
	s.listener = listener
	return nil
}

// SocketPath is where this server is listening.
func (s *Server) SocketPath() string { return s.cfg.Layout.SocketPath() }

// Serve accepts connections until ctx is done or the listener is closed.
//
// Every connection is handled in its own goroutine and every one of them
// is cancelled when Serve returns, which is what makes shutting the
// runner down also terminate the hooks it owns rather than orphan them.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	var conns sync.WaitGroup
	defer conns.Wait()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("hostrunner: the workflow runner stopped accepting connections: %w", err)
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			defer conn.Close()
			s.handleConn(ctx, conn)
		}()
	}
}

// Close stops the listener and removes the socket.
func (s *Server) Close() error {
	if s.listener == nil {
		return nil
	}
	err := s.listener.Close()
	_ = os.Remove(s.cfg.Layout.SocketPath())
	return err
}

// handleConn authenticates one connection and answers one operation.
//
// ONE operation, not a loop, and that is the lease's doing: while a step
// is running, the connection is the thing being watched for the engine's
// disappearance, so it cannot also be a request channel. `cancel` arrives
// on its own connection and finds the step through the registry, which is
// also what lets an engine cancel a step whose own connection is wedged.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	if err := s.handshake(conn); err != nil {
		writeFailure(conn, err)
		return
	}

	msg, err := ReadMessage(conn)
	if err != nil {
		writeFailure(conn, &Failure{Code: CodeMalformed, Message: err.Error()})
		return
	}
	if msg.Kind != KindRequest || msg.Request == nil {
		writeFailure(conn, &Failure{Code: CodeMalformed, Message: fmt.Sprintf("expected a request frame after the handshake and got %q", msg.Kind)})
		return
	}

	req := *msg.Request
	switch req.Op {
	case OpStatus:
		status := s.status()
		writeResult(conn, Result{State: StateExited, Status: &status})
	case OpSyntaxCheck:
		s.handleSyntaxCheck(ctx, conn, req)
	case OpExecute:
		s.handleExecute(ctx, conn, req)
	case OpCancel:
		s.handleCancel(conn, req)
	default:
		writeFailure(conn, &Failure{Code: CodeMalformed, Message: fmt.Sprintf("%q is not an operation this runner has. It has syntax-check, execute, cancel and status, and nothing that takes a path", req.Op)})
	}
}

// handshake authenticates one connection.
//
// Order: protocol, then credential, then version. The credential before
// the version, deliberately -- an unauthenticated caller learns only that
// it is unauthenticated, and a legitimate engine of the wrong release
// still gets the specific sentence it needs, because it has the
// credential.
func (s *Server) handshake(conn net.Conn) *Failure {
	msg, err := ReadMessage(conn)
	if err != nil {
		return &Failure{Code: CodeMalformed, Message: err.Error()}
	}
	if msg.Kind != KindHello || msg.Hello == nil {
		return &Failure{Code: CodeMalformed, Message: fmt.Sprintf("the first frame on a connection must be a hello, and this was %q", msg.Kind)}
	}
	hello := *msg.Hello

	if hello.Protocol != Protocol {
		return &Failure{
			Code:    CodeVersionMismatch,
			Message: fmt.Sprintf("this runner speaks %s and the client speaks %q", Protocol, hello.Protocol),
		}
	}
	if subtle.ConstantTimeCompare([]byte(hello.Token), s.cfg.Token) != 1 {
		return &Failure{
			Code:    CodeUnauthorized,
			Message: "the client did not present this installation's workflow-runner credential",
		}
	}
	if hello.Version != s.cfg.Version {
		return &Failure{
			Code: CodeVersionMismatch,
			Message: fmt.Sprintf("the engine is version %s and this runner is version %s. They are one program in two processes and are upgraded as a pair; run the matching runner, or finish the upgrade that was interrupted",
				hello.Version, s.cfg.Version),
		}
	}

	status := s.status()
	return failureOf(WriteMessage(conn, Message{
		Kind:    KindWelcome,
		Welcome: &Welcome{Protocol: Protocol, Version: s.cfg.Version, Runner: status},
	}))
}

func (s *Server) handleSyntaxCheck(ctx context.Context, conn net.Conn, req Request) {
	if err := s.exec.Verify(req); err != nil {
		writeFailure(conn, err)
		return
	}
	if err := s.exec.Bash.SyntaxCheck(ctx, req.Script); err != nil {
		writeFailure(conn, err)
		return
	}
	writeResult(conn, Result{State: StateExited, ExitCode: intPtr(0)})
}

// handleExecute runs one step, streams its output, and holds the lease.
func (s *Server) handleExecute(ctx context.Context, conn net.Conn, req Request) {
	key, failure := s.claim(req)
	if failure != nil {
		writeFailure(conn, failure)
		return
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	s.register(key, cancel)
	defer s.release(key)
	defer cancel(nil)

	// The lease watcher. A read returning ANYTHING ends the lease: EOF
	// and a connection error are the engine going away, and unsolicited
	// bytes are a client that is not speaking this protocol, which is
	// not something to keep executing a hook for either.
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		var scratch [1]byte
		_, err := conn.Read(scratch[:])
		if err != nil {
			cancel(errLeaseExpired)
			return
		}
		cancel(fmt.Errorf("%w: the engine sent data on a connection that was streaming a hook's output", ErrProtocol))
	}()

	// One writer for the connection. The chunk sink and the final frame
	// are written from different goroutines' timelines, and two
	// interleaved WriteMessage calls would produce a frame that is
	// neither.
	var writes sync.Mutex
	sink := SinkFunc(func(c Chunk) error {
		writes.Lock()
		defer writes.Unlock()
		return WriteMessage(conn, Message{Kind: KindChunk, Chunk: &c})
	})

	result, err := s.exec.Execute(runCtx, req, sink)

	writes.Lock()
	defer writes.Unlock()
	if err != nil {
		writeFailure(conn, err)
		return
	}
	_ = WriteMessage(conn, Message{Kind: KindResult, Result: &result})
}

func (s *Server) handleCancel(conn net.Conn, req Request) {
	key, failure := stepKey(req)
	if failure != nil {
		writeFailure(conn, failure)
		return
	}
	s.mu.Lock()
	cancel, running := s.active[key]
	s.mu.Unlock()
	if !running {
		writeFailure(conn, &Failure{Code: CodeNotFound, Message: fmt.Sprintf("this runner is not running %s", key)})
		return
	}
	cancel(context.Canceled)
	writeResult(conn, Result{State: StateCanceled})
}

// claim reserves one run/step so a second execute for the same step is
// refused rather than run twice.
//
// Twice is not a theoretical concern: an engine that retried after a
// timeout it decided locally, or two engines pointed at one runner by a
// misconfigured deployment, would both produce it, and the visible
// symptom is a working directory whose script file already exists (see
// writeScript's O_EXCL). Refusing here makes the reason legible instead.
func (s *Server) claim(req Request) (string, *Failure) {
	key, failure := stepKey(req)
	if failure != nil {
		return "", failure
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, running := s.active[key]; running {
		return "", &Failure{Code: CodeBusy, Message: fmt.Sprintf("this runner is already running %s", key)}
	}
	return key, nil
}

func stepKey(req Request) (string, *Failure) {
	if err := ValidID("run id", req.RunID); err != nil {
		return "", &Failure{Code: CodeRefused, Message: err.Error()}
	}
	if err := ValidID("step id", req.StepID); err != nil {
		return "", &Failure{Code: CodeRefused, Message: err.Error()}
	}
	return req.RunID + "/" + req.StepID, nil
}

func (s *Server) register(key string, cancel context.CancelCauseFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[key] = cancel
}

func (s *Server) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, key)
}

// status is the preflight answer: everything an engine needs to decide
// whether a `.local.sh` hook can run, before a backup starts.
func (s *Server) status() Status {
	s.mu.Lock()
	active := make([]string, 0, len(s.active))
	for key := range s.active {
		active = append(active, key)
	}
	s.mu.Unlock()

	return Status{
		Version:     s.cfg.Version,
		BashPath:    s.cfg.Bash.Path,
		BashVersion: s.cfg.Bash.Version,
		User:        s.cfg.Username,
		UID:         s.cfg.EUID,
		SocketPath:  s.cfg.Layout.SocketPath(),
		RuntimeDir:  s.cfg.Layout.RuntimeDir,
		Active:      active,
	}
}

// CurrentUsername is the account this process runs as, for Config.
// A uid with no passwd entry is entirely normal in a container and on a
// NAS, so the uid itself is the fallback rather than an error.
func CurrentUsername(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid " + strconv.Itoa(uid)
}

func writeFailure(conn net.Conn, err error) {
	failure := failureFrom(err)
	_ = WriteMessage(conn, Message{Kind: KindFailure, Failure: failure})
}

func writeResult(conn net.Conn, result Result) {
	_ = WriteMessage(conn, Message{Kind: KindResult, Result: &result})
}

// failureFrom renders any error as a Failure, preserving the code when
// there is one.
//
// The default is CodeInternal rather than CodeRefused: an error that
// reached here without a code is this runner failing at its own job, and
// calling that a refusal would tell an operator to fix their
// configuration for a bug in this file.
func failureFrom(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	return &Failure{Code: CodeInternal, Message: err.Error()}
}

// failureOf turns a write error into a Failure, or nil.
func failureOf(err error) *Failure {
	if err == nil {
		return nil
	}
	return &Failure{Code: CodeInternal, Message: err.Error()}
}

func intPtr(v int) *int { return &v }
