// Records the moving pictures on docs/site/workflows.html.
//
// Run it from the repository root:
//
//     node docs/site/tools/capture-workflows.mjs
//
// It writes GIFs into docs/site/screens/ and prints one line per clip
// with its frame count, duration and size, so a diff of the output tells
// you which picture moved and what it cost.
//
// harness.mjs holds the arrangement: where the app comes from, why it is
// never a real deployment, why the clock, the locale, the timezone and
// the port are pinned, and why a clip is a list of held frames rather
// than a video. Read that first.
//
// # What is on this page and what is not
//
// Ten clips, and every one of them is a claim EPIC L (#807) makes that a
// still cannot carry. The run screen and the two configuration cards are
// #814; the per-step terminal is #815; this page and these recordings
// are #817.
//
//   wf-run-timeline     Five stages, always five, always in the order a
//                       run executes them. The claim is about the ones
//                       with nothing in them: a stage that planned no
//                       scripts and a stage with no directory at all are
//                       drawn, named and told apart, because "did not
//                       run" and "not configured" are different facts
//                       and a ladder that hid either would read as the
//                       hooks having been skipped.
//   wf-step-terminal    One terminal, belonging to the selection, read
//                       by cursor and read-only. It ends on the step
//                       whose output the ENGINE truncated, because that
//                       marker is a statement about the log rather than
//                       a line the script printed, and the two must not
//                       look alike.
//   wf-before-failure   A before hook failed, so the backup was never
//                       attempted: backup_status is SKIPPED and not
//                       failed. A product that wrote "failed" there
//                       would be reporting a transfer that never
//                       started.
//   wf-status-split     The three verdicts are three, and none of them
//                       is derived from the others. Four runs in one
//                       clip, because one run can only ever show one
//                       triple and the claim is about the combinations.
//   wf-recovery         recovery_required, with both ways out in shot
//                       and no third one. There is no dismiss on this
//                       banner and nothing in this clip closes it: the
//                       only exits are resuming the cleanup and taking
//                       responsibility for it in writing.
//   wf-set-workflow-tab One backup set's discovered scripts, checked on
//                       demand. The check hashes every script and parses
//                       its shell in this process; nothing in a script
//                       is executed to produce a finding, and BSH003 is
//                       the only rule today whose severity refuses a
//                       save.
//   wf-env-table        Precedence, and a secret that is a LOCATION. The
//                       whole BACKUPD_ prefix is refused in
//                       configuration rather than silently overridden at
//                       merge time, which is why the clip types one in.
//   wf-settings-runner  The deployment-wide card, ending on the row that
//                       says what this read does NOT know. Nothing on
//                       that card contacts the Host Workflow Runner, so
//                       it reports a CONFIGURED address and never a live
//                       one.
//   wf-dark-mode        A whole window at once, which is the only way to
//                       see a theme, driven by the application's own
//                       toggle.
//   wf-narrow           The same run at 480px, where the verdict triple
//                       stacks. It is the one layout claim on the page.
//
// # No resolved secret is ever in frame
//
// PGPASSWORD is in three of these clips and its value is in none of
// them, because there is no value to be in them: the fixture holds a
// reference (`from_secret.file`) and no read on this API carries what it
// resolves to. The cell says where the value comes from and "never
// shown", and there is no control anywhere on the card that offers to
// reveal one.
//
// # Why most of these are whole-window rather than cropped
//
// A cropped clip anchors on one region in document coordinates, which is
// what lets a crop survive a scroll. It cannot survive a NAVIGATION: the
// anchor is measured on the first frame, and the same region on the next
// run sits at a different offset because that run has a different number
// of banners above it. Four of these clips move between runs or scroll
// most of a long card, so they photograph the window. Nothing is staged
// to make that read better; it is the page as the application drew it.

import { Clip, mb, openApp, screensTotal, settle, typeInto, VIEWPORT, withDevServer } from "./harness.mjs";

/** The GIF is written narrower than the window it was captured from, so
 *  a 2x capture downsamples into it. That is what keeps 13px UI text
 *  readable at documentation size. */
const WINDOW = VIEWPORT;
const WIDTH = 1100;

/** The one layout claim on the page, and the supported way to make it:
 *  a viewport, passed to openApp for that block only. */
const NARROW = { width: 480, height: 900 };
const NARROW_WIDTH = 460;

const clips = [];

await withDevServer(async (app) => {
  /** The same URL openApp builds, for the clips that move between runs
   *  inside one recording. A run screen carries no link to another run —
   *  it is reached from a backup set or from the run list — so this is
   *  the address bar and not a control that was missed. */
  const run = (id) => app.base + "/workflow-runs/" + id + "?scenario=default";

  // ------------------------------------------- five stages, in one order
  //
  // The live run first, because it is the one that shows the ladder
  // being read rather than reported: one stage is done, one is running
  // now, the two after it are pending, and the backup sits between the
  // before and after halves as a row of its own that is not a hook step.
  // Its "Backup set before" stage has a directory and planned nothing,
  // and says exactly that.
  //
  // Then a second run, for the other empty stage. wfr_7b03d9's
  // deployment configures no global before directory at all, and the
  // sentence it gets is a different sentence. Two stages that ran
  // nothing for two different reasons is the whole point of drawing
  // five.
  {
    const { page } = await openApp(app, { path: "/workflow-runs/wfr_a91f07", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await page.getByRole("heading", { name: "Stages", level: 2 }).waitFor();
    await settle(page, 600);

    const clip = new Clip(page, "wf-run-timeline", { width: WIDTH });
    await clip.frame(2.6);
    await page.mouse.move(640, 500);
    await page.mouse.wheel(0, 420);
    await clip.frame(2.6);
    await page.mouse.wheel(0, 420);
    await clip.frame(3.0);
    await page.mouse.wheel(0, 420);
    await clip.frame(2.8);

    await page.goto(run("wfr_7b03d9"));
    await page.getByText("No directory is configured for this stage").waitFor();
    await settle(page, 700);
    await clip.frame(2.2);
    await page.mouse.move(640, 500);
    await page.mouse.wheel(0, 380);
    await clip.frame(3.6);
    clips.push(await clip.write());
  }

  // ----------------------------------------- one terminal, per selection
  //
  // #815. Nothing is open until something is selected, one is open at a
  // time, and it is a read: no input reaches the script, and the
  // sequences that could retitle a window or plant a link are removed
  // before drawing rather than escaped into it.
  //
  // The second selection is the one worth the hold. 40-quiesce-remote
  // was signalled on its timeout and never confirmed its exit, its last
  // captured line is on stderr, and above it sits the engine's own
  // truncation marker — output this product dropped, drawn as a
  // statement about the log. The header says "output ended" because the
  // step is finished; a running step's terminal keeps following by
  // cursor instead, which is what the live run's does.
  {
    const { page } = await openApp(app, { path: "/workflow-runs/wfr_2f91a4", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await page.getByText("No step is selected").waitFor();
    await page.mouse.move(640, 500);
    await page.mouse.wheel(0, 640);
    await settle(page, 500);

    const clip = new Clip(page, "wf-step-terminal", { width: WIDTH });
    await clip.frame(2.4);

    await page.getByRole("button", { name: /20-freeze-db\.local\.sh/ }).click();
    const freeze = page.getByRole("region", { name: "Log for 20-freeze-db.local.sh" });
    await freeze.waitFor();
    await freeze.scrollIntoViewIfNeeded();
    await settle(page, 700);
    await clip.frame(3.4);

    await page.getByRole("button", { name: /40-quiesce-remote\.remote\.sh/ }).click();
    const quiesce = page.getByRole("region", { name: "Log for 40-quiesce-remote.remote.sh" });
    await quiesce.waitFor();
    await quiesce.scrollIntoViewIfNeeded();
    await settle(page, 700);
    await clip.frame(4.4);

    // The timestamps are a rendering choice this terminal owns, and
    // turning them off is the cheapest proof that what is on screen is
    // being drawn from records rather than pasted.
    await quiesce.getByRole("button", { name: "Timestamps" }).click();
    await settle(page, 400);
    await clip.frame(2.6);
    clips.push(await clip.write());
  }

  // ------------------------- a before hook failed, so nothing was backed up
  //
  // The verdict that matters is Backup: SKIPPED. The run never got as
  // far as a transfer, so there is no backup to call failed, and the
  // workflow cell names the script that stopped it rather than leaving
  // an operator to find it in the ladder. Its cleanup still ran and
  // still succeeded, which is the third verdict doing its own job.
  {
    const { page } = await openApp(app, { path: "/workflow-runs/wfr_7b03d9", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await page.getByRole("group", { name: "Workflow run verdicts" }).waitFor();
    await settle(page, 500);

    const clip = new Clip(page, "wf-before-failure", { width: WIDTH });
    await clip.frame(4.0);
    await page.mouse.move(640, 500);
    await page.mouse.wheel(0, 360);
    await clip.frame(3.0);

    await page.getByRole("button", { name: /10-freeze\.local\.sh/ }).click();
    const log = page.getByRole("region", { name: "Log for 10-freeze.local.sh" });
    await log.waitFor();
    await log.scrollIntoViewIfNeeded();
    await settle(page, 700);
    await clip.frame(4.0);
    clips.push(await clip.write());
  }

  // ------------------------------------------ three verdicts, three facts
  //
  // Four runs, because the claim is about combinations and one run only
  // has one. A backup that landed beside a cleanup that did not
  // (wfr_2f91a4). A run recovered by acknowledgement, whose cleanup is
  // still recorded as failed because acknowledging is somebody saying
  // they dealt with it and not the hooks having run (wfr_44b2e1). A
  // failed backup whose hooks all did their job (wfr_c17e88). And a
  // bypassed run, where the workflow verdict is Skipped with a sentence:
  // a green one there would say hooks ran and passed.
  {
    const { page } = await openApp(app, { path: "/workflow-runs/wfr_2f91a4", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await page.getByRole("group", { name: "Workflow run verdicts" }).waitFor();
    await settle(page, 500);

    const clip = new Clip(page, "wf-status-split", { width: WIDTH });
    await clip.frame(4.0);
    for (const id of ["wfr_44b2e1", "wfr_c17e88", "wfr_5c40b2"]) {
      await page.goto(run(id));
      await page.getByRole("group", { name: "Workflow run verdicts" }).waitFor();
      await settle(page, 700);
      await clip.frame(4.0);
    }
    clips.push(await clip.write());
  }

  // --------------------------------------- a hold, and the two ways out
  //
  // Reconcile marked this run recovery_required on startup and replayed
  // nothing. The backup set is blocked until somebody accounts for it,
  // and the banner offers exactly the two exits the engine has: resume
  // the cleanup out of the run's own retained scripts, or acknowledge in
  // writing that it was dealt with by hand.
  //
  // There is no third control and nothing in this clip closes the
  // banner. The acknowledgement is opened, a reason is typed — the
  // service requires one, so the button is off until there is one — and
  // then cancelled, which puts the two exits back exactly as they were.
  // A banner an operator could dismiss would be a banner that stopped
  // being the record of a machine possibly still quiesced.
  {
    const { page } = await openApp(app, { path: "/workflow-runs/wfr_2f91a4", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await page.getByRole("button", { name: "Resume cleanup" }).waitFor();
    await settle(page, 500);

    const clip = new Clip(page, "wf-recovery", { width: WIDTH });
    await clip.frame(3.6);
    await page.getByRole("button", { name: "Resume cleanup" }).hover();
    await clip.frame(1.6);
    await page.getByRole("button", { name: "Acknowledge\u2026" }).click();
    await page.getByRole("button", { name: "Record acknowledgement" }).waitFor();
    await settle(page, 500);
    await clip.frame(3.2);
    await typeInto(
      clip,
      page.getByRole("textbox", { name: "What was done about this run" }),
      "thawed the database and unmounted the scratch volume by hand"
    );
    await clip.frame(3.4);
    await page.getByRole("button", { name: "Cancel acknowledgement" }).click();
    await settle(page, 500);
    await clip.frame(2.6);
    clips.push(await clip.write());
  }

  // ------------------------- one set's scripts, and what checking them is
  //
  // The check is a button and not a poll because it opens a socket to
  // the Host Workflow Runner and an SSH connection to the source, and a
  // panel that read it on mount would probe somebody's production
  // database host every time a page was opened.
  //
  // What comes back is an order, a target, a size and a hash per script,
  // and a findings badge that is never "clean" unless the rules actually
  // ran: the 884 B remote script reads "not examined", which is not a
  // pass. The badge that is opened is the blocking one. BSH003 — a
  // recursive forced delete that becomes a root-level delete the moment
  // an expansion is empty — is the only rule today at error severity,
  // and an error is what refuses a save of this configuration. Nothing
  // in any of these scripts was executed to produce a finding; the bytes
  // were parsed and walked in this process.
  {
    const { page } = await openApp(app, { path: "/sets/production/postgres-primary", viewport: WINDOW });
    await settle(page, 1400);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    const check = page.getByRole("button", { name: "Check this set's hooks" });
    await check.scrollIntoViewIfNeeded();
    await settle(page, 600);

    const clip = new Clip(page, "wf-set-workflow-tab", { width: WIDTH });
    await clip.frame(2.8);
    await check.click();
    await page.getByText("before/20-freeze-db.local.sh").waitFor();
    await settle(page, 900);
    await clip.frame(4.0);

    const findings = page.getByRole("button", { name: "1 error, 3 more" });
    await findings.scrollIntoViewIfNeeded();
    await settle(page, 400);
    await clip.frame(2.0);
    await findings.click();
    const bsh = page.getByText("BSH003", { exact: true }).first();
    await bsh.waitFor();
    await bsh.scrollIntoViewIfNeeded();
    await settle(page, 700);
    await clip.frame(4.6);
    clips.push(await clip.write());
  }

  // -------------------------------- what a hook will actually be handed
  //
  // Two tables, because they answer two questions. The first is what
  // THIS set configures. The second is what a hook ends up with after
  // the merge, with the layer each value came from and what it shadowed.
  //
  // In between, the rule that is easiest to get wrong: the BACKUPD_
  // prefix is refused in configuration, at validation time, rather than
  // being accepted and then silently overridden at merge time. Typing
  // one in is the whole demonstration — the name is refused with a
  // reason and the save is off, so a request that could not succeed
  // never leaves the browser.
  //
  // And PGPASSWORD, which is the reason the second table exists in this
  // form. Its cell is a location and the words "never shown", because
  // the value is not something any read on this API carries. Nothing
  // here resolves it, nothing offers to reveal it, and a hook's
  // inherited environment is not the daemon's: it is the sanitized
  // baseline, plus these layers, plus the built-ins.
  {
    const { page } = await openApp(app, { path: "/sets/production/postgres-primary", viewport: WINDOW });
    await settle(page, 1400);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    const name = page.getByRole("textbox", { name: "Name", exact: true });
    await name.scrollIntoViewIfNeeded();
    await settle(page, 600);

    const clip = new Clip(page, "wf-env-table", { width: WIDTH });
    await clip.frame(3.0);
    await typeInto(clip, name, "BACKUPD_PHASE", { chunks: 2 });
    await page.getByText("set by this product from the run it belongs to").waitFor();
    await settle(page, 500);
    await clip.frame(4.0);
    await name.fill("");
    await settle(page, 400);
    await clip.frame(1.6);

    const merged = page.getByText("What a hook will see");
    await merged.scrollIntoViewIfNeeded();
    await settle(page, 600);
    await clip.frame(4.4);
    await page.mouse.move(500, 500);
    await page.mouse.wheel(0, 300);
    await clip.frame(3.4);
    clips.push(await clip.write());
  }

  // ------------------- the deployment-wide card, and the row it cannot fill
  //
  // The two stage directories and the default timeout are policy, so
  // they are fields with a Save each. The runner's socket and its token
  // file are reported and not editable: the installer writes them,
  // because they differ between a container and a bare-metal install of
  // the same deployment.
  //
  // The last frame is the honest one. "Host Workflow Runner: Configured"
  // is the presence of both halves of an address and nothing on this
  // read contacts the runner at all, so the row beneath it — liveness,
  // version and execution user — says "Not on this read" rather than
  // inventing a health check. A cell that said "Answering" here would
  // report a configured-but-dead runner as healthy, which is the one
  // wrong answer this card must never give. The live answer comes from a
  // set's own hook check, or from `backupd workflow-runner status`.
  {
    const { page } = await openApp(app, { path: "/settings", viewport: WINDOW });
    await settle(page, 1400);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await page.getByRole("heading", { name: "Workflow", exact: true, level: 2 }).scrollIntoViewIfNeeded();
    await settle(page, 600);

    const clip = new Clip(page, "wf-settings-runner", { width: WIDTH });
    await clip.frame(3.2);
    await page.mouse.move(500, 500);
    await page.mouse.wheel(0, 360);
    await clip.frame(3.0);
    const liveness = page.getByText("Runner liveness, version and execution user");
    await liveness.scrollIntoViewIfNeeded();
    await settle(page, 500);
    await clip.frame(4.8);
    clips.push(await clip.write());
  }

  // ------------------------------------------------------------ dark mode
  //
  // #618's rule, on this wave's surfaces. The whole window is in frame
  // because that is the only way to see a theme, and the toggle pressed
  // is the application's own. The step terminal is opened while dark,
  // because a terminal is the one panel that already had its own dark
  // palette and is therefore the one that can look wrong in the other
  // theme without anybody noticing.
  {
    const { page } = await openApp(app, { path: "/workflow-runs/wfr_2f91a4", viewport: WINDOW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await page.getByRole("group", { name: "Workflow run verdicts" }).waitFor();
    await settle(page, 500);

    const clip = new Clip(page, "wf-dark-mode", { width: WIDTH });
    await clip.frame(2.4);
    await page.getByRole("button", { name: "Toggle colour theme" }).click();
    await settle(page, 500);
    await clip.frame(3.4);
    await page.mouse.move(640, 500);
    await page.mouse.wheel(0, 640);
    await clip.frame(2.6);
    await page.getByRole("button", { name: /20-freeze-db\.local\.sh/ }).click();
    const log = page.getByRole("region", { name: "Log for 20-freeze-db.local.sh" });
    await log.waitFor();
    await log.scrollIntoViewIfNeeded();
    await settle(page, 700);
    await clip.frame(4.0);
    await page.getByRole("button", { name: "Toggle colour theme" }).click();
    await settle(page, 500);
    await clip.frame(2.4);
    clips.push(await clip.write());
  }

  // --------------------------------------------------- 480px, stacked
  //
  // The verdict triple is a three-column grid that collapses to one, and
  // a collapse is a claim about a window rather than about a component.
  // The stage ladder and the step rows keep their own columns at this
  // width, which is the thing worth photographing: a run screen an
  // operator reaches from a phone while the source is sitting quiesced
  // is the case this layout exists for.
  {
    const { page } = await openApp(app, { path: "/workflow-runs/wfr_2f91a4", viewport: NARROW });
    await settle(page, 1200);
    await page.getByRole("button", { name: "Hide terminal" }).click();
    await page.getByRole("group", { name: "Workflow run verdicts" }).waitFor();
    await settle(page, 500);

    const clip = new Clip(page, "wf-narrow", { width: NARROW_WIDTH });
    await clip.frame(3.0);
    await page.mouse.move(240, 500);
    await page.mouse.wheel(0, 460);
    await clip.frame(3.0);
    await page.mouse.wheel(0, 460);
    await clip.frame(3.0);
    await page.getByRole("button", { name: /20-freeze-db\.local\.sh/ }).click();
    const log = page.getByRole("region", { name: "Log for 20-freeze-db.local.sh" });
    await log.waitFor();
    await log.scrollIntoViewIfNeeded();
    await settle(page, 700);
    await clip.frame(4.0);
    clips.push(await clip.write());
  }
});

const total = clips.reduce((n, c) => n + c.bytes, 0);
console.log("\n" + clips.length + " clips, " + mb(total));
const { count, bytes } = screensTotal();
console.log("screens/ now holds " + count + " files, " + mb(bytes));
