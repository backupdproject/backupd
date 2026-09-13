package workflowlint

// The bound that keeps a hostile hook script from killing the process
// (#906, review BLOCKER 1).
//
// # Why a pre-scan and not a recover()
//
// mvdan.cc/sh's parser is recursive descent and imposes no depth limit of
// its own: each `$(` in `$($($(...)))` is another `cmdSubst` frame calling
// `wordPart` calling `cmdSubst`. A script of about a megabyte of `$(` --
// which is a LEGAL script at the default workflows.max_script_size_bytes,
// since that default and MaxScriptBytes are the same 1 MiB -- exhausts the
// goroutine stack.
//
// In Go that is `fatal error: stack overflow`. It is not a panic. A
// deferred recover() does not see it, a goroutine boundary does not
// contain it, and the runtime does not unwind: the whole process dies. And
// both callers of Report are reachable by somebody who can write a file
// into a hook directory -- a workflow configuration write (the save gate)
// and an authenticated GET of a set's workflow validation -- so "the
// daemon exits" is a denial of service against the thing that takes the
// backups.
//
// So the depth is MEASURED before the parser is handed anything. That is
// the only order that works: any check performed by or around the parser
// is a check that runs after the damage.
//
// # Why over-counting is the safe direction
//
// maxNestingDepth does not parse. It walks the bytes once and counts the
// simultaneous depth of every construct that produces parser recursion,
// and it deliberately does NOT track quoting, escaping, comments or
// heredocs -- which means a script with a thousand literal `(` characters
// inside single quotes counts them all.
//
// That is the direction to be wrong in. Over-counting can only make this
// package report "not examined", which fails OPEN: the save proceeds, the
// reason is reported to the operator, and nothing is claimed about the
// script. Under-counting would let a script through to the parser, which
// is the outcome that ends the process. The cap is then set far above any
// depth a person writes and far below the depth that crashes, so the
// inaccuracy costs nothing in practice.

// MaxNestingDepth is how deeply a script may nest recursion-producing
// constructs and still be parsed.
//
// The numbers it sits between were measured rather than guessed: nesting
// of 262144 `$(` parsed without incident and 524288 (a full 1 MiB of
// `$(`, the exact shape a default-configured deployment accepts) killed
// the process. So the crash floor on this platform is somewhere in
// between, and any cap in the low thousands is two orders of magnitude
// clear of it -- including on a platform whose stacks are smaller, and
// including the frames the rule walk adds on top of the parser's.
//
// Two thousand is also two orders of magnitude above anything a person
// writes: a hook with ten levels of command substitution is already
// unreadable. A script that exceeds this is not a hook somebody is
// maintaining, it is generated output or a probe.
const MaxNestingDepth = 2048

// maxNestingDepth counts the deepest simultaneous nesting of
// recursion-producing constructs in src.
//
// One pass, no allocation, no parsing. The openers are the ones that make
// the parser descend:
//
//	$(    command substitution          )
//	$((   arithmetic expansion          ))
//	${    parameter expansion           }
//	`     command substitution (old)    `
//	(     subshell / group / array      )
//	{     brace group                   }
//	[     test / index / glob class     ]
//
// `$((` is counted as one level and closed by the first `)` of its `))`,
// which leaves the second `)` to close the `(` that the pair also opened:
// counting it as the two nested parens the parser actually descends
// through is accurate enough for a bound and keeps this a single pass.
//
// A backtick toggles rather than nests, because that is what the shell
// does with it: there is no such thing as two open backticks at the same
// level.
func maxNestingDepth(src []byte) int {
	depth, deepest, backtick := 0, 0, false

	for i := range src {
		switch src[i] {
		case '(', '{', '[':
			depth++
			if depth > deepest {
				deepest = depth
			}
		case ')', '}', ']':
			if depth > 0 {
				depth--
			}
		case '`':
			// Toggling, and each open backtick is a level: `a `b`` is
			// not a thing the shell nests, so the depth rises by one
			// while a substitution is open and falls when it closes.
			if backtick {
				if depth > 0 {
					depth--
				}
			} else {
				depth++
				if depth > deepest {
					deepest = depth
				}
			}
			backtick = !backtick
		}

		// The whole point is to answer without reading a megabyte when
		// the answer is already known: a script this deep is refused,
		// and counting the rest of it would change nothing.
		if deepest > MaxNestingDepth {
			return deepest
		}
	}

	return deepest
}
