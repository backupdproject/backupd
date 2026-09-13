/**
 * Declaring a repository domain (EPIC K #788, made writable by #862).
 *
 * # The screen exists because the decision does
 *
 * Creating a domain is where co-tenancy is settled, and co-tenancy is the
 * one choice in this product whose consequences land on OTHER backup sets:
 * one key, one credential, one maintenance owner, one deduplication pool,
 * one blast radius, one disk. The wizard's domain step can offer to NAME a
 * new domain, but it cannot ask any of that, because a set's wizard is the
 * wrong place to decide what a boundary several sets sit inside is for.
 *
 * # What Create actually creates
 *
 * A DECLARATION. POST /repositories persists the domain into this
 * deployment's configuration and writes no store: the repository itself is
 * created by the first backup run that puts a snapshot in the domain,
 * which is the same lifecycle a domain named on the add-backup-set wizard
 * already has. So the create itself opens no storage and resolves no
 * passphrase reference -- what comes back is the declaration, reported as
 * a domain whose store has not been written yet -- and the fleet list
 * this screen navigates to shows the new domain answering but holding no
 * repository until something has run into it. That is the truth about it
 * rather than a failed create, which is why the success path says so on
 * the way out.
 *
 * # Why the passphrase box takes a reference and not a passphrase
 *
 * Because the field that took a passphrase would be the most dangerous
 * input in this product: the request body carrying it reaches an access
 * log, and what it protects is every snapshot in the domain. The contract
 * has no field for one. This screen collects a PATH or a variable NAME,
 * exactly as the configuration file does, and says which it is sending.
 *
 * The third spelling the contract accepts -- a command whose stdout is the
 * passphrase -- is deliberately not a box here. It is an argv array, and a
 * single input would have to guess where a shell would split it; the CLI
 * takes it a word at a time instead, and the note below names that verb
 * rather than pretending this screen can do everything the API can.
 *
 * # And why the engine gate is explained rather than pre-checked
 *
 * A deployment that does not run the incremental engine refuses this write
 * with 409 INCREMENTAL_ENGINE_DISABLED, and the refusal's own sentence
 * names the config key and the environment variable that answer it. The
 * screen renders that sentence rather than deriving a second opinion about
 * the gate from a read: two places deciding whether the engine is on is
 * how they come to disagree.
 *
 * # Where the hover help comes from (issue #873)
 *
 * Every control here, and every element that STATES something, names a
 * `repositories.new.*` id in ui/shared/src/tooltips/tooltips.json and
 * carries no copy of its own. The screen already argues its decisions in
 * prose beside the fields, and the tooltips deliberately do not repeat
 * that prose: each one says what the control SENDS and what the
 * deployment does with it — the wire field, the refusal code, the thing
 * that is written and the thing that is not. That is the half an operator
 * reading a contract or a CLI transcript is looking for, and keeping it
 * in the registry is what lets it be reviewed for one voice with the
 * other few hundred sentences in this interface (#834).
 */
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { BackupdError } from "@shared/api/contracts";
import { PageHeader } from "@shared/components/PageHeader";
import { WarningBanner } from "@shared/components/WarningBanner";
import { Choice } from "@shared/components/Choice";
import { Note, WireField } from "@shared/components/Definitions";
import { DOMAIN_BOUNDARIES } from "@shared/components/EngineBadge";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";
import type { TooltipId } from "@shared/tooltips/tooltips";

type Isolation = "shared" | "isolated";
type Ownership = "this" | "another-instance";
type PassphraseSource = "file" | "env";

/** What went wrong, kept apart by what an operator does about it. A
 *  refusal the deployment makes about itself (the engine is off) is not
 *  the same as one it makes about this form (the id is taken), and a
 *  screen that rendered both under the Create button would tell somebody
 *  to edit a field that is already right. */
type Refusal = { kind: "engine" | "form"; message: string };

export function RepositoryDomainNewPage() {
  const navigate = useNavigate();
  const api = useApi();
  const [isolation, setIsolation] = useState<Isolation>("shared");
  const [ownership, setOwnership] = useState<Ownership>("this");
  const [domain, setDomain] = useState("");
  const [description, setDescription] = useState("");
  const [location, setLocation] = useState("");
  const [passphraseSource, setPassphraseSource] = useState<PassphraseSource>("file");
  const [passphraseRef, setPassphraseRef] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [saving, setSaving] = useState(false);

  const ready = domain.trim() !== "" && passphraseRef.trim() !== "";

  async function create() {
    setRefusal(null);
    setSaving(true);
    try {
      await api.createRepositoryDomain({
        domain: domain.trim(),
        description: description.trim(),
        isolation,
        passphrase:
          passphraseSource === "file"
            ? { file: passphraseRef.trim() }
            : { env: passphraseRef.trim() },
        location: location.trim(),
        maintenanceOwner: ownership
      });
      // The fleet list re-reads on mount, so the domain is there when it
      // draws — reported as answering and not yet readable, because
      // nothing has written its store.
      navigate("/repositories");
    } catch (e) {
      const message =
        e instanceof BackupdError ? e.api.message : "This repository domain could not be declared.";
      setRefusal({
        kind:
          e instanceof BackupdError && e.api.code === "INCREMENTAL_ENGINE_DISABLED"
            ? "engine"
            : "form",
        message
      });
    } finally {
      setSaving(false);
    }
  }

  return (
    <>
      <PageHeader
        back={{ label: "Repository domains", onClick: () => navigate("/repositories") }}
        title="Define a repository domain"
        tip="repositories.define"
        subtitle="A new encrypted store that backup sets can be pointed at."
      />

      {refusal?.kind === "engine" ? (
        <WarningBanner
          tone="warn"
          eyebrow="This deployment does not run the incremental engine"
          title="Nothing here can be declared until the incremental engine is enabled"
          tip="repositories.new.engine-gate"
          dismissible={false}
        >
          {refusal.message} Repository domains only exist for that engine, so this deployment has
          nowhere to put one; the artifact backup sets it already runs are unaffected. Nothing was
          written, and this form keeps what you typed.
        </WarningBanner>
      ) : null}

      <WarningBanner
        tone="info"
        eyebrow="Declaring is not creating"
        title="Create writes the declaration; the store is written by the first backup run into it"
        tip="repositories.new.declaration"
        dismissible={false}
      >
        This saves the domain into this deployment&rsquo;s configuration —{" "}
        <WireField name="POST /repositories" /> — with its id, its sharing rule and a REFERENCE to
        the passphrase that will open it. No repository is created now and nothing here opens
        storage or reads your passphrase, so the domain appears on the fleet list as not yet
        readable — it holds no repository — until a backup set stores its first snapshot in it.
        That is the same lifecycle a domain named on the Repository domain step of Add backup set
        already has.
      </WarningBanner>

      <section className="card">
        <div className="card__header">
          <InfoTooltip id="repositories.new.identity">
            <h2 className="eyebrow">Identity</h2>
          </InfoTooltip>
        </div>
        <div className="card__body">
          <div
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fit, minmax(240px, 1fr))",
              gap: "15px 18px"
            }}
          >
            <Field
              label="Domain id"
              wire="id"
              tip="repositories.new.id"
              placeholder="offsite-b2"
              mono
              value={domain}
              onChange={setDomain}
            />
            <Field
              label="Description"
              wire="description"
              tip="repositories.new.description"
              placeholder="Second copy, off site"
              value={description}
              onChange={setDescription}
            />
            <Field
              label="Storage location"
              wire="location"
              tip="repositories.new.location"
              placeholder="this deployment's own storage location"
              mono
              value={location}
              onChange={setLocation}
            />
            <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
              <label className="field">
                <span className="field__label">
                  {passphraseSource === "file"
                    ? "Passphrase file on this NAS"
                    : "Passphrase environment variable"}
                </span>
                {/* The host wraps the input rather than sitting beside the
                    label, for the reason InfoTooltip's own doc gives: a
                    host inside a <label> would be read into the field's
                    accessible name. Wrapping puts the copy on the control
                    as a description and leaves the name alone — and a
                    one-cell grid that stretches hands the field's width
                    through to the input, which `.tooltip`'s own
                    `align-items: center` would otherwise shrink to a
                    default-width box. */}
                <InfoTooltip
                  id="repositories.new.passphrase-ref"
                  style={{ display: "grid", alignItems: "stretch" }}
                >
                  <input
                    className="input input--mono"
                    type="text"
                    value={passphraseRef}
                    placeholder={
                      passphraseSource === "file"
                        ? "/etc/backupd/offsite-b2.passphrase"
                        : "BACKUPD_OFFSITE_B2_PASSPHRASE"
                    }
                    onChange={(e) => setPassphraseRef(e.target.value)}
                  />
                </InfoTooltip>
              </label>
              <WireField name={"passphrase." + passphraseSource} />
              <div style={{ display: "flex", gap: 8 }}>
                <InfoTooltip id="repositories.new.passphrase-file">
                  <button
                    className={"btn" + (passphraseSource === "file" ? " btn--primary" : "")}
                    onClick={() => setPassphraseSource("file")}
                  >
                    A file
                  </button>
                </InfoTooltip>
                <InfoTooltip id="repositories.new.passphrase-env">
                  <button
                    className={"btn" + (passphraseSource === "env" ? " btn--primary" : "")}
                    onClick={() => setPassphraseSource("env")}
                  >
                    An environment variable
                  </button>
                </InfoTooltip>
              </div>
            </div>
          </div>
          <Note>
            The passphrase itself is never typed here and never travels over this API: what is
            saved is where to read it from. Losing it loses every snapshot in the domain, because
            nothing else can open the store. A passphrase produced by a COMMAND is declared from a
            terminal instead — <WireField name="backupd repository create --passphrase-command" /> —
            because it is a program and its arguments, and one box could only guess where they
            split.
          </Note>
          <Note>
            Leave the storage location empty and the domain is stored under this deployment&rsquo;s
            own storage location, which is the only place this build can put one. A location naming
            anywhere else is refused rather than quietly ignored.
          </Note>
        </div>
      </section>

      <section className="card">
        <div className="card__header">
          <InfoTooltip id="repositories.may-share">
            <h2 className="eyebrow">Sharing</h2>
          </InfoTooltip>
        </div>
        <div className="card__body">
          <div
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))",
              gap: 10
            }}
          >
            <Choice
              name="domain-isolation"
              title="Shared"
              wire="may_share=true"
              tip="repositories.new.shared"
              detail="Several backup sets may store snapshots here and deduplicate against each other."
              checked={isolation === "shared"}
              onChange={() => setIsolation("shared")}
            />
            <Choice
              name="domain-isolation"
              title="Isolated"
              wire="may_share=false"
              tip="repositories.new.isolated"
              detail="Exactly one backup set. A second set pointed here is refused rather than quietly admitted."
              checked={isolation === "isolated"}
              onChange={() => setIsolation("isolated")}
            />
          </div>

          <div style={{ marginTop: 14 }}>
            <WarningBanner
              tone={isolation === "shared" ? "warn" : "info"}
              eyebrow="What sharing means"
              title={
                isolation === "shared"
                  ? "Sets in this domain share all six of these"
                  : "An isolated domain shares none of these with any other set"
              }
              tip="repositories.new.boundaries"
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
        </div>
      </section>

      <section className="card">
        <div className="card__header">
          <InfoTooltip id="repositories.maintenance-transfer">
            <h2 className="eyebrow">Maintenance ownership</h2>
          </InfoTooltip>
        </div>
        <div className="card__body">
          <div
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fit, minmax(280px, 1fr))",
              gap: 10
            }}
          >
            <Choice
              name="domain-ownership"
              title="This instance maintains it"
              tip="repositories.new.maintain-here"
              detail="This deployment compacts the store and reclaims its space, if nothing else already maintains it: declaring is refused when a maintenance record names somebody else."
              checked={ownership === "this"}
              onChange={() => setOwnership("this")}
            />
            <Choice
              name="domain-ownership"
              title="Another instance maintains it"
              wire="maintenance_owner=another-instance"
              tip="repositories.new.maintain-elsewhere"
              detail="This deployment reads and writes snapshots here. It is the answer that lets the declaration through when the store is already maintained elsewhere; nothing about maintenance is recorded by it."
              checked={ownership === "another-instance"}
              onChange={() => setOwnership("another-instance")}
            />
          </div>
          <div style={{ marginTop: 14 }}>
            <Note>
              Neither answer claims anything. Maintenance ownership is a durable record taken by
              whichever instance first maintains an unclaimed repository, and after that it moves
              only by transfer (ADR 0017) — so declaring a domain cannot make this deployment its
              maintainer. What the answer decides is whether this declaration is ALLOWED to be a
              claim: with &ldquo;this instance&rdquo; chosen, a domain whose maintenance record
              already names somebody else is refused, and the refusal says who holds it.
            </Note>
          </div>
          <Note>
            {ownership === "this"
              ? "Exactly one instance may maintain a domain, and ownership moves by transfer, never by claim: an instance that simply decided it was the owner is how two of them compact one store at once. Choosing this answer records nothing — it only decides that a domain somebody else already maintains is refused here rather than quietly re-declared."
              : "This deployment will still read and write snapshots here. Nothing on this screen can make it the owner, or stop it becoming one: ownership is transferred by the instance that holds it, and this answer is not written into the configuration at all."}
          </Note>
        </div>
      </section>

      {refusal?.kind === "form" ? (
        <WarningBanner
          tone="danger"
          eyebrow="Not declared"
          title="This deployment refused the declaration"
          tip="repositories.new.refused"
          dismissible={false}
        >
          {refusal.message} Nothing was written, and this form keeps what you typed.
        </WarningBanner>
      ) : null}

      <div style={{ display: "flex", gap: 10, justifyContent: "flex-end" }}>
        <InfoTooltip id="repositories.new.back">
          <button className="btn" onClick={() => navigate("/repositories")}>
            Back to repository domains
          </button>
        </InfoTooltip>
        <InfoTooltip id="repositories.new.create" alignEnd>
          <button
            className="btn btn--primary"
            disabled={!ready || saving}
            title={
              ready
                ? "Declare this repository domain"
                : "A domain needs an id and somewhere to read its passphrase from"
            }
            onClick={() => void create()}
          >
            {saving ? "Declaring\u2026" : "Create domain"}
          </button>
        </InfoTooltip>
      </div>
    </>
  );
}

/** One field of the declaration, carrying the wire field it maps to,
 *  because the operator most likely to be on this screen is the one who
 *  would otherwise be writing this into config.yaml by hand.
 *
 *  `tip` is what the field MEANS, from the registry (#834), and the host
 *  wraps the input rather than standing beside the label: an icon host
 *  inside a <label> is read into the field's accessible name, and every
 *  test and every screen reader finds this input by that name. Wrapping
 *  lands the copy on the control as a description and leaves the name
 *  exactly as it was. The host is a one-cell stretching grid so the input
 *  keeps the field's full width: `.tooltip` centres its content, which on
 *  a text input means a default-width box in the middle of the column. */
function Field({
  label,
  wire,
  tip,
  placeholder,
  mono,
  value,
  onChange
}: {
  label: string;
  wire: string;
  tip: TooltipId;
  placeholder: string;
  mono?: boolean;
  value: string;
  onChange(next: string): void;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
      <label className="field">
        <span className="field__label">{label}</span>
        <InfoTooltip id={tip} style={{ display: "grid", alignItems: "stretch" }}>
          <input
            className={"input" + (mono ? " input--mono" : "")}
            type="text"
            value={value}
            placeholder={placeholder}
            onChange={(e) => onChange(e.target.value)}
          />
        </InfoTooltip>
      </label>
      <WireField name={wire} />
    </div>
  );
}
