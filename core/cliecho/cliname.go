package cliecho

// The one spelling of the command an operator types, and the one place a
// rename touches.
//
// # Why one place rather than fifty
//
// The name was a string literal in about fifty places: every diagnostic
// that prefixes itself with it, the usage block's first line, every
// sentence that tells an operator which command to run next, every command
// core/cliecho echoes into the Web UI's terminal panel, and the filename
// core/tests/compat builds the binary under before it pins what that
// binary prints. None of those knew about each other.
//
// That is survivable while the name never moves, and it stops being
// survivable the moment it does. Renaming the command then means reading
// fifty string literals and deciding, one at a time, whether each is the
// CLI or something that merely looks like it. EPIC R (#885) is the rename
// that proved the point: it moves the command from `backupd` to `retnd`,
// and three shapes in this tree still spell "backupd" while being NOT this
// constant, so every one of them would have been swept up by a careless
// search-and-replace:
//
//   - filesystem paths (/etc/backupd/config, /var/lib/backupd)
//     which packaging mounts and an operator's existing deployment already
//     has on disk;
//   - the project, the image and the compose service (ghcr.io/spdrman/
//     backupd, the "backupd" service in container/
//     compose.yaml, "Backupd" as the product's name);
//   - wire identity that a log or an audit trail may already be matched
//     on, which is the User-Agent core/internal/apiclient sends.
//
// Those three move on their own issues and their own schedules -- the
// runtime identifiers in R1.4 (#889), the deployment identity in R1.5
// (#890), the product's own name in phase 2 -- precisely because they are
// not the command and cannot be renamed on the command's timetable.
//
// So this is not a tidy-up. It is the line between "the command" and
// "everything else called that", drawn once, in a place a reader can see
// it, so a rename is this file and nothing else.
//
// # Why it is in cliecho rather than in a package of its own
//
// It started as core/cliname, which is where it belongs and is not where it
// can live. container/Dockerfile copies core/ one named directory at a time
// (apicontract, cliecho, cmd, internal, service, migrations) rather than
// wholesale, deliberately, so that a stray untracked file cannot reach the
// build context. A new top-level package under core/ is therefore invisible
// to the image until somebody adds two COPY lines, and until they do the
// image build dies with "cannot find module providing package" while every
// `go build` and `go test` from a checkout stays green, because a checkout
// has the whole module. That is not hypothetical: the same trap caught
// core/apicontract when #543 gave the CLI an engine to talk to, and the
// Dockerfile carries a paragraph about it.
//
// cliecho is on that list already, in both build stages, and it is not an
// arbitrary hiding place: naming the command an action corresponds to is
// the whole of what this package does, and newCmd below prepends this exact
// constant to every line it builds. So the CLI reading its own name from
// here means there is one spelling of it in the product, which is the
// point.
//
// # Why a constant and not something read at runtime
//
// What this binary calls itself is a fact about the build, not about how
// somebody reached it. main() hands os.Args[1:] to run() and dispatches on
// the first word of that, so the filename an operator happened to type
// never enters a decision.
//
// That is a property worth keeping, and EPIC R is the release in which it
// earns its keep twice over. The runtime stage's two files are still
// /backupd and /backupd-web -- container-internal paths are R1.5's (#890),
// not this issue's -- so for the length of that window the filename an
// operator reaches this build through is the PREVIOUS name. Reading argv[0]
// would make the binary print `backupd` at somebody who cannot type it any
// more. It would still be wrong once R1.5 lands, and for a reason no rename
// removes: a wrapper script, a busybox-style multi-call link or a
// `docker run --entrypoint` under any other filename would make this
// build print a command nobody can type.
//
// What can be tested is the half that lives in this module, and
// TestNothingDispatchesOnArgv0 in core/cmd/retnd does: it reads
// every non-test file under core/ and requires os.Args to appear in
// exactly one shape, os.Args[1:], with os.Executable unused. So the name
// this package prints is the name this package declares, whatever the
// binary was reached as.
const (
	// Binary is the command an operator types, and the name this build
	// prints when it names itself: the prefix on a diagnostic, the
	// "usage:" line, and every sentence that says which command to run
	// next.
	//
	// It was `backupd` until EPIC R (#885), and R1.3 (#888) retired that
	// name rather than aliasing it: the image ships no symlink under the
	// old spelling, so anything already automated against it has to move
	// across. Printing two names is how a reference stops being one,
	// and shipping two is how a rename never finishes.
	Binary = "retnd"

	// WebBinary is the other command in the image, the one that serves the
	// Web UI. It is derived rather than spelled so that the two names
	// cannot drift apart, which is the whole reason this file exists.
	//
	// The package directories are named for these two constants --
	// core/cmd/retnd and apps/generic/cmd/retnd-web -- and R1.3 moved them
	// in the same commit that moved the constant. They do not DERIVE from
	// it: a Go package path is not operator-visible, and a directory cannot
	// be a constant expression.
	WebBinary = Binary + "-web"
)
