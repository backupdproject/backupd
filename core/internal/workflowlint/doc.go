// Package workflowlint answers, about one hook script's bytes and
// without running them, the two questions an operator would otherwise
// find out the answer to at 2am: is this a shell program at all, and does
// it contain one of the handful of mistakes that turn a backup hook into
// an outage (#906).
//
// # This is backupd's own shell check, and it is not ShellCheck
//
// Said first because the codes look familiar and the resemblance stops
// there. The findings here are produced by rules in rules.go, written
// against mvdan.cc/sh's typed syntax tree, carrying this product's own
// BSH codes. Nothing is derived from ShellCheck: not its analysis, not
// its codes, not its messages, and no ShellCheck artifact is in this
// repository, in this module's graph or in any shipped image. ShellCheck
// is GPL-3.0 and this product is Apache-2.0, and a tool that cannot be
// shipped is not a tool a save gate can depend on.
//
// The consequence is stated rather than implied: this is a SMALL,
// deliberately conservative set of rules. It is not a general shell
// linter and it will not find everything. An operator who wants the full
// analysis should run ShellCheck themselves, which is a thing they can
// install and this product cannot.
//
// # Why mvdan.cc/sh rather than `bash -n`
//
// The parse verdict is the half that must always work, because #906 makes
// it a PRECONDITION OF SAVING. `bash -n` needs a bash: the engine's
// canonical runtime is a distroless container with no shell in it, a
// `.local.sh` hook is parsed by asking the Host Workflow Runner over a
// socket, and a `.remote.sh` hook by opening an SSH connection to the
// source host. All three can be unreachable at the moment somebody
// presses Save, and refusing every save on a deployment whose runner
// socket is down would be a gate on the wrong thing.
//
// mvdan.cc/sh is a shell parser written in Go, linked into this binary,
// needing no interpreter, no runner, no network and no filesystem. It
// also reports a POSITION -- line and column -- where `bash -n` reports a
// sentence about a line number in a temporary file. So the runner's and
// the far side's `bash -n` are kept, and they keep answering the question
// they are the only thing that can answer (can this executor run this
// plan at all), while the syntax verdict an operator reads and a save
// gate refuses on comes from here.
//
// # What a rule has to be to live in rules.go
//
// Sound on ordinary scripts, first. A finding on a working hook is how
// this feature becomes the thing an operator turns off, and an
// error-severity false positive is a save somebody cannot perform. So
// every rule is narrowed until the shapes it fires on are shapes that are
// wrong rather than shapes that are unusual, and the narrowing is written
// down at the rule.
//
// Positional, second: a finding carries the line and column of the thing
// it is about, because "SC2086 somewhere in your script" is not
// actionable and this product's whole reason for reporting at all is that
// an operator can go and look.
//
// # What bounds it
//
// A hook directory is a place another program's output can end up, so
// nothing here may hang or exhaust memory:
//
//   - MaxScriptBytes bounds the input before the parser sees it. The
//     parser is recursive descent, a stack overflow in Go is fatal and
//     cannot be recovered with a deferred recover(), so a bound on the
//     input is the only thing that makes a megabyte of "$(" safe;
//   - nothing is executed, nothing is resolved, no file other than the
//     bytes handed in is read, and no network call is made, so the
//     analysis cannot be made to do work by the thing it is analysing.
//
// # What the report may say
//
// Line, column, a code, a severity and a message. Never a resolved
// secret: nothing on this path resolves one. A message may quote a
// variable NAME or a literal from the script -- the script's own text,
// which is the reason a finding is actionable -- and a variable's VALUE
// is not something this package ever has.
//
// Findings are sorted by position and then by code, so two reports over
// unchanged bytes are the same report: an operator diffing yesterday's
// output against today's is the whole point of a validation surface.
package workflowlint
