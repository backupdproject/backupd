/**
 * The nine identifiers EPIC R's rename must NOT touch, named one at a
 * time, and the assertion that each is still spelled the way it is.
 *
 * `check-brand-drift.sh` sweeps this tree case-insensitively for the old
 * brand, and its own header calls these its principal false positive:
 * `BackupDetailPage` is the domain word `backup` followed by a capital
 * `D`, not the product's name, and so are eight more — 50 occurrences
 * between them, against another 9,800 uses of the word `backup`. R2.4
 * (#894) renamed 757 occurrences of `Backupd` across this workspace and
 * had to stop exactly here.
 *
 * scripts/rename/selftest.sh already plants all nine in a throwaway tree
 * and requires the GUARD to stay green on them. That is the other half of
 * the property and not this one: a guard that does not flag a name says
 * nothing about whether the name still exists. This file is the half that
 * goes red when a sweep takes one of them — rename `BackupDetailPage` to
 * `RetndDetailPage` and every assertion about that row fails.
 *
 * A LITERAL LIST, not a pattern. `/BackupD[a-zA-Z]/` over the tree would
 * pass on a tree where all nine had been renamed to `RetndDetail…` and
 * one new lookalike had been added, which is the failure this is for. So
 * every name below is written out, beside the file that carries it and
 * the full identifier it is part of, and both are asserted.
 *
 * The sources are pulled in with `?raw` rather than read through
 * `node:fs` on purpose: `ui/shared/tsconfig.json` deliberately ships no
 * `@types/node` (its own comment argues the case), and an import that
 * cannot resolve is itself a red — a file moving out from under a pinned
 * path is exactly as much a break as the identifier changing inside it.
 */
import { describe, expect, it } from "vitest";

import backupDetailPage from "@shared/pages/BackupDetailPage.tsx?raw";
import backupDefaultsPage from "@shared/pages/BackupDefaultsPage.tsx?raw";
import profileTest from "../../../../apps/common/platform/profile/profile_test.go?raw";
import parityTest from "../../../../apps/common/webhost/serve/parity_test.go?raw";
import composeContractTest from "../../../../distribution/compose/contract_test.go?raw";
import composeSeparationTest from "../../../../distribution/compose/separation_test.go?raw";
import sourceAdapterTest from "../../../../core/internal/backupengine/source/adapter_test.go?raw";
import workflowResumeTest from "../../../../core/internal/workflowrun/resume_test.go?raw";

/**
 * One protected name per row.
 *
 * `protected` is the name as #894 and the guard's false-positive list
 * spell it. `identifier` is the whole identifier in the tree, and for
 * four of the nine the two differ: the function is
 * `TestPrivateStateAndBackupDataAreSeparateMounts`, and the list's
 * `TestBackupDataAreSeparateMounts` is an abbreviation of it. So
 * `lookalike` is the third column and the load-bearing one — the exact
 * `BackupD…` run inside the identifier, which is the text a
 * case-insensitive sweep for the old brand sees and the text a careless
 * rename would rewrite. `declaration` is the line that declares the
 * identifier, so a name surviving only in a comment does not pass.
 */
const PROTECTED = [
  {
    protected: "BackupDetailPage",
    path: "ui/shared/src/pages/BackupDetailPage.tsx",
    source: backupDetailPage,
    identifier: "BackupDetailPage",
    lookalike: "BackupDetailPage",
    declaration: "export function BackupDetailPage("
  },
  {
    protected: "BackupDefaultsPage",
    path: "ui/shared/src/pages/BackupDefaultsPage.tsx",
    source: backupDefaultsPage,
    identifier: "BackupDefaultsPage",
    lookalike: "BackupDefaultsPage",
    declaration: "export function BackupDefaultsPage("
  },
  {
    protected: "BackupDomainPolicy",
    path: "apps/common/platform/profile/profile_test.go",
    source: profileTest,
    identifier: "TestProfileCarriesNoBackupDomainPolicy",
    lookalike: "BackupDomainPolicy",
    declaration: "func TestProfileCarriesNoBackupDomainPolicy("
  },
  {
    protected: "BackupDetail",
    path: "ui/shared/src/pages/BackupDetailPage.tsx",
    source: backupDetailPage,
    identifier: "BackupDetailPage",
    lookalike: "BackupDetail",
    declaration: "export function BackupDetailPage("
  },
  {
    protected: "BackupDomain",
    path: "apps/common/webhost/serve/parity_test.go",
    source: parityTest,
    identifier: "TestProfileSelectionIsInertForTheBackupDomain",
    lookalike: "BackupDomain",
    declaration: "func TestProfileSelectionIsInertForTheBackupDomain("
  },
  {
    protected: "TestBackupDataAreSeparateMounts",
    path: "distribution/compose/contract_test.go",
    source: composeContractTest,
    identifier: "TestPrivateStateAndBackupDataAreSeparateMounts",
    lookalike: "BackupDataAreSeparateMounts",
    declaration: "func TestPrivateStateAndBackupDataAreSeparateMounts("
  },
  {
    protected: "TestBackupDataOnEveryClaimedPlatform",
    path: "distribution/compose/separation_test.go",
    source: composeSeparationTest,
    identifier: "TestPrivateStateIsSeparateFromBackupDataOnEveryClaimedPlatform",
    lookalike: "BackupDataOnEveryClaimedPlatform",
    declaration: "func TestPrivateStateIsSeparateFromBackupDataOnEveryClaimedPlatform("
  },
  {
    protected: "TestBackupDoesNotReturnWhileAWorkerIsStillReading",
    path: "core/internal/backupengine/source/adapter_test.go",
    source: sourceAdapterTest,
    identifier: "TestBackupDoesNotReturnWhileAWorkerIsStillReading",
    lookalike: "BackupDoesNotReturnWhileAWorkerIsStillReading",
    declaration: "func TestBackupDoesNotReturnWhileAWorkerIsStillReading("
  },
  {
    protected: "TestBackupDoesNotLeaveItRunningForever",
    path: "core/internal/workflowrun/resume_test.go",
    source: workflowResumeTest,
    identifier: "TestACrashDuringTheBackupDoesNotLeaveItRunningForever",
    lookalike: "BackupDoesNotLeaveItRunningForever",
    declaration: "func TestACrashDuringTheBackupDoesNotLeaveItRunningForever("
  }
] as const;

describe("the nine identifiers the rename to retnd must not touch", () => {
  it("names exactly nine, so deleting a row is as visible as renaming one", () => {
    expect(PROTECTED.map((row) => row.protected)).toEqual([
      "BackupDetailPage",
      "BackupDefaultsPage",
      "BackupDomainPolicy",
      "BackupDetail",
      "BackupDomain",
      "TestBackupDataAreSeparateMounts",
      "TestBackupDataOnEveryClaimedPlatform",
      "TestBackupDoesNotReturnWhileAWorkerIsStillReading",
      "TestBackupDoesNotLeaveItRunningForever"
    ]);
  });

  for (const row of PROTECTED) {
    describe(row.protected, () => {
      it("is still declared in " + row.path, () => {
        expect(row.source, row.path).toContain(row.declaration);
      });

      it("still carries the protected spelling", () => {
        expect(row.identifier, row.protected).toContain(row.lookalike);
        expect(row.source, row.path).toContain(row.identifier);
      });

      /** The specific mutation #894's acceptance watches: the sweep
       *  reading `BackupD…` as the old brand and writing `Retnd…`. */
      it("has not been swept to a retnd spelling", () => {
        const swept = row.lookalike.replace("Backup", "Retnd");
        expect(swept).not.toBe(row.lookalike);
        expect(row.source, row.path + " contains " + swept).not.toContain(swept);
      });
    });
  }
});
