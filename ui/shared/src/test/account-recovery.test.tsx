import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { UserEvent } from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { httpApi } from "@shared/api/client";
import { RetndError } from "@shared/api/contracts";
import type { RetndApi } from "@shared/api/contracts";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import { App } from "@shared/App";
import { EnrollmentPage } from "@shared/auth/EnrollmentPage";
import { ResetPasswordPage } from "@shared/auth/ResetPasswordPage";
import { SettingsPage } from "@shared/pages/SettingsPage";
import type { VersionInfo } from "@shared/types/operation";

/**
 * Issue #830: account recovery, asserted where getting it wrong costs an
 * operator the instance.
 *
 * Four properties are worth pinning and each is a real failure somebody
 * would otherwise ship:
 *
 *   - Enrolment collects the whole mail endpoint and sends exactly what
 *     was typed. A wizard that drops the port, or the security mode, or
 *     sends the confirm field as the password, produces an account whose
 *     recovery has never worked and whose owner is told it does.
 *   - A refused SEND is not a refused enrolment. The service writes
 *     nothing and does not spend the one-time token, so the page has to
 *     say the link still works; told only that creation failed, the
 *     reasonable next move is hunting for a fresh link nothing will
 *     print.
 *   - A reset link is single-use, so a page that submits a mistyped
 *     password spends it on something nobody knows, and a page that
 *     reports an expired link without offering another leaves the
 *     operator at a dead end.
 *   - A blank SMTP password means KEEP the stored one. There is no read
 *     that could prefill that field, so blank is what an untouched form
 *     holds, and a client that sent it as "" would clear the stored
 *     credential every time somebody corrected a port.
 *
 * The last one is asserted twice on purpose, at the two layers where it
 * can break independently: the card passing the blank through, and the
 * client omitting the field from the body rather than sending an empty
 * string.
 */

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

const PASSPHRASE = "a-long-enough-passphrase";

function renderEnrollment(api: RetndApi) {
  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <EnrollmentPage onEnrolled={() => {}} />
      </ApiProvider>
    </MemoryRouter>
  );
}

/** Everything except the recovery address, which each case supplies
 *  itself: the address is the field under test in half of them. */
async function fillCredentialsAndSmtp(user: UserEvent) {
  await user.type(screen.getByLabelText("Username"), "bm-admin");
  await user.type(screen.getByLabelText(/^Password/), PASSPHRASE);
  await user.type(screen.getByLabelText("Confirm password"), PASSPHRASE);
  await user.type(screen.getByLabelText("SMTP host"), "smtp.example.net");
  await user.clear(screen.getByLabelText("Port"));
  await user.type(screen.getByLabelText("Port"), "2525");
  await user.selectOptions(screen.getByLabelText("Security"), "tls");
  await user.type(screen.getByLabelText("SMTP username"), "relay-user");
  await user.type(screen.getByLabelText(/^SMTP password/), "relay-secret");
  await user.type(screen.getByLabelText("From address"), "retnd@example.com");
}

const createButton = () => screen.getByRole("button", { name: "Create administrator" });

describe("first-run enrolment collects the way back into the account", () => {
  afterEach(() => {
    resetMockFixtures();
    cleanup();
    vi.restoreAllMocks();
  });

  it("will not submit an address that is not one, and says so at the field", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const enroll = vi.spyOn(api, "enrollAdministrator");
    renderEnrollment(api);

    await fillCredentialsAndSmtp(user);
    await user.type(screen.getByLabelText("Recovery email"), "ops-at-example.com");

    expect(createButton()).toBeDisabled();
    // The complaint reaches a screen reader, not only the page: it is
    // referenced by the field it is about.
    const field = screen.getByLabelText("Recovery email");
    const described = (field.getAttribute("aria-describedby") ?? "")
      .split(/\s+/)
      .map((id) => document.getElementById(id)?.textContent ?? "")
      .join(" ");
    expect(described).toContain("Enter an email address");

    await user.click(createButton());
    expect(enroll).not.toHaveBeenCalled();
  });

  it("sends the credentials, the address and every SMTP field it collected", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const enroll = vi.spyOn(api, "enrollAdministrator");
    renderEnrollment(api);

    await fillCredentialsAndSmtp(user);
    await user.type(screen.getByLabelText("Recovery email"), "ops@example.com");
    await user.click(createButton());

    await waitFor(() => expect(enroll).toHaveBeenCalledTimes(1));
    expect(enroll).toHaveBeenCalledWith("bm-admin", PASSPHRASE, "ops@example.com", {
      host: "smtp.example.net",
      // A number, not the string the input holds: the contract's port is
      // numeric and a quoted one is refused as an invalid request.
      port: 2525,
      security: "tls",
      username: "relay-user",
      password: "relay-secret",
      from: "retnd@example.com"
    });
  });

  it("says nothing was created and the link still works when the mail could not be sent", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    vi.spyOn(api, "enrollAdministrator").mockRejectedValue(
      new RetndError({
        code: "SMTP_SEND_FAILED",
        message: "dial tcp 10.0.0.5:587: connection refused",
        correlationId: "cid_smtp502"
      })
    );
    renderEnrollment(api);

    await fillCredentialsAndSmtp(user);
    await user.type(screen.getByLabelText("Recovery email"), "ops@example.com");
    await user.click(createButton());

    await screen.findByText(/no account was created/i);
    // The operator's actual next step: this same link, again, once the
    // fields are fixed.
    expect(screen.getByText(/has not been used up/i)).toBeInTheDocument();
    // The mail server's own words, which are the only thing that
    // distinguishes a blocked port from a rejected credential.
    expect(screen.getByText(/connection refused/)).toBeInTheDocument();
    expect(screen.queryByText(/Restart retnd/)).toBeNull();
  });
});

function renderReset(api: RetndApi, url: string) {
  render(
    <MemoryRouter initialEntries={[url]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/reset-password" element={<ResetPasswordPage />} />
          <Route path="/forgot-password" element={<p>Ask for a link</p>} />
          <Route path="/" element={<p>The sign-in screen</p>} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

const setPasswordButton = () => screen.getByRole("button", { name: "Set password" });

describe("redeeming a reset link", () => {
  afterEach(() => {
    resetMockFixtures();
    cleanup();
    vi.restoreAllMocks();
  });

  it("offers no form at all when the link carried no token", () => {
    renderReset(createMockApi(), "/reset-password");

    expect(screen.queryByLabelText(/^New password/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Set password" })).toBeNull();
    expect(screen.getByRole("link", { name: /Ask for a new one/i })).toBeInTheDocument();
  });

  it("refuses a too-short password and a mismatched confirmation", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const reset = vi.spyOn(api, "resetPassword");
    renderReset(api, "/reset-password?token=tok-123");

    await user.type(screen.getByLabelText(/^New password/), "short");
    expect(screen.getByText("Minimum 12 characters.")).toBeInTheDocument();
    expect(setPasswordButton()).toBeDisabled();

    await user.clear(screen.getByLabelText(/^New password/));
    await user.type(screen.getByLabelText(/^New password/), PASSPHRASE);
    await user.type(screen.getByLabelText(/^Confirm new password/), PASSPHRASE + "-typo");
    expect(screen.getByText("Passwords do not match.")).toBeInTheDocument();
    expect(setPasswordButton()).toBeDisabled();

    await user.click(setPasswordButton());
    // The link is single-use: a submitted typo spends it on a password
    // nobody knows.
    expect(reset).not.toHaveBeenCalled();
  });

  it("sends the token out of the URL, then says every session is signed out", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const reset = vi.spyOn(api, "resetPassword");
    renderReset(api, "/reset-password?token=tok-123");

    await user.type(screen.getByLabelText(/^New password/), PASSPHRASE);
    await user.type(screen.getByLabelText(/^Confirm new password/), PASSPHRASE);
    await user.click(setPasswordButton());

    await waitFor(() => expect(reset).toHaveBeenCalledWith("tok-123", PASSPHRASE));
    await screen.findByText(/every session has been signed out/i);
    // Nothing is signed in afterwards, so the only place to go is the
    // sign-in screen, and it has to be offered rather than implied.
    await user.click(screen.getByRole("link", { name: /Go to sign in/i }));
    expect(screen.getByText("The sign-in screen")).toBeInTheDocument();
  });

  it("offers another link when this one has expired or been used", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    vi.spyOn(api, "resetPassword").mockRejectedValue(
      new RetndError({
        code: "RESET_TOKEN_INVALID",
        message: "this reset link has expired or has already been used",
        correlationId: "cid_reset401"
      })
    );
    renderReset(api, "/reset-password?token=stale");

    await user.type(screen.getByLabelText(/^New password/), PASSPHRASE);
    await user.type(screen.getByLabelText(/^Confirm new password/), PASSPHRASE);
    await user.click(setPasswordButton());

    await screen.findByText(/expired or has already been used/i);
    await user.click(screen.getByRole("link", { name: "Email a new reset link" }));
    expect(screen.getByText("Ask for a link")).toBeInTheDocument();
  });
});

async function renderSettings(api: RetndApi) {
  act(() => {
    graph.commit("test/seed-version", (tx) =>
      tx.set(versionNode, { data: VERSION, error: null, loading: false })
    );
  });

  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          <SettingsPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
  await screen.findByLabelText("Recovery email");
}

describe("the Settings recovery card", () => {
  beforeEach(() => {
    resetGraphForTests();
  });

  afterEach(() => {
    resetMockFixtures();
    cleanup();
    vi.restoreAllMocks();
    resetGraphForTests();
  });

  it("keeps the stored SMTP password when the field was left blank", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const update = vi.spyOn(api, "updateRecoverySettings");
    await renderSettings(api);

    // The field starts blank because no read can return a password, and
    // the card says what blank means rather than leaving it to be
    // guessed.
    expect(screen.getByLabelText(/^SMTP password/)).toHaveValue("");
    expect(screen.getByText(/Leave this blank to keep it/i)).toBeInTheDocument();

    await user.clear(screen.getByLabelText("Port"));
    await user.type(screen.getByLabelText("Port"), "465");
    await user.type(screen.getByLabelText("Administrator password"), PASSPHRASE);
    await user.click(screen.getByRole("button", { name: "Save recovery settings" }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    const [sent] = update.mock.calls[0];
    expect(sent.smtp?.port).toBe(465);
    expect(sent.smtp?.password).toBe("");
    expect(sent.currentPassword).toBe(PASSPHRASE);
  });

  // The UI half of #830's first security finding: this card writes the
  // two settings that decide where a password reset link is delivered,
  // so it re-authenticates rather than trusting the session cookie the
  // browser already has. A save button that stayed live without one
  // would send a request the service refuses, and teach an operator
  // that the refusal is a bug.
  it("will not save until the administrator password is given", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const update = vi.spyOn(api, "updateRecoverySettings");
    await renderSettings(api);

    const save = screen.getByRole("button", { name: "Save recovery settings" });
    expect(save).toBeDisabled();

    await user.type(screen.getByLabelText("Administrator password"), PASSPHRASE);
    expect(save).toBeEnabled();
    await user.click(save);

    await waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    expect(update.mock.calls[0][0].currentPassword).toBe(PASSPHRASE);
    // The credential is not kept around afterwards: the field is blank
    // again, so a second save has to be authorised on its own.
    await waitFor(() => expect(screen.getByLabelText("Administrator password")).toHaveValue(""));
  });

  it("reports a refused password as a refusal rather than as a mail problem", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    await renderSettings(api);

    await user.type(screen.getByLabelText("Administrator password"), "wrong-current-password");
    await user.click(screen.getByRole("button", { name: "Save recovery settings" }));

    expect(await screen.findByText(/that password was not accepted/i)).toBeInTheDocument();
    expect(screen.getByText(/nothing was changed/i)).toBeInTheDocument();
  });

  it("reports the mail server's own words when a test message is refused", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    vi.spyOn(api, "sendRecoveryTestEmail").mockRejectedValue(
      new RetndError({
        code: "SMTP_SEND_FAILED",
        message: "535 5.7.8 authentication failed",
        correlationId: "cid_test502"
      })
    );
    await renderSettings(api);

    await user.click(screen.getByRole("button", { name: "Send test email" }));

    await screen.findByText(/535 5.7.8 authentication failed/);
    // The point of the test button: what it proves about recovery, not
    // merely that a button failed.
    expect(screen.getByText(/recovery mail would not arrive either/i)).toBeInTheDocument();
  });
});

describe("the write-only SMTP password on the wire", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function stubbedFetch() {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers(),
      json: async () => ({
        recoveryEmail: "ops@example.com",
        recoveryEmailConfirmed: true,
        smtp: {
          host: "smtp.example.net", port: 465, security: "tls",
          username: "relay-user", from: "retnd@example.com", passwordSet: true
        }
      })
    });
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }

  const smtp = {
    host: "smtp.example.net",
    port: 465,
    security: "tls" as const,
    username: "relay-user",
    password: "",
    from: "retnd@example.com"
  };

  it("omits the password entirely rather than sending an empty one", async () => {
    const fetchMock = stubbedFetch();

    await httpApi.updateRecoverySettings({ currentPassword: PASSPHRASE, smtp });

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/auth/recovery");
    expect(init.method).toBe("PATCH");
    const body = JSON.parse(init.body as string) as { smtp: Record<string, unknown> };
    // An empty string is a password; an absent field is "keep the stored
    // one". Sending the former clears the credential of anybody who
    // corrected a port.
    expect("password" in body.smtp).toBe(false);
    expect(body.smtp.port).toBe(465);
  });

  it("sends a typed password, and reads the answer back without one", async () => {
    const fetchMock = stubbedFetch();

    const answer = await httpApi.updateRecoverySettings({ currentPassword: PASSPHRASE, smtp: { ...smtp, password: "new-secret" } });

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const body = JSON.parse(init.body as string) as { smtp: Record<string, unknown> };
    expect(body.smtp.password).toBe("new-secret");
    // The read side has no field for it at all, which is what the card
    // renders "a password is stored" from.
    expect(answer.smtp).toEqual({
      host: "smtp.example.net", port: 465, security: "tls",
      username: "relay-user", from: "retnd@example.com", passwordSet: true
    });
  });

  it("reports an administrator with no endpoint as none, rather than as an empty one", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers(),
      json: async () => ({
        recoveryEmail: "",
        recoveryEmailConfirmed: false,
        // What a headlessly provisioned administrator really answers.
        smtp: null
      })
    });
    vi.stubGlobal("fetch", fetchMock);

    const answer = await httpApi.getRecoverySettings();

    expect(answer.smtp).toBeNull();
    expect(answer.recoveryEmailConfirmed).toBe(false);
  });
});

/** Signed OUT, which is the only state these two routes are reachable
 *  from and therefore the only one worth walking them in. */
const SIGNED_OUT: AuthContext = { authenticated: false, username: null, mode: "local-account" };
const signedOutBridge: PlatformBridge = {
  ...genericBridge,
  getAuthContext: () => Promise.resolve(SIGNED_OUT)
};

function renderApp(api: RetndApi, route: string) {
  render(
    <MemoryRouter initialEntries={[route]}>
      <ApiProvider api={api}>
        <PlatformProvider bridge={signedOutBridge}>
          <App />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("asking for a reset link, from a browser nobody is signed in on", () => {
  afterEach(() => {
    resetMockFixtures();
    cleanup();
    vi.restoreAllMocks();
    resetGraphForTests();
  });

  it("is reachable from the sign-in screen", async () => {
    const user = userEvent.setup();
    renderApp(createMockApi(), "/");

    // The catch-all route serves sign-in, so a mistyped path in App.tsx
    // would leave this link landing back on the form it came from.
    await user.click(await screen.findByRole("link", { name: "Forgot password?" }));

    expect(screen.getByRole("heading", { name: "Reset your password" })).toBeInTheDocument();
  });

  it("serves the reset route itself to a signed-out browser", async () => {
    renderApp(createMockApi(), "/reset-password?token=tok-123");

    // The one route that MUST work without a session: whoever opens it
    // has lost the password that would create one.
    expect(await screen.findByRole("heading", { name: "Choose a new password" })).toBeInTheDocument();
  });

  it("sends the username, and does not claim a link was sent when the service did not answer", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const ask = vi.spyOn(api, "requestPasswordReset");
    renderApp(api, "/forgot-password");

    await user.type(await screen.findByLabelText("Username"), "bm-admin");
    await user.click(screen.getByRole("button", { name: "Email a reset link" }));

    await waitFor(() => expect(ask).toHaveBeenCalledWith("bm-admin"));
    await screen.findByText(/a reset link is on its way/i);

    ask.mockRejectedValue(new TypeError("Failed to fetch"));
    await user.click(screen.getByRole("button", { name: "Email a reset link" }));

    // A request that never reached the service says nothing about the
    // account, so reporting it leaks nothing - and leaving the previous
    // confirmation up would tell an operator mail is coming that nobody
    // was ever asked to send.
    await screen.findByText(/did not answer/i);
    expect(screen.queryByText(/a reset link is on its way/i)).toBeNull();
  });
});
