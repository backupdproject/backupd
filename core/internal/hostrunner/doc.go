// Package hostrunner is the Host Workflow Runner: the out-of-container
// helper that executes a workflow's local hook scripts on the real
// backupd host, and the protocol the engine reaches it through.
//
// # Why this process exists at all
//
// The runtime this product ships is distroless, read-only, non-root,
// cap_drop ALL, no-new-privileges, and has NO SHELL (container/Dockerfile,
// container/compose.yaml, docs/runtime-contract.md). That is the single
// most valuable security property of the deployment and it is pinned by
// gates rather than by intention.
//
// A `.local.sh` hook means "run this on the machine backupd is installed
// on". There are exactly four ways to satisfy that sentence, and three of
// them are the same mistake wearing different clothes:
//
//   - add a shell to the image. That is the contract, deleted.
//   - mount the Docker socket, or add CAP_SYS_ADMIN and nsenter into the
//     host namespaces. That is root on the host for anybody who reaches
//     the engine, which is strictly worse than the shell.
//   - bind-mount the host root read-write and chroot. Same, with extra
//     steps.
//   - run a SEPARATE, small, version-matched process on the host, and let
//     the engine ask it -- over one Unix-domain socket, with a narrow
//     vocabulary, with no path ever crossing the boundary.
//
// This package is the fourth. The container is not touched: it gains one
// socket directory and one read-only scripts directory, and nothing else.
//
// # What crosses the boundary, and what deliberately does not
//
// BYTES cross. A path never does. The engine has already captured every
// script into a run-scoped spool under its own state directory
// (internal/workflow's plan.go and spool.go argue why), verified against a
// sha256 taken from the descriptor the bytes were read through. What it
// sends here is that byte string, its size and its hash, plus the run and
// step ids it belongs to -- and this package re-checks size and hash
// before anything executes.
//
// The reason is the shape of the alternative. A runner that accepted
// "execute /path/on/the/host" would be a general-purpose remote execution
// service with a socket in front of it: whoever reached the socket could
// run anything already on the host, and the whole custody argument
// upstream (O_NOFOLLOW, mode checks, a private spool) would protect
// nothing, because the final hop would not be using any of it. So the
// wire has no field that can hold a path, the JSON decoder is strict about
// unknown fields, and an engine that tries to invent one is refused rather
// than ignored.
//
// # Two doors, and why both are locked
//
// The socket lives in a 0700 directory and is itself 0600, owned by the
// service account this process runs as. That is the kernel enforcing the
// boundary, which is the only enforcement that cannot be bypassed by a bug
// in this file.
//
// It is not the only one. Every connection must also present the
// installation-scoped credential written into backupd's secrets area at
// install time, compared in constant time. Defense in depth reads as
// belt-and-braces until you list the ways file permissions have actually
// failed on the deployments this product targets: a NAS package manager
// that runs everything as one account, an operator who chmod'ed a parent
// directory to debug something else, a container runtime that maps uids
// in a way nobody predicted, a backup of /etc restored with the wrong
// ownership. The token is what turns "the permissions were wrong for a
// week" into "and nothing could use it anyway".
//
// A VERSION MISMATCH is refused explicitly and early, for a different
// reason: the engine and the runner are one program cut in half by a
// socket. A 0.4.0 engine talking to a 0.3.9 runner is two halves of two
// programs, and the failure mode of guessing is a hook that runs with an
// environment or a timeout the other half did not mean. Upgrades pair the
// two; this refusal is what makes an unpaired upgrade a loud, immediate
// error with a sentence in it instead of a subtle one at 3am.
//
// # The lease, which is the part that is easy to leave out
//
// The connection IS the lease. If the engine crashes, is killed, or is
// recreated by a container runtime mid-hook, the socket read fails here,
// and this package terminates the hook's process group and removes its
// working directory.
//
// Without that, the worst outcome of this design is a runaway: an engine
// dies during a `before` hook that has quiesced a database, nothing is
// left to time the hook out or to run the `after` hook, and the database
// stays quiesced until somebody notices. A hook with no owner is worse
// than a hook that was killed, so a hook with no owner does not exist
// here.
//
// # Every hook runs in an ephemeral container (#865)
//
// The fourth option above says "run a separate process on the host". It
// does -- and what that process runs is not a shell on the host either.
// Each hook gets one `docker run --rm`: no network by default, no new
// privileges, every capability dropped, a read-only rootfs with a small
// tmpfs at /tmp, a pids limit, a NON-ROOT user (this process's own uid,
// so the files a hook writes are files this process can later remove),
// the image's own entrypoint replaced by the bash the preflight proved
// inside it, and only two mounts: the per-step working directory
// read-write and the runner's copy of the captured script read-only,
// both at their own host paths.
//
// A hook reaches nothing else on the host unless an operator declared it
// (--hook-mount PATH[:ro|:rw], read-only by default). The ENGINE cannot
// declare one, because this protocol has no field that can hold a path,
// and no docker socket is mounted into a hook container under any name.
//
// Killing a hook is killing its container, by the name this process
// minted before the container existed: TERM, a grace period, KILL, and
// then a LOOK at whether the daemon still knows it. That is strictly
// stronger than the process group it replaced -- a child that ignores
// SIGTERM or changed its process group goes with the cgroup -- and the
// container is REMOVED as well as stopped, so the lease guarantee is "no
// runaway AND no leftover".
//
// The capability is proven ONCE, at startup: docker present at a fixed
// path, the daemon reachable as this account, the hook image present for
// this platform, and a probe container -- started with the same
// hardening a hook gets -- that emits this repository's own marker and
// reports the interpreter, the uid and the absence of a terminal from
// inside. A host that cannot do all four does not serve. There is NO
// fallback to a shell on the host, and there is no code here that could
// find one: a fallback would make every property in this doc conditional
// on a daemon nobody checked.
//
// The privilege that buys -- this process can reach the Docker daemon,
// which on a NAS is root-equivalent -- is contained by living here and
// nowhere else. The engine's container gains nothing: no socket, no
// group, no capability.
//
// # The execution envelope
//
// Fixed bash path, established by the capability probe INSIDE the hook
// image and reported by `status`; invoked --noprofile --norc so no
// profile, rc file or BASH_ENV script runs first; a built environment
// that never inherits this process's own; BASH_ENV, ENV, SHELLOPTS and
// BASHOPTS deleted unconditionally even when an operator configured
// them, because each of them makes bash execute something the plan did
// not capture, and every DOCKER_ name deleted too, because the block
// this package builds becomes the docker client's own environment for
// the duration of one exec; the environment delivered as `--env NAME`
// with the value read from that block rather than through `export K=V`
// text, because text is a quoting bug waiting for a value with a quote
// in it and a value on a command line is a value in `ps`; `bash -n` on
// the captured bytes before anything runs, in the hook image's own bash,
// reported as its own refusal; and NOTHING injected -- no set -e, no -u,
// no pipefail, no -x. The bytes an operator wrote are the bytes bash is
// given.
//
// The last one is a product decision rather than an omission. A hook
// author's script is a script: it behaves here the way it behaves when
// they run it themselves, and a runner that silently added `set -e` would
// change the meaning of every existing hook in a way that is invisible in
// the file they are reading.
//
// core/internal/remoteexec (#810) executes the same envelope over SSH,
// and stays SSH-direct: a third-party source host cannot be assumed to
// have Docker. The two were agreed as one contract and differ in exactly
// one place, documented at Executor.Execute: bytes reach bash through a
// runner-private file (mounted read-only) here and through stdin there,
// because a remote host must be left with no residue.
package hostrunner
