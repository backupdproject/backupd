package packaging

import (
	"sort"
	"strings"
)

// Issue #890 (R1.5, EPIC R #885) renamed this project's deployment
// identity, and this file is the whole of the overlap that rename needs
// on the packaging side. Everything in it is temporary, every piece of it
// names the same two issues, and issue #895 deletes the file.
//
// TWO CLAIMS, DELIBERATELY SEPARATE, because they are about two different
// artifacts and they retire on two different schedules:
//
//   - The IMAGE answers to one pre-rename entrypoint. canonical.json's
//     `retainedBinaries` is the data; container/Dockerfile links
//     /backupd-web to /retnd-web. It exists so that an operator's PINNED
//     compose file, which this repository cannot edit, still starts. Any
//     rule that asks what an argv[0] or a healthcheck command may SAY has
//     to accept it, so that a deployment running a pinned pre-#891
//     provider file does not fail a gate for saying something the image
//     still honours. Every adapter IN THIS TREE says /retnd-web, as of
//     #891; this claim is about the copies outside it.
//
//   - The RELEASE MANIFEST of an already-published release keys its
//     hashes under the pre-rename binary NAMES. That is not an alias and
//     not a compatibility shim: container/release-manifest.json records
//     the SHA-256 of bytes that were built and pushed before the rename,
//     and re-keying evidence to change a label would invalidate it. So a
//     manifest lookup accepts both spellings, new first.
//
// WHY A BRAND TOKEN AND NOT A TABLE. core/legacypath makes the same
// choice for FR-38's state adoption and for the same reason: the rename
// renamed a NAME. Substituting the token covers /etc/retnd -> /etc/backupd
// and retnd-web -> backupd-web without a list anybody has to remember to
// extend, and it is the same derivation on both sides of the product, so
// the packaging gate and the running process cannot disagree about what
// the legacy spelling of something is.
const (
	// brandToken is this project's name as it appears as a whole path
	// segment or as the leading token of a binary name.
	brandToken = "retnd"
	// retiredBrandToken is what it was called before #890.
	retiredBrandToken = "backupd"
)

// LegacyBrandPath returns p with every path segment that is exactly the
// brand token replaced by the retired one, or "" when p has no such
// segment.
//
// Segments, not substrings: /etc/retnd/config has one and
// /data/retention does not, and a substring rule would rewrite the
// second.
func LegacyBrandPath(p string) string {
	segments := strings.Split(p, "/")
	found := false
	for i, s := range segments {
		if s == brandToken {
			segments[i] = retiredBrandToken
			found = true
		}
	}
	if !found {
		return ""
	}
	return strings.Join(segments, "/")
}

// LegacyBrandName returns name with a LEADING brand token replaced by the
// retired one -- "retnd" -> "backupd", "retnd-web" -> "backupd-web" --
// or "" when name does not begin with one.
//
// Leading only. A name that merely contains the token ("my-retnd-thing")
// is not a binary this project renamed, and the release manifest has no
// pre-rename key for it.
func LegacyBrandName(name string) string {
	switch {
	case name == brandToken:
		return retiredBrandToken
	case strings.HasPrefix(name, brandToken+"-"):
		return retiredBrandToken + strings.TrimPrefix(name, brandToken)
	}
	return ""
}

// KnowsBinary reports whether path is a binary the canonical image
// answers to: one it contains, or a retained alias of one.
func (c Canonical) KnowsBinary(path string) bool {
	if contains(c.Binaries, path) {
		return true
	}
	_, retained := c.RetainedBinaries[path]
	return retained
}

// BinarySpellings is every path KnowsBinary accepts, sorted, for a
// refusal message that tells the reader what it would have taken.
func (c Canonical) BinarySpellings() []string {
	out := append([]string(nil), c.Binaries...)
	for alias := range c.RetainedBinaries {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

// CommandSpellings returns argv plus every spelling of it that names a
// retained alias of the same binary, so a caller can hold a provider to
// "runs the canonical command" without holding it to a name the image
// still answers to.
//
// Sorted by the alias so the answer is deterministic; a map iteration
// here would reorder the argv a failure message prints.
func (c Canonical) CommandSpellings(argv []string) [][]string {
	out := [][]string{argv}
	if len(argv) == 0 {
		return out
	}
	aliases := make([]string, 0, len(c.RetainedBinaries))
	for alias, real := range c.RetainedBinaries {
		if real == argv[0] {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		spelling := make([]string, 0, len(argv))
		spelling = append(spelling, alias)
		spelling = append(spelling, argv[1:]...)
		out = append(out, spelling)
	}
	return out
}

// sameEntrypoint rewrites argv[0] from a retained alias to the binary it
// links to, so two artifacts that name the same inode compare equal.
//
// CommandSpellings' inverse, and it exists because equivalence is a
// comparison of two artifacts rather than of one artifact against the
// contract: neither side is "the wanted argv" whose spellings could be
// enumerated, so the alias is normalised away instead.
func (c Canonical) sameEntrypoint(argv []string) []string {
	if len(argv) == 0 {
		return argv
	}
	real, retained := c.RetainedBinaries[argv[0]]
	if !retained {
		return argv
	}
	out := make([]string, 0, len(argv))
	out = append(out, real)
	out = append(out, argv[1:]...)
	return out
}
