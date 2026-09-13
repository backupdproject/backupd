// Package workflowexec is the EXECUTION ENVELOPE every hook script runs
// inside, wherever it runs: the shell invocation, the environment
// encoding, and the shape of the output that comes back.
//
// # Why this is a package and not two copies
//
// EPIC L has two executors. #809 runs a NAME.local.sh on the backup server
// through a host runner; #810 runs a NAME.remote.sh on the source host
// through an SSH exec channel. They share nothing mechanically -- one has a
// process, the other a channel -- and they have to share EVERYTHING
// semantically, because the same hook script moved from one to the other
// must mean the same thing.
//
// So what lives here is exactly the part that must not differ:
//
//   - the shell invocation: a FIXED, preflight-validated bash path, invoked
//     --noprofile --norc, with no PTY and with NOTHING injected -- no
//     set -e, no nounset, no pipefail, no xtrace. A hook's bytes run
//     exactly as they were captured, because a shell option this product
//     added silently changes what somebody else's script means.
//   - the environment: a sanitized baseline with BASH_ENV, ENV, SHELLOPTS
//     and BASHOPTS removed before anything is layered on, values that may
//     contain newlines and ordinary UTF-8 but never a NUL, and values that
//     are never, under any encoding, interpreted as shell syntax.
//   - the output: stdout and stderr as two separate logical streams whose
//     chunks carry sequence numbers from ONE counter, so a reader can
//     recover the interleaving the hook actually produced.
//   - termination certainty: confirmed, unconfirmed, or not asked. Three
//     values, and deliberately no fourth for "probably".
//
// # The two encodings, and why there are two
//
// ProcessEnv is for an executor that can hand an environment block to
// execve. StdinPayload is for one that cannot: an SSH exec channel offers
// no way to set a variable that does not depend on the server's SendEnv
// and AcceptEnv configuration, and putting values on the remote command
// line would publish them in the remote process list. So the remote
// envelope emits shell text on the channel's stdin instead.
//
// Both run the SAME validation, in the same function, which is the point of
// them living together: an environment one executor accepts and the other
// refuses would be a hook that works on the backup server and fails on the
// source host, for a reason no operator could see.
//
// # What is NOT here
//
// No transport, no process, no journal, no policy. This package opens
// nothing, spawns nothing and logs nothing: it turns decided inputs into
// bytes and turns writes into chunks. That is what lets both executors
// share it without either one's dependencies reaching the other.
package workflowexec
