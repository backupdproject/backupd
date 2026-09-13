// Package workflow is the domain model, the secure script discovery and the
// environment model behind scripted backup workflows (EPIC L, #807; this
// package is #808).
//
// # What a workflow is
//
// An operator drops shell scripts into a read-only tree, by convention
// mounted at /workflows, and this product runs them around a backup: a
// "before" stage in front of the backup and an "after" stage behind it, at
// two scopes (once for the whole deployment, once for one backup set). A
// script's basename says where it runs -- NAME.local.sh on the machine
// running this daemon, NAME.remote.sh on the host the backup set pulls
// from -- and nothing else about it is inferred.
//
// This package decides WHAT will run. It never runs anything: there is no
// exec call in it, no connection, no process. Execution is #809/#810,
// sequencing and cleanup are #811, and the operator surfaces are #813 and
// #814. What is here is the part all four of those have to agree on, and
// the part that has to be right before any of them may run a single byte.
//
// # The plan is the only execution authority
//
// Snapshot is the whole point of the package. It canonicalizes the
// configured hook directories, discovers the scripts in them, validates
// every one, reads each file EXACTLY ONCE, hashes it, copies the bytes
// into a run-scoped spool under the state directory, and returns an
// immutable Plan.
//
// Nothing downstream re-opens a path in /workflows. That is not an
// optimisation, it is the security property: between "this script passed
// validation" and "this script ran" there is no window in which the file
// could be replaced, because the file is never consulted again. Editing or
// deleting a script after a Plan exists cannot change what that run -- or
// a recovery of that run tomorrow, after a restart -- executes.
// TestSnapshotIsImmuneToAPostSnapshotReplacement is that claim.
//
// A Plan is therefore OPAQUE, and a consumer reaches a script through one
// function: Plan.OpenScript, which takes a step ID and returns verified
// bytes. It is a capability rather than a path accessor because the layers
// that will use it (#810, #811) have to be unable to execute anything the
// plan did not decide -- so the path is re-derived from the plan, the
// spool's whole ancestry is re-checked with O_NOFOLLOW, and the bytes are
// re-hashed against the sha256 taken at snapshot time. Every one of those
// holds for a RECOVERED plan (RecoverPlan) as much as for a fresh one,
// which is the case that matters: after a restart the spool is a directory
// on disk and the plan is a set of database rows, and neither is something
// this process watched being written.
//
// # Why discovery refuses so much
//
// A hook directory is a list of programs this daemon will execute, some of
// them as root, some of them on a remote host. So the rules are the SSH
// key and secret-file rules (internal/secretref's ErrCustody, and
// internal/transport/rclone/ssh.go's key mode check) applied to the same
// question: can this process vouch for what it is about to run?
//
// A script must be a regular file reached without following a symbolic
// link, its own mode must not let another account write it, it must be
// owned by this process's user or root, no directory containing it may
// let another account write in it (the sticky-bit exception a SECRET
// file's walk makes is wrong here, and discover.go says why), it must be
// inside the approved root, and its basename must match ScriptNamePattern
// exactly. A
// plain "backup.sh" is refused rather than defaulted to local, because a
// script that runs in the wrong place is the failure this naming rule
// exists to make impossible, and picking a side on the operator's behalf
// is how that failure ships quietly.
//
// Every refusal names the path and what to do about it. An operator reads
// these in a failed backup, not in a debugger.
//
// # The environment model
//
// An environment entry is a NAME and either a literal value or a
// reference to a secret (internal/secretref's file/env/command triple, the
// one shape this product accepts a secret in). There is no field to paste
// a secret into, here or anywhere else.
//
// Resolution happens at execution time and produces Resolved, which
// carries secret-backed values as obs.Secret so they cannot be rendered by
// accident. A resolved value is never part of a Plan, never part of
// ResolvedPlanHash, never written to the spool and never persisted: what
// the plan carries is WHERE the secret comes from, exactly as config.yaml
// carries it.
//
// The UNRESOLVED environment, by contrast, is durable, and the distinction
// is the whole design. internal/state stores each variable's name, its
// literal (if it has one) and the LOCATION its secret comes from, in the
// same transaction as the run and its steps, because a run interrupted
// mid-workflow has to be recovered with the environment it was PLANNED
// with -- and an operator who edits or deletes a variable while the daemon
// is down must not change what that run finishes with. The plan hash
// covers the environment and cannot reconstruct it; a hash is one-way,
// which is the property it was chosen for.
//
// Precedence runs sanitized baseline < workflows.environment < backup-set
// environment < BACKUPD_* built-ins, and the built-ins are reserved: an
// operator cannot configure a name this product injects, because a hook
// that reads BACKUPD_BACKUP_STATUS has to be reading this product's answer
// rather than one somebody wrote into a config file.
package workflow
