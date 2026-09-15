import { StrictMode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi, resetMockFixtures } from "@shared/api/mock";
import { RetndError } from "@shared/api/contracts";
import type { RetndApi, RecoverySettings } from "@shared/api/contracts";
import { resetGraphForTests } from "@shared/state/graph";
import { VerifyEmailPage } from "@shared/auth/VerifyEmailPage";
import { RecoveryVerificationBanner } from "@shared/components/RecoveryVerificationBanner";

/**
 * Issue #830 §§8-9: the two surfaces that stand between an operator and
 * an account the service is about to delete.
 *
 * Three properties are worth pinning, and each one is a real failure
 * somebody would otherwise ship.
 *
 *   - The verification page has to SPEND the token out of the URL
 *     without being asked twice. The operator already decided, in their
 *     mail client; a page that waits behind a button is one more thing to
 *     abandon, and a page that fires the effect twice reports the
 *     operator's own success back to them as an already-used link.
 *   - A refused link is a dead end unless the page says how to get
 *     another one. The token is single-use and expiring, so "it did not
 *     work" without a route to a fresh link leaves somebody watching an
 *     account lapse.
 *   - The banner must name the DEADLINE while one is running. It is the
 *     only place that instant is visible - the service publishes it and
 *     nothing else renders it - and its absence is what turns a
 *     documented deletion into a mystery.
 */

const UNVERIFIED: RecoverySettings = {
  recoveryEmail: "ops@example.com",
  recoveryEmailConfirmed: true,
  recoveryEmailVerified: false,
  verificationDeadline: "2026-03-01T12:30:00Z",
  smtp: {
    host: "smtp.example.net",
    port: 587,
    security: "starttls",
    username: "ops@example.com",
    from: "retnd@example.net",
    passwordSet: true
  }
};

function renderVerify(api: RetndApi, url: string) {
  render(
    <MemoryRouter initialEntries={[url]}>
      <ApiProvider api={api}>
        <Routes>
          <Route path="/verify-email" element={<VerifyEmailPage />} />
          <Route path="/" element={<p>The sign-in screen</p>} />
        </Routes>
      </ApiProvider>
    </MemoryRouter>
  );
}

function renderBanner(api: RetndApi) {
  render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <RecoveryVerificationBanner />
      </ApiProvider>
    </MemoryRouter>
  );
}

describe("opening the verification link", () => {
  afterEach(() => {
    resetMockFixtures();
    // The recovery block lives in the graph (state/appNodes.ts) so the
    // banner and the verification page cannot contradict each other, and
    // a graph node outlives a render: without this, one case's unverified
    // fixture is still there when the next case asserts on a verified
    // one.
    resetGraphForTests();
    cleanup();
    vi.restoreAllMocks();
  });

  it("spends the token out of the URL exactly once and says the account is no longer provisional", async () => {
    const api = createMockApi();
    const verify = vi.spyOn(api, "verifyRecoveryEmail");
    renderVerify(api, "/verify-email?token=verify-tok-123");

    await waitFor(() => expect(verify).toHaveBeenCalledWith("verify-tok-123"));
    // Once. The token is single-use, so a second call would refuse the
    // operator's own successful verification.
    expect(verify).toHaveBeenCalledTimes(1);
    // The message that sent them here threatened removal; this is the
    // sentence that withdraws the threat.
    expect(await screen.findByText(/no longer provisional/i)).toBeInTheDocument();
  });

  // The same property under the conditions that actually break it.
  // React 18's development StrictMode mounts every effect twice, and
  // this effect spends a SINGLE-USE token: an unguarded second pass
  // reports the operator's own successful verification back to them as
  // an already-used link, on the one screen whose whole job is to say
  // the account is safe. The test above renders without StrictMode and
  // would pass with the guard deleted, so this is the one that holds
  // it.
  it("spends the token once and renders the success under StrictMode's double mount", async () => {
    const api = createMockApi();
    const verify = vi.spyOn(api, "verifyRecoveryEmail");
    render(
      <StrictMode>
        <MemoryRouter initialEntries={["/verify-email?token=verify-tok-strict"]}>
          <ApiProvider api={api}>
            <Routes>
              <Route path="/verify-email" element={<VerifyEmailPage />} />
            </Routes>
          </ApiProvider>
        </MemoryRouter>
      </StrictMode>
    );

    expect(await screen.findByText(/no longer provisional/i)).toBeInTheDocument();
    expect(verify).toHaveBeenCalledTimes(1);
    expect(verify).toHaveBeenCalledWith("verify-tok-strict");
    expect(screen.queryByText(/expired or has already been used/i)).not.toBeInTheDocument();
  });

  it("offers nothing to submit when the link carried no token", () => {
    const api = createMockApi();
    const verify = vi.spyOn(api, "verifyRecoveryEmail");
    renderVerify(api, "/verify-email");

    expect(verify).not.toHaveBeenCalled();
    expect(screen.getByText(/needs the one-time token/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /sign in/i })).toBeInTheDocument();
  });

  it("routes an expired link to a fresh one instead of leaving a dead end", async () => {
    const api = createMockApi();
    vi.spyOn(api, "verifyRecoveryEmail").mockRejectedValue(
      new RetndError({
        code: "VERIFY_TOKEN_INVALID",
        message: "that verification link has expired or has already been used",
        correlationId: "cid_verify401"
      })
    );
    renderVerify(api, "/verify-email?token=stale");

    expect(await screen.findByText(/expired or has already been used/i)).toBeInTheDocument();
    // The two ways out, both named, because from here it is not knowable
    // which one applies: the link lapsed, or the whole account did.
    expect(screen.getByText(/Resend link/)).toBeInTheDocument();
    expect(screen.getByText(/enrollment is open again/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /sign in/i })).toBeInTheDocument();
  });
});

describe("the unverified-recovery banner", () => {
  afterEach(() => {
    resetMockFixtures();
    // The recovery block lives in the graph (state/appNodes.ts) so the
    // banner and the verification page cannot contradict each other, and
    // a graph node outlives a render: without this, one case's unverified
    // fixture is still there when the next case asserts on a verified
    // one.
    resetGraphForTests();
    cleanup();
    vi.restoreAllMocks();
  });

  it("names the address and the deadline, and cannot be dismissed", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getRecoverySettings").mockResolvedValue(UNVERIFIED);
    renderBanner(api);

    expect(await screen.findByText(/Unverified/)).toBeInTheDocument();
    expect(screen.getByText("ops@example.com")).toBeInTheDocument();
    // The machine-readable instant, asserted through the element rather
    // than through a locale-formatted string: the rendered text depends
    // on the test machine's locale and the guarantee does not.
    const deadline = document.querySelector("time");
    expect(deadline?.getAttribute("datetime")).toBe("2026-03-01T12:30:00Z");
    expect(screen.getByText(/will be\s+removed/)).toBeInTheDocument();
    // #620's opt-out: this is the banner whose consequence is that the
    // console stops having an administrator.
    expect(screen.queryByRole("button", { name: /dismiss/i })).toBeNull();
  });

  it("renders nothing for a verified account", async () => {
    const api = createMockApi();
    const read = vi.spyOn(api, "getRecoverySettings");
    renderBanner(api);

    // The fixture is verified, so the assertion has to wait for the read
    // to have happened - otherwise "nothing on screen" would pass before
    // the banner had any chance to render.
    await waitFor(() => expect(read).toHaveBeenCalled());
    expect(screen.queryByText(/Unverified/)).toBeNull();
  });

  it("resends the link, then re-reads the state the service now holds", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const read = vi.spyOn(api, "getRecoverySettings").mockResolvedValue(UNVERIFIED);
    const resend = vi.spyOn(api, "resendRecoveryEmailVerification");
    renderBanner(api);

    await screen.findByText(/Unverified/);
    await user.click(screen.getByRole("button", { name: "Resend link" }));

    await waitFor(() => expect(resend).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(/fresh link has just been sent/i)).toBeInTheDocument();
    // The deadline and the verified flag are the service's to report, so
    // a successful resend re-reads them rather than assuming.
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
  });

  it("reports a mail server that refused the resend, without clearing the warning", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    vi.spyOn(api, "getRecoverySettings").mockResolvedValue(UNVERIFIED);
    vi.spyOn(api, "resendRecoveryEmailVerification").mockRejectedValue(
      new RetndError({
        code: "SMTP_SEND_FAILED",
        message: "dial tcp 10.0.0.5:587: connection refused",
        correlationId: "cid_smtp502"
      })
    );
    renderBanner(api);

    await screen.findByText(/Unverified/);
    await user.click(screen.getByRole("button", { name: "Resend link" }));

    expect(await screen.findByText(/connection refused/)).toBeInTheDocument();
    // Still on screen: the account is still lapsing, and a failed resend
    // is the worst possible moment to hide that.
    expect(screen.getByText(/Unverified/)).toBeInTheDocument();
  });
});

/**
 * The reason the recovery block lives in the graph rather than in the
 * banner's own state: the banner sits above the router and is never
 * unmounted by navigating to the verification page, so an operator who
 * opens the link in the tab the console is already signed in on would
 * otherwise read "verified" and "this account will be removed" at the
 * same time, one directly above the other.
 */
describe("verifying in the tab the console is already open in", () => {
  afterEach(() => {
    resetMockFixtures();
    resetGraphForTests();
    cleanup();
    vi.restoreAllMocks();
  });

  it("clears the banner the moment the page reports success", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getRecoverySettings").mockResolvedValue(UNVERIFIED);
    render(
      <MemoryRouter initialEntries={["/verify-email?token=abc123"]}>
        <ApiProvider api={api}>
          <RecoveryVerificationBanner />
          <Routes>
            <Route path="/verify-email" element={<VerifyEmailPage />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );

    // Both are on screen to begin with, which is what makes the
    // disappearance below meaningful rather than vacuous.
    expect(await screen.findByText(/Unverified/)).toBeInTheDocument();
    expect(await screen.findByText(/no longer provisional/i)).toBeInTheDocument();

    await waitFor(() => expect(screen.queryByText(/Unverified/)).toBeNull());
  });
});
