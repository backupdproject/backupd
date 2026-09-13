// Package remoteexec runs a captured hook script on a backup set's remote
// host over an SSH exec channel (#810).
//
// # The claim this package exists to make true
//
// An SFTP credential is not an exec credential. The posture
// docs/ssh-setup.md recommends for a backup SOURCE is an account forced
// into internal-sftp, chrooted, with no shell at all: it authenticates, it
// transfers artifacts, and `ssh host command` on it answers "This service
// allows sftp connections only." and exits 1. A forced-command account
// (the rrsync shape) is worse, because it answers successfully while
// running somebody else's program instead of the hook.
//
// So an execution connection is modelled SEPARATELY from the transfer
// connection, and exec capability is PROVEN per run rather than assumed:
//
//   - a step's connection reference resolves either to a declared
//     workflows.exec_connections entry, which is an execution connection
//     with its own credential, or to a backup set ("source/set"), which
//     means "reuse that set's own source connection";
//   - either way, Preflight runs a real probe through the real envelope
//     before a hook is sent, and a connection that cannot execute a
//     command is refused there, with a reason naming the missing
//     capability;
//   - a refusal fails the REMOTE HOOK, not the backup. An SFTP-only source
//     goes on backing up exactly as it did before this package existed.
//
// # The envelope
//
// internal/workflowexec owns it, so that a hook moved between the host
// runner (#809) and this executor means the same thing. What this package
// adds is what only the remote side has to decide:
//
//   - No PTY, ever. A PTY would make sshd deliver signals and job control,
//     which would make termination easier, and it would also make a hook's
//     stdout and stderr the same stream. Those two are not separable
//     facts, and separate streams is the requirement that wins.
//   - A FIXED remote command: the validated bash path, --noprofile --norc
//     -s, and one non-secret step token. Nothing from configuration and no
//     environment value is ever part of it, because the remote command
//     line is world-readable in the remote process list.
//   - The environment and the script arrive on the channel's STDIN, as
//     shell text whose every value is a single-quoted literal. There is no
//     SendEnv/AcceptEnv dependency (a hardened sshd refuses env requests)
//     and no file is written on the remote host at any point, so there is
//     no residue to clean up and nothing to leak if the connection dies
//     mid-step.
//
// # What "confirmed" means, and why it is not a guess
//
// Without a PTY, closing an SSH channel does not reliably kill what was
// running behind it. So termination is requested with the strongest
// semantics the protocol offers -- an exec-channel signal request, then a
// reaper session that kills the step's process GROUP by its token, then
// closing the channel -- and the certainty recorded is a fact rather than
// an intention:
//
//   - confirmed: after termination was requested, the session completed.
//     The remote command's exit was reported AND both output streams
//     reached end of file, so nothing of the step still holds the channel.
//   - unconfirmed: it did not. Something may still be running.
//
// A deliberately detached descendant (nohup, setsid, a double fork) is
// explicitly OUTSIDE the guarantee, and it is outside it in the honest
// direction: such a descendant keeps the channel's stdout open, the session
// never completes, and the step records unconfirmed rather than claiming a
// clean stop it cannot see.
//
// # What is not here
//
// Stage sequencing, cleanup and recovery (#811), the local executor
// (#809), and every operator surface (#813, #814). This package resolves a
// connection, proves a capability, runs one script, and reports what
// happened.
package remoteexec
