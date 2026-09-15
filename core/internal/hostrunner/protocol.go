package hostrunner

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// The wire, and the two properties it is built around: nothing on it can
// name a path, and nothing on it can be silently ignored.
//
// # Framing
//
// A four-byte big-endian length, then that many bytes of JSON. Not
// newline-delimited JSON, because a hook's output is arbitrary bytes and
// a framing that has to escape them is a framing with an escaping bug in
// its future; not a streaming JSON decoder over the raw connection,
// because a malformed or hostile frame would then desynchronise the
// stream rather than fail one message. A length prefix bounds every read
// before it happens, which is the property that matters on a socket a
// local process can reach.
//
// The bound is MaxFrameSize, and it is checked BEFORE the allocation,
// which is the whole reason the prefix is read separately.
//
// # Strictness
//
// Every frame is decoded with DisallowUnknownFields. That single call is
// what makes "this protocol cannot express a path" true rather than
// aspirational: an engine (or anything else that reached the socket)
// sending {"op":"execute","script_path":"/etc/shadow"} is REFUSED as a
// malformed frame instead of having the field quietly dropped and
// executing whatever the rest of the request said. A protocol that
// ignores what it does not understand is a protocol whose security
// properties are "whatever the current version happens to read".
//
// # Vocabulary
//
// Four operations, and they are the whole surface: syntax-check, execute,
// cancel, status. There is no "run this command", no "read this file",
// no "list that directory". Each one exists because a specific thing
// upstream cannot be done without it, and a fifth needs the same
// argument.

// Protocol is the wire protocol's own identity, separate from the product
// version.
//
// Two versions, deliberately. The product version is what pairs an engine
// with a runner (see Hello.Version) and moves on every release; this one
// moves only when the frames change. A runner reading a frame it cannot
// parse should say "this is not the protocol I speak" rather than
// "unmarshal error", and a future in which the two can legitimately
// differ -- a protocol frozen across several releases -- is the normal
// case rather than a hypothetical.
const Protocol = "backupd/workflow-runner/1"

// MaxFrameSize bounds one frame.
//
// internal/workflow's MaxConfigurableScriptSize is 16 MiB, and a script
// travels here base64-encoded inside JSON, which is a third larger again.
// 32 MiB clears that with room for the envelope and is still small enough
// that a frame is one allocation nobody notices.
const MaxFrameSize = 32 << 20

// MaxChunkSize bounds one output chunk, which is also the read buffer
// used on each of the child's pipes.
//
// 64 KiB: a pipe's own capacity on Linux, so a full buffer is one frame
// rather than a burst of tiny ones, and small enough that a hook printing
// steadily produces output the engine can stream rather than one frame at
// the end.
const MaxChunkSize = 64 << 10

// ErrProtocol is every refusal about the wire itself: a frame past the
// bound, a kind this build does not know, a field that does not exist.
var ErrProtocol = errors.New("hostrunner: this runner protocol frame cannot be read")

// Kind discriminates a frame. One envelope type with a kind, rather than
// a bare union, so that DisallowUnknownFields can run over the whole
// message including the discriminator.
type Kind string

const (
	// KindHello is the client's first frame: who it is and what version
	// it is. Nothing else may precede it.
	KindHello Kind = "hello"

	// KindWelcome is the server's answer to a hello it accepted.
	KindWelcome Kind = "welcome"

	// KindRequest is one operation.
	KindRequest Kind = "request"

	// KindChunk is one piece of a running step's output.
	KindChunk Kind = "chunk"

	// KindResult ends an operation that succeeded in being carried out,
	// whatever the hook itself did.
	KindResult Kind = "result"

	// KindFailure ends an operation that was not carried out, and says
	// why. A hook that exits 1 is a Result; a request this runner
	// refused is a Failure. Conflating the two would make "the hook
	// failed" and "we never ran the hook" the same answer, which is the
	// one distinction a workflow engine cannot do without.
	KindFailure Kind = "failure"
)

// Op is one operation's name.
type Op string

const (
	// OpSyntaxCheck parses captured bytes with `bash -n` and runs
	// nothing. It exists so `.local.sh` validation can fail BEFORE a
	// backup starts rather than in the middle of one.
	OpSyntaxCheck Op = "syntax-check"

	// OpExecute runs one step's captured bytes and streams its output.
	OpExecute Op = "execute"

	// OpCancel terminates a running step's process group from another
	// connection.
	OpCancel Op = "cancel"

	// OpStatus reports what this runner is and what it is doing. It is
	// the preflight/health answer: reachable, authenticated, bash found
	// at a fixed path, execution account known.
	OpStatus Op = "status"
)

// Code is a refusal's machine-readable reason. The engine branches on
// these; the sentence beside it is for the operator.
type Code string

const (
	// CodeUnauthorized is a connection that did not present the
	// installation's credential.
	CodeUnauthorized Code = "unauthorized"

	// CodeVersionMismatch is an engine and a runner from different
	// releases. See Hello.Version.
	CodeVersionMismatch Code = "version_mismatch"

	// CodeMalformed is a frame this runner could not read, including one
	// carrying a field that does not exist in this protocol.
	CodeMalformed Code = "malformed"

	// CodeScriptMismatch is captured bytes whose size or sha256 does not
	// match what the request claimed.
	CodeScriptMismatch Code = "script_mismatch"

	// CodeSyntax is `bash -n` refusing the captured bytes. It is its own
	// code rather than an execution failure because nothing ran: a
	// workflow can report "this hook does not parse" before a backup
	// starts only if the two are distinguishable.
	CodeSyntax Code = "syntax_error"

	// CodeBusy is a second execute for a run and step already running.
	CodeBusy Code = "busy"

	// CodeNotFound is a cancel for a step this runner is not running.
	CodeNotFound Code = "not_found"

	// CodeRefused is a well-formed request this runner will not carry
	// out: an id that cannot be a path component, a script past the
	// bound, an environment entry that cannot survive execve.
	CodeRefused Code = "refused"

	// CodeInternal is this runner failing at its own job: a directory it
	// could not create, a pipe it could not open.
	CodeInternal Code = "internal"

	// CodeContainerUnavailable is this host being unable to run a hook
	// in a container: no docker client, no reachable daemon, no hook
	// image, or a probe container that did not come back (#865).
	//
	// Its own code rather than CodeInternal, because it is the one
	// refusal an OPERATOR can act on and because the engine has to be
	// able to tell it from a hook that failed: "this deployment cannot
	// run local hooks until Docker is reachable" and "your script
	// exited 1" lead to completely different actions. It is also what
	// makes the absence of a fallback legible -- a runner that quietly
	// ran the hook on the host instead would report success, and
	// nothing anywhere would say the containment was not applied.
	CodeContainerUnavailable Code = "container_unavailable"
)

// State is what became of an execution.
type State string

const (
	// StateExited is a process that ran and whose exit status this
	// runner observed. ExitCode is set; it may be non-zero.
	StateExited State = "exited"

	// StateTimedOut is a process killed because it outlived the step's
	// timeout.
	StateTimedOut State = "timed_out"

	// StateCanceled is a process killed because the engine asked.
	StateCanceled State = "canceled"

	// StateLeaseExpired is a process killed because the engine went
	// away. See Server's lease.
	StateLeaseExpired State = "lease_expired"
)

// TerminationCertainty records whether this runner PROVED that what it
// killed is gone.
//
// internal/workflow's Step.TerminationConfirmed is the field this feeds,
// and its doc says why the distinction is worth a wire field: a hook that
// is still running after the product stopped waiting for it is the worst
// case a workflow has, because the backup proceeds while something is
// still touching what the hook was quiescing.
type TerminationCertainty string

const (
	// CertaintyNotApplicable is a process that exited on its own.
	CertaintyNotApplicable TerminationCertainty = ""

	// CertaintyConfirmed means the process group was signalled and then
	// observed to be gone.
	CertaintyConfirmed TerminationCertainty = "confirmed"

	// CertaintyUnconfirmed means the signal was sent and something in
	// the group was still there when this runner stopped looking. The
	// working directory is deliberately NOT removed in this case.
	CertaintyUnconfirmed TerminationCertainty = "unconfirmed"
)

// Hello is the client's first frame.
type Hello struct {
	// Protocol is the wire protocol identity. A mismatch is refused
	// before anything else is read.
	Protocol string `json:"protocol"`

	// Version is the ENGINE's product version, and it must equal the
	// runner's exactly.
	//
	// Exactly, not "compatible with". A compatibility range is a promise
	// that every future change to the execution envelope, the
	// environment model or the timeout semantics will be negotiated
	// across it, and nothing in this product is in a position to make
	// that promise: the two halves are one program, shipped together,
	// upgraded together by the installer. An exact match turns an
	// unpaired upgrade into an immediate refusal with both versions in
	// it, which is a five-minute fix; a range turns it into a hook
	// behaving subtly differently, which is not.
	Version string `json:"version"`

	// Token is the installation-scoped credential from backupd's
	// secrets area.
	Token string `json:"token"`
}

// Welcome is the server's answer to an accepted hello. It carries the
// same facts `status` does, so a client that connected has already
// learned whether bash is present without a second round trip.
type Welcome struct {
	Protocol string `json:"protocol"`
	Version  string `json:"version"`
	Runner   Status `json:"runner"`
}

// Status is what this runner is: the preflight answer.
type Status struct {
	// Version is the runner's product version.
	Version string `json:"version"`

	// BashPath is the absolute path of the interpreter INSIDE the hook
	// image, and BashVersion is what it reported for itself when the
	// capability probe ran it. Both are observable by an operator
	// precisely so that "which bash ran my hook" is answerable -- and
	// since #865 the honest answer is about the image rather than about
	// this host, because no hook runs on the host's own shell.
	BashPath    string `json:"bash_path"`
	BashVersion string `json:"bash_version"`

	// The container facts (#865): which client and daemon this runner
	// proved at startup, which image a hook runs in and what id that
	// tag resolved to, which network it joins, which uid:gid it runs as
	// inside, and every host path an operator has allowed a hook to
	// see.
	//
	// HookMounts is rendered rather than structured because it is read
	// by a person: "which directories can my hook touch" is the
	// question an operator asks before writing one, and it is otherwise
	// answerable only by reading a unit file.
	DockerPath          string   `json:"docker_path,omitempty"`
	DockerServerVersion string   `json:"docker_server_version,omitempty"`
	HookImage           string   `json:"hook_image,omitempty"`
	HookImageID         string   `json:"hook_image_id,omitempty"`
	HookNetwork         string   `json:"hook_network,omitempty"`
	HookUser            string   `json:"hook_user,omitempty"`
	HookMounts          []string `json:"hook_mounts,omitempty"`

	// User and UID are the account hooks run as. An operator writing a
	// hook needs to know this before they write it, not after it fails
	// on a permission.
	User string `json:"user"`
	UID  int    `json:"uid"`

	// SocketPath is where this runner is listening; RuntimeDir is the
	// directory holding that socket, and the only one the engine's
	// container has a mount for; WorkspaceDir is where the per-step
	// working directories are, deliberately outside it.
	SocketPath   string `json:"socket_path"`
	RuntimeDir   string `json:"runtime_dir"`
	WorkspaceDir string `json:"workspace_dir"`

	// Active names the steps running right now, as "<run id>/<step id>".
	Active []string `json:"active,omitempty"`
}

// Request is one operation.
//
// There is no path field, and there never will be: see this file's
// preamble and the package doc. The fields below are the complete set,
// and DisallowUnknownFields is what holds that sentence up.
type Request struct {
	// Op is which operation this is.
	Op Op `json:"op"`

	// RunID and StepID identify the step. Both must be path components
	// by ValidID's rule, because both become directory names.
	RunID  string `json:"run_id,omitempty"`
	StepID string `json:"step_id,omitempty"`

	// Script is the captured bytes themselves, base64 in JSON. This is
	// the ONLY way a script reaches this process.
	Script []byte `json:"script,omitempty"`

	// ScriptSize and ScriptSHA256 are what the engine's plan recorded.
	// They are re-checked here rather than trusted, so that a request
	// whose bytes were altered in flight, truncated by a framing bug, or
	// assembled from the wrong step is refused rather than run.
	ScriptSize   int64  `json:"script_size,omitempty"`
	ScriptSHA256 string `json:"script_sha256,omitempty"`

	// Env is the hook's environment, already merged by
	// internal/workflow. RETND_WORK_DIR in it is ignored: only this
	// process knows the working directory it is about to create, so only
	// this process may state it. See Executor.Execute.
	Env EnvSet `json:"env,omitzero"`

	// TimeoutMS is the step's bound, resolved at snapshot time so that a
	// configuration edit mid-run cannot change what a running step is
	// held to. Zero takes DefaultStepTimeout.
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// Timeout is TimeoutMS as a duration.
func (r Request) Timeout() time.Duration { return time.Duration(r.TimeoutMS) * time.Millisecond }

// Result ends an operation that was carried out.
type Result struct {
	// State is what became of the process.
	State State `json:"state"`

	// ExitCode is nil unless a process exited and this runner observed
	// the status. A killed step leaves it nil, and nil is emphatically
	// not zero: internal/workflow's Step.ExitCode carries the same
	// distinction into the journal.
	ExitCode *int `json:"exit_code,omitempty"`

	// TerminationCertainty is set only when this runner killed
	// something.
	TerminationCertainty TerminationCertainty `json:"termination_certainty,omitempty"`

	// DurationMS is how long the process ran.
	DurationMS int64 `json:"duration_ms,omitempty"`

	// WorkDirRemoved says whether the per-step working directory was
	// cleaned up. It is false when termination could not be confirmed,
	// because removing a directory something may still be writing into
	// is how a "cleanup" turns into a corrupted dump nobody can explain.
	WorkDirRemoved bool `json:"work_dir_removed"`

	// DroppedEnvNames lists cursed variables this runner deleted from
	// the hook's environment. Empty in every ordinary run.
	DroppedEnvNames []string `json:"dropped_env_names,omitempty"`

	// Chunks is how many output chunks were streamed, so a consumer can
	// tell a truncated stream from a silent hook.
	Chunks uint64 `json:"chunks,omitempty"`

	// Status answers OpStatus.
	Status *Status `json:"status,omitempty"`
}

// Failure ends an operation that was refused or could not be attempted.
type Failure struct {
	Code Code `json:"code"`

	// Message is the operator-facing sentence. It never contains an
	// environment value: this type is what a refusal gets logged as, and
	// a refusal that quoted the environment would put a repository
	// passphrase in a log file.
	Message string `json:"message"`
}

// Error makes Failure usable as an error on the client side.
func (f *Failure) Error() string { return string(f.Code) + ": " + f.Message }

// IsCode reports whether err is a Failure carrying code.
func IsCode(err error, code Code) bool {
	var f *Failure
	return errors.As(err, &f) && f.Code == code
}

// Message is the one envelope every frame travels in.
type Message struct {
	Kind    Kind     `json:"kind"`
	Hello   *Hello   `json:"hello,omitempty"`
	Welcome *Welcome `json:"welcome,omitempty"`
	Request *Request `json:"request,omitempty"`
	Chunk   *Chunk   `json:"chunk,omitempty"`
	Result  *Result  `json:"result,omitempty"`
	Failure *Failure `json:"failure,omitempty"`
}

// WriteMessage frames and writes one message.
func WriteMessage(w io.Writer, m Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("hostrunner: this runner cannot encode a %s frame: %w", m.Kind, err)
	}
	if len(body) > MaxFrameSize {
		return fmt.Errorf("%w: a %s frame of %d bytes is past the %d-byte bound", ErrProtocol, m.Kind, len(body), MaxFrameSize)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadMessage reads one framed message.
//
// The length is read and CHECKED before the body is allocated, so a frame
// claiming four gigabytes is a refusal rather than an allocation. The
// decode runs with DisallowUnknownFields; see this file's preamble for
// why that is a security property here rather than a strictness
// preference.
func ReadMessage(r io.Reader) (Message, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Message{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return Message{}, fmt.Errorf("%w: an empty frame", ErrProtocol)
	}
	if size > MaxFrameSize {
		return Message{}, fmt.Errorf("%w: a frame of %d bytes is past the %d-byte bound", ErrProtocol, size, MaxFrameSize)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return Message{}, err
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var m Message
	if err := dec.Decode(&m); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	if dec.More() {
		return Message{}, fmt.Errorf("%w: a frame carrying more than one JSON value", ErrProtocol)
	}
	if err := m.checkArms(); err != nil {
		return Message{}, err
	}
	return m, nil
}

// checkArms holds a decoded frame to the invariant its Kind states:
// exactly one payload, and the one the Kind names.
//
// DisallowUnknownFields does not do this. Every arm below is a field
// this type really has, so a frame of kind "request" that also carries a
// "failure" decodes cleanly and the extra arm is silently dropped by
// whichever branch reads only what it expected. That is a frame meaning
// two things at once, accepted -- and the shape of every parser
// differential worth having: a sender that gets one half read here and
// the other half read somewhere else has found a way to say something
// this protocol has no word for.
func (m Message) checkArms() error {
	arms := []struct {
		kind    Kind
		present bool
	}{
		{KindHello, m.Hello != nil},
		{KindWelcome, m.Welcome != nil},
		{KindRequest, m.Request != nil},
		{KindChunk, m.Chunk != nil},
		{KindResult, m.Result != nil},
		{KindFailure, m.Failure != nil},
	}

	var carried []string
	matched := false
	for _, arm := range arms {
		if !arm.present {
			continue
		}
		carried = append(carried, string(arm.kind))
		if arm.kind == m.Kind {
			matched = true
		}
	}

	switch {
	case len(carried) == 0:
		return fmt.Errorf("%w: a %s frame carrying no payload at all", ErrProtocol, m.Kind)
	case len(carried) > 1:
		return fmt.Errorf("%w: a %s frame carrying %s. One frame is one thing, and a frame that is two is not a frame this runner will guess about", ErrProtocol, m.Kind, strings.Join(carried, " and "))
	case !matched:
		return fmt.Errorf("%w: a %s frame whose payload is a %s", ErrProtocol, m.Kind, carried[0])
	}
	return nil
}
