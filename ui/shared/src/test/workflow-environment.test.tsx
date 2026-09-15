/**
 * The workflow environment editor and its merged preview (issue #814,
 * screen 4).
 *
 * Two properties, and both of them are the kind that a screen can be
 * shipped without and nobody notices until a hook misbehaves:
 *
 *   - PRECEDENCE is visible. `sanitized baseline < deployment < backup
 *     set < BACKUPD_* built-ins`. An operator who cannot see that a
 *     set-scope PGHOST is hiding the deployment's has no way to explain a
 *     hook connecting to the wrong database.
 *   - a SECRET is never shown. Not in the editor, not in the preview, not
 *     behind a reveal control. The wire carries a location and there is
 *     no field on the contract a resolved value could arrive in, so the
 *     assertion here is that the location is what renders and the
 *     rendered text never contains anything value-shaped.
 *
 * The merge itself is also asserted without a DOM, because what is being
 * checked is true of every row and a rendered sweep only sees the rows a
 * fixture happens to carry.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { WorkflowEnvironmentEditor } from "@shared/components/WorkflowEnvironmentEditor";
import {
  BUILTIN_ENV_NAMES,
  isReservedEnvName,
  mergeWorkflowEnvironment
} from "@shared/components/workflowPresentation";
import { BackupdError } from "@shared/api/contracts";
import type { WorkflowEnvVariable, WorkflowEnvVariableInput } from "@shared/api/contracts";

const GLOBAL: WorkflowEnvVariable[] = [
  { name: "PGHOST", value: "postgres-primary.internal", hasValue: true },
  { name: "PGPASSWORD", hasValue: false, secret: { file: "/etc/backupd/secrets/pg" } }
];

const SET: WorkflowEnvVariable[] = [
  { name: "PGHOST", value: "postgres-replica.internal", hasValue: true },
  { name: "DUMP_LEVEL", value: "", hasValue: true }
];

/** The editor's two write props, named so a `vi.fn` standing in for one
 *  keeps its parameter list: an inferred zero-argument mock type-checks
 *  here and then cannot be asked what it was called with. */
type SetVariable = (name: string, entry: WorkflowEnvVariableInput) => Promise<unknown>;
type UnsetVariable = (name: string) => Promise<unknown>;

function renderEditor(
  scope: "global" | "set",
  overrides: Partial<{ onSet: SetVariable; onUnset: UnsetVariable }> = {}
) {
  const onSet = overrides.onSet ?? (() => Promise.resolve(undefined));
  const onUnset = overrides.onUnset ?? (() => Promise.resolve(undefined));
  return render(
    <WorkflowEnvironmentEditor
      scope={scope}
      global={GLOBAL}
      set={scope === "set" ? SET : null}
      readOnly={false}
      onSet={onSet}
      onUnset={onUnset}
    />
  );
}

/** One row of either table, found by the variable name in its first
 *  cell, so a row is addressed by what it is about rather than by index.
 *  `nth` picks between the configured table and the preview, which both
 *  carry a row for the same name. */
function rowFor(name: string, nth = 0): HTMLElement {
  const cells = screen.getAllByText(name).filter((node) => node.tagName === "TD");
  const row = cells[nth]?.closest("tr");
  if (!row) throw new Error("no row " + nth + " for " + name);
  return row;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("the merge", () => {
  it("gives a backup set's value precedence over the deployment's, and records what it shadowed", () => {
    const merged = mergeWorkflowEnvironment(GLOBAL, SET);
    const pghost = merged.find((row) => row.name === "PGHOST");

    expect(pghost?.source).toBe("set");
    expect(pghost?.entry?.value).toBe("postgres-replica.internal");
    expect(pghost?.shadowed).toEqual(["global"]);
  });

  it("leaves a deployment-only variable at the deployment scope, shadowing nothing", () => {
    const merged = mergeWorkflowEnvironment(GLOBAL, SET);
    const password = merged.find((row) => row.name === "PGPASSWORD");

    expect(password?.source).toBe("global");
    expect(password?.shadowed).toEqual([]);
    expect(password?.secret).toBe(true);
  });

  it("puts every built-in in the merge, after the configured entries, and shadowable by neither scope", () => {
    const merged = mergeWorkflowEnvironment(GLOBAL, SET);
    const builtins = merged.filter((row) => row.source === "builtin");

    expect(builtins.map((row) => row.name)).toEqual([...BUILTIN_ENV_NAMES]);
    // Last, so an operator scanning for a variable they wrote does not
    // read past eighteen of ours.
    expect(merged.slice(-builtins.length).every((row) => row.source === "builtin")).toBe(true);
    // No built-in carries a configured entry, which is what makes it
    // unrenderable as a value: this product supplies it per run.
    expect(builtins.every((row) => row.entry === undefined)).toBe(true);
  });

  it("reports a configured name that collides with a built-in as the losing one", () => {
    // The service refuses such a write, so this is a defensive reading of
    // a configuration written before the reservation. What must not
    // happen is the operator's value being reported as effective.
    const merged = mergeWorkflowEnvironment(
      [{ name: "RETND_RUN_ID", value: "mine", hasValue: true }],
      []
    );
    const row = merged.find((entry) => entry.name === "RETND_RUN_ID" && entry.source === "builtin");

    expect(row?.shadowed).toEqual(["global"]);
    expect(row?.entry).toBeUndefined();
  });

  it("reserves the whole BACKUPD_ namespace and the bare name, not just today's list", () => {
    expect(isReservedEnvName("BACKUPD")).toBe(true);
    expect(isReservedEnvName("RETND_RUN_ID")).toBe(true);
    // The point of a prefix rule: a name this build has never heard of is
    // reserved too, so an operator cannot configure one that silently
    // stops working when the product starts setting it.
    expect(isReservedEnvName("RETND_SOMETHING_NEW")).toBe(true);
    expect(isReservedEnvName("PGHOST")).toBe(false);
    expect(isReservedEnvName("MY_BACKUPD_FLAG")).toBe(false);
  });
});

describe("what the editor renders", () => {
  it("shows a secret's location and never a value, in both tables", () => {
    renderEditor("set");

    // The preview is the only table carrying PGPASSWORD here, since it is
    // configured at the deployment scope and this is the set's editor.
    const preview = rowFor("PGPASSWORD");
    // The LOCATION, and the statement that the value is not shown. The
    // location is the answer an operator needs ("which file does this
    // hook's password come from"); the value is something no read on
    // this API can carry at all.
    expect(within(preview).getByText(/from file \/etc\/backupd\/secrets\/pg/)).toBeTruthy();
    expect(within(preview).getByText(/never shown/)).toBeTruthy();
    // And no control offers to reveal one.
    expect(screen.queryByRole("button", { name: /reveal|show value/i })).toBeNull();
  });

  it("draws a deliberately empty variable as empty rather than as absent", () => {
    renderEditor("set");

    const configured = rowFor("DUMP_LEVEL");
    expect(within(configured).getByText("(empty)")).toBeTruthy();
  });

  it("says which scope won and what it shadowed", () => {
    renderEditor("set");

    // Two rows for PGHOST: the editable one and the preview's.
    const preview = rowFor("PGHOST", 1);
    expect(within(preview).getByText("Backup Set")).toBeTruthy();
    expect(within(preview).getByText(/shadows the global value/)).toBeTruthy();
  });

  it("marks a built-in read-only, with no control that could change it", () => {
    renderEditor("set");

    const builtin = rowFor("RETND_RUN_ID");
    expect(within(builtin).getByText("Built-in")).toBeTruthy();
    expect(within(builtin).getByText("set per run")).toBeTruthy();
    expect(within(builtin).getByText("cannot be overridden")).toBeTruthy();
    expect(within(builtin).queryByRole("button")).toBeNull();
  });

  it("offers no Unset for a variable this scope does not configure", () => {
    // The deployment's PGPASSWORD is not this set's to remove, and a
    // control here would send a DELETE against a name this scope has
    // never held.
    renderEditor("set");

    expect(screen.getAllByRole("button", { name: "Unset" }).length).toBe(SET.length);
  });
});

describe("writing a variable", () => {
  it("sends an empty literal as a literal, not as an absent one", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn<SetVariable>(() => Promise.resolve(undefined));
    renderEditor("global", { onSet });

    await user.type(screen.getByLabelText("Name"), "DUMP_LEVEL");
    await user.click(screen.getByRole("button", { name: "Save variable" }));

    await waitFor(() => expect(onSet).toHaveBeenCalledTimes(1));
    // `{ value: "" }` and not `{}`: an empty variable is a real
    // configuration and an absent literal means the value comes from a
    // reference.
    expect(onSet.mock.calls[0]).toEqual(["DUMP_LEVEL", { value: "" }]);
  });

  it("sends a secret as a location, never as a value", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn<SetVariable>(() => Promise.resolve(undefined));
    renderEditor("global", { onSet });

    await user.type(screen.getByLabelText("Name"), "PGPASSWORD");
    await user.selectOptions(
      screen.getByLabelText("Where the value comes from"),
      "Read from a file on the host"
    );
    await user.type(screen.getByLabelText("Location"), "/etc/backupd/secrets/pg");
    await user.click(screen.getByRole("button", { name: "Save variable" }));

    await waitFor(() => expect(onSet).toHaveBeenCalledTimes(1));
    expect(onSet.mock.calls[0]).toEqual([
      "PGPASSWORD",
      { secret: { file: "/etc/backupd/secrets/pg" } }
    ]);
  });

  it("sends a command reference as an argv rather than a shell line", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn<SetVariable>(() => Promise.resolve(undefined));
    renderEditor("global", { onSet });

    await user.type(screen.getByLabelText("Name"), "PGPASSWORD");
    await user.selectOptions(
      screen.getByLabelText("Where the value comes from"),
      "Output of a command"
    );
    await user.type(screen.getByLabelText("Command"), "/usr/local/bin/read-secret pg");
    await user.click(screen.getByRole("button", { name: "Save variable" }));

    await waitFor(() => expect(onSet).toHaveBeenCalledTimes(1));
    expect(onSet.mock.calls[0][1]).toEqual({
      secret: { command: ["/usr/local/bin/read-secret", "pg"] }
    });
  });

  it("refuses a reserved name in front of the request, with the reason", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn<SetVariable>(() => Promise.resolve(undefined));
    renderEditor("global", { onSet });

    await user.type(screen.getByLabelText("Name"), "RETND_BACKUP_STATUS");

    expect(screen.getByRole("button", { name: "Save variable" })).toBeDisabled();
    expect(screen.getByText(/set by this product from the run it belongs to/)).toBeTruthy();
    expect(onSet).not.toHaveBeenCalled();
  });

  it("reports a refused write in the service's own words rather than clearing the form", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn<SetVariable>(() =>
      Promise.reject(
        new BackupdError({
          code: "INVALID_REQUEST",
          message: "this value contains a NUL byte",
          correlationId: "cid_env400"
        })
      )
    );
    renderEditor("global", { onSet });

    await user.type(screen.getByLabelText("Name"), "PGHOST");
    await user.click(screen.getByRole("button", { name: "Save variable" }));

    await waitFor(() =>
      expect(screen.getByText(/this value contains a NUL byte/)).toBeTruthy()
    );
    // The draft survives the refusal: an operator retypes nothing.
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("PGHOST");
  });
});
