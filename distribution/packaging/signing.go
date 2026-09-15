package packaging

import "strings"

// The keyless signing identity, and the command a verifier runs against it.
//
// Sigstore keyless signing binds a signature to the Subject Alternative
// Name of the short-lived Fulcio certificate the release workflow gets in
// exchange for its GitHub OIDC token. GitHub builds that SAN as
//
//	<repository URL>/<workflow path>@<the ref the run was triggered on>
//
// so the ref half of it is decided by the workflow's trigger and by
// nothing else. That is the whole reason these constants and
// .github/workflows/release.yml cannot be allowed to drift apart:
// signing.identity in the provenance bundle, and the command the
// compliance docs print from it, are only true while they name the ref
// the workflow actually runs on.
//
// Issue #510. This used to record @refs/tags/*, and the printed command
// pinned a regexp anchored on @refs/tags/. The workflow has never run on
// a tag: it publishes on a push to the release branch. Run against the
// published 0.2.0, the documented command failed with
//
//	Error: no matching signatures: none of the expected identities matched
//	what was in the certificate, got subjects
//	[https://github.com/backupdproject/backupd/.github/workflows/release.yml@refs/heads/release]
//	with issuer https://token.actions.githubusercontent.com
//
// and the certificate on that signature carries
// githubWorkflowRef=refs/heads/release. That is the most damaging thing a
// compliance record can do. It is not "verification is unavailable": it
// is a real, correctly signed release reported as unverifiable to
// somebody who followed our own instructions, whose reasonable conclusion
// is that the artifact was forged. The same command with the identity
// below passes against the same image.
//
// TestSigningIdentityMatchesTheWorkflowTriggerThatPublishes reads the
// workflow and refuses any disagreement between its trigger and these
// constants, so moving one forces the other.

const (
	// SigningWorkflowPath is the workflow whose OIDC identity signs a
	// release, relative to the repository root. It is half of the
	// certificate SAN, so it is a path that has to keep its name.
	SigningWorkflowPath = ".github/workflows/release.yml"

	// SigningWorkflowBranch is the branch a push to which publishes, and
	// therefore the ref the signing certificate is issued against. See
	// docs/release-branch.md for why `release` is the only branch that
	// can be it.
	SigningWorkflowBranch = "release"

	// SigningRepositoryURL is the repository the workflow lives in. It
	// repeats sourceRepository.url from compliance.json rather than
	// reading it, because this is a security contract that should be
	// readable in one place, and the test holds the two to each other.
	//
	// FR-41's cutover moved it. The repository was transferred from
	// `backupdproject/backupd` to `retnd/retnd` on 2026-09-15, and
	// GitHub builds the certificate SAN out of the repository the run
	// happened in, so every release published from here on carries the
	// identity below and every release already published carries
	// PreCutoverSigningIdentity. A transfer redirects a clone; it cannot
	// reach back into a certificate that was already issued.
	SigningRepositoryURL = "https://github.com/retnd/retnd"

	// SigningCertificateIssuer is the OIDC issuer a verifier pins
	// alongside the identity. Pinning the identity without the issuer
	// would accept the same-looking subject from any issuer Fulcio
	// trusts.
	SigningCertificateIssuer = "https://token.actions.githubusercontent.com"

	// SigningIdentity is the exact certificate SAN a release carries.
	//
	// Exact, not a regexp. The ref is a single fixed branch, so there is
	// nothing to match loosely, and an exact identity cannot be widened
	// by a missing anchor: the regexp this replaced ended at
	// `@refs/tags/` with no `$`, which would have accepted every tag ref
	// in the repository had any of them ever signed anything.
	SigningIdentity = SigningRepositoryURL + "/" + SigningWorkflowPath + "@refs/heads/" + SigningWorkflowBranch
)

// The pre-cutover identity, and the release boundary between the two.
//
// FR-41: `cosign verify` takes ONE `--certificate-identity`, and a
// signature carries the identity of the repository the run happened in.
// So after a transfer there is no single command that verifies both
// sides of it, and the documented command has to be two commands with a
// release number between them (ADR 0023, Decision 5.1). Pretending
// otherwise is #510 again from the other end: a correctly signed
// pre-cutover release reported as unverifiable because the reader was
// handed the new identity for it.
//
// These constants are NOT a deprecation shim and they have no removal
// release. A published signature is immutable, so this stays true for as
// long as anybody can pull 0.3.3.
const (
	// PreCutoverSigningRepositoryURL is the repository path releases up
	// to and including LastPreCutoverRelease were signed under.
	PreCutoverSigningRepositoryURL = "https://github.com/backupdproject/backupd"

	// PreCutoverSigningIdentity is the exact SAN those signatures carry.
	// The workflow path and the ref did not move at the cutover, only the
	// repository did, so this is built the same way.
	PreCutoverSigningIdentity = PreCutoverSigningRepositoryURL + "/" + SigningWorkflowPath + "@refs/heads/" + SigningWorkflowBranch

	// LastPreCutoverRelease is the boundary, and it is a version rather
	// than a date because a version is what a verifier holds. 0.3.3 is
	// the newest release published before the transfer; 0.4.0 was cut and
	// not pushed at the time of it, so the first release to carry the new
	// identity is the first one actually published afterwards.
	LastPreCutoverRelease = "0.3.3"
)

// SigningVerifyCommand is the command that verifies a published
// reference, as one line, so the provenance bundle and the compliance
// docs cannot print two different commands.
func SigningVerifyCommand(reference string) string {
	return strings.Join([]string{
		"cosign verify",
		"--certificate-oidc-issuer", SigningCertificateIssuer,
		"--certificate-identity", SigningIdentity,
		reference,
	}, " ")
}

// PreCutoverSigningVerifyCommand is the same command for a release
// published before the transfer, built through the same function so the
// two cannot drift into different shapes: the only difference between
// them is the identity, which is the whole point.
func PreCutoverSigningVerifyCommand(reference string) string {
	return strings.Join([]string{
		"cosign verify",
		"--certificate-oidc-issuer", SigningCertificateIssuer,
		"--certificate-identity", PreCutoverSigningIdentity,
		reference,
	}, " ")
}
