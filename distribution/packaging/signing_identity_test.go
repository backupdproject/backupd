// Does the identity a verifier is told to pin match the one the release
// workflow can actually produce?
//
// Issue #510. The bundle recorded @refs/tags/* and the docs printed a
// regexp anchored on @refs/tags/, while the workflow publishes on a push
// to `release`, so the certificate carries @refs/heads/release. Every
// piece of that was individually plausible and the combination was
// unverifiable: `cosign verify` against the published 0.2.0 with the
// documented command answered "no matching signatures", which reads to
// anyone running it as a forged artifact rather than as a wrong string in
// our own documentation.
//
// Nothing in the tree connected the two. The workflow's trigger and the
// recorded identity were separate literals in separate files, and the
// trigger had already moved once (workflow_dispatch on a tag, then a push
// to a branch) without anything noticing that the identity had not moved
// with it. This is that connection: the test reads the trigger out of the
// workflow and refuses any identity that a run of that workflow could not
// produce, so changing one forces the other.
package packaging

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// releaseWorkflowTriggers is the part of .github/workflows/release.yml
// that decides the ref a signing certificate is issued against.
//
// yaml.v3 resolves the bare `on` key as a string rather than as the YAML
// 1.1 boolean, so it is reachable by name and this stays a parse of the
// real file rather than a grep over it.
type releaseWorkflowTriggers struct {
	On struct {
		Push struct {
			Branches []string `yaml:"branches"`
			Tags     []string `yaml:"tags"`
		} `yaml:"push"`
		WorkflowDispatch map[string]any `yaml:"workflow_dispatch"`
	} `yaml:"on"`
}

func readReleaseWorkflowTriggers(t *testing.T) releaseWorkflowTriggers {
	t.Helper()
	raw, err := os.ReadFile(Path(SigningWorkflowPath))
	if err != nil {
		t.Fatalf("cannot read %s, which is the workflow whose identity signs a release: %v", SigningWorkflowPath, err)
	}
	var wf releaseWorkflowTriggers
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("cannot parse %s: %v", SigningWorkflowPath, err)
	}
	return wf
}

// TestSigningIdentityMatchesTheWorkflowTriggerThatPublishes is the guard
// #510 was missing.
//
// It is deliberately about the trigger and not about the string. Asserting
// that the identity ends in "@refs/heads/release" would have been just
// another literal to go stale beside the first one. Reading the branch out
// of the workflow means the failure arrives the moment the two disagree,
// and says which of them moved.
func TestSigningIdentityMatchesTheWorkflowTriggerThatPublishes(t *testing.T) {
	wf := readReleaseWorkflowTriggers(t)
	push := wf.On.Push

	if len(push.Tags) > 0 {
		t.Errorf("%s triggers on tags %v as well as branches, so a run can be signed under either a refs/tags/ or a refs/heads/ identity. "+
			"SigningIdentity is one exact string and cannot cover both. Either drop the tag trigger or make the recorded identity a regexp that covers every ref this workflow can run on, and change the documented command with it",
			SigningWorkflowPath, push.Tags)
	}
	if len(push.Branches) == 0 {
		t.Fatalf("%s declares no push branches, so nothing in the tree says which ref a signing certificate is issued against. "+
			"SigningIdentity claims %q; if the workflow no longer publishes on a push, the identity has to be rewritten for whatever does",
			SigningWorkflowPath, SigningIdentity)
	}
	if len(push.Branches) > 1 {
		t.Errorf("%s publishes on a push to any of %v. Each of those produces a different certificate identity, and SigningIdentity is one exact string, so at most one of them is verifiable with the documented command",
			SigningWorkflowPath, push.Branches)
	}

	branch := push.Branches[0]
	if branch != SigningWorkflowBranch {
		t.Errorf("%s publishes on a push to %q and SigningWorkflowBranch is %q.\n"+
			"The Fulcio certificate carries the ref of the run that asked for it, so a release cut on %q signs as @refs/heads/%s and the documented command pins @refs/heads/%s. "+
			"One of the two moved without the other: that is #510 happening again, and the visible symptom is a genuinely signed image that `cosign verify` reports as unverifiable",
			SigningWorkflowPath, branch, SigningWorkflowBranch, branch, branch, SigningWorkflowBranch)
	}

	want := SigningRepositoryURL + "/" + SigningWorkflowPath + "@refs/heads/" + branch
	if SigningIdentity != want {
		t.Errorf("SigningIdentity is\n  %s\nbut a push to %q in %s produces\n  %s", SigningIdentity, branch, SigningWorkflowPath, want)
	}
	if strings.Contains(SigningIdentity, "refs/tags/") {
		t.Errorf("SigningIdentity is %q, which pins a tag ref. %s has no tag trigger, so no release has ever been signed under one", SigningIdentity, SigningWorkflowPath)
	}
}

// TestSigningIdentityIsTheRepositoryComplianceDeclares catches the other
// half of the SAN. The ref is what #510 got wrong; the repository URL is
// the half that would break on a rename, and it is declared once in
// compliance.json already.
func TestSigningIdentityIsTheRepositoryComplianceDeclares(t *testing.T) {
	c := MustLoadCompliance()
	declared := strings.TrimSuffix(c.SourceRepository.URL, "/")
	if declared == "" {
		t.Fatal("compliance.json declares no sourceRepository.url, so there is nothing to hold the signing identity to")
	}
	if SigningRepositoryURL != declared {
		t.Errorf("SigningRepositoryURL is %q and compliance.json declares sourceRepository.url %q. "+
			"GitHub builds the certificate SAN from the repository the workflow ran in, so the identity a verifier pins has to be the same repository this record is about",
			SigningRepositoryURL, declared)
	}
	if !strings.HasPrefix(SigningIdentity, declared+"/") {
		t.Errorf("SigningIdentity %q is not under the declared repository %q", SigningIdentity, declared)
	}
}

// TestRecordedSigningIdentityIsTheOneWeVerifyWith holds the generated
// bundle and the printed command to the constants above, which is what
// makes the trigger check reach the artifacts a reader actually sees.
func TestRecordedSigningIdentityIsTheOneWeVerifyWith(t *testing.T) {
	canonical := MustLoad()
	p := readProvenance(t)

	if p.Signing.Identity != SigningIdentity {
		t.Errorf("%s records signing.identity\n  %s\nand the tree's identity is\n  %s\nRegenerate with: (cd distribution && go run ./cmd/provenance -write)",
			ProvenancePath, p.Signing.Identity, SigningIdentity)
	}
	if strings.Contains(p.Signing.Identity, "refs/tags/") {
		t.Errorf("%s records signing.identity %q, which pins a tag ref no release is signed under", ProvenancePath, p.Signing.Identity)
	}

	if p.Signing.Status == "signed" {
		want := "Verify with: " + SigningVerifyCommand(canonical.Image.Reference)
		if !containsLine(p.Signing.Note, want) {
			t.Errorf("%s records a signed image but none of its signing notes is the verification command:\n  want %s\n  got  %v",
				ProvenancePath, want, p.Signing.Note)
		}
	}
	for _, note := range p.Signing.Note {
		if strings.Contains(note, "refs/tags/") {
			t.Errorf("%s prints a signing note pinning a tag ref, which cannot match a certificate this workflow issues:\n  %s", ProvenancePath, note)
		}
	}
}

// TestComplianceDocsPrintTheCommandThatPasses is the last hop. The docs
// are where somebody outside the project reads the command, so a verified
// bundle and a stale doc is still #510 from their side of it.
//
// FR-41 (#895) made this two commands rather than one. The repository was
// transferred on 2026-09-15 and a certificate SAN is built out of the
// repository the run happened in, so a release published before the
// transfer verifies only against the old identity and one published after
// it only against the new one. `cosign verify` takes a single
// `--certificate-identity`, so there is no command that covers both and
// the doc has to present them keyed by version.
//
// Which means the rule here is no longer "every pin is the current
// identity". It is: every pin is one of exactly TWO known identities,
// both appear, and the release that divides them is named. A pin that is
// neither is still the #510 failure, and a doc that dropped one of the
// two would hand somebody the wrong identity for their release, which is
// the same failure with the same symptom.
func TestComplianceDocsPrintTheCommandThatPasses(t *testing.T) {
	const doc = "docs/compliance/release-provenance.md"
	raw, err := os.ReadFile(Path(doc))
	if err != nil {
		t.Fatalf("cannot read %s: %v", doc, err)
	}
	text := string(raw)

	if !strings.Contains(text, SigningCertificateIssuer) {
		t.Errorf("%s never names the OIDC issuer %s; pinning an identity without an issuer accepts the same subject from anywhere Fulcio trusts", doc, SigningCertificateIssuer)
	}

	// Every line that pins an identity, rather than every mention of one.
	// The prose above the command explains what #510 got wrong and has to
	// be able to name the ref it got wrong, so a blanket search for
	// "refs/tags/" would forbid the file from describing its own history.
	// What a reader copies is the flag.
	var pins []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "--certificate-identity") {
			pins = append(pins, strings.TrimSpace(line))
		}
	}
	if len(pins) == 0 {
		t.Fatalf("%s prints no --certificate-identity at all, so it does not tell a reader what to pin", doc)
	}

	var current, pre int
	for _, pin := range pins {
		if strings.Contains(pin, "refs/tags/") {
			t.Errorf("%s pins a tag ref:\n  %s\nNo release has been signed under one, so a reader following this gets a verification failure against a genuinely signed image, which is #510", doc, pin)
			continue
		}
		switch {
		case strings.Contains(pin, SigningIdentity):
			current++
		case strings.Contains(pin, PreCutoverSigningIdentity):
			pre++
		default:
			t.Errorf("%s pins\n  %s\nwhich is neither the identity this workflow produces now:\n  %s\nnor the one releases up to %s were signed under:\n  %s",
				doc, pin, SigningIdentity, LastPreCutoverRelease, PreCutoverSigningIdentity)
		}
	}

	if current == 0 {
		t.Errorf("%s pins no command carrying the identity this workflow produces (%s), so a reader verifying a release published after the transfer is told to pin an identity it cannot carry", doc, SigningIdentity)
	}
	if pre == 0 {
		t.Errorf("%s pins no command carrying %s, so a reader verifying %s or earlier gets \"no matching signatures\" against a correctly signed image, which is #510 with the identity moved instead of the ref",
			doc, PreCutoverSigningIdentity, LastPreCutoverRelease)
	}
	if !strings.Contains(text, LastPreCutoverRelease) {
		t.Errorf("%s prints two identities and never names %s, the release that divides them, so a reader cannot tell which of the two applies to the image they hold", doc, LastPreCutoverRelease)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
