package hostrunner

import (
	"fmt"
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
//   - `--env NAME` reads NAME out of the CLIENT's own environment and
//     passes nothing else through. That is docker's actual contract and
//     the property the whole "no value on a command line" argument rests
//     on, so the fake implements it with `env -i` and a bash array --
//     no re-parsing, so a value with a newline, a quote or a `$(` in it
//     reaches the target process exactly as docker would deliver it.
//   - a container is a PROCESS GROUP. `set -m` makes the launched job a
//     group leader, and `kill` signals the negated group id, which is
//     the closest a shell gets to a cgroup: a child that ignores SIGTERM
//     still dies on the group's SIGKILL, exactly as it would in a real
//     container.
//   - `--rm` removes the container's record when the process exits, and
//     `ps --filter name=` answers from that record, so "is the container
//     gone" is a real question with a real answer.
//   - an UNKNOWN FLAG is exit 125, loudly. That is what makes this fake
//     safe to rely on: a hardening flag added to hookArgs and not taught
//     to the fake fails every test in the package instead of being
//     silently ignored, which is how a fake starts lying.
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

	// sticky keeps a container's record after its process exits and
	// ignores `rm`, which is a daemon that will not let go: the runner
	// must report that termination as UNCONFIRMED and must not remove
	// the working directory.
	sticky bool

	// daemonGoneAfterStart makes every `ps` fail once a container has
	// been started, which is the case that must NOT be read as "the
	// container is gone": a question that could not be asked is not an
	// answer.
	daemonGoneAfterStart bool
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
func writeFakeDockerIn(t *testing.T, state string, cfg fakeDocker) string {
	t.Helper()

	bash := hostBashForFake(t)
	path := filepath.Join(state, "docker")
	body := fmt.Sprintf(fakeDockerScript,
		bash,
		state,
		standInPlatform,
		shellSingleQuote(cfg.versionFails),
		boolWord(cfg.imageMissing),
		shellSingleQuote(firstNonEmptyString(cfg.imagePlatform, standInPlatform)),
		shellSingleQuote(cfg.probeSays),
		boolWord(cfg.sticky),
		boolWord(cfg.daemonGoneAfterStart),
	)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
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
// The distinction is not pedantry: every `docker run` vector contains
// `--rm`, so a substring search for "rm " matches the launch itself and a
// test asserting "the container was removed by name" would pass against
// a runner that never removed anything. That mutation was live once.
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
// about. A leftover is a file here, which is what "no leftover container"
// is asserted against.
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
// the two configuration values that are text.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fakeDockerScript is the stand-in client. See this file's preamble for
// what it implements on purpose and what it deliberately does not.
const fakeDockerScript = `#!%s
# Written by core/internal/hostrunner's tests. Not a docker client.
set -u
STATE=%s
STAND_IN_PLATFORM=%s
VERSION_FAILS=%s
IMAGE_MISSING=%s
IMAGE_PLATFORM=%s
PROBE_SAYS=%s
STICKY=%s
DAEMON_GONE_AFTER_START=%s

# Global client flags come before the verb, exactly as docker requires.
while [ $# -gt 0 ]; do
  case "$1" in
    --config|--host|--context) shift 2 ;;
    *) break ;;
  esac
done

verb="${1:-}"
shift || true
printf '%%s %%s\n' "$verb" "$*" >> "$STATE/calls"

case "$verb" in
  version)
    if [ -n "$VERSION_FAILS" ]; then printf '%%s\n' "$VERSION_FAILS" >&2; exit 1; fi
    printf '27.0.0-stand-in '"$STAND_IN_PLATFORM"'\n'
    exit 0 ;;
  image)
    if [ "$IMAGE_MISSING" = yes ]; then printf 'Error: No such image\n' >&2; exit 1; fi
    printf 'sha256:feedfacefeedfacefeedfacefeedface %%s\n' "$IMAGE_PLATFORM"
    exit 0 ;;
  ps)
    if [ "$DAEMON_GONE_AFTER_START" = yes ] && [ -f "$STATE/started" ]; then
      printf 'Cannot connect to the Docker daemon\n' >&2
      exit 1
    fi
    filter=""
    while [ $# -gt 0 ]; do
      case "$1" in --filter) filter="$2"; shift 2 ;; *) shift ;; esac
    done
    name="${filter#name=^}"
    name="${name%%'$'}"
    if [ -f "$STATE/c-$name" ]; then printf 'stand-in-%%s\n' "$name"; fi
    exit 0 ;;
  kill)
    sig=KILL
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --signal=*) sig="${1#--signal=}"; shift ;;
        --signal) sig="$2"; shift 2 ;;
        *) target="$1"; shift ;;
      esac
    done
    if [ ! -f "$STATE/c-$target" ]; then
      printf 'Error response from daemon: No such container: %%s\n' "$target" >&2
      exit 1
    fi
    pid="$(cat "$STATE/c-$target")"
    # The negated pid, behind a --: the launch below makes the job a
    # process group leader, so this reaches everything the hook started,
    # which is what a container's cgroup does. Without the -- bash reads
    # a leading-dash pid as another signal specification.
    kill -s "$sig" -- "-$pid" 2>/dev/null || true
    exit 0 ;;
  rm)
    target=""
    while [ $# -gt 0 ]; do
      case "$1" in -*) shift ;; *) target="$1"; shift ;; esac
    done
    if [ "$STICKY" != yes ]; then rm -f "$STATE/c-$target"; fi
    exit 0 ;;
  run) ;;
  *)
    printf 'stand-in docker: unknown verb %%s\n' "$verb" >&2
    exit 125 ;;
esac

name=""
entrypoint=""
workdir=""
declare -a envnames=()
while [ $# -gt 0 ]; do
  case "$1" in
    --rm|--read-only|--interactive|-i) shift ;;
    --name) name="$2"; shift 2 ;;
    --entrypoint) entrypoint="$2"; shift 2 ;;
    --env) envnames+=("$2"); shift 2 ;;
    --workdir) workdir="$2"; shift 2 ;;
    --label|--network|--security-opt|--cap-drop|--tmpfs|--pids-limit|--user|--volume|--platform) shift 2 ;;
    -*)
      # Loudly, so a hardening flag this fake has not been taught fails
      # the suite instead of being silently dropped.
      printf 'stand-in docker: unknown flag %%s\n' "$1" >&2
      exit 125 ;;
    *) break ;;
  esac
done
shift || true   # the image

if [ -n "$PROBE_SAYS" ]; then
  for arg in "$@"; do
    if [ "$arg" = "-c" ]; then printf '%%s' "$PROBE_SAYS"; exit 0; fi
  done
fi

# --env NAME: the value comes from THIS process's environment and nothing
# else is passed through. env -i plus an array means no value is ever
# re-parsed by a shell.
declare -a envargs=()
for n in ${envnames[@]+"${envnames[@]}"}; do
  if [ -n "${!n+set}" ]; then envargs+=("$n=${!n}"); fi
done

: > "$STATE/started"
if [ -n "$workdir" ]; then cd "$workdir" || exit 125; fi
set -m
env -i ${envargs[@]+"${envargs[@]}"} "$entrypoint" "$@" &
child=$!
if [ -n "$name" ]; then printf '%%s\n' "$child" > "$STATE/c-$name"; fi
wait "$child"
status=$?
# The daemon's own teardown: when a container's main process exits, what
# is left in its cgroup goes with it. Emulated, because otherwise this
# stand-in would be quietly WEAKER than docker -- a hook's orphaned
# child would survive here and not in a deployment, and the runner
# relies on the opposite.
kill -s KILL -- "-$child" 2>/dev/null || true
if [ "$STICKY" != yes ] && [ -n "$name" ]; then rm -f "$STATE/c-$name"; fi
exit "$status"
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
