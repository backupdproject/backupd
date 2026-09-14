// Re-shoots every screenshot on docs/site/first-run.html.
//
// Run it from the repository root:
//
//     node docs/site/tools/capture-first-run.mjs
//
// It writes PNGs into docs/site/screens/, overwriting whatever is there,
// and prints one line per screen so a diff of the output tells you which
// picture moved.
//
// Everything about how the app is started, why it is never pointed at a
// real deployment, why there is an auth stub and where Playwright comes
// from lives in harness.mjs, which this and every other capture script
// here share. Read that file first; this one is just the walk.
//
// `?scenario=first-run` is what puts the app in the unconfigured state:
// `createMockApi` starts with `configured = false` for that scenario only,
// so the app renders the unconfigured shell instead of a dashboard with
// data behind it. It is a real scenario in the product's own fixtures,
// not something added here.
//
// # The shape of this flow changed under the old version of this file
//
// It used to walk a dedicated `FirstRunPage` whose heading was "Set up
// Backupd". That page is gone. #275 replaced it with the ordinary
// add-backup-set wizard running with `firstRun` set, reached the way an
// operator reaches it: enrol, land on a dashboard that says there is no
// configuration yet, and press Add backup set. Everything below follows
// the flow that exists rather than the one the old script described.
//
// # The rail this walks, and which name a locator uses
//
// #788 grew that wizard from six steps to eight, and the walk below is
// the shipped rail in order:
//
//     1 Source · 2 Connection test · 3 Engine · 4 Repository ·
//     5 Consistency · 6 Verification · 7 Retention · 8 Review
//
// Two of the six it replaced are gone as steps rather than renamed.
// "Authentication" and "Verify server" are one step now — the
// credentials, the host key AND the connection test that proves what
// those two can actually do, which is why step 2 is where the save gate
// now bites instead of Review. "Backup discovery" is gone entirely: the
// directory a run walks is part of naming the source and is asked for on
// step 1, and the completion method it used to carry is the artifact
// engine's answer on step 6.
//
// Every step is clicked by its RAIL label, because the rail's buttons
// carry exactly that as their accessible name (components/WizardStep.tsx
// sets aria-label to the label, so the step number is not read into it).
// Four of the eight then draw a fuller heading in the body —
// Repository/"Repository domain", Consistency/"Source consistency",
// Retention/"Storage, retention and holds", and step 6, which is titled
// "Verification" on the incremental engine and "Completion and
// validation" on the artifact one. `NEW_SET_DEFAULTS.engine` is
// `artifact`, so that is the heading this walk waits for; a wait on
// "Verification" here would hang on the shipped default.
//
// # Why the viewport is 1800 tall, and why there is no terminal in these
//
// #617 pinned the global terminal to the browser window. A fixed element
// and a full-page screenshot do not mix: Chromium paints the fixed panel
// where it sits, so a tall page captured full-page gets the terminal
// stamped across it with a field of empty background underneath, which is
// a picture of nothing that happens. A viewport taller than the longest
// screen here means every capture is a real viewport and no full-page
// stitch is ever needed.
//
// The cost is that the terminal, being pinned to the bottom of a window
// that is now taller than any of these pages, falls outside the clip. It
// is genuinely there on every one of these screens and it is genuinely
// not in these pictures, which first-run.html says in one line rather
// than leaving a reader to wonder. It has a page of its own, with moving
// pictures, because a docked log is a thing you watch rather than a thing
// you look at.

import { EXAMPLE, mb, openApp, optimise, screensTotal, settle, shot, withDevServer } from "./harness.mjs";

/** The sign-in and enrolment screens centre a 452px card in a full
 *  viewport, so a plain capture is nine parts empty background. Clip
 *  those to the card plus a margin instead.
 *
 *  Asked for as "whatever holds the card", not as a chain of child
 *  selectors from #root. It was `#root > div > div`, and #874's
 *  delegated-tooltip layer put another `display: contents` wrapper in
 *  that chain: the selector then resolved to AuthFrame's own
 *  full-viewport grid, so these pictures came out as the whole 1280x1800
 *  window with a card in the middle of it. A `display: contents` element
 *  has no box at all, which is why the sign-in page — carrying one
 *  wrapper more than enrolment — failed outright rather than quietly
 *  photographing the wrong rectangle. Both pages have exactly one card,
 *  and its parent is the column that also carries the wordmark. */
const AUTH_CARD = "div:has(> .card)";

/** Deliberately not the harness's standard VIEWPORT, and named so it
 *  cannot be mistaken for it. The standard one is 900 tall and several
 *  wizard steps are longer than that, which under a pinned terminal
 *  forces a full-page stitch and the artefact described above. The width
 *  is the standard width, so a step's picture is the standard layout.
 *
 *  1800 was enough for the six-step wizard. #788's step 2 is three of
 *  those steps in one card — credentials, host key, and a connection
 *  report of seven rows — and a clip is clamped to the viewport it was
 *  taken in, so at 1800 the proven-connection picture lost its
 *  write-permission verdict and its footer off the bottom edge without
 *  anything saying so. The height is the tallest step plus room, not a
 *  round number. */
const TALL = { width: 1280, height: 2600 };

/** From the wizard's own heading to the bottom of its card, which has no
 *  wrapper element of its own, so it is asked for as a union. Clipping to
 *  <main> instead does not work: the shell gives it a minimum of a full
 *  viewport, so on a window made tall on purpose a main-shaped picture is
 *  two thirds empty background. */
const MAIN = ["main h1", "main section.card"];

/** The whole application window, at the size somebody actually has one.
 *  A crop of a viewport made tall on purpose, and used only for the two
 *  pictures that are about the shell rather than about a form. */
const WINDOW = { x: 0, y: 0, width: 1280, height: 900 };

await withDevServer(async (app) => {
  const { page, session } = await openApp(app, { path: null, authenticated: false, viewport: TALL });
  const shots = [];
  // 16px of margin rather than the default 28: the union below ends at
  // the bottom of the wizard card and the shell's footer line sits just
  // under it, so a wider pad catches half a sentence of it.
  const take = async (name, sel) => shots.push(await shot(page, name, sel, { pad: 16 }));

  /** A rail button, by the step label it carries as its accessible name
   *  (components/WizardStep.tsx). Narrowed with `data-complete`, which
   *  only the rail's own buttons have: the docked terminal's filter
   *  toolbar has an "Engine" button too, so on a signed-in page the label
   *  alone is ambiguous for at least that step.
   *
   *  The rail, and not Continue, because a walk that pressed Continue
   *  eight times would prove only that the footer advances. Clicking the
   *  step is also how an operator revisits one. */
  const railStep = (label) =>
    page.getByRole("button", { name: label }).and(page.locator("[data-complete]"));

  // --------------------------------------------------------------- enrolment
  //
  // The real entry point. The engine prints an enrollment link on first
  // start (apps/common/auth/local/service.go, PrintBootstrapNotice) and
  // this is the page at the other end of it. The token in the URL is a
  // placeholder: the mock does not check it, and a real one must never
  // be committed.
  await page.goto(app.base + "/enroll?scenario=first-run&token=" + EXAMPLE.token);
  await page.getByRole("heading", { name: /Create Backupd administrator/ }).waitFor();
  await take("01-enrolment-empty", AUTH_CARD);

  // Exact, and by role: #830 put an "SMTP username" on this same form, so
  // a substring label lookup for "Username" now matches two fields.
  await page.getByRole("textbox", { name: "Username", exact: true }).fill(EXAMPLE.adminUser);
  await page.getByLabel(/^Password/).fill("short");
  await page.getByText(/Minimum 12 characters/).first().waitFor();
  await take("02-enrolment-password-too-short", AUTH_CARD);

  await page.getByLabel(/^Password/).fill(EXAMPLE.adminPassword);
  // Not getByLabel: PasswordInput puts a "Show confirm password" toggle
  // inside the same label, so the accessible name matches a button as
  // well as the field and a label lookup is ambiguous. The role pins
  // which of the two is meant.
  await page.getByRole("textbox", { name: "Confirm password" }).fill(EXAMPLE.adminPassword);

  // #830: the account is only as recoverable as the address enrolment
  // was given, so the same form takes one and the SMTP details to reach
  // it, and the server sends a confirmation before it writes the record.
  // Four fields are filled here and three are deliberately left alone:
  // Security defaults to STARTTLS, and the mock's submission asks for no
  // credential, so SMTP username and SMTP password stay empty rather
  // than carrying a placeholder somebody could copy into a real
  // deployment.
  await page.getByLabel("Recovery email").fill(EXAMPLE.recoveryEmail);
  await page.getByLabel("SMTP host").fill(EXAMPLE.smtpHost);
  await page.getByLabel("Port").fill(EXAMPLE.smtpPort);
  await page.getByLabel("From address").fill(EXAMPLE.smtpFrom);

  await page.getByRole("button", { name: "Create administrator" }).waitFor({ state: "visible" });
  await take("03-enrolment-ready", AUTH_CARD);

  // Enrolment succeeds against the mock, and the app then asks the
  // bridge who is signed in. Flip the stub first so the answer is the
  // administrator that was just created.
  session.authenticated = true;
  await page.getByRole("button", { name: "Create administrator" }).click();

  // ------------------------------------------------- the unconfigured shell
  //
  // Not a setup page. The ordinary application, with nothing behind it
  // and a banner saying so. The banner deliberately carries no button of
  // its own, because the two pages that can act on it already offer Add
  // backup set and a banner repeating it would put the same primary
  // action on one page twice.
  await page.getByRole("navigation", { name: "Sections" }).waitFor();
  await page.getByText("Backupd has no configuration yet").waitFor();
  await take("04-first-run-dashboard", WINDOW);

  // ---------------------------------------------------------------- step 1
  await page.getByRole("button", { name: "Add backup set" }).first().click();
  await page.getByRole("heading", { name: "Add backup set", level: 1 }).waitFor();
  await take("05-wizard-step-1-source", MAIN);

  // A regex, not { exact: true }: a couple of these inputs sit inside a
  // <label> that also carries a hint and a button, so the accumulated
  // accessible name is the field name plus all of that. Anchoring at the
  // start still cannot collide with any other field on the same step.
  const fill = async (label, value) => {
    const field = page.getByLabel(new RegExp("^" + label));
    await field.fill(value);
    await field.blur();
  };
  await fill("Backup set name", EXAMPLE.setName);
  await fill("Server hostname", EXAMPLE.host);
  await fill("SSH port", EXAMPLE.port);
  await fill("Username", EXAMPLE.user);
  // Both of these were the "Backup discovery" step's until #788 moved
  // them onto Source, which is also why nothing here fills a field called
  // "Remote folder" or "Include patterns" any more. The second one is
  // "Filename patterns to back up" since #928 renamed it (#927): it had
  // been labelled as an exclude list while being an include list in every
  // other respect, and the symptom was an empty backup rather than an
  // error, so the picture below is worth re-shooting for the label alone.
  await fill("Directory to back up", EXAMPLE.remoteFolder);
  await fill("Filename patterns to back up", EXAMPLE.include);
  await take("06-wizard-step-1-filled", MAIN);

  // ------------------------------------------------------- connection test
  //
  // One step, and one picture of each state it passes through, because
  // the credentials, the host key and the proof are one long card now: a
  // picture of "the host-key section" and a picture of "the credentials
  // section" would be the same picture of the same step.
  await railStep("Connection test").click();
  await page.getByRole("heading", { name: "Connection test", level: 2 }).waitFor();
  // Opening the step fires the host-key probe (the page's own probeHost),
  // so wait for the fingerprint it fetches rather than photographing a
  // panel with a request still in flight. Until it lands the panel says
  // it has nothing to show, which is a real state and not this one.
  await page.getByText(/SHA256:/).first().waitFor();
  await settle(page, 400);
  await take("07-wizard-step-2-connection-test", MAIN);

  await page.getByRole("radio", { name: /Import key/ }).check();
  await page.getByLabel(/Private key/).waitFor();
  await take("08-wizard-step-2-import-key", MAIN);

  await page.getByLabel(/Private key/).fill(EXAMPLE.fakeKey);
  await page.getByRole("button", { name: "Import key" }).click();
  await page.getByText("Key imported").waitFor();
  await take("09-wizard-step-2-key-imported", MAIN);

  await page.getByRole("button", { name: "Trust host" }).click();
  await page.getByRole("button", { name: "Host trusted" }).waitFor();
  await take("10-wizard-step-2-host-trusted", MAIN);

  // #624, on the step it moved to. Nothing has been proven until this
  // runs, and the rail does not open step 3 until it comes back clean —
  // so this click is also what makes every picture below reachable.
  await page.getByRole("button", { name: "Test connection" }).click();
  await page.getByText("This source has been proven").waitFor();
  await settle(page, 400);
  await take("11-wizard-step-2-connection-proven", MAIN);

  // ---------------------------------------------------------------- engine
  await railStep("Engine").click();
  await page.getByRole("heading", { name: "Engine", level: 2 }).waitFor();
  await settle(page, 300);
  await take("12-wizard-step-3-engine", MAIN);

  // ----------------------------------------------------- repository domain
  //
  // Photographed for the artifact engine, which is the default and so the
  // one a first run meets: the step says why it does not apply rather
  // than vanishing, and a rail that changed length under the operator is
  // exactly what #788 refused to draw.
  await railStep("Repository").click();
  await page.getByRole("heading", { name: "Repository domain", level: 2 }).waitFor();
  await settle(page, 300);
  await take("13-wizard-step-4-repository-domain", MAIN);

  // ---------------------------------------------------- source consistency
  await railStep("Consistency").click();
  await page.getByRole("heading", { name: "Source consistency", level: 2 }).waitFor();
  await settle(page, 300);
  await take("14-wizard-step-5-consistency", MAIN);

  // ------------------------------------------------ completion and validation
  //
  // Rail label "Verification", body title "Completion and validation":
  // the artifact engine's answer to the same question, which is why the
  // step explains itself here instead of being skipped. One picture, with
  // Atomic rename chosen — the default is the completion marker, and a
  // second picture of the same step with a different radio filled in is
  // not a second screen.
  await railStep("Verification").click();
  await page.getByRole("heading", { name: "Completion and validation", level: 2 }).waitFor();
  // The validator picklist is a real read of the backend's catalog, so
  // wait for it rather than photographing "Loading the available
  // validators…".
  await page.getByText("No application validator: transfer verification only.").waitFor();
  await page.getByRole("radio", { name: /Atomic rename/ }).check();
  await take("15-wizard-step-6-completion-atomic-rename", MAIN);

  // --------------------------------------- storage, retention and holds
  await railStep("Retention").click();
  await page.getByRole("heading", { name: "Storage, retention and holds", level: 2 }).waitFor();
  await fill("NAS destination", EXAMPLE.destination);
  // An unconfigured instance does not serve the retention read at all
  // (mock.ts's SERVED_WHILE_UNCONFIGURED, from the real unconfigured
  // router), so this step says the deployment's chain could not be read.
  // That IS the first-run state, and waiting for the sentence is what
  // keeps the picture off the "Reading…" frame before it.
  await page.getByText(/retention chain could not be read/).waitFor();
  await take("16-wizard-step-7-storage-retention", MAIN);

  await page.getByRole("checkbox", { name: /I understand the remote backup will be removed/ }).check();
  await take("17-wizard-step-7-acknowledged", MAIN);

  // ----------------------------------------------------------------- review
  await railStep("Review").click();
  await page.getByRole("heading", { name: "Review", level: 2 }).waitFor();
  await settle(page, 300);
  await take("18-wizard-step-8-review", MAIN);

  // ----------------------------------------------------------- configured app
  //
  // "Finish setup", not "Save & enable": during first run the same button
  // is labelled for the thing it actually finishes, and the third option
  // ("Save disabled") stays, because writing the configuration and
  // starting to back up are two decisions.
  await page.getByRole("button", { name: "Finish setup" }).click();
  await page.getByRole("heading", { name: "Backup sets", level: 1 }).waitFor();
  await settle(page, 800);
  await take("19-configured-backup-sets", WINDOW);

  // -------------------------------------------------------------- sign in
  //
  // Last, not first. An operator meets this screen on the SECOND visit,
  // or on a first visit that did not go through the printed link. A
  // reload rebuilds the mock, so the app is unconfigured again and the
  // session stub decides what renders.
  session.authenticated = false;
  await page.goto(app.base + "/?scenario=first-run");
  await page.getByRole("heading", { name: "Sign in" }).waitFor();
  await take("20-sign-in", AUTH_CARD);

  console.log("\n" + shots.length + " screens written to docs/site/screens/");
  optimise(shots);
  const { count, bytes } = screensTotal();
  console.log("screens/ now holds " + count + " files, " + mb(bytes));
});
