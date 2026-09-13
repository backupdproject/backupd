/**
 * Adding a backup set, in six steps, and the same six steps as an
 * unconfigured instance's first run.
 *
 * Nearly all of the state here is local, and that is a decision rather
 * than a default: nothing outside this component reads an answer while
 * the wizard is open, so only the one fact another screen also reports, a
 * changed host key, lives on the shared graph. state/wizardNodes.ts holds
 * the argument.
 *
 * Two shapes recur and are worth knowing before reading the steps. Every
 * field is controlled rather than defaulted, because a step's subtree
 * unmounts while another step is showing and a defaulted input would hand
 * the review step the example text instead of what was typed. And several
 * pieces of state remember WHAT they were established for, not just that
 * they were: host trust records the host and port it was granted for, the
 * probe records which host its results describe. Editing a hostname after
 * trusting a different one must not leave the page saying "trusted".
 *
 * Nothing in this component ever holds key material beyond the single
 * import call. What the review step and the save path carry are
 * references: a key id, and the known-hosts line that trust actually
 * anchors to.
 */
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { usePlatform } from "@shared/platform/PlatformContext";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { BackupdError } from "@shared/api/contracts";
import type { ConnectionTestOutcome, SSHKeyListing, ValidatorCatalogEntry } from "@shared/api/contracts";
import { describeFailure } from "@shared/api/failure";
import { PageHeader } from "@shared/components/PageHeader";
import { WarningBanner } from "@shared/components/WarningBanner";
import { FingerprintDisplay } from "@shared/components/FingerprintDisplay";
import { FieldHelp, HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";
import type { CompletionMethod } from "@shared/types/backup";
import { graph, useCausl } from "@shared/state/graph";
import { setsNode } from "@shared/state/appNodes";
import { fetchResource } from "@shared/state/resource";
import { resetWizardAnswers, wizardCanSaveNode, wizardHostKeyChangedNode } from "@shared/state/wizardNodes";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";
// Promoted out of this file by #788 so EPIC K's flows draw the same rail,
// the same step body and the same radio/checkbox shapes rather than
// copies of them (docs/design/788-incremental-ui-mockup.md). The rail
// also stopped hardcoding six columns on the way out, which is what lets
// this wizard be eight steps and the restore flow four.
import { StepBody, StepRail } from "@shared/components/WizardStep";
import { Choice, Toggle } from "@shared/components/Choice";
import { Rows, Row } from "@shared/components/Definitions";
import {
  CONSISTENCY_COPY,
  DOMAIN_BOUNDARIES,
  ENGINE_COPY,
  VERIFICATION_COPY
} from "@shared/components/EngineBadge";
import { NEW_SET_DEFAULTS } from "@shared/pages/incrementalConfigFields";
import type { BackupEngine, SourceConsistency, VerificationLevel } from "@shared/types/snapshot";

/**
 * Eight steps, and the ORDER is the argument (issue #788's design gate,
 * docs/design/788-incremental-ui-mockup.md).
 *
 *   1. Source            — where the data is, and which directory a run walks.
 *   2. Connection test   — the credentials, the host key and what those
 *                          two could actually DO, including the write
 *                          probe (#852). It comes second because its
 *                          answer constrains step 7.
 *   3. Engine            — the one irreversible choice, asked once there
 *                          is enough context to answer it.
 *   4. Repository domain — only a real question for the incremental
 *                          engine, which is why it cannot come earlier.
 *   5. Consistency       — what the operator arranged on the server.
 *   6. Verification      — how hard each backup is checked. For an
 *                          artifact set this is the completion method and
 *                          the validator, which are the same question
 *                          answered by the other engine.
 *   7. Storage/retention — where the copy lives, how long it is kept, and
 *                          the source-deletion control #852 governs.
 *   8. Review            — every answer, then one save.
 *
 * Steps 4 and 5 do not disappear for an Artifact set: they say why they
 * do not apply. A rail that changes length under the operator teaches
 * nothing; a step that explains itself teaches the difference between the
 * two engines at the moment it matters.
 */
const STEPS = [
  "Source",
  "Connection test",
  "Engine",
  "Repository",
  "Consistency",
  "Verification",
  "Retention",
  "Review"
] as const;

/** The value `domainChoice` holds while the operator is naming a domain
 *  this deployment does not declare yet. A sentinel rather than a second
 *  boolean, so "which domain" has exactly one answer at all times. */
const NEW_DOMAIN = "\u0000new";

/** Both closed vocabularies in the order they are offered, which is
 *  weakest first: the choice is a ladder, and an operator reading it
 *  top-to-bottom should be reading it in one direction. */
const CONSISTENCY_ORDER: readonly SourceConsistency[] = [
  "live_best_effort",
  "externally_quiesced",
  "external_snapshot"
];

const VERIFICATION_ORDER: readonly VerificationLevel[] = [
  "structural",
  "content_sample",
  "content_full",
  "restore_drill"
];

/** Shown only until the real probe (issue #146) resolves for the first
 *  time — see the "Verify server" step below — so step 3 never renders
 *  a completely blank fingerprint while that request is in flight.
 *  Never what "Trust host" actually trusts: that always reads the real
 *  probedKnownHostsLine state, never this constant. */

function errorMessage(e: unknown, fallback: string): string {
  return e instanceof BackupdError ? e.api.message : fallback;
}

function completionStrategyFor(method: CompletionMethod): "rename" | "marker" | "stable" {
  if (method === "atomic-rename") return "rename";
  if (method === "stable-size") return "stable";
  return "marker";
}

function completionSummaryLabel(method: CompletionMethod): string {
  if (method === "atomic-rename") return "atomic rename";
  if (method === "stable-size") return "stable size";
  return "completion marker";
}

/** Issue #176: the same wizard, run on an instance that has no
 *  configuration yet.
 *
 *  Only two things change, and both are consequences of there being
 *  nothing on disk rather than of a different flow. The save goes to POST
 *  /api/v1/system/first-run instead of POST /api/v1/backup-sets, because
 *  there is no configuration to fold a set into. And "Save, enable & run"
 *  is not offered: an unconfigured instance has no service to submit a
 *  run to until the configuration it is about to write has been opened,
 *  so a button promising an immediate run would be promising something
 *  the backend deliberately does not do (core/service's
 *  CreateInitialConfig ignores run_immediately, and says why). */
export interface BackupSetWizardPageProps {
  readOnly: boolean;
  firstRun?: boolean;
  /** Called with the wizard's own persisted result instead of navigating
   *  to /sets, which does not exist yet on an unconfigured instance. */
  onFirstRunComplete?: (restartRequired: boolean) => void;
}

export function BackupSetWizardPage({ readOnly, firstRun = false, onFirstRunComplete }: BackupSetWizardPageProps) {
  const navigate = useNavigate();
  const { bridge } = usePlatform();
  const caps = bridge.capabilities();
  const api = useApi();

  const [step, setStep] = useState(1);

  // Source/authentication field values are local, in-progress wizard
  // state (nothing outside this component reads them while the wizard is
  // open) — but they must be CONTROLLED, not `defaultValue`, or they are
  // lost the moment another step's panel is shown (each step's subtree
  // unmounts while it isn't the active one) and the review step below
  // has nothing real to read back.
  const [source, setSource] = useState({
    name: "Production PostgreSQL",
    host: "prod-db-01.internal",
    port: "22",
    username: "backup-agent"
  });
  const updateSource = (field: keyof typeof source, value: string) =>
    setSource((s) => ({ ...s, [field]: value }));

  const [keySource, setKeySource] = useState<"generate" | "managed" | "import">("generate");
  const [importPasted, setImportPasted] = useState("");
  const [importedFingerprint, setImportedFingerprint] = useState<string | null>(null);
  // The backend reference the import step's fingerprint stands for
  // (issue #146): CreateBackupSetRequest.sshKeyId, never the key
  // material itself, which never lives in this component's state at
  // all past the one importSSHKey call below.
  const [importedKeyId, setImportedKeyId] = useState<string | null>(null);
  // Issue #592: the keys this deployment already holds, which is what
  // finally puts something behind the "Use managed key" radio. null is
  // "not read yet" and [] is "read, and there are none", and those are
  // rendered differently: a wizard that showed an empty list before it
  // had asked would be telling a brand new operator they have no keys.
  const [managedKeys, setManagedKeys] = useState<SSHKeyListing[] | null>(null);
  const [managedKeysError, setManagedKeysError] = useState<string | null>(null);
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);

  // Discovery/storage fields a valid create-backup-set request needs
  // (issue #146): promoted from #98's `defaultValue`-only placeholders
  // to real controlled state, same reasoning as `source` above — the
  // Save buttons need to read back whatever an operator actually typed
  // here, not the built-in example text.
  const [remoteFolder, setRemoteFolder] = useState("/backups/postgresql/");
  const [includePatterns, setIncludePatterns] = useState("*.dump.zst");
  const [localDestination, setLocalDestination] = useState(() => bridgeDefaultPath(bridge.deployment.storageMount));

  // The wizard's other answers — completion method and the deletion
  // acknowledgement — are local too, same tier as `source` above: nothing
  // outside this component reads them while the wizard is open (see
  // state/wizardNodes.ts's module comment for the actual local-vs-graph
  // rule).
  const [completion, setCompletion] = useState<CompletionMethod>("completion-marker");
  const [acknowledged, setAcknowledged] = useState(false);
  // Issue #316: "pull from here, never delete here" (#282), declared at
  // save time rather than only by hand-editing config.yaml afterward.
  // Read at Review below to swap the remote-source-handling copy, and in
  // saveDisabled below to waive the deletion acknowledgement — there is
  // nothing to acknowledge deleting when this is checked, the same
  // reasoning "Save disabled" already gets for free by being its own
  // button (see saveDisabled's own comment).
  const [readOnlySource, setReadOnlySource] = useState(false);

  /**
   * EPIC K's answers (issue #788), local for the same reason every other
   * wizard answer is: nothing outside this component reads them while the
   * wizard is open.
   *
   * The engine defaults to Artifact, and the reason is not timidity: it
   * is the choice that cannot be undone. `BackupSetSpec.engine` defaults
   * to "artifact" server-side, so a wizard that pre-selected the other
   * one would make the UI's default and the contract's default disagree,
   * and an operator who pressed through without reading step 3 would get
   * a set whose engine can never be changed and whose history lives in a
   * repository they did not choose. The step argues for both engines and
   * Incremental is one click away.
   */
  const [engine, setEngine] = useState<BackupEngine>(NEW_SET_DEFAULTS.engine);
  const incremental = engine === "kopia";
  const [domainChoice, setDomainChoice] = useState("");
  const [newDomain, setNewDomain] = useState("");
  const [consistency, setConsistency] = useState<SourceConsistency>(NEW_SET_DEFAULTS.sourceConsistency);
  const [verificationLevel, setVerificationLevel] = useState<VerificationLevel>(
    NEW_SET_DEFAULTS.verificationLevel
  );
  // The verification budget as TEXT, because these three fields are
  // numbers an operator types and an empty one means "inherit the
  // deployment's own setting" — which a number-typed state cannot say
  // without inventing a sentinel. They are parsed once, at save.
  const [samplePercent, setSamplePercent] = useState(NEW_SET_DEFAULTS.samplePercent);
  const [fullEveryDays, setFullEveryDays] = useState(NEW_SET_DEFAULTS.fullEveryDays);
  const [drillEveryDays, setDrillEveryDays] = useState(NEW_SET_DEFAULTS.drillEveryDays);

  // The domains this deployment declares, for step 4's picker. A failure
  // is reported as a failure rather than turned into an empty list: "you
  // have no repository domains" is an ordinary first-run state and "I
  // could not read them" is a fault, and only one of them means the
  // operator should stop and look.
  const domains = useAsync(() => api.listRepositories(), [api]);
  // The deployment's retention chain, which step 7 REPORTS rather than
  // offers to change: retention is one global policy (#111), configured
  // on the Settings page, and a second editor for it here is the
  // decorative per-set chain #299 removed from this wizard.
  const retention = useAsync(() => api.getSettings().then((s) => s.retention), [api]);

  // The domain this set will actually be created in: whichever existing
  // one was picked, or the name being typed for a new one. Empty means
  // "the deployment's default", which is what the service does with an
  // absent repository_domain.
  const repositoryDomain = domainChoice === NEW_DOMAIN ? newDomain.trim() : domainChoice;

  // Host trust is local too, but it needs to remember WHICH host/port it
  // was granted for, not just whether it was granted: editing the
  // hostname after trusting host A must not leave host B showing
  // "trusted" (see revalidateHostTrust below, wired to the field's
  // onBlur).
  const [hostTrusted, setHostTrusted] = useState(false);
  const [trustedHostKey, setTrustedHostKey] = useState<string | null>(null);
  // The known_hosts line CreateBackupSetRequest.knownHostsLine carries
  // once "Trust host" is actually clicked — the real trust anchor a
  // subsequent connection is checked against, not merely display text
  // (issue #146).
  const [trustedKnownHostsLine, setTrustedKnownHostsLine] = useState<string | null>(null);

  // Issue #624: the connection test, and what it was run for.
  //
  // Two pieces of state rather than one, on exactly the reasoning host
  // trust above already follows: what matters is not that a check passed
  // once, it is that it passed for the values this form is about to save.
  // A result that outlived the host it was about would be a green tick
  // standing for a connection nobody ever made, which is the same defect
  // as a "trusted" badge under an edited hostname.
  //
  // Local state, not a graph node. Nothing outside this component reads
  // it while the wizard is open, which is the bar state/wizardNodes.ts
  // holds every other wizard answer to.
  const [testing, setTesting] = useState(false);
  const [connectionResult, setConnectionResult] = useState<ConnectionTestOutcome | null>(null);
  const [connectionError, setConnectionError] = useState<string | null>(null);
  const [connectionTestedFor, setConnectionTestedFor] = useState<string | null>(null);

  // Real host-key probe (issue #146), replacing #98's hardcoded
  // fingerprint constants: probedFor is the "host:port" the CURRENT
  // probe results are for, so the effect below re-probes automatically
  // whenever source.host/source.port changes while step 3 is open,
  // without needing revalidateHostTrust to also manage this state.
  const [probing, setProbing] = useState(false);
  const [probeError, setProbeError] = useState<string | null>(null);
  const [probedFor, setProbedFor] = useState<string | null>(null);
  const [probedFingerprint, setProbedFingerprint] = useState<string | null>(null);
  const [probedAlgorithm, setProbedAlgorithm] = useState<string | null>(null);
  const [probedKnownHostsLine, setProbedKnownHostsLine] = useState<string | null>(null);

  // wizard.hostKeyChanged lives on the shared graph — see
  // state/wizardNodes.ts for why this one node earns that spot when the
  // rest of the wizard's answers don't. Resetting it on mount means
  // opening "Add backup set" a second time never inherits a previous
  // session's stale value.
  //
  // Committed synchronously during render, not in an effect — an effect
  // runs after the first paint has already happened, so a freshly opened
  // wizard would flash whatever a previous session last left on the
  // graph before self-correcting on the next render. Same pattern (and
  // same reasoning) as PlatformContext.tsx's bridge-mounted commit.
  // Step 5's application-validator picklist (issue #162). Local
  // useState, not a graph node: nothing outside this component reads
  // either the catalog or the operator's choice while the wizard is
  // open, which is the same bar state/wizardNodes.ts holds every other
  // wizard answer to.
  //
  // validatorCatalog is null until the fetch settles, "unavailable" if
  // it failed. The distinction matters: an empty picklist and a picklist
  // that could not be loaded look identical on screen and mean opposite
  // things, and this step's whole history is a control that looked real
  // and did nothing.
  const [validatorCatalog, setValidatorCatalog] = useState<ValidatorCatalogEntry[] | null>(null);
  const [validatorCatalogFailed, setValidatorCatalogFailed] = useState(false);
  const [validatorId, setValidatorId] = useState("");

  useEffect(() => {
    let cancelled = false;
    api
      .listValidators()
      .then((catalog) => {
        if (!cancelled) setValidatorCatalog(catalog);
      })
      .catch(() => {
        if (!cancelled) setValidatorCatalogFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [api]);

  // Issue #592: the key store, read once when the wizard opens so the
  // "Use managed key" radio has something behind it. A failure is
  // reported rather than turned into an empty list, because "you have no
  // keys" and "I could not read the store" are different sentences and
  // only one of them is ever true.
  useEffect(() => {
    let cancelled = false;
    api
      .listSSHKeys()
      .then((keys) => {
        if (!cancelled) setManagedKeys(keys);
      })
      .catch((e: unknown) => {
        if (!cancelled) {
          setManagedKeys([]);
          setManagedKeysError(
            describeFailure(e, "The keys this deployment already holds could not be read.").message
          );
        }
      });
    return () => {
      cancelled = true;
    };
  }, [api]);

  const selectedValidator = (validatorCatalog ?? []).find((v) => v.id === validatorId);

  const [hasResetOnMount, setHasResetOnMount] = useState(false);
  if (!hasResetOnMount) {
    resetWizardAnswers();
    setHasResetOnMount(true);
  }

  const hostKeyChanged = useCausl(wizardHostKeyChangedNode);
  // §12 — the graph answers "is saving structurally possible at all"
  // (not blocked by the app-wide readOnly node (#106) or a changed host
  // key (WP 2.3's "changed host key blocks operation")); combined here
  // with the session's own acknowledgement to get the actual gate. This
  // is the only place `readOnly` is consulted for gating — the `readOnly`
  // prop below is read only to pick which hint text to show.
  const canSave = useCausl(wizardCanSaveNode);
  // M7 (#146 review): folds in the two preconditions this issue itself
  // added — an imported key and a trusted host — that handleSave already
  // refuses to save without (see its own early-return guards below).
  // Before this, only !canSave || !acknowledged gated the button, so
  // clicking Save with no key imported or no host trusted was not
  // disabled at all: it fired handleSave, which then rejected the
  // request via its own ad hoc check and a freshly-set saveError string,
  // instead of the button structurally refusing to be clicked in the
  // first place — the exact clickable-then-rejected shape this
  // safety-tool's own review flags everywhere else it appears.
  // readOnlySource waives the acknowledgement, not canSave/keySource/
  // trustedKnownHostsLine: a read-only set still needs a real, trusted
  // connection to pull backups FROM — declaring it read-only only takes
  // away the one thing there would otherwise be to acknowledge deleting.
  // Issue #624: the values a connection test is about, as one string.
  //
  // Everything the six steps actually prove and nothing else: who is
  // dialling what, with which key, trusting which line, to read which
  // folder. A change to any of them makes an earlier result an answer
  // about a different connection, so comparing this against
  // connectionTestedFor is what expires it. The local path, the
  // completion method and the validator are deliberately absent: no
  // connection test has an opinion about any of them, and expiring a
  // proven connection because somebody fixed a typo in a local directory
  // would train an operator to press the button without reading it.
  const connectionSubject = [
    source.host,
    source.port,
    source.username,
    importedKeyId ?? "",
    trustedKnownHostsLine ?? "",
    remoteFolder
  ].join("\u0000");
  const connectionProven =
    connectionResult !== null && connectionResult.ok && connectionTestedFor === connectionSubject;

  // Issue #852: the connection test also proves whether these
  // credentials may WRITE to the source, and a source that refused the
  // write probe cannot be deleted from, so this set cannot be saved with
  // delete-from-source on. The service refuses exactly that request
  // (ErrSourceNotWritable / 409 BACKUP_SET_SOURCE_NOT_WRITABLE), so a
  // form that let it be submitted would be offering a save it knows will
  // be refused.
  //
  // Read off connectionProven rather than off the result alone: a result
  // for OTHER values proves nothing about the source on the form now,
  // which is the same rule Save already applies.
  const sourceNotWritable = connectionProven && !connectionResult.writable;
  // readOnlyEffective, not readOnlySource, is what the rest of this page
  // means by "read-only" from here on. Deriving it rather than pushing
  // the value into state on a test result keeps one source of truth: an
  // effect that set the checkbox would leave an operator's own answer
  // overwritten and unrecoverable when they point the form at a
  // writable host instead.
  const readOnlyEffective = readOnlySource || sourceNotWritable;
  // A stable id so the disabled checkbox can point at the sentence that
  // explains it (aria-describedby), which is the only way a screen
  // reader gets the reason: a disabled control announces nothing about
  // why it is disabled.
  const readOnlyForcedNoteId = "wizard-read-only-forced";

  const saveDisabled =
    !canSave ||
    (!acknowledged && !readOnlyEffective) ||
    keySource === "generate" ||
    !importedKeyId ||
    !trustedKnownHostsLine ||
    // Issue #624: a source is not relied on until it has been proven.
    // The destination side has worked this way since #594, where the S3
    // wizard's Save stays disabled until its candidate check comes back
    // ok; before this, a backup set could be saved, relied on, and only
    // THEN tested, which is the order that issue turns around. A trusted
    // host key settles that the machine answering is the one whose
    // fingerprint somebody compared, and settles nothing about whether
    // the key authenticates or whether the account can read the folder.
    !connectionProven;

  // Runs the six-step check against the values on this form, through the
  // same route `backup-set create` runs before it writes (issue #624).
  //
  // The candidate mode, not the by-id one: there is no set yet, which is
  // the whole point. Nothing is persisted by this call and no trust
  // decision is made or revised; the known_hosts line travels as the one
  // the operator already trusted on step 3.
  async function runConnectionTest() {
    const subject = connectionSubject;
    setTesting(true);
    setConnectionError(null);
    // Cleared up front rather than overwritten on success, so a check in
    // flight for edited values cannot leave the PREVIOUS result standing
    // behind an enabled Save button while it runs.
    setConnectionResult(null);
    setConnectionTestedFor(null);
    try {
      const outcome = await api.testCandidateConnection({
        host: source.host,
        port: Number(source.port) || 22,
        user: source.username,
        sshKeyId: importedKeyId ?? "",
        knownHostsLine: trustedKnownHostsLine ?? "",
        remotePath: remoteFolder
      });
      setConnectionResult(outcome);
      // Recorded as the subject the check was RUN for, captured before
      // the request rather than read again after it: an operator who
      // edits the host while the request is in flight must not have the
      // answer land against the new value.
      setConnectionTestedFor(subject);
    } catch (e) {
      setConnectionError(errorMessage(e, "Could not test this connection."));
    } finally {
      setTesting(false);
    }
  }

  const trustHost = () => {
    setHostTrusted(true);
    setTrustedHostKey(source.host + ":" + source.port);
    setTrustedKnownHostsLine(probedKnownHostsLine);
    graph.commit("wizard/trustHost", (tx) => tx.set(wizardHostKeyChangedNode, false));
  };

  // M1 fix (#98 PR #145 review): a host trusted on step 3 must not still
  // read as trusted once the operator goes back and points step 1's
  // hostname/port at a different server — that would defeat the whole
  // point of a fingerprint-pinning UI. Scoped to blur (a field losing
  // focus with a changed value), not every keystroke, so retyping the
  // same hostname mid-edit doesn't re-prompt trust on every character.
  const revalidateHostTrust = () => {
    if (!hostTrusted || trustedHostKey === null) return;
    if (trustedHostKey !== source.host + ":" + source.port) {
      setHostTrusted(false);
      setTrustedHostKey(null);
      setTrustedKnownHostsLine(null);
    }
  };

  // probeHost is issue #146's real replacement for #98's hardcoded
  // TRUSTED_FINGERPRINT/CHANGED_FINGERPRINT constants: it fetches
  // host:port's actual current host key, trusting nothing itself (see
  // core's ProbeHostKey doc) — only "Trust host" (above) turns a probed
  // result into something a later connection is actually checked
  // against.
  async function probeHost() {
    const key = source.host + ":" + source.port;
    setProbing(true);
    setProbeError(null);
    // Cleared up front, not just overwritten on success: while a new
    // probe (for a just-edited host) is in flight, "Trust host" must
    // not stay enabled against the PREVIOUS host's stale fingerprint —
    // see !probedFingerprint in the button's own disabled condition
    // below.
    setProbedFingerprint(null);
    setProbedAlgorithm(null);
    setProbedKnownHostsLine(null);
    try {
      const result = await api.probeHostKey(source.host, Number(source.port) || 22);
      setProbedFingerprint(result.fingerprint);
      setProbedAlgorithm(result.algorithm);
      setProbedKnownHostsLine(result.knownHostsLine);
    } catch (e) {
      setProbeError(errorMessage(e, "Could not reach this server to fetch its host key."));
    } finally {
      setProbedFor(key);
      setProbing(false);
    }
  }

  // Probes automatically the first time the Connection test step is
  // opened, and again whenever source.host/source.port changes while it
  // stays open — "Re-fetch fingerprint" (below) calls probeHost()
  // directly for an explicit re-check of the SAME host.
  //
  // Step 2 since #788 folded the credentials and the host key into one
  // step. The number is load-bearing: it is the only thing that starts
  // the probe, so a rail reorder that left it pointing at the old step
  // would leave "Trust host" trusting a known_hosts line nothing ever
  // fetched.
  useEffect(() => {
    const key = source.host + ":" + source.port;
    if (step === 2 && probedFor !== key && !probing) {
      // probeHost's first statement is setProbing(true), a genuine,
      // deliberate "start loading" state update synchronous with this
      // effect running — the canonical fetch-on-mount shape, not the
      // unbounded render cascade this rule otherwise guards against
      // (there is nothing recursive here: probedFor is set at the end
      // of probeHost, which is what stops this same effect from
      // re-triggering itself once the fetch resolves).
      // eslint-disable-next-line react-hooks/set-state-in-effect
      void probeHost();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, source.host, source.port]);

  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  // The set was created and the immediate run was not started (issue
  // #194's review, M4). It is neither a save failure nor a plain success,
  // so it gets its own state rather than being folded into saveError:
  // there is nothing left to retry here, and the operator has to be told
  // that no backup is running before they walk away believing one is.
  const [runNotStarted, setRunNotStarted] = useState<string | null>(null);
  // The refusal a create over an id that already has artifacts on record
  // gets when it points somewhere other than where they came from (issue
  // #411). It carries the two Save arguments as well as the message,
  // because the way out of it is to re-send THIS save with the
  // acknowledgement, not some default one: "Save disabled" that came back
  // refused has to stay a disabled save when it is confirmed.
  const [repointRefusal, setRepointRefusal] =
    useState<{ message: string; disabled: boolean; runImmediately: boolean } | null>(null);

  // handleSave is every one of the wizard's three Save buttons' onClick
  // (issue #146): "Save, enable & run" and "Save & enable" pass
  // disabled: false, differing only in runImmediately; "Save disabled"
  // passes disabled: true. A failure surfaces inline via saveError
  // (never a silent no-op); a success navigates back to the sets list,
  // after refreshing the shared setsNode so BackupSetsPage shows the
  // new set with no manual refetch of its own (appNodes.ts's setsNode +
  // resource.ts's fetchResource, exactly as that module's own doc
  // describes for a mutation elsewhere).
  async function handleSave(disabled: boolean, runImmediately: boolean, acknowledgeRepoint = false) {
    if (keySource === "generate" || !importedKeyId) {
      setSaveError(
        keySource === "generate"
          ? "Generating a key on save isn't available yet — pick a key this deployment already holds, or import one, on the Connection test step."
          : "Pick a key on the Connection test step before saving."
      );
      return;
    }
    if (!trustedKnownHostsLine) {
      setSaveError("Trust the host's fingerprint on the Connection test step before saving.");
      return;
    }
    // Issue #624. The button is already structurally disabled without
    // this; the guard is here for the same reason the two above it are,
    // which is that a handler reachable by any other route (a keyboard
    // path, a future caller, a test) must not be able to save a set
    // whose connection nothing proved.
    if (!connectionProven) {
      setSaveError("Test connection on the Connection test step before saving.");
      return;
    }

    setSaving(true);
    setSaveError(null);
    setRepointRefusal(null);
    try {
      const request = {
        name: source.name,
        host: source.host,
        port: Number(source.port) || 22,
        user: source.username,
        sshKeyId: importedKeyId,
        knownHostsLine: trustedKnownHostsLine,
        remotePath: remoteFolder,
        localPath: localDestination,
        include: includePatterns
          .split(",")
          .map((p) => p.trim())
          .filter(Boolean),
        completionStrategy: completionStrategyFor(completion),
        // Omitted, never sent as "", when no validator was chosen: the
        // backend reads an empty id as "no validator", but leaving the
        // key out says the same thing without asking it to.
        validatorId: validatorId || undefined,
        stableForSeconds: completion === "stable-size" ? 3600 : undefined,
        disabled,
        readOnly: readOnlyEffective,
        runImmediately: firstRun ? false : runImmediately,
        // Sent only when the operator actually answered the refusal, so
        // an ordinary save is never a pre-acknowledged one.
        acknowledgeRepoint: acknowledgeRepoint || undefined,
        // EPIC K (issue #788). The engine travels on every create; the
        // four incremental settings travel only for the incremental
        // engine, because an artifact set has none of them and sending a
        // repository domain with one would be asking the service to
        // either refuse the request or ignore a field.
        engine,
        ...(incremental
          ? {
              // Omitted when empty, which is the "use this deployment's
              // default domain" request rather than a domain with no
              // name.
              repositoryDomain: repositoryDomain || undefined,
              sourceConsistency: consistency,
              verificationLevel,
              // Each parsed once, here, and omitted when the field was
              // left empty: an absent key inherits the deployment's own
              // setting, and a 0 is a real and different answer ("never
              // raise the level on a cadence") that has to be able to
              // reach the wire.
              verificationSamplePercent: optionalCount(samplePercent),
              verificationFullEverySeconds: optionalDays(fullEveryDays),
              verificationRestoreDrillEverySeconds: optionalDays(drillEveryDays)
            }
          : {})
      };
      if (firstRun) {
        const result = await api.completeFirstRun(request);
        onFirstRunComplete?.(result.restartRequired);
        return;
      }
      const created = await api.createBackupSet(request);
      fetchResource(setsNode, () => api.listSets());
      if (created.runError) {
        // Deliberately no navigate: the sets list shows the set, which is
        // exactly the "it worked" reading this response contradicts. Stay
        // here and say what did not happen, with the Save buttons off,
        // because the set already exists and pressing one again would
        // create a second.
        setRunNotStarted(created.runError);
        return;
      }
      navigate("/sets");
    } catch (e) {
      const message = errorMessage(
        e,
        firstRun ? "Could not save this configuration." : "Could not save this backup set."
      );
      if (
        e instanceof BackupdError &&
        e.api.code === "BACKUP_SET_HISTORY_REPOINT_NOT_ACKNOWLEDGED"
      ) {
        // Not a save error under the buttons. The service is not saying
        // anything on this form is wrong: it is saying this id already
        // has backups on record and this form would create the set
        // somewhere else, which is a decision with its own two answers.
        setRepointRefusal({ message, disabled, runImmediately });
        return;
      }
      setSaveError(message);
    } finally {
      setSaving(false);
    }
  }

  // M7 (#146 review): each new precondition gets its own hint, in the
  // same words handleSave's own early-return guards already use, rather
  // than every disabled reason falling through to the acknowledgement
  // hint below regardless of which precondition actually failed.
  let saveHint = "";
  if (hostKeyChanged) {
    saveHint = "The host key changed since it was trusted — resolve that on the Verify server step before saving.";
  } else if (keySource === "generate" || !importedKeyId) {
    saveHint =
      keySource === "generate"
        ? "Generating a key on save isn't available yet — pick a key this deployment already holds, or import one, on the Authentication step."
        : keySource === "managed"
          ? "Pick one of the keys this deployment already holds before saving."
          : "Import an SSH key on the Authentication step before saving.";
  } else if (!trustedKnownHostsLine) {
    saveHint = "Trust the host's fingerprint on the Verify server step before saving.";
  } else if (!acknowledged && !readOnlyEffective && !readOnly) {
    // The acknowledgement comes before the connection test, and the
    // condition is now that precondition rather than the catch-all
    // saveDisabled it used to be. Both matter.
    //
    // The order is about what an operator is looking at: the
    // acknowledgement is a box on this screen with an empty tick in it,
    // and the connection test is a button they have not pressed yet, so
    // naming the box first is naming the thing they can see is unfinished.
    //
    // The condition had to become explicit the moment there was a fourth
    // precondition after it. saveDisabled is true for any of them, so a
    // catch-all here would have gone on saying "acknowledge" to somebody
    // who had already acknowledged and had not tested, which is the
    // stated-reason-is-not-the-real-reason shape M7 removed from every
    // other branch in this chain.
    saveHint = "Acknowledge remote-source handling to enable saving.";
  } else if (!connectionProven) {
    // Its own hint, on the same rule: a disabled button whose stated
    // reason is not the reason it is disabled is worse than one with no
    // reason at all.
    saveHint =
      connectionResult !== null && !connectionResult.ok
        ? "The connection test did not pass. Fix what it reports and test connection again before saving."
        : "Test connection before saving. Trusting the host key proves which machine answers, not that this key works or that the folder can be read. To build configuration for a source that cannot be reached yet, use backupd backup-set create --no-verify.";
  } else if (saveError) {
    saveHint = saveError;
  }

  return (
    <div style={{ maxWidth: 900, width: "100%", margin: "0 auto", display: "flex", flexDirection: "column", gap: 16 }}>
      <PageHeader
        back={{ label: "Cancel and return to backup sets", onClick: () => navigate("/sets") }}
        title="Add backup set"
      />

      {/* The promoted rail (components/WizardStep.tsx), which is where
          this markup used to live inline with six columns hardcoded into
          it. One tooltip id for the whole rail: what a step is, and that
          any of them can be revisited, is one explanation (#834). */}
      <StepRail steps={STEPS} step={step} onSelect={setStep} tip="wizard.set.step" />

      <section className="card">
        <div style={{ padding: "20px 22px 22px" }}>
          {step === 1 ? (
            <StepBody
              title="Source"
              lede="The remote server that produces the backup artifacts. Backupd pulls — it is never given write access to your data."
            >
              <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(228px, 1fr))", gap: "15px 18px" }}>
                <Field
                  label="Backup set name" value={source.name} onChange={(v) => updateSource("name", v)} span
                  help={FIELD_HELP.wizardSetName}
                />
                <Field
                  label="Server hostname" value={source.host} onChange={(v) => updateSource("host", v)}
                  onBlur={revalidateHostTrust} mono help={FIELD_HELP.wizardHostname}
                />
                <Field
                  label="SSH port" value={source.port} onChange={(v) => updateSource("port", v)}
                  onBlur={revalidateHostTrust} mono help={FIELD_HELP.wizardSshPort}
                />
                <Field
                  label="Username" value={source.username} onChange={(v) => updateSource("username", v)} mono
                  help={FIELD_HELP.wizardUsername}
                />
                {/* Moved here from the "Backup discovery" step #788's
                    rail replaced: what a run walks is part of naming the
                    source, and asking for it three steps later meant the
                    connection test — which lists this very folder — ran
                    before anything had said which folder. */}
                <Field
                  label="Directory to back up" value={remoteFolder} onChange={setRemoteFolder} mono span
                  help={FIELD_HELP.wizardRemoteFolder}
                />
                <Field
                  label="Ignore paths matching" value={includePatterns} onChange={setIncludePatterns} mono
                  help={FIELD_HELP.wizardIncludePatterns}
                />
              </div>
            </StepBody>
          ) : null}

          {step === 2 ? (
            <StepBody
              title="Connection test"
              lede="What Backupd could actually do with these credentials, right now. Every line below is a thing it tried, not a thing it assumes."
            >
              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", marginBottom: 10 }}>
                Credentials
              </div>
              <p style={{ margin: "0 0 14px", fontSize: 13, color: "var(--text-2)", maxWidth: "78ch" }}>
                Install the public key on the remote server. Private keys stay on this NAS and are
                never shown after creation.
              </p>
              {/* One tooltip for the whole group, on the radiogroup container
                  itself: aria-describedby is valid there, and there is no
                  single control the way HelpField's .field shape assumes.
                  The copy is honest about a fact split across three radios,
                  that only one of them actually lets you finish this
                  wizard, which is exactly the kind of thing no single
                  radio's own label says. */}
              <FieldHelp label="Key source" help={FIELD_HELP.wizardKeySource}>
                {(helpId) => (
                  <div
                    role="radiogroup" aria-label="Key source" aria-describedby={helpId}
                    style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(216px, 1fr))", gap: 10 }}
                  >
                    <Choice
                      name="keysrc" title="Generate dedicated SSH key" detail="Recommended · scoped to this set only"
                      checked={keySource === "generate"} onChange={() => setKeySource("generate")}
                    />
                    <Choice
                      name="keysrc" title="Use managed key" detail="Reuse an existing Backupd key"
                      checked={keySource === "managed"} onChange={() => setKeySource("managed")}
                    />
                    <Choice
                      name="keysrc" title="Import key" detail="Paste once · stored encrypted, never displayed"
                      checked={keySource === "import"} onChange={() => setKeySource("import")}
                    />
                  </div>
                )}
              </FieldHelp>

              {/* Issue #299: "Generate" used to show a fixed sample public
                  key (never actually generated per set) with a "Copy
                  public key" button, plus an authorized_keys instruction
                  that always named "backup-agent" regardless of the
                  username actually entered on the Source step —
                  fabricated specifics, the same class of problem as the
                  webhook line the Settings page used to show. This path
                  is already refused at save (see handleSave/saveHint
                  below), exactly like "Use managed key", so both panels
                  now say the same honest thing instead of inventing
                  detail for a path that cannot be saved. */}
              {keySource === "generate" ? (
                <div className="banner banner--info" style={{ marginTop: 18, fontSize: "var(--text-sm)" }}>
                  <span aria-hidden="true">i</span>
                  <span>
                    Generating a key on save isn&rsquo;t available yet — import a key on the Authentication
                    step instead.
                  </span>
                </div>
              ) : null}

              {/* Issue #299 stripped this radio's picklist, which showed
                  two hardcoded key names and a fabricated "Already
                  installed on 2 other backup sets" count. That was the
                  right call and it left an honest control that could not
                  do anything, for exactly one reason: nothing could list
                  the key store.

                  #592 built that listing, so the picklist is real now.
                  The names are ids the server returned, the fingerprints
                  are the public halves it read, and "used by" is counted
                  from the sets that actually reference each key rather
                  than invented. */}
              {keySource === "managed" ? (
                <div style={{ marginTop: 18, display: "flex", flexDirection: "column", gap: 8 }}>
                  {managedKeys === null ? (
                    <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                      Reading the key store&hellip;
                    </span>
                  ) : managedKeysError ? (
                    <div className="banner banner--warn" style={{ fontSize: "var(--text-sm)" }}>
                      <span aria-hidden="true">!</span>
                      <span>{managedKeysError}</span>
                    </div>
                  ) : managedKeys.length === 0 ? (
                    <div className="banner banner--info" style={{ fontSize: "var(--text-sm)" }}>
                      <span aria-hidden="true">i</span>
                      <span>
                        This deployment holds no imported keys yet. Import one below and it
                        will be here for the next backup set.
                      </span>
                    </div>
                  ) : (
                    managedKeys.map((k) => (
                      // The host, not the button, carries what makes the
                      // row full width: a flex host stretches its one
                      // child exactly as the column did, so the button's
                      // own style is left as it was.
                      <InfoTooltip key={k.id} id="wizard.set.key-stored" block style={{ display: "flex" }}>
                        <button
                          type="button"
                          className="btn"
                          aria-pressed={importedKeyId === k.id}
                          disabled={k.passphraseProtected || k.fingerprint === ""}
                          style={{
                            textAlign: "left",
                            height: "auto",
                            padding: "10px 14px",
                            borderColor: importedKeyId === k.id ? "var(--accent)" : undefined
                          }}
                          onClick={() => {
                            setImportedKeyId(k.id);
                            setImportedFingerprint(k.fingerprint);
                          }}
                        >
                          <span style={{ display: "block", fontWeight: 600, fontSize: "var(--text-base)" }}>
                            {(k.algorithm || "key") + (k.importedAt ? ", imported " + k.importedAt.slice(0, 10) : "")}
                          </span>
                          <span className="mono" style={{ display: "block", marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                            {k.fingerprint === "" ? "no fingerprint available" : k.fingerprint}
                          </span>
                          <span style={{ display: "block", marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                            {k.passphraseProtected
                              ? "Needs its passphrase to be resolvable before it can be used."
                              : k.usedBy.length === 0
                                ? "Not used by any backup set"
                                : "Used by " + k.usedBy.join(", ")}
                          </span>
                        </button>
                      </InfoTooltip>
                    ))
                  )}
                </div>
              ) : null}

              {keySource === "import" ? (
                <div style={{ marginTop: 18, display: "flex", flexDirection: "column", gap: 10 }}>
                  {importedFingerprint ? (
                    <div className="banner banner--ok" style={{ alignItems: "flex-start" }}>
                      <span aria-hidden="true" style={{ color: "var(--ok)" }}>✓</span>
                      <span style={{ flex: 1 }}>
                        <span style={{ display: "block", fontWeight: 600, fontSize: "var(--text-base)" }}>Key imported</span>
                        <span className="mono" style={{ display: "block", marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                          {importedFingerprint}
                        </span>
                        <span style={{ display: "block", marginTop: 5, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                          The pasted key material has already been discarded from this screen. It cannot be shown again.
                        </span>
                      </span>
                      <InfoTooltip id="wizard.set.key-replace" alignEnd>
                        <button
                          className="btn btn--sm"
                          onClick={() => {
                            setImportedFingerprint(null);
                            setImportPasted("");
                          }}
                        >
                          Replace
                        </button>
                      </InfoTooltip>
                    </div>
                  ) : (
                    <>
                      <HelpField label="Private key (OpenSSH or PEM)" help={FIELD_HELP.wizardPrivateKey}>
                        {(helpId) => (
                          <textarea
                            className="input input--mono"
                            aria-describedby={helpId}
                            rows={5}
                            value={importPasted}
                            onChange={(e) => setImportPasted(e.target.value)}
                            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                            style={{ height: "auto", padding: "10px 11px", resize: "vertical" }}
                            // M2 (#98 PR #145 review): key material must never
                            // leave this screen unhashed, and cloud/"enhanced"
                            // spellcheck (on by default in some browsers) sends
                            // the full contents of an unmasked text field to a
                            // third-party service as the user types or pastes.
                            spellCheck={false}
                            autoComplete="off"
                            autoCorrect="off"
                            autoCapitalize="off"
                          />
                        )}
                      </HelpField>
                      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                        <InfoTooltip id="wizard.set.key-import">
                          <button
                            className="btn btn--primary"
                            disabled={importPasted.trim().length === 0 || importing}
                            onClick={async () => {
                              setImporting(true);
                              setImportError(null);
                              try {
                                const result = await api.importSSHKey(importPasted);
                                setImportedFingerprint(result.algorithm + " · " + result.fingerprint);
                                setImportedKeyId(result.id);
                                setImportPasted("");
                              } catch (e) {
                                setImportError(errorMessage(e, "Could not import this key."));
                              } finally {
                                setImporting(false);
                              }
                            }}
                          >
                            {importing ? "Importing…" : "Import key"}
                          </button>
                        </InfoTooltip>
                        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                          {importPasted.trim().length === 0
                            ? "Paste a private key to enable Import."
                            : "Nothing checks this on this screen — the backend validates it against the server on import."}
                        </span>
                      </div>
                      {importError ? (
                        <div className="banner banner--danger" style={{ fontSize: "var(--text-sm)" }}>
                          <span aria-hidden="true">!</span>
                          <span>{importError}</span>
                        </div>
                      ) : null}
                      <div className="banner banner--info" style={{ fontSize: "var(--text-sm)" }}>
                        <span aria-hidden="true">i</span>
                        <span>
                          Sent once, straight to the backend. Never written to this page&rsquo;s own logs, never
                          included in a config export, never echoed back after import.
                        </span>
                      </div>
                    </>
                  )}
                </div>
              ) : null}
              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", margin: "22px 0 10px" }}>
                Host key
              </div>
              <p style={{ margin: "0 0 14px", fontSize: 13, color: "var(--text-2)", maxWidth: "78ch" }}>
                Confirm this fingerprint through a channel other than this connection before
                trusting the host. It settles which machine answers, and nothing else.
              </p>
              {hostKeyChanged ? (
                <div style={{ marginBottom: 16 }}>
                  <WarningBanner tone="danger" eyebrow="Host key changed">
                    The host key for {source.host || "this server"} has changed since it was trusted. This can
                    mean the server was rebuilt, or that something is intercepting the connection — verify the
                    new fingerprint independently before trusting it.
                  </WarningBanner>
                </div>
              ) : null}
              {probeError ? (
                <div style={{ marginBottom: 16 }}>
                  <WarningBanner tone="danger" eyebrow="Could not fetch the host key">
                    {probeError}
                  </WarningBanner>
                </div>
              ) : null}
              {/* Before the probe answers there is no key, and that is now
                  an empty list rather than a placeholder digest under a
                  guessed "ssh-ed25519": the panel says it has nothing to
                  show, which is what is true until the host has been
                  asked. After it answers, both values are the probe's own. */}
              <FingerprintDisplay
                host={source.host + ":" + source.port}
                keys={
                  probedFingerprint && probedAlgorithm
                    ? [{ algorithm: probedAlgorithm, fingerprint: probedFingerprint }]
                    : []
                }
                emptyNote="Verify the server to fetch the host key it presents."
                trustedAt={hostTrusted && !hostKeyChanged ? new Date().toISOString() : null}
              />
              <div style={{ display: "flex", gap: 10, marginTop: 16, flexWrap: "wrap", alignItems: "center" }}>
                <InfoTooltip id="wizard.set.trust-host">
                  <button
                    className={"btn " + (hostKeyChanged ? "btn--destructive-confirm" : "btn--primary")}
                    disabled={(hostTrusted && !hostKeyChanged) || probing || !probedFingerprint}
                    onClick={trustHost}
                  >
                    {hostKeyChanged ? "Trust new fingerprint" : hostTrusted ? "Host trusted" : "Trust host"}
                  </button>
                </InfoTooltip>
                <InfoTooltip id="wizard.set.refetch-fingerprint">
                  <button className="btn" disabled={probing} onClick={() => void probeHost()}>
                    {probing ? "Fetching…" : "Re-fetch fingerprint"}
                  </button>
                </InfoTooltip>
              </div>
              <div style={{ marginTop: 16 }}>
                <WarningBanner tone="danger" eyebrow="If this ever changes">
                  A changed host key stops all backup operations for the set and blocks
                  remote artifact deletion until an administrator verifies the new
                  fingerprint independently.
                </WarningBanner>
              </div>
              {/* Issue #624's report, now on a step of its own (#788).

                  It used to sit on Review, because that was the first
                  point at which everything it needs had been answered.
                  #788's rail reorders the flow so the test comes SECOND
                  and its answer is available to every step that depends
                  on it: the write probe is what decides whether step 7
                  may offer to delete from the source at all, and asking
                  after the engine, the domain and the retention chain
                  had been chosen would be asking too late to matter.

                  Six rows, never a single verdict. DNS, the connect, the
                  host key, the key material, the authentication and the
                  listing used to be one boolean, so a typo'd hostname, an
                  unauthorised key, a rotated host key and a folder that is
                  not there read identically, and those are four different
                  afternoons (#596). A skipped step is drawn as skipped and
                  never as a pass, for the reason that issue's own report
                  gives. */}
              <div style={{ border: "1px solid var(--border-strong)", borderRadius: 9, overflow: "hidden", marginTop: 18 }}>
                <div
                  style={{
                    padding: "12px 16px", background: "var(--surface-2)",
                    borderBottom: "1px solid var(--border)", display: "flex",
                    alignItems: "center", gap: 9, flexWrap: "wrap"
                  }}
                >
                  <span className="eyebrow" style={{ fontSize: "var(--text-xs)", color: "var(--text)", fontWeight: 600, flex: 1 }}>
                    Connection
                  </span>
                  <InfoTooltip id="wizard.set.test-connection" alignEnd>
                    <button
                      className="btn btn--primary btn--sm"
                      type="button"
                      disabled={testing || !importedKeyId || !trustedKnownHostsLine}
                      onClick={() => void runConnectionTest()}
                    >
                      {testing ? "Testing…" : "Test connection"}
                    </button>
                  </InfoTooltip>
                </div>
                <div style={{ padding: 16, display: "flex", flexDirection: "column", gap: 12 }}>
                  {connectionError ? (
                    <WarningBanner tone="danger" eyebrow="The test could not be run">
                      {connectionError}
                    </WarningBanner>
                  ) : null}

                  {connectionResult === null ? (
                    <p style={{ margin: 0, fontSize: 13.5, maxWidth: "78ch", color: "var(--text-2)" }}>
                      Nothing has been proven yet. Trusting the host key settles which machine
                      answers; this settles whether the key authenticates and whether this account
                      can read the folder the backups are in.
                    </p>
                  ) : (
                    <>
                      {/* One region for the whole report: the mark, the
                          step name, the outcome word and the detail are
                          one explanation, and a host per row would open
                          six pop-ups down one list (#834). */}
                      <InfoTooltip id="wizard.set.connection-checks" block>
                        <ol style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 7 }}>
                          {connectionResult.checks.map((c) => (
                            <li key={c.step} style={{ display: "flex", gap: 10, alignItems: "baseline", fontSize: 13 }}>
                              <span
                                aria-hidden="true"
                                style={{
                                  color:
                                    c.outcome === "passed"
                                      ? "var(--ok)"
                                      : c.outcome === "failed"
                                        ? "var(--danger)"
                                        : "var(--text-3)"
                                }}
                              >
                                {c.outcome === "passed" ? "✓" : c.outcome === "failed" ? "!" : "–"}
                              </span>
                              <span className="mono" style={{ minWidth: 108, fontSize: "var(--text-xs)", color: "var(--text-2)" }}>
                                {c.step}
                              </span>
                              <span style={{ flex: 1 }}>
                                {/* The outcome word is printed as well as
                                    drawn, because "skipped" and "passed"
                                    must not be told apart by colour alone. */}
                                <strong style={{ fontWeight: 600 }}>{c.outcome}</strong>
                                {c.detail ? " · " + c.detail : ""}
                              </span>
                            </li>
                          ))}
                        </ol>
                      </InfoTooltip>
                      {connectionResult.checks.length === 0 ? (
                        <p style={{ margin: 0, fontSize: 13.5, color: "var(--text-2)" }}>
                          This deployment reported a verdict and no breakdown of it.
                        </p>
                      ) : null}
                      {connectionResult.ok ? (
                        <p style={{ margin: 0, fontSize: 13.5, color: "var(--text-2)" }}>
                          {connectionProven
                            ? "This source has been proven. Saving is enabled."
                            : "This result was for different values. Test connection again for the ones on the form now."}
                        </p>
                      ) : (
                        <WarningBanner tone="danger" eyebrow="This source could not be reached">
                          {connectionResult.message ??
                            "One of the steps above failed. Saving stays disabled until a test passes."}
                        </WarningBanner>
                      )}

                      {/* Issue #852's verdict, stated where it was
                          produced rather than only where it takes effect.
                          The write probe created a scratch file under the
                          remote path and removed it again, and that
                          result — nothing softer — is what arms the
                          source-deletion control on step 7. An operator
                          who reads this line is never surprised by a
                          disabled control three steps later. */}
                      {connectionProven ? (
                        connectionResult.writable ? (
                          <WarningBanner
                            tone="ok"
                            eyebrow="Write permission"
                            tip="wizard.incremental.write-probe"
                            title="Backupd can write to, and delete from, the source"
                            dismissible={false}
                          >
                            {"A scratch file was created under " + remoteFolder + " and removed again." +
                              " Deleting the original after a verified backup is available to you on the" +
                              " Retention step."}
                          </WarningBanner>
                        ) : (
                          <WarningBanner
                            tone="warn"
                            eyebrow="Write permission"
                            tip="wizard.incremental.write-probe"
                            title="These credentials are read-only on the source"
                            dismissible={false}
                          >
                            Backups will run: reading is all a backup needs. Deleting the original
                            after a verified backup will be unavailable, because Backupd will not
                            offer to remove a file it has not proved it can remove. Read-only is a
                            perfectly good posture for a backup account, and the recommended one
                            unless you want Backupd to free space on the server for you.
                          </WarningBanner>
                        )
                      ) : null}
                    </>
                  )}
                </div>
              </div>

            </StepBody>
          ) : null}

                    {step === 3 ? (
            <StepBody
              title="Engine"
              lede="How this set stores what it collects. This is the one answer that cannot be changed later."
            >
              <div
                style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(300px, 1fr))", gap: 10 }}
              >
                <Choice
                  name="wizard-engine"
                  title={ENGINE_COPY.artifact.name}
                  wire="engine=artifact"
                  detail={ENGINE_COPY.artifact.summary}
                  checked={engine === "artifact"}
                  onChange={() => setEngine("artifact")}
                >
                  <span
                    style={{ display: "block", marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}
                  >
                    For a producer that drops finished dumps somewhere: one file in, one backup kept.
                  </span>
                </Choice>
                <Choice
                  name="wizard-engine"
                  title={ENGINE_COPY.kopia.name}
                  wire="engine=kopia"
                  detail={ENGINE_COPY.kopia.summary}
                  checked={engine === "kopia"}
                  onChange={() => setEngine("kopia")}
                >
                  <span
                    style={{ display: "block", marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}
                  >
                    For a directory tree that mostly stays the same: every run keeps a full restore
                    point, and only what changed is stored.
                  </span>
                </Choice>
              </div>

              <div style={{ marginTop: 16 }}>
                <WarningBanner
                  tone="info"
                  eyebrow="Why it is permanent"
                  tip="wizard.incremental.engine"
                  title="A set's history belongs to its engine"
                  dismissible={false}
                >
                  Snapshots and whole-file backups are different objects in different places.
                  Switching a set that has run would leave everything it has collected behind and
                  start again from nothing, so Backupd asks you to create a new set instead — and
                  the edit form for a saved set has no field for this at all.
                </WarningBanner>
              </div>
            </StepBody>
          ) : null}

          {step === 4 ? (
            <StepBody
              title="Repository domain"
              lede="The encrypted store this set's snapshots live in."
            >
              {incremental ? (
                <>
                  <div
                    style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))", gap: 10 }}
                  >
                    {(domains.data?.repositories ?? []).map((option) => (
                      <Choice
                        key={option.domain}
                        name="wizard-domain"
                        title={option.domain}
                        wire={"repository_domain=" + option.domain}
                        detail={
                          (option.mayShare
                            ? option.backupSets.length + " set(s) here, sharing content and a key"
                            : "isolated: this set only") +
                          (option.detail ? " \u00b7 " + option.detail : "")
                        }
                        checked={domainChoice === option.domain}
                        onChange={() => setDomainChoice(option.domain)}
                      />
                    ))}
                    <Choice
                      name="wizard-domain"
                      title="Define a new domain"
                      detail="A store of its own, with its own key and its own maintenance."
                      checked={domainChoice === NEW_DOMAIN}
                      onChange={() => setDomainChoice(NEW_DOMAIN)}
                    />
                  </div>

                  {/* The read is reported as a read: a deployment whose
                      repositories could not be listed must not look like
                      a deployment with none, because the second one is a
                      perfectly ordinary first-run state and the first is
                      a fault. */}
                  {domains.error ? (
                    <div style={{ marginTop: 14 }}>
                      <WarningBanner tone="warn" eyebrow="The declared domains could not be read">
                        {describeFailure(domains.error, "The repository domains could not be read.").message}
                        {" You can still name a domain below; a name nothing declares yet is created" +
                          " with this deployment's own defaults."}
                      </WarningBanner>
                    </div>
                  ) : null}

                  {domainChoice === NEW_DOMAIN ? (
                    <div style={{ marginTop: 16 }}>
                      <Field
                        label="New domain id"
                        value={newDomain}
                        onChange={setNewDomain}
                        mono
                      />
                      <p style={{ margin: "10px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)", maxWidth: "78ch" }}>
                        A domain nothing declares yet is created when this set first runs, with this
                        deployment&rsquo;s own storage location and passphrase reference. Where it
                        lives, and whether other sets may join it, are declared in the
                        deployment&rsquo;s configuration rather than here: they are properties of a
                        security boundary several sets share, not of this one set.
                      </p>
                    </div>
                  ) : (
                    <div style={{ marginTop: 16 }}>
                      <WarningBanner
                        tone="warn"
                        eyebrow="What joining a domain shares"
                        tip="wizard.incremental.domain"
                        title={"This set will share all of this with everything else in " + domainChoice}
                        dismissible={false}
                      >
                        <ul
                          style={{
                            margin: "6px 0 0",
                            paddingLeft: 18,
                            fontSize: 13,
                            display: "flex",
                            flexDirection: "column",
                            gap: 3
                          }}
                        >
                          {DOMAIN_BOUNDARIES.map((boundary) => (
                            <li key={boundary.title}>
                              <strong>{boundary.title}</strong>
                              {" \u2014 " + boundary.detail}
                            </li>
                          ))}
                        </ul>
                      </WarningBanner>
                    </div>
                  )}
                </>
              ) : (
                <NotForThisEngine what="A repository domain is where snapshots live." />
              )}
            </StepBody>
          ) : null}

          {step === 5 ? (
            <StepBody
              title="Source consistency"
              lede="What you have arranged on the server for the duration of a run. Backupd records this rather than detecting it, and reports a run that contradicts it."
            >
              {incremental ? (
                <>
                  <div
                    style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: 10 }}
                  >
                    {CONSISTENCY_ORDER.map((mode) => (
                      <Choice
                        key={mode}
                        name="wizard-consistency"
                        title={CONSISTENCY_COPY[mode].name}
                        wire={"source_consistency=" + mode}
                        detail={CONSISTENCY_COPY[mode].summary}
                        checked={consistency === mode}
                        onChange={() => setConsistency(mode)}
                      >
                        <span
                          style={{ display: "block", marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}
                        >
                          {"Point in time: " + CONSISTENCY_COPY[mode].pointInTime}
                        </span>
                      </Choice>
                    ))}
                  </div>
                  {consistency === "live_best_effort" ? null : (
                    <div style={{ marginTop: 16 }}>
                      <WarningBanner
                        tone="info"
                        eyebrow="What Backupd will do about it"
                        tip="wizard.incremental.consistency"
                        title="A change seen during a run will be reported, not ignored"
                        dismissible={false}
                      >
                        You have told Backupd that nothing writes to this source during a run. If
                        something does, the backup still completes and the run says so, because a
                        snapshot you believe is a point in time and is not is the failure worth
                        reporting.
                      </WarningBanner>
                    </div>
                  )}
                </>
              ) : (
                <NotForThisEngine what="Consistency describes a tree being walked." />
              )}
            </StepBody>
          ) : null}

          {step === 6 ? (
            <StepBody
              title={incremental ? "Verification" : "Completion and validation"}
              lede={
                incremental
                  ? "How far each backup is checked before it counts as a restore point. A run that proves less than the level you pick fails, so this is a floor and not a target."
                  : "How Backupd knows an artifact is finished being written, and how it is proven good once it arrives."
              }
            >
              {incremental ? (
                <>
                  <div
                    style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(250px, 1fr))", gap: 10 }}
                  >
                    {VERIFICATION_ORDER.map((level) => (
                      <Choice
                        key={level}
                        name="wizard-verification"
                        title={VERIFICATION_COPY[level].name}
                        wire={"verification_level=" + level}
                        detail={VERIFICATION_COPY[level].finds}
                        checked={verificationLevel === level}
                        onChange={() => setVerificationLevel(level)}
                      >
                        <span
                          style={{ display: "block", marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}
                        >
                          {VERIFICATION_COPY[level].cost}
                        </span>
                      </Choice>
                    ))}
                  </div>
                  <div
                    style={{
                      marginTop: 16,
                      display: "grid",
                      gridTemplateColumns: "repeat(auto-fit, minmax(228px, 1fr))",
                      gap: "15px 18px"
                    }}
                  >
                    <Field
                      label="Sampled share of files (%)"
                      value={samplePercent}
                      onChange={setSamplePercent}
                      mono
                    />
                    <Field
                      label="Read every file every (days)"
                      value={fullEveryDays}
                      onChange={setFullEveryDays}
                      mono
                    />
                    <Field
                      label="Restore drill every (days)"
                      value={drillEveryDays}
                      onChange={setDrillEveryDays}
                      mono
                    />
                  </div>
                  <div style={{ marginTop: 14 }}>
                    <WarningBanner
                      tone="info"
                      eyebrow="The budget, not the bar"
                      tip="wizard.incremental.verification"
                      title="The two cadences may raise the level for one run; nothing lowers it"
                      dismissible={false}
                    >
                      {"Leave a field empty to inherit this deployment's own setting. A zero" +
                        " cadence is a real answer and a different one: it means never raise the" +
                        " level on a schedule."}
                    </WarningBanner>
                  </div>
                </>
              ) : (
                <>
                  {/* The artifact engine's answer to the same question,
                      which is why this step is not simply "skipped" for
                      it: a whole-file backup is proven on arrival, and
                      the completion method and the validator are how. */}
                  <NotForThisEngine what="A verification level describes a snapshot of a tree." />
                  <div style={{ marginTop: 18 }}>
                    <ArtifactCompletionFields
                      completion={completion}
                      setCompletion={setCompletion}
                      validatorId={validatorId}
                      setValidatorId={setValidatorId}
                      validatorCatalog={validatorCatalog}
                      validatorCatalogFailed={validatorCatalogFailed}
                      selectedValidatorSummary={selectedValidator?.summary}
                    />
                  </div>
                </>
              )}
            </StepBody>
          ) : null}

          {step === 7 ? (
            <StepBody
              title="Storage, retention and holds"
              lede="Where the copy on this NAS lives, how long backups are kept, and whether Backupd may free space on the server."
            >
              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", marginBottom: 10 }}>
                Storage
              </div>
              <HelpField
                label="NAS destination" help={FIELD_HELP.wizardNasDestination}
                labelStyle={{ maxWidth: 560 }}
              >
                {(helpId) => (
                  <>
                    <span style={{ display: "flex", gap: 8 }}>
                      <input
                        className="input input--mono"
                        aria-describedby={helpId}
                        style={{ flex: 1 }}
                        value={localDestination}
                        onChange={(e) => setLocalDestination(e.target.value)}
                      />
                      {/* Do not fake a native picker the platform does not have (§22). */}
                      <button className="btn" style={{ whiteSpace: "nowrap" }}>
                        {caps.storagePicker ? "Browse volumes…" : "Validate path"}
                      </button>
                    </span>
                    <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                      {caps.storagePicker
                        ? "Uses the native " + bridge.name + " storage picker."
                        : "This platform integration has no native storage picker — enter the mounted path directly (" + bridge.deployment.storageMount + ")."}
                    </span>
                  </>
                )}
              </HelpField>

              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", margin: "22px 0 10px" }}>
                Retention
              </div>
              {/* Read, not restated. The chain a new set is retained
                  under is the deployment's own (#111 decided retention is
                  one global policy, configured on the Settings page), and
                  this step shows what that chain currently says rather
                  than offering a second place to set it — which is the
                  decorative per-set chain #299 removed from this wizard. */}
              {retention.data ? (
                <Rows>
                  <Row
                    label="Inherited chain"
                    value={retention.data.tiers
                      .map((tier) => tier.name + " " + tier.keep)
                      .join(" \u00b7 ")}
                  />
                  <Row
                    label="Newest known-good"
                    value={
                      retention.data.protectLastKnownGood
                        ? "protected: never expired by any tier"
                        : "not protected by the deployment's policy"
                    }
                  />
                  <Row label="Set from" value={"Settings \u2192 Retention, for every set that does not override it"} />
                </Rows>
              ) : (
                <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
                  {retention.error
                    ? "The deployment's retention chain could not be read. This set will still be retained under whatever it says."
                    : "Reading the deployment's retention chain\u2026"}
                </p>
              )}
              <p style={{ margin: "12px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)", maxWidth: "78ch" }}>
                A hold placed on a backup later overrides all of this: a held snapshot is never
                expired, by any tier, until the hold is released.
              </p>

              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", margin: "22px 0 10px" }}>
                Remote source handling
              </div>
              <div style={{ border: "1.5px solid var(--warn)", borderRadius: 9, overflow: "hidden", marginTop: 18 }}>
                <div
                  style={{
                    padding: "12px 16px", background: "var(--warn-quiet)",
                    borderBottom: "1px solid var(--warn)", display: "flex", alignItems: "center", gap: 9
                  }}
                >
                  <span aria-hidden="true" style={{ color: "var(--warn)" }}>▲</span>
                  <InfoTooltip id="wizard.set.remote-handling">
                    <span className="eyebrow" style={{ fontSize: "var(--text-xs)", color: "var(--text)", fontWeight: 600 }}>
                      Remote source handling
                    </span>
                  </InfoTooltip>
                </div>
                <div style={{ padding: 16, display: "flex", flexDirection: "column", gap: 14 }}>
                  {/* Issue #316: declared here, at the point this page
                      already explains what deleting the remote source
                      means, rather than as an unexplained toggle earlier
                      in the flow. Checking it changes what the rest of
                      this box says, because there is no deletion left to
                      walk through or acknowledge once it is checked. */}
                  <label
                    style={{
                      display: "flex", gap: 10, padding: "13px 14px",
                      border: "1px solid var(--border-strong)", borderRadius: "var(--radius-lg)",
                      background: "var(--surface-2)", fontSize: 13,
                      cursor: sourceNotWritable ? "not-allowed" : "pointer"
                    }}
                  >
                    {/* Issue #852: forced on and DISABLED when the write
                        probe proved this source non-writable. Disabled
                        rather than hidden, and checked rather than
                        cleared, because the state it is in is the answer:
                        this set will not delete from the source, and an
                        operator has to be able to see that is the case
                        and read why. The tooltip beside it is the why. */}
                    <input
                      type="checkbox"
                      checked={readOnlyEffective}
                      disabled={sourceNotWritable}
                      aria-describedby={sourceNotWritable ? readOnlyForcedNoteId : undefined}
                      onChange={(e) => setReadOnlySource(e.target.checked)}
                      style={{ marginTop: 2, accentColor: "var(--accent)" }}
                    />
                    <span>
                      This source is read-only — pull backups from here, but never delete
                      the remote original.
                    </span>
                  </label>

                  {sourceNotWritable ? (
                    <p
                      id={readOnlyForcedNoteId}
                      style={{ margin: 0, fontSize: 13.5, maxWidth: "78ch", display: "flex", gap: 8, alignItems: "baseline" }}
                    >
                      <InfoTooltip id="source.read-only-credentials">
                        <span style={{ fontWeight: 600 }}>These SSH credentials are read-only on the source.</span>
                      </InfoTooltip>
                      <span>
                        The connection test could not create a file under this remote path, so
                        Backupd cannot delete there. Deleting the original after backup is
                        unavailable until the account is granted write permission on the source.
                      </span>
                    </p>
                  ) : null}

                  {readOnlyEffective ? (
                    <p style={{ margin: 0, fontSize: 13.5, maxWidth: "78ch" }}>
                      Backupd will keep every backup from this source's remote
                      copy for good, however completely it passes transfer, verification
                      and commit. Releasing that storage, if it is ever wanted, is a
                      decision made outside this manager.
                    </p>
                  ) : (
                    <>
                      <p style={{ margin: 0, fontSize: 13.5, maxWidth: "78ch" }}>
                        After a backup has been successfully transferred, verified, durably
                        committed to this NAS, and recorded as safe, Backupd will
                        delete the original backup artifact from the remote server.
                      </p>
                      <ol
                        className="mono"
                        style={{
                          margin: 0, padding: 0, listStyle: "none", display: "flex",
                          flexWrap: "wrap", alignItems: "center", gap: 8,
                          fontSize: "var(--text-xs)", color: "var(--text-2)"
                        }}
                      >
                        {["Discovered", "Transferred", "Verified", "Committed", "Safe state persisted"].map((p) => (
                          <li key={p} style={{ display: "flex", gap: 8 }}>
                            <span>{p}</span>
                            <span aria-hidden="true">→</span>
                          </li>
                        ))}
                        <li style={{ color: "var(--warn)", fontWeight: 600 }}>Remote artifact deleted</li>
                      </ol>
                      <FieldHelp label="Acknowledgement" help={FIELD_HELP.wizardAcknowledge}>
                        {(helpId) => (
                          <label
                            style={{
                              display: "flex", gap: 10, padding: "13px 14px",
                              border: "1px solid var(--border-strong)", borderRadius: "var(--radius-lg)",
                              background: "var(--surface-2)", fontSize: 13, cursor: "pointer"
                            }}
                          >
                            <input
                              type="checkbox"
                              aria-describedby={helpId}
                              checked={acknowledged}
                              onChange={(e) => setAcknowledged(e.target.checked)}
                              style={{ marginTop: 2, accentColor: "var(--accent)" }}
                            />
                            <span>
                              I understand the remote backup will be removed only after the NAS
                              copy has been safely committed.
                            </span>
                          </label>
                        )}
                      </FieldHelp>
                    </>
                  )}
                </div>
              </div>

            </StepBody>
          ) : null}

          {step === 8 ? (
            <StepBody title="Review" lede="Confirm the configuration and the remote-source handling policy.">
              <div
                style={{
                  display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(224px, 1fr))",
                  gap: 1, background: "var(--border)", border: "1px solid var(--border)",
                  borderRadius: "var(--radius-lg)", overflow: "hidden"
                }}
              >
                <Summary label="Source" tip="wizard.set.review.source" lines={[source.host, remoteFolder]} />
                <Summary label="Destination" tip="wizard.set.review.destination" lines={[localDestination]} />
                {/* Issue #299: this card used to show a hardcoded
                    "7 daily" / "13 weekly" / "12 monthly" that summarized
                    fields removed above — retention is one global policy
                    now (see Settings), not something this wizard's Review
                    step reports per set. "SHA-256" below is gone for the
                    same reason as the Checksum verification toggle: this
                    product never actually sets a hash algorithm. */}
                {/* EPIC K (issue #788). The engine is stated first among
                    these because it is the one answer on this page that
                    cannot be changed after the save, and the three that
                    follow it read "not applicable" rather than blank for
                    an artifact set — an absent property said as an
                    absence, which is the rule the whole engine split
                    follows. */}
                <Summary
                  label="Engine"
                  tip="wizard.incremental.engine"
                  lines={[ENGINE_COPY[engine].name, "engine=" + ENGINE_COPY[engine].wire, "fixed after creation"]}
                />
                <Summary
                  label="Repository domain"
                  tip="wizard.incremental.domain"
                  lines={
                    incremental
                      ? [repositoryDomain || "this deployment's default", "fixed after creation"]
                      : ["not applicable"]
                  }
                />
                <Summary
                  label="Consistency"
                  tip="wizard.incremental.consistency"
                  lines={
                    incremental
                      ? [CONSISTENCY_COPY[consistency].name, CONSISTENCY_COPY[consistency].pointInTime]
                      : ["not applicable"]
                  }
                />
                <Summary
                  label="Verification"
                  tip="wizard.incremental.verification"
                  lines={
                    incremental
                      ? [
                          VERIFICATION_COPY[verificationLevel].name,
                          samplePercent.trim() === "" ? "sample inherited" : samplePercent.trim() + "% sampled",
                          fullEveryDays.trim() === "" ? "cadence inherited" : "every file every " + fullEveryDays.trim() + " days"
                        ]
                      : ["checksum on arrival"]
                  }
                />
                <Summary
                  label="Validation"
                  tip="wizard.set.review.validation"
                  lines={
                    incremental
                      ? ["transfer verify", "snapshot verification, above"]
                      : [
                          "transfer verify",
                          validatorId || "no application validator",
                          completionSummaryLabel(completion)
                        ]
                  }
                />
                <Summary
                  label="Host trust"
                  tip="wizard.set.review.host-trust"
                  lines={[hostKeyChanged ? "Host key changed — blocked" : hostTrusted ? "Trusted" : "Not yet trusted"]}
                />
                <Summary
                  label="Delete from source"
                  tip={readOnlyEffective ? "sets.source-delete.read-only" : "sets.source-delete"}
                  lines={[
                    sourceNotWritable
                      ? "unavailable — read-only source"
                      : readOnlySource
                        ? "no — this source is read-only by choice"
                        : "yes, after a verified backup"
                  ]}
                />
              </div>

              {runNotStarted ? (
                <div style={{ marginTop: 18 }}>
                  <WarningBanner
                    tone="warn"
                    eyebrow="Saved, but the run did not start"
                    actions={
                      <InfoTooltip id="wizard.set.go-to-sets">
                        <button className="btn" onClick={() => navigate("/sets")}>
                          Go to backup sets
                        </button>
                      </InfoTooltip>
                    }
                  >
                    The backup set was created and is enabled. The immediate run did not
                    start: {runNotStarted}. Nothing is backing up yet — start a run from
                    the backup sets list once that is resolved.
                  </WarningBanner>
                </div>
              ) : null}

              {repointRefusal ? (
                <div style={{ marginTop: 18 }}>
                  <WarningBanner
                    tone="warn"
                    eyebrow="This id already has backups on record"
                    actions={
                      <>
                        <InfoTooltip id="wizard.set.create-anyway">
                          <button
                            className="btn btn--primary"
                            disabled={saving}
                            onClick={() =>
                              void handleSave(repointRefusal.disabled, repointRefusal.runImmediately, true)
                            }
                          >
                            Create anyway
                          </button>
                        </InfoTooltip>
                        <InfoTooltip id="wizard.set.change-id">
                          <button className="btn" disabled={saving} onClick={() => setRepointRefusal(null)}>
                            Go back and change it
                          </button>
                        </InfoTooltip>
                      </>
                    }
                  >
                    {repointRefusal.message}
                  </WarningBanner>
                </div>
              ) : null}

              <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap", marginTop: 18 }}>
                {firstRun ? null : (
                  <InfoTooltip id="wizard.set.save-enable-run">
                    <button
                      className="btn btn--primary"
                      disabled={saveDisabled || saving || runNotStarted !== null}
                      onClick={() => void handleSave(false, true)}
                    >
                      {saving ? "Saving…" : "Save, enable & run"}
                    </button>
                  </InfoTooltip>
                )}
                <InfoTooltip id="wizard.set.save-enable">
                  <button
                    className={firstRun ? "btn btn--primary" : "btn"}
                    disabled={saveDisabled || saving || runNotStarted !== null}
                    onClick={() => void handleSave(false, false)}
                  >
                    {saving ? "Saving…" : firstRun ? "Finish setup" : "Save & enable"}
                  </button>
                </InfoTooltip>
                <InfoTooltip id="wizard.set.save-disabled">
                  <button
                    className="btn btn--quiet"
                    disabled={saving || runNotStarted !== null}
                    onClick={() => void handleSave(true, false)}
                  >
                    {saving ? "Saving…" : "Save disabled"}
                  </button>
                </InfoTooltip>
                {saveHint ? (
                  <InfoTooltip id="wizard.set.save-hint">
                    <span
                      style={{
                        fontSize: "var(--text-sm)",
                        color: hostKeyChanged || saveError ? "var(--danger)" : "var(--text-3)"
                      }}
                    >
                      {saveHint}
                    </span>
                  </InfoTooltip>
                ) : null}
              </div>
            </StepBody>
          ) : null}
        </div>

        <div className="card__footer" style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 12 }}>
          <InfoTooltip id="wizard.set.back">
            <button className="btn" onClick={() => setStep(Math.max(1, step - 1))} disabled={step === 1}>Back</button>
          </InfoTooltip>
          <InfoTooltip id="wizard.set.step-count">
            <span className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              {"Step " + step + " of 6"}
            </span>
          </InfoTooltip>
          <InfoTooltip id="wizard.set.continue" alignEnd>
            <button className="btn btn--primary" onClick={() => setStep(Math.min(6, step + 1))} disabled={step === 6}>
              Continue
            </button>
          </InfoTooltip>
        </div>
      </section>
    </div>
  );
}

function bridgeDefaultPath(mount: string) {
  return mount.replace(/\/$/, "") + "/production/postgres/";
}


/**
 * One labelled text input, controlled or (for the wizard's own still-
 * decorative fields, see fieldHelpCopy.ts) `defaultValue`. `help` is
 * optional and deliberately so: passing it is what turns this into an
 * explained field (#278); the decorative call sites below never pass it,
 * so they keep rendering exactly as before, with no pop-up and no
 * aria-describedby for a claim this file can't stand behind.
 *
 * `help`'s presence, not a separate boolean, decides which shape renders.
 * When it's set, `style` (the grid-span object `span` builds) is forwarded
 * to HelpField's own `style`, not to the `<label>` inside it: `gridColumn`
 * only affects a DIRECT grid child, and once HelpField wraps the label,
 * its own outer div is that direct child instead.
 */
function Field(
  props:
    | {
        label: string; value: string; onChange: (v: string) => void; onBlur?: () => void;
        mono?: boolean; span?: boolean; help?: FieldHelpCopy; defaultValue?: undefined;
      }
    | {
        label: string; defaultValue: string; mono?: boolean; span?: boolean; help?: FieldHelpCopy;
        value?: undefined; onChange?: undefined; onBlur?: undefined;
      }
) {
  const { label, mono, span, help } = props;
  const style = span ? { gridColumn: "1 / -1", maxWidth: 420 } : undefined;
  const className = "input" + (mono ? " input--mono" : "");
  const input = (helpId?: string) =>
    "onChange" in props && props.onChange ? (
      <input
        className={className}
        aria-describedby={helpId}
        value={props.value}
        onChange={(e) => props.onChange(e.target.value)}
        onBlur={props.onBlur}
      />
    ) : (
      <input className={className} aria-describedby={helpId} defaultValue={props.defaultValue} />
    );

  if (help) {
    return (
      <FieldHelp label={label} help={help} style={style}>
        {(helpId) => (
          <label className="field">
            <span className="field__label">{label}</span>
            {input(helpId)}
          </label>
        )}
      </FieldHelp>
    );
  }

  return (
    <label className="field" style={style}>
      <span className="field__label">{label}</span>
      {input()}
    </label>
  );
}



function Summary({ label, tip, lines }: { label: string; tip: TooltipId; lines: string[] }) {
  return (
    <div style={{ background: "var(--surface)", padding: "14px 16px" }}>
      {/* The tile's own label is wrapped rather than given an icon
          beside it: a host inside this div would become part of the
          label's text, and the label is what a reader looks the tile up
          by (#834). */}
      <InfoTooltip id={tip} block>
        <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>{label}</div>
      </InfoTooltip>
      <div className="mono" style={{ marginTop: 7, fontSize: "var(--text-sm)", lineHeight: 1.7 }}>
        {lines.map((l) => <div key={l}>{l}</div>)}
      </div>
    </div>
  );
}

/**
 * What a step that does not apply to the chosen engine says instead of
 * disappearing (issue #788).
 *
 * A step that vanishes leaves an operator counting a rail that changes
 * length under them; a step that states why it is empty teaches the
 * difference between the two engines at the moment it matters.
 */
function NotForThisEngine({ what }: { what: string }) {
  return (
    <div
      style={{
        padding: "26px 22px",
        textAlign: "center",
        border: "1px dashed var(--border-strong)",
        borderRadius: "var(--radius-xl)",
        background: "var(--surface-2)"
      }}
    >
      <div style={{ fontSize: 15, fontWeight: 600 }}>Not asked for an Artifact set</div>
      <p style={{ margin: "6px auto 0", maxWidth: "52ch", fontSize: 13, color: "var(--text-2)" }}>
        {what + " An artifact set keeps whole files instead, so there is nothing here to choose."}
      </p>
    </div>
  );
}

/**
 * The artifact engine's answer to "how is a backup proven good": how
 * Backupd knows a file is finished being written, and which registered
 * validator reads it once it arrives.
 *
 * It is a component rather than inline JSX because it is the OTHER branch
 * of one step, and keeping the two branches the same size in the step
 * body is what stops the artifact path from reading as an afterthought.
 * Both controls are the ones this wizard has always had; what changed is
 * that they are now asked only of the engine that has them.
 */
function ArtifactCompletionFields({
  completion,
  setCompletion,
  validatorId,
  setValidatorId,
  validatorCatalog,
  validatorCatalogFailed,
  selectedValidatorSummary
}: {
  completion: CompletionMethod;
  setCompletion(next: CompletionMethod): void;
  validatorId: string;
  setValidatorId(next: string): void;
  validatorCatalog: ValidatorCatalogEntry[] | null;
  validatorCatalogFailed: boolean;
  selectedValidatorSummary?: string;
}) {
  return (
    <>
      <FieldHelp label="Completion method" help={FIELD_HELP.wizardCompletionMethod}>
        {(helpId) => (
          <fieldset aria-describedby={helpId} style={{ margin: 0, padding: 0, border: "none" }}>
            <legend
              style={{
                padding: "0 0 9px",
                fontSize: "var(--text-sm)",
                fontWeight: 500,
                color: "var(--text-2)"
              }}
            >
              Completion method
            </legend>
            <div style={{ display: "flex", flexDirection: "column", gap: 9 }}>
              <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>Recommended</div>
              <Choice
                name="cm"
                title="Atomic rename"
                detail="Producer writes to a temporary name, then renames into place."
                checked={completion === "atomic-rename"}
                onChange={() => setCompletion("atomic-rename")}
              />
              <Choice
                name="cm"
                title="Completion marker / manifest"
                detail="Producer writes a sidecar manifest when the artifact is complete."
                checked={completion === "completion-marker"}
                onChange={() => setCompletion("completion-marker")}
              />
              <div className="eyebrow" style={{ fontSize: "var(--text-xs)", marginTop: 4 }}>Advanced</div>
              <Choice
                name="cm"
                title="Stable file size / timestamp"
                detail="Use only when the producer cannot signal completion."
                checked={completion === "stable-size"}
                onChange={() => setCompletion("stable-size")}
              >
                {completion === "stable-size" ? (
                  <div style={{ marginTop: 9 }}>
                    <WarningBanner tone="warn">
                      This method infers completion and provides less assurance than a
                      producer-provided completion marker.
                    </WarningBanner>
                  </div>
                ) : null}
              </Choice>
            </div>
          </fieldset>
        )}
      </FieldHelp>

      <div className="eyebrow" style={{ fontSize: "var(--text-xs)", margin: "20px 0 10px" }}>
        Validation
      </div>
      <div style={{ marginBottom: 8 }}>
        {/* Transfer verification is unconditional server-side — there is
            no field anywhere that could turn it off — so this is a
            disabled status rather than a control. It reads as one
            deliberately: an operator scanning this step for "what proves
            my backup arrived intact" should find it here. */}
        <Toggle label="Transfer verification" note="always on" checked disabled />
      </div>
      {/* Issue #162: a real picklist over the backend's own registered
          catalog (GET /api/v1/validators). The operator picks an id;
          there is deliberately no field here, or anywhere else in this
          app, for naming a command (docs/EPIC-B-multi-nas.md §26). */}
      <HelpField label="Application validation" help={FIELD_HELP.wizardValidatorId}>
        {(helpId) => (
          <>
            <select
              className="select"
              aria-describedby={helpId}
              value={validatorId}
              disabled={validatorCatalogFailed || validatorCatalog === null}
              onChange={(e) => setValidatorId(e.target.value)}
            >
              <option value="">None (transfer verification only)</option>
              {(validatorCatalog ?? []).map((v) => (
                <option key={v.id} value={v.id}>
                  {v.id}
                </option>
              ))}
            </select>
            <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              {validatorCatalogFailed
                ? "Could not load the available validators. Save without one, or retry after reloading."
                : validatorCatalog === null
                  ? "Loading the available validators…"
                  : (selectedValidatorSummary ??
                    "No application validator: transfer verification only.")}
            </span>
            <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              A validator runs against every artifact once it is transferred. Rejecting one
              quarantines it and leaves the remote copy in place.
            </span>
          </>
        )}
      </HelpField>
    </>
  );
}

/**
 * A typed number, or undefined for a field the operator left empty.
 *
 * Undefined is what makes an absent key absent, which is how the service
 * is asked to apply its own default. A zero is NOT the same request and
 * survives: on both cadences it means "never raise the level on a
 * schedule", so `Number("")` being 0 is exactly the trap these two
 * helpers exist to avoid.
 */
function optionalCount(value: string): number | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  const n = Number(trimmed);
  return Number.isFinite(n) && n >= 0 ? n : undefined;
}

/** The same, in days, sent as the seconds the contract declares. */
function optionalDays(value: string): number | undefined {
  const days = optionalCount(value);
  return days === undefined ? undefined : days * 24 * 3600;
}
