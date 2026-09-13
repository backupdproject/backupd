package hostrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stand-in docker client, and an exact statement of what it can and
// cannot prove -- because a fake this central is either documented or
// dangerous.
//
// # Why there is one at all
//
// core/internal/testtier's rule is that nothing under core/internal may
// need a container daemon: `go test ./internal/...` runs on every commit,
// six container-backed tests living in unit packages is what #448 spent a
// release undoing, and a suite that goes red because another worktree is
// hammering one docker daemon is a suite people learn to ignore.
//
// This package's job, though, is now entirely about launching containers.
// So the split is: everything about what THIS RUNNER DOES is asserted
// here, through a client that records what it was asked and obeys the
// parts of docker's contract the runner depends on; and everything about
// what a real container IS gets asserted in core/tests/containerhooks,
// against a real daemon.
//
// # What it really implements, rather than pretends
//
//   - the LIFECYCLE the runner drives: `create` registers a container and
//     starts nothing, `start --attach` runs it and streams its two
//     streams, `inspect` reports the status and the exit code of the
//     container's own process, `rm` removes the record. That split is the
//     whole point of the fake now: a runner that collapsed back to
//     `docker run` could not tell a hook that exited 125 from a container
//     that was never created, and this client is what makes the
//     difference observable.
//   - `--env NAME` reads NAME out of the CLIENT's own environment at
//     CREATE time and passes nothing else through, which is docker's
//     actual contract and the property the whole "no value on a command
//     line" argument rests on. The values are stored NUL-terminated and
//     read back with `read -d ''`, so a value with a newline, a quote or
//     a `$(` in it reaches the target process exactly as docker would
//     deliver it.
//   - a container is a PROCESS GROUP. `set -m` makes the launched job a
//     group leader, and `kill` signals the negated group id, which is
//     the closest a shell gets to a cgroup: a child that ignores SIGTERM
//     still dies on the group's SIGKILL, exactly as it would in a real
//     container.
//   - there is no `--rm` on a hook launch, so a container's record
//     survives its process, which is what makes `inspect` able to answer
//     about an exit status at all -- and what makes a leftover a real
//     leftover.
//   - `ps --filter label=` answers from the records, so "what does the
//     daemon still have carrying this launch's label" is a real question
//     with a real answer, including for a container the runner never saw
//     being created.
//   - an UNKNOWN FLAG, an unknown filter or an unknown `--format` is exit
//     125, loudly. That is what makes this fake safe to rely on: a
//     hardening flag added to hookArgs and not taught to the fake fails
//     every test in the package instead of being silently ignored, which
//     is how a fake starts lying.
//
// # What it cannot prove, and where that is proven instead
//
// Isolation. Whether --read-only makes the rootfs read-only, whether
// --cap-drop drops anything, whether --network none really has no
// network, whether the docker socket is absent inside, whether the
// environment survives the daemon's JSON round trip. None of that is a
// fact about this repository, and a fake asserting it would be this
// repository agreeing with itself. core/tests/containerhooks asserts each
// one against a real daemon, from inside a real container.

// fakeDocker configures the stand-in client's behaviour. The zero value
// is a working docker with a working daemon and a present image.
type fakeDocker struct {
	// versionFails is stderr text for a `docker version` that exits 1,
	// for the unreachable-daemon and permission-denied refusals.
	versionFails string

	// imageMissing makes `image inspect` exit 1, for the absent-image
	// refusal.
	imageMissing bool

	// imagePlatform is what `image inspect` reports for the image's
	// os/arch. Empty agrees with the daemon; anything else is the
	// wrong-architecture image the preflight refuses.
	imagePlatform string

	// probeSays replaces what a probe container prints. Empty runs the
	// probe script for real under the host's bash, which is what makes
	// the ordinary preflight path exercised rather than stubbed.
	probeSays string

	// sticky keeps a container's record after `rm`, which is a daemon
	// that will not let go: the runner must report that termination as
	// UNCONFIRMED and must not remove the working directory.
	sticky bool

	// daemonGoneAfterStart makes every `ps` fail once a container has
	// been started, which is the case that must NOT be read as "the
	// container is gone": a question that could not be asked is not an
	// answer.
	daemonGoneAfterStart bool

	// createLatencySeconds makes `create` register the container after a
	// delay, from a writer detached from the client -- and makes the
	// client itself hang until it is killed.
	//
	// It is the daemon that accepted a creation and completed it after
	// the client asking for it was gone, which is what a cancel landing
	// inside a launch really looks like. A runner that armed its
	// termination against the name it minted, once, found nothing and
	// left that container behind.
	createLatencySeconds string

	// clientExitsEarly makes `start --attach` return 0 while the
	// container's process runs on, with the record left in `running`.
	//
	// A daemon restart, a dropped transport or an OOM-killed client: the
	// client says the step is over and the container says otherwise.
	// What the runner must not do is read the client's exit as the
	// hook's.
	clientExitsEarly bool

	// autoRemoveFails makes a `--rm` run keep its container's record,
	// which is what --rm not happening looks like: a promise about an
	// exit, unkept because the daemon was busy or the client was killed
	// between the creation and the start. The runner's own
	// reconciliation is what has to clean that up.
	autoRemoveFails bool

	// hangAfterStart makes every control call -- kill, ps, rm, inspect,
	// start -- block forever once a container has been started, which is
	// a wedged daemon. A runner with an unbounded call anywhere on the
	// termination path never returns at all.
	hangAfterStart bool
}

// fakeDockerState is where the stand-in keeps its records and its log.
func fakeDockerState(t *testing.T) string {
	t.Helper()
	// Short, for testLayout's reason: these paths end up in argv and in
	// the socket-length arithmetic of the tests that serve a runner.
	dir, err := os.MkdirTemp("/tmp", "bdfd")
	if err != nil {
		t.Fatalf("preparing the stand-in client's state directory: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// writeFakeDocker writes the stand-in client and returns its path.
func writeFakeDocker(t *testing.T, cfg fakeDocker) string {
	t.Helper()
	state := fakeDockerState(t)
	return writeFakeDockerIn(t, state, cfg)
}

// writeFakeDockerIn is writeFakeDocker with the state directory named, so
// a test can read the call log and the container records.
//
// The configuration is written BESIDE the client and sourced by it,
// rather than interpolated into it. A shell script assembled through
// Sprintf is a script in which every `%` in a parameter expansion has to
// be doubled, and one that is not is a fake that misbehaves in a way that
// looks like the runner misbehaving.
func writeFakeDockerIn(t *testing.T, state string, cfg fakeDocker) string {
	t.Helper()

	bash := hostBashForFake(t)
	config := strings.Join([]string{
		"STAND_IN_PLATFORM=" + shellSingleQuote(standInPlatform),
		"INSTANCE_LABEL=" + shellSingleQuote(LabelInstance),
		"HOOK_LABEL=" + shellSingleQuote(LabelHook),
		"VERSION_FAILS=" + shellSingleQuote(cfg.versionFails),
		"IMAGE_MISSING=" + boolWord(cfg.imageMissing),
		"IMAGE_PLATFORM=" + shellSingleQuote(firstNonEmptyString(cfg.imagePlatform, standInPlatform)),
		"PROBE_SAYS=" + shellSingleQuote(cfg.probeSays),
		"STICKY=" + boolWord(cfg.sticky),
		"DAEMON_GONE_AFTER_START=" + boolWord(cfg.daemonGoneAfterStart),
		"CREATE_LATENCY=" + shellSingleQuote(cfg.createLatencySeconds),
		"CLIENT_EXITS_EARLY=" + boolWord(cfg.clientExitsEarly),
		"AUTO_REMOVE_FAILS=" + boolWord(cfg.autoRemoveFails),
		"HANG_AFTER_START=" + boolWord(cfg.hangAfterStart),
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(state, "config.sh"), []byte(config), 0o600); err != nil {
		t.Fatalf("writing the stand-in client's configuration: %v", err)
	}

	path := filepath.Join(state, "docker")
	if err := os.WriteFile(path, []byte("#!"+bash+"\n"+fakeDockerScript), 0o700); err != nil {
		t.Fatalf("writing the stand-in docker client: %v", err)
	}
	return path
}

// dockerCalls is every subcommand the stand-in was asked for, in order,
// as `verb rest-of-argv` lines.
func dockerCalls(t *testing.T, state string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, "calls"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading the stand-in client's call log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// dockerCallsMatching is the subset of the call log whose verb-and-flags
// line contains every one of want.
func dockerCallsMatching(t *testing.T, state string, want ...string) []string {
	t.Helper()
	var matched []string
	for _, call := range dockerCalls(t, state) {
		all := true
		for _, w := range want {
			if !strings.Contains(call, w) {
				all = false
				break
			}
		}
		if all {
			matched = append(matched, call)
		}
	}
	return matched
}

// dockerCallsWithVerb is the subset of the call log for one subcommand,
// matched on the VERB rather than on a substring.
//
// The distinction is not pedantry: every launch vector contains `--rm` or
// a container name with a run id in it, so a substring search for "rm "
// matches the launch itself and a test asserting "the container was
// removed by name" would pass against a runner that never removed
// anything. That mutation was live once.
func dockerCallsWithVerb(t *testing.T, state, verb string, want ...string) []string {
	t.Helper()
	var matched []string
	for _, call := range dockerCalls(t, state) {
		if !strings.HasPrefix(call, verb+" ") {
			continue
		}
		all := true
		for _, w := range want {
			if !strings.Contains(call, w) {
				all = false
				break
			}
		}
		if all {
			matched = append(matched, call)
		}
	}
	return matched
}

// containerRecords lists the containers the stand-in daemon still knows
// about. A leftover is a record here, which is what "no leftover
// container" is asserted against.
func containerRecords(t *testing.T, state string) []string {
	t.Helper()
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatalf("reading the stand-in client's state: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if name, ok := strings.CutPrefix(entry.Name(), "c-"); ok {
			names = append(names, name)
		}
	}
	return names
}

// runningContainerRecords is containerRecords narrowed to the containers
// whose process the stand-in daemon has not seen exit.
//
// The distinction exists because a stopped container and a running one
// are different faults: the first is a leftover, the second is the
// RUNAWAY this package exists to prevent, and a test that could not tell
// them apart would pass on either.
func runningContainerRecords(t *testing.T, state string) []string {
	t.Helper()
	var running []string
	for _, name := range containerRecords(t, state) {
		status, err := os.ReadFile(filepath.Join(state, "c-"+name, "status"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(status)) == "running" {
			running = append(running, name)
		}
	}
	return running
}

// hostBashForFake finds a bash for the stand-in client to be written in
// and to exec.
//
// This is the one place this package still looks for a shell on the host,
// and it is a TEST fixture rather than an execution path: the runner
// itself has no code that resolves a host interpreter any more, which is
// the structural half of "no silent fallback to host bash". A machine
// with no bash cannot run this package's tests, which is a failure rather
// than a skip for the reason helpers_test.go's preamble gives.
func hostBashForFake(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash", "/opt/homebrew/bin/bash"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	t.Fatal("this host has no bash, so the stand-in docker client cannot run anything and none of this package's claims can be checked")
	return ""
}

func boolWord(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// shellSingleQuote renders a string as a single-quoted shell literal, for
// the configuration values that are text.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fakeDockerScript is the stand-in client. See this file's preamble for
// what it implements on purpose and what it deliberately does not.
//
// It takes its state directory from its OWN path, so the script is a
// constant and its configuration is a file beside it.
const fakeDockerScript = `# Written by core/internal/hostrunner's tests. Not a docker client.
set -u
STATE="$(cd "$(dirname "$0")" && pwd)"
. "$STATE/config.sh"

# Global client flags come before the verb, exactly as docker requires.
while [ $# -gt 0 ]; do
  case "$1" in
    --config|--host|--context) shift 2 ;;
    *) break ;;
  esac
done

verb="${1:-}"
shift || true
printf '%s %s\n' "$verb" "$*" >> "$STATE/calls"

# wedged is the daemon that has stopped answering. Every control call goes
# through it, so a runner with one unbounded call on the termination path
# hangs here forever instead of reporting an unconfirmed termination.
# The hang is an exec with its streams closed, deliberately: a sleeping
# CHILD would keep this client's stdout and stderr pipes open, so the
# runner's own bound would kill the client and then block in Wait until
# the child exited -- a hang in the harness that looks exactly like the
# hang the runner is being tested for.
wedged() {
  if [ "$HANG_AFTER_START" = yes ] && [ -f "$STATE/started" ]; then
    exec sleep 600 >/dev/null 2>&1
  fi
}

# resolve maps a target -- a name this runner minted, or the id this
# daemon printed for it -- onto the container's record.
resolve() {
  case "$1" in
    stand-in-*) printf '%s' "$STATE/c-${1#stand-in-}" ;;
    *) printf '%s' "$STATE/c-$1" ;;
  esac
}

# launch runs a created container's process, with the environment the
# create stored, and records what became of it.
LAUNCH_STATUS=0
launch() {
  local dir="$1" entrypoint workdir value name
  entrypoint="$(cat "$dir/entrypoint")"
  workdir="$(cat "$dir/workdir")"

  local -a envargs=()
  for f in "$dir"/env/*; do
    [ -e "$f" ] || continue
    name="$(basename "$f")"
    IFS= read -r -d '' value < "$f" || true
    envargs+=("$name=$value")
  done

  local -a cmdargs=()
  while IFS= read -r -d '' value; do cmdargs+=("$value"); done < "$dir/args"

  # Only a HOOK's start arms the after-start knobs below: the capability
  # probe and the syntax check are this runner's own containers, and a
  # fake that wedged its daemon during the preflight would be testing
  # that a runner cannot start rather than what it does when a step's
  # daemon goes away mid-hook.
  if [ "$verb" = start ]; then : > "$STATE/started"; fi
  printf 'running\n' > "$dir/status"
  if [ -n "$workdir" ]; then cd "$workdir" || exit 125; fi

  # set -m makes the job a process group leader, so a signal to the
  # negated pid reaches everything it started -- which is what a
  # container's cgroup does.
  set -m
  if [ "$CLIENT_EXITS_EARLY" = yes ] && [ "$verb" = start ]; then
    # A client that exited while its container ran on. The process keeps
    # the container's own streams rather than this client's, exactly as a
    # real one does, so the runner sees its pipes close when the client
    # goes and has to ask the daemon what the container is doing.
    env -i ${envargs[@]+"${envargs[@]}"} "$entrypoint" ${cmdargs[@]+"${cmdargs[@]}"} \
      > "$dir/stdout" 2> "$dir/stderr" &
    printf '%s\n' "$!" > "$dir/pid"
    exit 0
  fi
  env -i ${envargs[@]+"${envargs[@]}"} "$entrypoint" ${cmdargs[@]+"${cmdargs[@]}"} &
  local child=$!
  printf '%s\n' "$child" > "$dir/pid"
  wait "$child"
  LAUNCH_STATUS=$?
  printf '%s\n' "$LAUNCH_STATUS" > "$dir/exit"
  printf 'exited\n' > "$dir/status"
  # The daemon's own teardown: when a container's main process exits,
  # what is left in its cgroup goes with it. Emulated, because otherwise
  # this stand-in would be quietly WEAKER than docker -- a hook's orphaned
  # child would survive here and not in a deployment, and the runner
  # relies on the opposite.
  kill -s KILL -- "-$child" 2>/dev/null || true
}

case "$verb" in
  version)
    if [ -n "$VERSION_FAILS" ]; then printf '%s\n' "$VERSION_FAILS" >&2; exit 1; fi
    printf '27.0.0-stand-in %s\n' "$STAND_IN_PLATFORM"
    exit 0 ;;
  image)
    if [ "$IMAGE_MISSING" = yes ]; then printf 'Error: No such image\n' >&2; exit 1; fi
    printf 'sha256:feedfacefeedfacefeedfacefeedface %s\n' "$IMAGE_PLATFORM"
    exit 0 ;;
  ps)
    if [ "$DAEMON_GONE_AFTER_START" = yes ] && [ -f "$STATE/started" ]; then
      printf 'Cannot connect to the Docker daemon\n' >&2
      exit 1
    fi
    wedged
    declare -a filters=()
    while [ $# -gt 0 ]; do
      case "$1" in
        --filter) filters+=("$2"); shift 2 ;;
        --all|--quiet|--no-trunc|-a|-q) shift ;;
        -*) printf 'stand-in docker: unknown ps flag %s\n' "$1" >&2; exit 125 ;;
        *) shift ;;
      esac
    done
    for dir in "$STATE"/c-*; do
      [ -d "$dir" ] || continue
      cname="$(basename "$dir")"
      cname="${cname#c-}"
      cinstance=""
      [ -f "$dir/instance" ] && cinstance="$(cat "$dir/instance")"
      match=yes
      for f in ${filters[@]+"${filters[@]}"}; do
        case "$f" in
          "label=$INSTANCE_LABEL="*)
            [ "$cinstance" = "${f#label=$INSTANCE_LABEL=}" ] || match=no ;;
          "label=$HOOK_LABEL="*) ;;
          *) printf 'stand-in docker: unknown ps filter %s\n' "$f" >&2; exit 125 ;;
        esac
      done
      if [ "$match" = yes ]; then printf 'stand-in-%s\n' "$cname"; fi
    done
    exit 0 ;;
  inspect)
    wedged
    format=""
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --format) format="$2"; shift 2 ;;
        -*) printf 'stand-in docker: unknown inspect flag %s\n' "$1" >&2; exit 125 ;;
        *) target="$1"; shift ;;
      esac
    done
    if [ "$format" != '{{.State.Status}} {{.State.Running}} {{.State.ExitCode}}' ]; then
      # Loudly, for the unknown-flag reason: an inspect whose format this
      # fake does not implement would otherwise be answered with
      # something the runner would happily misread.
      printf 'stand-in docker: unknown inspect format %s\n' "$format" >&2
      exit 125
    fi
    dir="$(resolve "$target")"
    if [ ! -d "$dir" ]; then
      printf 'Error: No such object: %s\n' "$target" >&2
      exit 1
    fi
    status="$(cat "$dir/status")"
    running=false
    [ "$status" = running ] && running=true
    printf '%s %s %s\n' "$status" "$running" "$(cat "$dir/exit")"
    exit 0 ;;
  kill)
    wedged
    sig=KILL
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --signal=*) sig="${1#--signal=}"; shift ;;
        --signal) sig="$2"; shift 2 ;;
        *) target="$1"; shift ;;
      esac
    done
    dir="$(resolve "$target")"
    if [ ! -d "$dir" ]; then
      printf 'Error response from daemon: No such container: %s\n' "$target" >&2
      exit 1
    fi
    if [ -f "$dir/pid" ]; then
      # The negated pid, behind a --: the launch makes the job a process
      # group leader, so this reaches everything the hook started, which
      # is what a container's cgroup does. Without the -- bash reads a
      # leading-dash pid as another signal specification.
      kill -s "$sig" -- "-$(cat "$dir/pid")" 2>/dev/null || true
    fi
    exit 0 ;;
  rm)
    wedged
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in -*) shift ;; *) target="$1"; shift ;; esac
    done
    dir="$(resolve "$target")"
    if [ "$STICKY" != yes ]; then
      # --force removes a running container, and removing a container
      # takes its process with it.
      if [ -f "$dir/pid" ]; then kill -s KILL -- "-$(cat "$dir/pid")" 2>/dev/null || true; fi
      rm -rf "$dir"
    fi
    exit 0 ;;
  create|run) ;;
  start)
    wedged
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --attach|-a) shift ;;
        -*) printf 'stand-in docker: unknown start flag %s\n' "$1" >&2; exit 125 ;;
        *) target="$1"; shift ;;
      esac
    done
    dir="$(resolve "$target")"
    if [ ! -d "$dir" ]; then
      printf 'Error response from daemon: No such container: %s\n' "$target" >&2
      exit 1
    fi
    launch "$dir"
    exit "$LAUNCH_STATUS" ;;
  *)
    printf 'stand-in docker: unknown verb %s\n' "$verb" >&2
    exit 125 ;;
esac

# create and run share every flag: run is create-and-start in one call,
# which is what the capability probe and the syntax check still use.
name=""
entrypoint=""
workdir=""
instance=""
declare -a envnames=()
while [ $# -gt 0 ]; do
  case "$1" in
    --rm|--read-only|--interactive|-i) shift ;;
    --name) name="$2"; shift 2 ;;
    --entrypoint) entrypoint="$2"; shift 2 ;;
    --env) envnames+=("$2"); shift 2 ;;
    --workdir) workdir="$2"; shift 2 ;;
    --label)
      case "$2" in
        "$INSTANCE_LABEL="*) instance="${2#$INSTANCE_LABEL=}" ;;
      esac
      shift 2 ;;
    --network|--security-opt|--cap-drop|--tmpfs|--pids-limit|--user|--volume|--platform) shift 2 ;;
    -*)
      # Loudly, so a hardening flag this fake has not been taught fails
      # the suite instead of being silently dropped.
      printf 'stand-in docker: unknown flag %s\n' "$1" >&2
      exit 125 ;;
    *) break ;;
  esac
done
shift || true   # the image

if [ -n "$PROBE_SAYS" ]; then
  for arg in "$@"; do
    if [ "$arg" = "-c" ]; then printf '%s' "$PROBE_SAYS"; exit 0; fi
  done
fi

if [ -z "$name" ]; then
  # Every container this runner starts is named, hook, probe and syntax
  # check alike: an unnamed one is a container nothing can address once
  # the client that started it has gone.
  printf 'stand-in docker: a %s with no --name\n' "$verb" >&2
  exit 125
fi

if [ -d "$STATE/c-$name" ]; then
  # docker's own answer, and the reason a container name has to be
  # unique per LAUNCH rather than per step: the client exits 125 and
  # nothing is created. A runner whose two concurrent steps minted the
  # same name met this, and then removed the name they shared.
  printf 'docker: Error response from daemon: Conflict. The container name "/%s" is already in use.\n' "$name" >&2
  exit 125
fi

register() {
  local dir="$STATE/c-$name" n
  mkdir -p "$dir/env"
  printf '%s\n' "$entrypoint" > "$dir/entrypoint"
  printf '%s\n' "$workdir" > "$dir/workdir"
  printf '%s\n' "$instance" > "$dir/instance"
  printf '0\n' > "$dir/exit"
  : > "$dir/args"
  for a in "$@"; do printf '%s\0' "$a" >> "$dir/args"; done
  # --env NAME: the value comes from THIS process's environment, at
  # create time, and nothing else is passed through. Stored
  # NUL-terminated so that reading it back cannot lose a trailing
  # newline or re-parse anything.
  for n in ${envnames[@]+"${envnames[@]}"}; do
    if [ -n "${!n+set}" ]; then printf '%s\0' "${!n}" > "$dir/env/$n"; fi
  done
  printf 'created\n' > "$dir/status"
}

if [ "$verb" = create ]; then
  if [ -n "$CREATE_LATENCY" ]; then
    # A daemon that accepted the creation and completed it after the
    # client asking for it was gone. The writer is detached from this
    # process's streams, so killing the client really does release them,
    # exactly as a daemon-side creation does -- and the client hangs
    # until somebody kills it.
    ( sleep "$CREATE_LATENCY"; register "$@" ) >/dev/null 2>&1 &
    exec sleep 600 >/dev/null 2>&1
  fi
  register "$@"
  printf 'stand-in-%s\n' "$name"
  exit 0
fi

# run: create and start in one call, with --rm removing the record on the
# way out.
register "$@"
launch "$STATE/c-$name"
if [ "$STICKY" != yes ] && [ "$AUTO_REMOVE_FAILS" != yes ]; then rm -rf "$STATE/c-$name"; fi
exit "$LAUNCH_STATUS"
`

// standInPlatform is the os/arch the stand-in daemon and its images agree
// on, so the preflight's platform check runs for real and passes. A test
// that wants the refusal sets fakeDocker.imagePlatform to something else.
const standInPlatform = "stand-in/arch"

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
