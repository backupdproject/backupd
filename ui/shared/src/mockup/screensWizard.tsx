/**
 * The guided wizard (issue #788): setting up an incremental backup set,
 * end to end, in eight steps.
 *
 * The step list is the design proposal, so the reasoning behind its shape
 * belongs here rather than in the doc alone:
 *
 *   1. Source            — where the data is. Unchanged from today's wizard.
 *   2. Connection test   — including the WRITE probe (#852), because the
 *                          answer decides what step 7 is allowed to offer.
 *   3. Engine            — the one irreversible choice, so it is asked
 *                          before anything that depends on it and after
 *                          enough context to answer it.
 *   4. Repository domain — only asked for the incremental engine, which is
 *                          why it cannot come earlier.
 *   5. Consistency       — what the operator arranged on the server.
 *   6. Verification and schedule — how hard each run is checked, how often.
 *   7. Retention and holds — how long snapshots live, and the source
 *                          deletion control #852 governs.
 *   8. Review            — every answer, then one save.
 *
 * Steps 4, 5 and 6 collapse to a single sentence when the engine chosen at
 * step 3 is Artifact: a wizard that asks an operator to choose a
 * repository domain for a set that will never write to one is a wizard
 * teaching them the wrong model of the product.
 */
import { useState } from "react";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { WarningBanner } from "@shared/components/WarningBanner";
import { bytes } from "@shared/utilities/format";
import {
  CONNECTION_CHECKS_READ_ONLY,
  CONNECTION_CHECKS_WRITABLE,
  CONSISTENCY_COPY,
  DOMAINS,
  DOMAIN_BOUNDARIES,
  ENGINE_COPY,
  RETENTION_TIERS,
  SOURCE_DELETE_COPY,
  VERIFICATION_COPY
} from "@shared/mockup/data";
import type { MockConsistencyMode, MockEngine, MockVerificationLevel } from "@shared/mockup/data";
import {
  Cell,
  CellGrid,
  CheckList,
  Choice,
  DOT,
  Field,
  FormGrid,
  Note,
  Row,
  Rows,
  Select,
  StepBody,
  StepControls,
  StepRail,
  Toggle,
  WireName
} from "@shared/mockup/parts";

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

export function WizardScreen() {
  const [step, setStep] = useState(1);
  const [engine, setEngine] = useState<MockEngine>("kopia");
  const [domain, setDomain] = useState("primary-nas");
  const [consistency, setConsistency] = useState<MockConsistencyMode>("external_snapshot");
  const [level, setLevel] = useState<MockVerificationLevel>("content_sample");
  const [writable, setWritable] = useState(true);
  const [deleteFromSource, setDeleteFromSource] = useState(false);
  const incremental = engine === "kopia";

  return (
    <div style={{ maxWidth: 980, width: "100%", margin: "0 auto", display: "flex", flexDirection: "column", gap: 16 }}>
      <PageHeader
        back={{ label: "Cancel and return to backup sets", onClick: () => undefined }}
        title="Add backup set"
        subtitle="Eight steps. Nothing is written until the last one, and every step can be revisited."
      />

      <StepRail steps={STEPS} step={step} onSelect={setStep} />

      <section className="card">
        <div style={{ padding: "20px 22px 22px" }}>
          {step === 1 ? (
            <StepBody
              title="Source"
              lede="The server holding the data. Backupd pulls; it is never given a route into your network."
            >
              <FormGrid>
                <Field label="Backup set name" value="Production file server" />
                <Field label="Server hostname" value="prod-files-01.internal" mono />
                <Field label="SSH port" value="22" mono />
                <Field label="Username" value="backup-agent" mono />
                <Field label="Directory to back up" value="/srv/shares" mono />
                <Field label="Ignore paths matching" value="*.tmp" mono />
              </FormGrid>
              <Note>
                The set is identified as <WireName name="production/file-server" /> everywhere else in
                Backupd, and the directory above is what a run walks.
              </Note>
            </StepBody>
          ) : null}

          {step === 2 ? (
            <StepBody
              title="Connection test"
              lede="What Backupd could actually do with these credentials, right now. Every line is a thing it tried, not a thing it assumes."
            >
              {/* The reviewer's switch. #852 defines two outcomes and a
                  static mock-up has to show both without a second copy of
                  the step. */}
              <div
                style={{
                  display: "flex",
                  alignItems: "center",
                  gap: 10,
                  marginBottom: 14,
                  padding: "9px 13px",
                  border: "1px dashed var(--border-strong)",
                  borderRadius: "var(--radius-lg)",
                  background: "var(--surface-2)",
                  fontSize: "var(--text-sm)",
                  color: "var(--text-2)"
                }}
              >
                <span style={{ flex: 1 }}>Mock-up control: which result this step is showing.</span>
                <button className="btn btn--sm" onClick={() => setWritable(true)} disabled={writable}>
                  Writable source
                </button>
                <button className="btn btn--sm" onClick={() => setWritable(false)} disabled={!writable}>
                  Read-only source
                </button>
              </div>

              <CheckList checks={writable ? CONNECTION_CHECKS_WRITABLE : CONNECTION_CHECKS_READ_ONLY} />

              <div style={{ marginTop: 16 }}>
                {writable ? (
                  <WarningBanner
                    tone="ok"
                    eyebrow="Write permission"
                    tip="wizard.incremental.write-probe"
                    title="Backupd can write to, and delete from, the source"
                    dismissible={false}
                  >
                    A scratch file was created under /srv/shares and removed again. Deleting from the
                    source after a verified backup is available to you on the Retention step.
                  </WarningBanner>
                ) : (
                  <WarningBanner
                    tone="warn"
                    eyebrow="Write permission"
                    tip="wizard.incremental.write-probe"
                    title="These credentials are read-only on the source"
                    dismissible={false}
                    actions={<button className="btn btn--sm">Test again</button>}
                  >
                    Backups will run: reading is all a backup needs. Deleting from the source after a
                    verified backup will be unavailable, because Backupd will not offer to remove a
                    file it has not proved it can remove.
                  </WarningBanner>
                )}
              </div>

              <Note>
                Read-only is a perfectly good posture for a backup account, and the recommended one
                unless you want Backupd to free space on the server for you. The write line is the{" "}
                <WireName name="write_probe" /> step, and its answer is{" "}
                <WireName name="writable" /> on the connection-test result: absent is read as false,
                because the control it arms deletes files on somebody&rsquo;s server.
              </Note>
            </StepBody>
          ) : null}

          {step === 3 ? (
            <StepBody
              title="Engine"
              lede="How this set stores what it collects. This is the one answer that cannot be changed later."
            >
              <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(300px, 1fr))", gap: 10 }}>
                <Choice
                  name="wizard-engine"
                  title={ENGINE_COPY.artifact.name}
                  wire="engine=artifact"
                  detail={ENGINE_COPY.artifact.summary}
                  checked={engine === "artifact"}
                  onChange={() => setEngine("artifact")}
                >
                  <span style={{ display: "block", marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
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
                  <span style={{ display: "block", marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                    For a directory tree that mostly stays the same: every run keeps a full restore
                    point, and only what changed is stored.
                  </span>
                </Choice>
              </div>

              <WarningBanner
                tone="info"
                eyebrow="Why it is permanent"
                title="A set's history belongs to its engine"
                dismissible={false}
              >
                Snapshots and whole-file backups are different objects in different places. Switching
                a set that has run would leave everything it has collected behind and start again
                from nothing, so Backupd asks you to create a new set instead.
              </WarningBanner>
            </StepBody>
          ) : null}

          {step === 4 ? (
            <StepBody
              title="Repository domain"
              lede="The encrypted store this set's snapshots live in."
            >
              {incremental ? (
                <>
                  <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))", gap: 10 }}>
                    {DOMAINS.map((option) => (
                      <Choice
                        key={option.id}
                        name="wizard-domain"
                        title={option.id}
                        wire={"repository_domain=" + option.id}
                        detail={
                          option.description +
                          DOT +
                          (option.isolation === "isolated"
                            ? "isolated, holds one set"
                            : option.sets.length + " sets, " + bytes(option.physicalBytes) + " stored")
                        }
                        checked={domain === option.id}
                        onChange={() => setDomain(option.id)}
                      />
                    ))}
                    <Choice
                      name="wizard-domain"
                      title="Define a new domain"
                      detail="A store of its own, with its own key and its own maintenance."
                      checked={domain === "new"}
                      onChange={() => setDomain("new")}
                    />
                  </div>

                  {domain === "new" ? (
                    <div style={{ marginTop: 16 }}>
                      <FormGrid>
                        <Field label="Domain id" value="file-server-only" mono />
                        <Field label="Storage location" value="/data/backups/repositories/file-server-only" mono />
                        <Select
                          label="Sharing"
                          value="isolated"
                          options={[
                            { value: "shared", label: "Shared — other sets may join" },
                            { value: "isolated", label: "Isolated — this set only" }
                          ]}
                          onChange={() => undefined}
                        />
                      </FormGrid>
                    </div>
                  ) : (
                    <WarningBanner
                      tone="warn"
                      eyebrow="What joining a domain shares"
                      title={"This set will share all of this with everything else in " + domain}
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
                            {" — " + boundary.detail}
                          </li>
                        ))}
                      </ul>
                    </WarningBanner>
                  )}
                </>
              ) : (
                <SkippedStep engine={engine} what="A repository domain is where snapshots live." />
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
                  <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: 10 }}>
                    {Object.entries(CONSISTENCY_COPY).map(([mode, copy]) => (
                      <Choice
                        key={mode}
                        name="wizard-consistency"
                        title={copy.name}
                        wire={"source_consistency=" + copy.wire}
                        detail={copy.summary}
                        checked={consistency === (mode as MockConsistencyMode)}
                        onChange={() => setConsistency(mode as MockConsistencyMode)}
                      >
                        <span
                          style={{ display: "block", marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}
                        >
                          {"Point in time: " + copy.pointInTime}
                        </span>
                      </Choice>
                    ))}
                  </div>
                  {consistency === "live_best_effort" ? null : (
                    <WarningBanner
                      tone="info"
                      eyebrow="What Backupd will do about it"
                      title="A change seen during a run will be reported, not ignored"
                      dismissible={false}
                    >
                      You have told Backupd that nothing writes to this source during a run. If
                      something does, the backup still completes and the run says so, because a
                      snapshot you believe is a point in time and is not is the failure worth
                      reporting.
                    </WarningBanner>
                  )}
                </>
              ) : (
                <SkippedStep engine={engine} what="Consistency describes a tree being walked." />
              )}
            </StepBody>
          ) : null}

          {step === 6 ? (
            <StepBody
              title="Verification and schedule"
              lede="How far each backup is checked before it counts as a restore point, and how often the set runs."
            >
              {incremental ? (
                <>
                  <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(250px, 1fr))", gap: 10 }}>
                    {Object.entries(VERIFICATION_COPY).map(([value, copy]) => (
                      <Choice
                        key={value}
                        name="wizard-verification"
                        title={copy.name}
                        wire={"verification_level=" + copy.wire}
                        detail={copy.finds}
                        checked={level === (value as MockVerificationLevel)}
                        onChange={() => setLevel(value as MockVerificationLevel)}
                      >
                        <span
                          style={{ display: "block", marginTop: 6, fontSize: "var(--text-xs)", color: "var(--text-3)" }}
                        >
                          {copy.cost}
                        </span>
                      </Choice>
                    ))}
                  </div>
                  <div style={{ marginTop: 16 }}>
                    <FormGrid>
                      <Field label="Sampled share of files" value="5%" wire="verification_sample_percent" />
                      <Field label="Read every file every" value="7 days" wire="verification_full_every_seconds" />
                      <Field
                        label="Restore drill every"
                        value="30 days"
                        wire="verification_restore_drill_every_seconds"
                      />
                      <Select
                        label="Check the source every"
                        wire="poll_interval"
                        value="6h"
                        options={[
                          { value: "30m", label: "30 minutes" },
                          { value: "6h", label: "6 hours" },
                          { value: "12h", label: "12 hours" },
                          { value: "24h", label: "24 hours" }
                        ]}
                        onChange={() => undefined}
                      />
                    </FormGrid>
                  </div>
                  <Note>
                    A run that proves less than the level you pick fails, so the level is a floor and
                    not a target. The two cadences may raise it for one run; nothing lowers it.
                  </Note>
                </>
              ) : (
                <SkippedStep engine={engine} what="Artifact backups are validated on arrival instead." />
              )}
            </StepBody>
          ) : null}

          {step === 7 ? (
            <StepBody
              title="Retention and holds"
              lede="How long backups are kept, what protects one from expiry, and whether Backupd may free space on the server."
            >
              <div className="table-scroll">
                <table className="table">
                  <thead>
                    <tr>
                      <th>Tier</th>
                      <th>Keeps</th>
                      <th>Inherited from</th>
                    </tr>
                  </thead>
                  <tbody>
                    {RETENTION_TIERS.map((tier) => (
                      <tr key={tier.name}>
                        <td>{tier.name}</td>
                        <td className="mono">{tier.keep}</td>
                        <td style={{ color: "var(--text-2)" }}>Deployment defaults</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>

              <div style={{ marginTop: 14, display: "flex", flexDirection: "column", gap: 10 }}>
                <Toggle
                  label="Never expire the newest verified backup"
                  note="recommended"
                  checked
                  onChange={() => undefined}
                />
                <Toggle
                  label="Use a retention chain of this set's own"
                  note="inherits the deployment chain above"
                  checked={false}
                  onChange={() => undefined}
                />
                {/* #852. The control is the same one the per-set
                    configuration page draws, in the same two states, and
                    its refusal names the connection test rather than
                    leaving an operator to guess. */}
                <Toggle
                  label={SOURCE_DELETE_COPY.label}
                  note={writable ? SOURCE_DELETE_COPY.note : SOURCE_DELETE_COPY.readOnlyNote}
                  checked={writable ? deleteFromSource : false}
                  onChange={writable ? () => setDeleteFromSource(!deleteFromSource) : undefined}
                  disabled={!writable}
                  tip={writable ? "sets.source-delete" : "sets.source-delete.read-only"}
                />
              </div>

              {writable ? null : (
                <WarningBanner
                  tone="info"
                  eyebrow="Why that control is off"
                  title="The connection test could not write to the source"
                  dismissible={false}
                  actions={
                    <button className="btn btn--sm" onClick={() => setStep(2)}>
                      Back to the connection test
                    </button>
                  }
                >
                  Grant the backup account write permission on /srv/shares and test again, or leave
                  it as it is: a read-only account backs up exactly as well and cannot delete
                  anything by mistake.
                </WarningBanner>
              )}

              <Note>
                A hold placed on a backup later overrides all of this: a held backup is never expired,
                by any tier, until the hold is released.
              </Note>
            </StepBody>
          ) : null}

          {step === 8 ? (
            <StepBody
              title="Review"
              lede="Everything this set will be. Saving writes the configuration; the first run happens on the schedule, or now if you ask for it."
            >
              <CellGrid min={230}>
                <Cell label="Backup set" value="production/file-server" mono tone="quiet" />
                <Cell label="Engine" value={ENGINE_COPY[engine].name} wire={"engine=" + ENGINE_COPY[engine].wire} tone="quiet" />
                <Cell label="Source" value="prod-files-01.internal:/srv/shares" mono tone="quiet" />
                <Cell
                  label="Repository domain"
                  value={incremental ? domain : "not applicable"}
                  wire="repository_domain"
                  mono={incremental}
                  tone="quiet"
                />
                <Cell
                  label="Consistency"
                  value={incremental ? CONSISTENCY_COPY[consistency].name : "not applicable"}
                  wire="source_consistency"
                  tone="quiet"
                />
                <Cell
                  label="Verification"
                  value={incremental ? VERIFICATION_COPY[level].name : "checksum on arrival"}
                  wire="verification_level"
                  tone="quiet"
                />
                <Cell label="Schedule" value="every 6 hours" wire="poll_interval" tone="quiet" />
                <Cell
                  label="Delete from source"
                  value={writable ? (deleteFromSource ? "yes, after verification" : "no") : "unavailable (read-only source)"}
                  tone="quiet"
                />
              </CellGrid>

              <div style={{ marginTop: 16 }}>
                <Rows>
                  <Row
                    label="Retention"
                    value={RETENTION_TIERS.map((tier) => tier.name + " " + tier.keep).join(DOT) + DOT + "newest verified backup protected"}
                  />
                  <Row
                    label="First run"
                    value={
                      <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
                        <StatusBadge tone="neutral" icon="status-idle">
                          Reads every byte once
                        </StatusBadge>
                        <span style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                          {"about " + bytes(1_412 * 1024 ** 3) + " to read, then only changes"}
                        </span>
                      </span>
                    }
                  />
                </Rows>
              </div>

              <div style={{ marginTop: 16 }}>
                <WarningBanner
                  tone="info"
                  eyebrow="What happens on save"
                  title="One durable operation, which you can watch and cancel"
                  dismissible={false}
                >
                  Saving submits the change and, if you ask for an immediate run, one backup
                  operation. Both appear on the Activity page with their own id, and survive a
                  restart of the service.
                </WarningBanner>
              </div>
            </StepBody>
          ) : null}
        </div>

        <StepControls
          step={step}
          total={STEPS.length}
          onBack={() => setStep(Math.max(1, step - 1))}
          onNext={() => setStep(Math.min(STEPS.length, step + 1))}
          finishLabel="Save and run now"
        />
      </section>
    </div>
  );
}

/** What a step that does not apply to the chosen engine says instead of
 *  disappearing. Steps that vanish leave an operator counting a rail that
 *  changes length under them; a step that states why it is empty teaches
 *  the difference between the two engines at the moment it matters. */
function SkippedStep({ engine, what }: { engine: MockEngine; what: string }) {
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
      <div style={{ fontSize: 15, fontWeight: 600 }}>
        {"Not asked for an " + ENGINE_COPY[engine].name + " set"}
      </div>
      <p style={{ margin: "6px auto 0", maxWidth: "52ch", fontSize: 13, color: "var(--text-2)" }}>
        {what + " An artifact set keeps whole files instead, so there is nothing here to choose."}
      </p>
    </div>
  );
}
