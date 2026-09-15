#!/usr/bin/env python3
"""Refuse a change to a published release's provenance record.

Issue #895 (R2.5), row R2.8 of `docs/conformance/epic-r-matrix.md`, and
FR-43's line about `provenance/**`:

    provenance/** records RELEASED artifacts and is append-only history;
    it is regenerated forward, never rewritten.

That sentence had no check behind it. `distribution/cmd/provenance`
regenerates the bundle and `TestComplianceArtifactsMatchThisTree` holds
the checked-in bytes to what this tree generates, which is the opposite
question: it asserts the record tracks the tree, and says nothing about
whether a fact about an artifact that has already been pushed to a
registry has been quietly restated. This is that check, and EPIC R is
why it is needed now rather than later: FR-41 moves the image reference
and re-issues the signing identity, and the honest way to do that is to
ADD a record for the releases published after the cutover, not to edit
0.4.0's to say it was published from an organisation that did not exist
when it shipped.

# WHAT IS IMMUTABLE, AND WHAT IS DELIBERATELY NOT

Most of `provenance/release-provenance.json` is a DERIVATION over the
current tree: the digests of `NOTICE`, the licence inventory, the SBOM
and the checksum manifest all move whenever a distributed artifact
changes, and they are supposed to. Measured over this file's own
history rather than assumed: `checksums.sha256` changed in 23 of the
commits that touched it, and `license.notice.sha256` in 10, while
`semanticVersion` stayed 0.4.0 and `published` stayed true throughout.
A guard that froze those fields would refuse every provider-artifact
edit in EPIC R's own Phase 2, which is how a guard gets deleted.

What cannot move is the record of what was PUSHED. A registry digest is
the identity a `cosign verify` pins and the thing an operator's compose
file resolves; restating it makes this project's compliance record
describe an artifact nobody can fetch. So four fields are frozen for a
version whose record says `published`:

    releaseManifest.published              true never becomes false
    releaseManifest.recordedBuildVersion   what the shipped binaries answer
    releaseManifest.architectures          what was built
    releaseManifest.registryDigests        what was pushed, per architecture

`imageReference` and `signing.identity` are NOT frozen, and that is a
decision rather than an omission: FR-41 re-issues the identity and moves
the registry path, and the documented `verify` command presents the old
identity for releases published before the cutover and the new one for
those after (FR-41's own words). Freezing them here would refuse the
cutover this epic is for. What keeps them honest is
`TestThePublishGateAgreesWithTheSigningIdentity` and
`TestOnlyTheReleaseRefCanPublish` in `distribution/packaging`, which is
where the identity's correctness already lives.

# WHY THE BASELINE IS `main` AND NOT THE FIRST RECORD EVER

0.4.0's registry digests were recorded three times before the release
was really out -- `b5825f60` recorded them, `aa752b0c` re-recorded after
a digest-recording commit re-triggered a publish, and `44d92fc7`
recorded the real final pair. Anchoring on the FIRST record of a version
would therefore report those three corrections, which were pre-release
fixes to a record of something that had not finished being published,
as rewrites. So the baseline is the merge base with `origin/main`: the
claim this enforces is FORWARD -- nothing that lands from here changes a
published fact -- which is exactly what "regenerated forward, never
rewritten" says.

# EXIT CODES

    0   no published fact moved (or this is a new release, or there is
        no baseline to compare against, which is stated rather than
        assumed)
    1   a published fact moved, and the run named the field, the value
        on the baseline and the value now
    2   the tool could not do its job: a bundle that will not parse, or
        a git invocation that failed

Run standalone:

    bash scripts/release/check-published-provenance.sh

Wired into `scripts/ci-local.sh` beside the other provenance guards and
into `.github/workflows/ci.yml`.
`scripts/bdtools/tests/published_provenance_guards.py` is the proof it
can still go red, and it plants both refusals.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

BUNDLE = "provenance/release-provenance.json"

# The fields frozen for a published version. Spelled as dotted paths
# rather than as nested lookups so a refusal can NAME the field the way
# a reader would find it in the file.
FROZEN = (
    "releaseManifest.published",
    "releaseManifest.recordedBuildVersion",
    "releaseManifest.architectures",
    "releaseManifest.registryDigests",
)


def run(args: list[str], cwd: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, cwd=cwd, capture_output=True, text=True)


def repo_root() -> Path:
    """The checkout this is being run against.

    From the CURRENT WORKING DIRECTORY, like the other guards in this
    directory and like `scripts/rename/check-brand-drift.sh`, which is
    what lets the self-test point it at a throwaway tree.
    """
    done = run(["git", "rev-parse", "--show-toplevel"], Path.cwd())
    if done.returncode != 0:
        raise SystemExit(f"check-published-provenance: not inside a git checkout: {done.stderr.strip()}")
    return Path(done.stdout.strip())


def baseline_ref(root: Path) -> str | None:
    """The revision whose record this run is held to.

    `PUBLISHED_PROVENANCE_BASELINE` overrides it, which is how the
    self-test pins a throwaway repository's baseline to a commit it
    made itself. Otherwise the merge base with `origin/main`, falling
    back to `origin/main` and then to `HEAD`: on a branch that is what
    the branch started from, and on `main` it is `HEAD`, so an
    uncommitted rewrite is still caught.
    """
    override = os.environ.get("PUBLISHED_PROVENANCE_BASELINE")
    if override:
        return override

    for candidate in (["git", "merge-base", "HEAD", "origin/main"], ["git", "rev-parse", "origin/main"]):
        done = run(candidate, root)
        if done.returncode == 0 and done.stdout.strip():
            return done.stdout.strip()

    done = run(["git", "rev-parse", "HEAD"], root)
    if done.returncode == 0 and done.stdout.strip():
        return done.stdout.strip()
    return None


def bundle_at(root: Path, ref: str) -> dict[str, Any] | None:
    """The bundle as of `ref`, or None if that revision has none."""
    done = run(["git", "show", f"{ref}:{BUNDLE}"], root)
    if done.returncode != 0:
        return None
    try:
        return json.loads(done.stdout)
    except json.JSONDecodeError as err:
        raise SystemExit(f"check-published-provenance: {BUNDLE} at {ref} is not JSON: {err}") from err


def current_bundle(root: Path) -> dict[str, Any]:
    path = root / BUNDLE
    try:
        return json.loads(path.read_text())
    except FileNotFoundError:
        raise SystemExit(f"check-published-provenance: {BUNDLE} does not exist in {root}") from None
    except json.JSONDecodeError as err:
        raise SystemExit(f"check-published-provenance: {BUNDLE} is not JSON: {err}") from err


def field(bundle: dict[str, Any], dotted: str) -> Any:
    """One frozen field, by its dotted path, or None if absent.

    None for absent rather than raising, because a field DISAPPEARING
    from a published record is itself a rewrite and has to be reportable
    as one rather than as a crash.
    """
    node: Any = bundle
    for part in dotted.split("."):
        if not isinstance(node, dict) or part not in node:
            return None
        node = node[part]
    return node


def render(value: Any) -> str:
    if isinstance(value, (dict, list)):
        return json.dumps(value, sort_keys=True)
    return json.dumps(value)


def main() -> int:
    root = repo_root()
    current = current_bundle(root)

    ref = baseline_ref(root)
    if ref is None:
        print("check-published-provenance: no baseline revision to compare against, so nothing is claimed")
        return 0

    before = bundle_at(root, ref)
    if before is None:
        print(f"check-published-provenance: {ref[:12]} carries no {BUNDLE}, so there is no published record to protect")
        return 0

    if not field(before, "releaseManifest.published"):
        print(
            f"check-published-provenance: the record at {ref[:12]} is "
            f"{render(field(before, 'semanticVersion'))} and is not published, so nothing is frozen yet"
        )
        return 0

    was = field(before, "semanticVersion")
    now = field(current, "semanticVersion")
    if was != now:
        print(
            f"check-published-provenance: the bundle moved from {render(was)} to {render(now)}, "
            "which is a release being cut rather than a published record being restated"
        )
        return 0

    moved = [(name, field(before, name), field(current, name)) for name in FROZEN]
    moved = [(name, b, c) for name, b, c in moved if b != c]
    if not moved:
        print(
            f"check-published-provenance: ok ({render(now)} is published and all "
            f"{len(FROZEN)} of its published-artifact facts are unchanged since {ref[:12]})"
        )
        return 0

    print(
        f"check-published-provenance: FAILED: {BUNDLE} restates a fact about {render(now)}, "
        "which is already published:",
        file=sys.stderr,
    )
    for name, b, c in moved:
        print(f"  {name}\n    published as: {render(b)}\n    now says:     {render(c)}", file=sys.stderr)
    print(
        "\ncheck-published-provenance: provenance/** records artifacts that have been PUSHED.\n"
        "  A registry digest is what a `cosign verify` pins and what an operator's compose\n"
        "  file resolves, so restating one makes this project's compliance record describe\n"
        "  an artifact nobody can fetch. Regenerate FORWARD: cut a release, which gives the\n"
        "  bundle a new semanticVersion and a new set of digests, rather than editing the\n"
        "  record of one that shipped (FR-43, and row R2.8 of\n"
        "  docs/conformance/epic-r-matrix.md).\n"
        "  The derived digests -- NOTICE, the licence inventory, the SBOM, the checksum\n"
        "  manifest -- are NOT frozen and are not what this refused; they track the tree on\n"
        "  purpose. Run (cd distribution && go run ./cmd/provenance -write) for those.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except SystemExit as err:
        if isinstance(err.code, str):
            print(err.code, file=sys.stderr)
            sys.exit(2)
        raise
