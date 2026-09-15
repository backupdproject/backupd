/**
 * The two npm scopes this repository publishes under, asserted because
 * nothing else in the gate does.
 *
 * R2.4 (#894) moved `@backupd/ui-shared` and
 * `@backupd/provider-conformance` to `@retnd/*`, and the acceptance for it
 * assumed a lockfile left behind on the old scope would fail the install.
 * It does not, and that was checked rather than assumed: both packages are
 * `private` and unpublished, nothing in the tree depends on either by
 * name, and `npm ci` validates the dependency graph rather than the root
 * `name`, so a `package-lock.json` still saying `@backupd/ui-shared`
 * against a `package.json` saying `@retnd/ui-shared` installs all 271
 * packages and exits 0.
 *
 * `check-brand-drift.sh` does not catch it either, and will not until the
 * end of EPIC R: `@backupd/` reduces to the token `backupd`, which is on
 * the guard's `pending` list and therefore allowed anywhere for as long as
 * the sweep is in flight.
 *
 * So the published identity of these two packages is gated here, where
 * `npm test` already runs, and the drift the acceptance describes turns
 * this red.
 */
import { describe, expect, it } from "vitest";

import uiSharedPackage from "../../package.json?raw";
import uiSharedLock from "../../package-lock.json?raw";
import conformancePackage from "../../../../apps/common/tests/package.json?raw";
import conformanceLock from "../../../../apps/common/tests/package-lock.json?raw";

/** The `name` of a parsed JSON document, narrowed rather than asserted:
 *  a document that is not an object, or carries no `name`, reads as
 *  `undefined` and fails the assertion that wanted one. */
function nameOf(node: unknown): unknown {
  if (node === null || typeof node !== "object" || !("name" in node)) return undefined;
  return node.name;
}

/** A lockfile's `packages[""]`, which is the root package's own entry and
 *  the second place the name is written down. */
function rootEntry(lock: unknown): unknown {
  if (lock === null || typeof lock !== "object" || !("packages" in lock)) return undefined;
  const packages = lock.packages;
  if (packages === null || typeof packages !== "object" || !("" in packages)) return undefined;
  return packages[""];
}

const WORKSPACES = [
  {
    path: "ui/shared",
    name: "@retnd/ui-shared",
    manifest: uiSharedPackage,
    lock: uiSharedLock
  },
  {
    path: "apps/common/tests",
    name: "@retnd/provider-conformance",
    manifest: conformancePackage,
    lock: conformanceLock
  }
] as const;

describe("the npm scopes", () => {
  for (const workspace of WORKSPACES) {
    describe(workspace.path, () => {
      it("publishes as " + workspace.name, () => {
        expect(nameOf(JSON.parse(workspace.manifest))).toBe(workspace.name);
      });

      /** A lockfile records the root name twice, at the top level and
       *  under `packages[""]`, and the two drift independently. */
      it("has a lockfile recording the same name in both places", () => {
        const lock: unknown = JSON.parse(workspace.lock);
        expect(nameOf(lock)).toBe(workspace.name);
        expect(nameOf(rootEntry(lock))).toBe(workspace.name);
      });

      it("is on no scope but @retnd", () => {
        expect(workspace.manifest).not.toContain("@backupd/");
        expect(workspace.lock).not.toContain("@backupd/");
      });
    });
  }
});
