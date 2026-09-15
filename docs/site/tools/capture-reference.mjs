// Re-shoots every screenshot on docs/site/reference.html.
//
// Run it from the repository root:
//
//     node docs/site/tools/capture-reference.mjs
//
// It writes PNGs into docs/site/screens/ under the `ref-` prefix,
// overwriting whatever is there, and prints one line per screen so a diff
// of the output tells you which picture moved. The `first-run` set that
// capture-first-run.mjs owns is left alone: the two tools share a
// directory and nothing else, and the prefix is what keeps a re-shoot of
// one from quietly deleting the other's work.
//
// harness.mjs holds the arrangement: where the app comes from, why it is
// never a real deployment, why the auth route is stubbed, why Playwright
// is borrowed rather than installed, and why the clock, the locale, the
// timezone and the port are all pinned. Read that first. This is the same
// harness pointed at the signed-in application instead of at the setup
// flow.
//
// The one difference worth stating here: capture-first-run.mjs walks a
// FLOW, so its order is the order an operator meets the screens in. This
// one walks a SURFACE, so its order is the navigation's, and each screen
// is reached by its own URL rather than by clicking through the previous
// one. A screen that fails to render therefore takes down its own capture
// and nothing else, which is what makes it safe to shoot ten of them in
// one run.
//
// # Why this file no longer carries its own dev server
//
// It used to, and that is how it came to be the one capture script on
// this site whose output was not reproducible. It predated harness.mjs
// and kept a private copy of the arrangement -- its own Playwright
// resolution, its own dev server on an EPHEMERAL port, its own browser
// context -- and the copy was missing the three things the shared one
// pins:
//
//   * `page.clock.setFixedTime`, so `ref-09-set-detail` re-recorded
//     differently every run. That screen renders the set's next scheduled
//     run as an interval from now, and with a live clock the interval
//     moves between one capture and the next. It was measured rather
//     than assumed: two consecutive runs of the old script over an
//     unchanged tree produced 14 identical files and that one different,
//     which is precisely the property these outputs exist to have.
//   * the locale and timezone, so every rendered time depended on the
//     machine doing the re-record.
//   * the port, so the global terminal's environment preamble carried
//     whatever four digits the kernel handed out that afternoon.
//
// Re-recording under this import is byte-identical run to run, and every
// screen on this page now agrees with every screen on the other three
// about what time it is.

import {
  EXAMPLE,
  mb,
  openApp,
  optimise,
  screensTotal,
  settle,
  shot,
  withDevServer,
  writeStable
} from "./harness.mjs";

/** The administrator the session stub reports. Never a real account, and
 *  the same one every other capture script reports, because a reader
 *  moving between two pages of this site should not meet two operators. */
const ADMIN = EXAMPLE.adminUser;

const shots = [];

await withDevServer(async (app) => {
  const { page } = await openApp(app, { path: null });

  /** Navigate, then wait for something only THAT screen renders.
   *
   *  A settle timer alone is not enough and the failure it produces is
   *  the worst kind: the previous screen is still on the page when the
   *  shutter opens, so the capture succeeds and the picture is of the
   *  wrong page. Waiting for a marker turns that into a timeout that
   *  names the screen instead. */
  const visit = async (path, marker) => {
    await page.goto(app.base + path);
    await marker(page).waitFor({ state: "visible", timeout: 20_000 });
  };

  /** One page capture, clipped to the content the way every other script
   *  on this site clips it. */
  const take = async (name) => {
    shots.push(await shot(page, name));
  };

  const text = (re) => (p) => p.getByText(re).first();
  const heading = (re) => (p) => p.getByRole("heading", { name: re }).first();

  /** Collapse the docked terminal before the page captures, and shoot it
   *  on its own afterwards.
   *
   *  The dock is `position: fixed`, so in a full-page capture it renders
   *  once, at the bottom of the FIRST viewport, which on a long page is
   *  the middle of the picture. Expanded that is 170px of a screen
   *  covered by a panel that is not covering it on the real thing.
   *  Collapsed it is the one-line bar every page genuinely carries, so
   *  leaving that in is documentation rather than an artefact. Its open
   *  state is in localStorage (ActivityDock), so this is clicked once
   *  and holds for the rest of the run. */
  const collapseDock = async () => {
    const hide = page.getByRole("button", { name: "Hide terminal" });
    if (await hide.count()) await hide.first().click();
  };

  const PAGES = [
    ["ref-01-dashboard", "/", text(/Recent activity/i)],
    ["ref-02-backup-sets", "/sets", text(/Backup sets|backup set/i)],
    ["ref-09-set-detail", "/sets/production/postgres-primary", heading(/Production PostgreSQL/i)],
    ["ref-03-backups", "/backups", text(/Backups|artifact/i)],
    // Three segments, not one (#677). An artifact id is a backup set id
    // plus the file's name, so this URL is the fixture's own composite
    // id spelled out; the single-segment form this line used to carry
    // matched no route at all, the catch-all redirected it, and the
    // capture timed out looking for a filename on the Dashboard.
    [
      "ref-10-backup-detail",
      "/backups/production/postgres-primary/postgres-prod-20260828.dump.zst",
      text(/postgres-prod-20260828\.dump\.zst/)
    ],
    ["ref-04-activity", "/activity", text(/Activity/i)],
    ["ref-05-quarantine", "/quarantine", text(/Quarantine/i)],
    ["ref-06-settings", "/settings", text(/Notifications/i)],
    ["ref-07-catalog-recovery", "/catalog-recovery", text(/Existing backup data detected/i)]
  ];

  // Once, before the first capture: goto the dashboard, collapse, and
  // let localStorage carry it through every navigation below.
  await visit("/", text(/Recent activity/i));
  await collapseDock();

  for (const [name, path, marker] of PAGES) {
    await visit(path, marker);
    await take(name);
  }

  // ------------------------------------------------ storage destinations
  //
  // Five pictures of one card, and the only sequence in this tool that
  // is DRIVEN rather than addressed by URL. That is not a departure
  // from the rule at the top of this file, it is what the rule leaves
  // room for: every one of these surfaces lives at /settings, because
  // EPIC I (#664) added no route. Each capture therefore starts by
  // navigating to /settings again and walks forward from a known
  // state, so a step that fails takes down its own picture and not the
  // four after it.
  //
  // Each is shot as the SUBJECT rather than as the page: the card for
  // the list, the wizard's own pane for a wizard step, the dialog for
  // the dialog. The destinations card sits well below the fold on a
  // screen this long, so a full-page capture is a picture of Settings
  // with the subject somewhere in the middle, and once a wizard is
  // open the card is four destination rows followed by the thing the
  // picture is actually about.
  const destinations = () => page.getByRole("region", { name: "Storage destinations" });

  /** One capture of a region of the Settings page.
   *
   *  The docked terminal is hidden for the duration and put back
   *  afterwards. It is `position: fixed`, so it does not scroll with
   *  the element being captured: `locator.screenshot` scrolls the
   *  subject into view and the collapsed bar stays where it is, which
   *  lands a strip of terminal across whatever part of the card
   *  happens to be at the bottom of the viewport. On a whole-page
   *  capture that bar is documentation, which is why the page shots
   *  above keep it; through the middle of a table it is an artefact.
   *
   *  A locator rather than a selector, which is why this does not go
   *  through harness.mjs's `shot`: every subject here is reached by its
   *  accessible name, and an accessible name is not a CSS selector. */
  const shotRegion = async (name, locator) => {
    await settle(page, 300);
    const hide = await page.addStyleTag({
      content: 'section[aria-label="Terminal"] { display: none !important; }'
    });
    try {
      await writeStable(page, name, () => locator.screenshot({ animations: "disabled" }));
    } finally {
      await hide.evaluate((node) => node.remove());
    }
    shots.push(name);
    console.log("  " + name + ".png");
  };

  /** Open Settings and scroll the destinations card into view. */
  const openDestinations = async () => {
    await visit("/settings", text(/Notifications/i));
    const card = destinations();
    await card.waitFor({ state: "visible", timeout: 20_000 });
    await card.scrollIntoViewIfNeeded();
    return card;
  };

  // The list. Two local volumes and two buckets, which is the epic's
  // whole argument in one frame: a destination is an instance of a
  // registered backend, and two instances of one backend is ordinary.
  // The fixture declares exactly that (api/mock.ts), so this picture
  // cannot go stale without the fixture going stale with it.
  await openDestinations();
  await shotRegion("ref-11-destinations", destinations());

  // Step 1 of adding one: which backend. Both registered manifests and
  // the dimmed row for a backend this build understands and does not
  // register, which is the part a reader would otherwise never know
  // was there.
  await openDestinations();
  // Scoped to the card. Every retention tier beside it offers its own
  // "Add a destination" shortcut, so the bare name matches four
  // buttons on this page and Playwright refuses the ambiguity rather
  // than picking one, which is the behaviour that wants keeping.
  await destinations().getByRole("button", { name: "Add a destination" }).click();
  await page.getByRole("group", { name: "Add a destination" }).waitFor({ timeout: 20_000 });
  await page.getByRole("list", { name: "Registered backends" }).waitFor({ timeout: 20_000 });
  await shotRegion("ref-12-add-backend", page.getByRole("group", { name: "Add a destination" }));

  /** Walk the add wizard's three steps and land on the configure step's
   *  form, which is the only way to reach it: the manifest-driven
   *  renderer is entered from the add flow and from nowhere else.
   *
   *  `local_volume` rather than `s3` on purpose. It is the backend
   *  whose manifest declares a path, a prefix and an enum and NO
   *  credential, so the form it produces and the probe it runs are both
   *  the ones that could not exist before this epic -- and its two
   *  skipped probe steps carry the manifest's own reasons, which is the
   *  thing worth a picture. */
  const walkToConfigureStep = async (instance) => {
    await openDestinations();
    await destinations().getByRole("button", { name: "Add a destination" }).click();
    await page.getByRole("radio", { name: /Local volume/ }).check();
    await page.getByRole("button", { name: "Next: name this instance" }).click();
    await page.getByLabel("Instance name").fill(instance);
    await page.getByRole("button", { name: "Next: confirm" }).click();
    await page.getByRole("button", { name: "Next: configure it" }).click();
    await page
      .getByRole("group", { name: "Configure " + instance })
      .waitFor({ state: "visible", timeout: 20_000 });
  };

  // The renderer, rendering a manifest. Nothing in the product knows
  // what a local volume's fields are: the labels, the help and the
  // "leave unset" wording are all declared data.
  //
  // The required field is filled before the shutter, and the two
  // optional ones are left alone. An untouched create form carries
  // "required: this backend cannot be configured without it" under
  // the directory, which is true and is the state a reader would
  // mistake for a broken capture; what the picture is for is the two
  // optional rows, where "Leave unset (readback)" is the manifest's
  // own `unset_means` rendered as a choice rather than pre-selected.
  // Exact, because "Subdirectory" contains "Directory" and this
  // manifest declares both.
  await walkToConfigureStep("usb_offsite");
  await page.getByLabel("Directory", { exact: true }).fill("/mnt/usb-offsite");
  await shotRegion(
    "ref-13-configure-manifest",
    page.getByRole("group", { name: "Configure usb_offsite" })
  );

  // The probe, as a step list. Entered with the required field filled,
  // then waited on until the last step has an outcome, because a
  // picture taken mid-flight is a picture of `waiting` rows and says
  // nothing about what the check proves.
  await walkToConfigureStep("usb_offsite");
  await page.getByLabel("Directory", { exact: true }).fill("/mnt/usb-offsite");
  await page.getByRole("button", { name: "Next: test connection" }).click();
  await page
    .locator('[data-testid="probe-step-row-delete"] [data-testid="probe-step-outcome"]')
    .filter({ hasText: /^passed$/ })
    .waitFor({ state: "visible", timeout: 30_000 });
  await shotRegion("ref-14-probe-steps", page.getByRole("group", { name: "Configure usb_offsite" }));

  // Handing the default over. Shot as the dialog rather than as the
  // card, because the dialog IS the surface here: one click, two
  // consequences, and the second one is the half nobody asked for.
  await openDestinations();
  await page
    .getByRole("group", { name: "Storage destination usb_shelf" })
    .getByRole("button", { name: "Make default" })
    .click();
  const transfer = page.getByRole("dialog");
  await transfer.waitFor({ state: "visible", timeout: 20_000 });
  await shotRegion("ref-15-default-transfer", transfer);

  // ------------------------------------------------------- the terminal
  //
  // Its own screen, because it is its own surface: every command the
  // interface runs on the operator's behalf is echoed here, with the
  // engine's own live feed beside it, and none of that is visible in a
  // picture of the bar it collapses to.
  await visit("/", text(/Recent activity/i));
  const show = page.getByRole("button", { name: "Show terminal" });
  if (await show.count()) await show.first().click();
  await page.getByRole("button", { name: "Hide terminal" }).first().waitFor();
  await settle(page, 400);
  const dock = await page
    .locator("section")
    .filter({ has: page.getByRole("button", { name: "Hide terminal" }) })
    .first()
    .boundingBox();
  if (!dock) throw new Error("the docked terminal did not lay out");
  const dockClip = {
    x: Math.round(dock.x),
    y: Math.round(dock.y),
    width: Math.round(dock.width),
    height: Math.round(dock.height)
  };
  await writeStable(page, "ref-08-terminal", () =>
    page.screenshot({ animations: "disabled", clip: dockClip }));
  shots.push("ref-08-terminal");
  console.log("  ref-08-terminal.png");
});

console.log("\n" + shots.length + " screens written to docs/site/screens/ (signed in as " + ADMIN + ")");
optimise(shots);
const { count, bytes } = screensTotal();
console.log("total " + mb(bytes) + " across " + count + " files in docs/site/screens/");
