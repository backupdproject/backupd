#!/usr/bin/env python3
"""Issue #895 (R2.5), row R2.8: prove the published-provenance guard can go red.

`scripts/bdtools/release/check_published_provenance.py` is an assertion
that something never happens, and row R2.8 of
`docs/conformance/epic-r-matrix.md` will not accept one that has never
been watched to fire: "the falsification is a diff touching a published
record". So this drives the real check, unchanged, in throwaway `git
init` repositories -- one per case -- and requires the exact refusal
rather than only a non-zero exit.

The exit code alone would prove nothing here, because every refusal the
check has exits 1. A code-only assertion could not tell "a registry
digest was restated" from "a published release was marked unpublished",
which are different defects with different fixes, and it would pass
against a check that refused every bundle it was ever handed.

The three POSITIVE controls are the half that makes the negatives mean
something:

  * an untouched published record passes, so these cases are not green
    against a guard that refuses everything;
  * a bundle whose DERIVED digests moved -- `checksums.sha256`,
    `license.notice.sha256`, `sbom.sha256`, `license.inventory.sha256`
    -- passes, because those track the tree on purpose and freezing
    them would refuse every provider-artifact edit in EPIC R's own
    Phase 2. This is the control that keeps the guard narrow enough to
    survive;
  * a bundle whose `semanticVersion` moved passes, because that is a
    release being cut and is the forward growth FR-43 asks for rather
    than a rewrite.

And one control on the field set itself: `imageReference` and
`signing.identity` moving must NOT be refused, because FR-41 re-issues
the identity and moves the registry path at the cutover. A guard that
froze those would refuse the very change this epic exists to make, and
that case exists so the omission is deliberate and visible rather than
an oversight somebody restores later.

The baseline comes in through `PUBLISHED_PROVENANCE_BASELINE`, which is
how a throwaway repository pins the commit its own record was written
at without needing an `origin/main`.
"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent.parent.parent))

from bdtools.tests.testkit import Suite, run

CHECK = Path(__file__).resolve().parent.parent.parent / "release" / "check-published-provenance.sh"

# A minimal bundle in the real file's shape: enough of it that every
# frozen field and every deliberately-unfrozen one is present, and not
# so much that a reader has to diff it against the real thing to see
# what a case mutated.
PUBLISHED = {
    "schema": "backupd/release-provenance/1",
    "semanticVersion": "0.4.0",
    "imageReference": "ghcr.io/retnd/retnd:0.4.0",
    "releaseManifest": {
        "path": "container/release-manifest.json",
        "sha256": "a" * 64,
        "commit": "b" * 40,
        "recordedBuildVersion": "0.4.0",
        "architectures": ["amd64", "arm64"],
        "published": True,
        "registryDigests": {"amd64": "sha256:" + "c" * 64, "arm64": "sha256:" + "d" * 64},
        "versionIsABuildStamp": False,
    },
    "license": {
        "file": {"path": "LICENSE", "sha256": "e" * 64},
        "notice": {"path": "NOTICE", "sha256": "f" * 64},
        "inventory": {"path": "provenance/third-party-licenses.json", "sha256": "1" * 64},
    },
    "sbom": {"path": "provenance/sbom.spdx.json", "sha256": "2" * 64},
    "checksums": {"path": "provenance/checksums.txt", "sha256": "3" * 64},
    "signing": {
        "status": "signed",
        "method": "sigstore-keyless",
        "identity": "https://github.com/retnd/retnd/.github/workflows/release.yml@refs/heads/release",
    },
}

BUNDLE = "provenance/release-provenance.json"


def git(repo: Path, *args: str) -> None:
    done = subprocess.run(["git", *args], cwd=repo, capture_output=True, text=True)
    if done.returncode != 0:
        raise SystemExit(f"published-provenance guards: git {' '.join(args)} failed in {repo}: {done.stderr.strip()}")


def new_repo(tmpdirs: list[str], bundle: dict[str, Any]) -> tuple[Path, str]:
    """A throwaway checkout whose one commit carries `bundle`.

    Returns the repository and the sha of that commit, which is what the
    cases hand the check as its baseline.
    """
    root = Path(tempfile.mkdtemp(prefix="published-provenance-"))
    tmpdirs.append(str(root))
    git(root, "init", "--quiet")
    git(root, "config", "user.email", "gate@example.invalid")
    git(root, "config", "user.name", "gate")
    (root / "provenance").mkdir()
    write(root, bundle)
    git(root, "add", "-A")
    git(root, "commit", "--quiet", "-m", "the published record")
    done = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root, capture_output=True, text=True)
    return root, done.stdout.strip()


def write(root: Path, bundle: dict[str, Any]) -> None:
    (root / BUNDLE).write_text(json.dumps(bundle, indent=2) + "\n")


def mutated(**overrides: Any) -> dict[str, Any]:
    """A deep copy of the published bundle with dotted paths replaced."""
    out: dict[str, Any] = json.loads(json.dumps(PUBLISHED))
    for dotted, value in overrides.items():
        node = out
        parts = dotted.replace("__", ".").split(".")
        for part in parts[:-1]:
            node = node[part]
        node[parts[-1]] = value
    return out


def check(repo: Path, baseline: str) -> tuple[str, int]:
    return run(
        ["bash", str(CHECK)],
        cwd=repo,
        env={"PATH": "/usr/bin:/bin:/usr/local/bin", "PUBLISHED_PROVENANCE_BASELINE": baseline},
    )


def main() -> int:
    suite = Suite("published-provenance guards")
    tmpdirs: list[str] = []
    try:
        # --- The positive control, first, so a red run below means the
        # --- mutation and not the harness.
        repo, base = new_repo(tmpdirs, PUBLISHED)
        out, rc = check(repo, base)
        suite.check(
            rc == 0,
            "an untouched published record passes",
            f"an untouched record was refused (exit {rc})",
            out,
        )
        suite.assert_contains("and says which version it held frozen", '"0.4.0" is published', out)

        # --- Refusal 1: a registry digest restated.
        repo, base = new_repo(tmpdirs, PUBLISHED)
        restated = {"amd64": "sha256:" + "9" * 64, "arm64": "sha256:" + "d" * 64}
        write(repo, mutated(releaseManifest__registryDigests=restated))
        out, rc = check(repo, base)
        suite.check(rc == 1, "a restated registry digest is refused", f"a restated digest exited {rc}", out)
        suite.assert_contains("and the refusal names the field", "releaseManifest.registryDigests", out)
        suite.assert_contains("and prints what was published", "published as:", out)
        suite.assert_not_contains("and does not blame the derived digests", "checksums.sha256", out)

        # --- Refusal 2: a published release marked unpublished.
        repo, base = new_repo(tmpdirs, PUBLISHED)
        write(repo, mutated(releaseManifest__published=False))
        out, rc = check(repo, base)
        suite.check(rc == 1, "un-publishing a published release is refused", f"un-publishing exited {rc}", out)
        suite.assert_contains("and the refusal names that field", "releaseManifest.published", out)

        # --- Refusal 3: the architectures or the recorded build version.
        repo, base = new_repo(tmpdirs, PUBLISHED)
        write(repo, mutated(releaseManifest__architectures=["amd64"]))
        out, rc = check(repo, base)
        suite.check(rc == 1, "dropping a built architecture is refused", f"dropping an architecture exited {rc}", out)

        repo, base = new_repo(tmpdirs, PUBLISHED)
        write(repo, mutated(releaseManifest__recordedBuildVersion="0.4.1"))
        out, rc = check(repo, base)
        suite.check(
            rc == 1,
            "restating what the shipped binaries answer with is refused",
            f"a restated recordedBuildVersion exited {rc}",
            out,
        )

        # --- Control: the derived digests are supposed to move.
        repo, base = new_repo(tmpdirs, PUBLISHED)
        write(
            repo,
            mutated(
                checksums__sha256="7" * 64,
                sbom__sha256="7" * 64,
                license__notice__sha256="7" * 64,
                license__inventory__sha256="7" * 64,
            ),
        )
        out, rc = check(repo, base)
        suite.check(
            rc == 0,
            "the four derived digests moving is not a rewrite",
            f"a regeneration of NOTICE, the inventory, the SBOM and the checksums was refused (exit {rc})",
            out,
        )

        # --- Control: FR-41's two references must stay movable.
        repo, base = new_repo(tmpdirs, PUBLISHED)
        write(
            repo,
            mutated(
                imageReference="ghcr.io/retnd/retnd:0.4.0",
                signing__identity="https://github.com/retnd/retnd/.github/workflows/release.yml@refs/heads/release",
            ),
        )
        out, rc = check(repo, base)
        suite.check(
            rc == 0,
            "FR-41's image reference and signing identity are not frozen here",
            f"the cutover's own two references were refused (exit {rc}), which would refuse FR-41 itself",
            out,
        )

        # --- Control: a release being cut is forward growth.
        repo, base = new_repo(tmpdirs, PUBLISHED)
        cut = {"amd64": "sha256:" + "8" * 64}
        write(repo, mutated(semanticVersion="0.5.0", releaseManifest__registryDigests=cut))
        out, rc = check(repo, base)
        suite.check(rc == 0, "cutting a new release is not a rewrite", f"a new release was refused (exit {rc})", out)
        suite.assert_contains("and the run says so", "a release being cut", out)

        # --- The unpublished baseline: nothing is frozen before a push.
        repo, base = new_repo(tmpdirs, mutated(releaseManifest__published=False))
        write(repo, mutated(releaseManifest__published=False, releaseManifest__registryDigests={}))
        out, rc = check(repo, base)
        suite.check(
            rc == 0,
            "a record that was never published freezes nothing",
            f"an unpublished baseline was treated as frozen (exit {rc})",
            out,
        )
        suite.assert_contains("and says why", "is not published", out)
    finally:
        for d in tmpdirs:
            shutil.rmtree(d, ignore_errors=True)

    return suite.finish()


if __name__ == "__main__":
    sys.exit(main())
