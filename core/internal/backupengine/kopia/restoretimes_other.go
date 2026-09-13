//go:build !unix

package kopia

import "time"

// lchtimes reports that this platform has no way to set a symbolic link's
// own times, so a restore here leaves them alone.
//
// Skipped rather than approximated. The alternative on a platform without
// an l-variant is os.Chtimes, which follows the link and would therefore
// stamp whatever it points at -- a write outside the restore destination,
// which is the escape this whole restore path exists to prevent. A link
// whose own timestamp is the moment it was restored is a documented,
// bounded loss of fidelity; a restore that reaches outside its
// destination is not.
//
// The caller distinguishes this from a real failure by the sentinel,
// which is why it is a sentinel rather than a nil return: a restore that
// silently reported success for metadata it never set would be the same
// class of quiet untruth the report's Complete field exists to prevent.
func lchtimes(string, time.Time) error { return errLinkTimesUnsupported }
