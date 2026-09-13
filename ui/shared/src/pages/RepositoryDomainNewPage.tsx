/**
 * What defining a repository domain asks, and what this deployment can
 * actually do about it today (EPIC K, issue #788).
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
 * # Why nothing here writes
 *
 * The /api/v1 contract this build speaks declares exactly two repository
 * routes, both GET: the fleet and one domain's maintenance record. There is
 * no POST /repositories, so there is no honest way to make a Create button
 * work, and a form that collected a passphrase and then could not store it
 * would be worse than one that does not collect it.
 *
 * So the identity fields are DISABLED rather than typable — an input that
 * accepts a passphrase it cannot save is a lie told in the most dangerous
 * field on the screen — while the two questions that are explanatory
 * remain live: choosing Shared or Isolated, and choosing who maintains it,
 * changes what the screen SAYS those answers mean. That is the part an
 * operator needs before they name a domain in the wizard or write one into
 * configuration, and it is true whichever way the write eventually lands.
 */
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { PageHeader } from "@shared/components/PageHeader";
import { WarningBanner } from "@shared/components/WarningBanner";
import { Choice } from "@shared/components/Choice";
import { Note, WireField } from "@shared/components/Definitions";
import { DOMAIN_BOUNDARIES } from "@shared/components/EngineBadge";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";

type Isolation = "shared" | "isolated";
type Ownership = "this" | "other";

export function RepositoryDomainNewPage() {
  const navigate = useNavigate();
  const [isolation, setIsolation] = useState<Isolation>("shared");
  const [ownership, setOwnership] = useState<Ownership>("this");

  return (
    <>
      <PageHeader
        back={{ label: "Repository domains", onClick: () => navigate("/repositories") }}
        title="Define a repository domain"
        tip="repositories.define"
        subtitle="A new encrypted store that backup sets can be pointed at."
      />

      <WarningBanner
        tone="warn"
        eyebrow="Not writable from here yet"
        title="This build's API has no route that creates a repository domain"
        dismissible={false}
      >
        The contract declares <WireField name="GET /repositories" /> and{" "}
        <WireField name="GET /repositories/{domain}/maintenance" /> and nothing that writes, so
        every control below is disabled. A domain is created today by naming it on the Repository
        domain step of Add backup set — a name nothing declares yet is created when that set first
        runs, with this deployment&rsquo;s own storage location and passphrase reference — and its
        location, key and sharing rule are declared in configuration. What this screen is for until
        that route exists is the decision itself: choose Shared or Isolated below and it says what
        each answer commits every set in the domain to.
      </WarningBanner>

      <section className="card">
        <div className="card__header">
          <h2 className="eyebrow">Identity</h2>
        </div>
        <div className="card__body">
          <div
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fit, minmax(240px, 1fr))",
              gap: "15px 18px"
            }}
          >
            <DisabledField label="Domain id" wire="domain" placeholder="offsite-b2" mono />
            <DisabledField label="Description" placeholder="Second copy, off site" />
            <DisabledField
              label="Storage location"
              wire="storage location"
              placeholder="b2://acme-backups/primary"
              mono
            />
            <DisabledField
              label="Encryption passphrase"
              placeholder="set in configuration, never displayed"
              type="password"
            />
          </div>
          <Note>
            A domain&rsquo;s passphrase is stored on this NAS and never displayed again. Losing it
            loses every snapshot in the domain: nothing else can open the store. That is why this
            field is not a box you can fill in on a screen that cannot save it.
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
              detail="Several backup sets may store snapshots here and deduplicate against each other."
              checked={isolation === "shared"}
              onChange={() => setIsolation("shared")}
            />
            <Choice
              name="domain-isolation"
              title="Isolated"
              wire="may_share=false"
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
              detail="This deployment compacts the store and reclaims its space on the schedule below."
              checked={ownership === "this"}
              onChange={() => setOwnership("this")}
            />
            <Choice
              name="domain-ownership"
              title="Another instance maintains it"
              detail="This deployment reads and writes snapshots here but never maintains the store."
              checked={ownership === "other"}
              onChange={() => setOwnership("other")}
            />
          </div>
          <div
            style={{
              marginTop: 14,
              display: "grid",
              gridTemplateColumns: "repeat(auto-fit, minmax(240px, 1fr))",
              gap: "15px 18px"
            }}
          >
            <DisabledField
              label="Full maintenance window"
              wire="maintenance window"
              placeholder={ownership === "this" ? "weekly, Sunday 02:00" : "owned elsewhere"}
            />
            <DisabledField
              label="Quick maintenance"
              placeholder={ownership === "this" ? "after every backup pass" : "owned elsewhere"}
            />
          </div>
          <Note>
            {ownership === "this"
              ? "Exactly one instance may maintain a domain, and ownership moves by transfer, never by claim: an instance that simply decided it was the owner is how two of them compact one store at once."
              : "This deployment will still read and write snapshots here. Nothing on this screen can make it the owner: ownership is transferred by the instance that holds it, never taken."}
          </Note>
        </div>
      </section>

      <div style={{ display: "flex", gap: 10, justifyContent: "flex-end" }}>
        <button className="btn" onClick={() => navigate("/repositories")}>
          Back to repository domains
        </button>
        <button className="btn btn--primary" disabled title="No API route creates a repository domain yet">
          Create domain
        </button>
      </div>
    </>
  );
}

/** A field this screen can show and cannot write. Disabled rather than
 *  read-only-styled, so nothing about it invites typing, and carrying the
 *  wire field it would map to, because the operator most likely to be on
 *  this screen is the one writing the configuration by hand. */
function DisabledField({
  label,
  wire,
  placeholder,
  mono,
  type
}: {
  label: string;
  wire?: string;
  placeholder: string;
  mono?: boolean;
  type?: "password";
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
      <label className="field">
        <span className="field__label">{label}</span>
        <input
          className={"input" + (mono ? " input--mono" : "")}
          type={type ?? "text"}
          value=""
          placeholder={placeholder}
          disabled
          readOnly
        />
      </label>
      {wire ? <WireField name={wire} /> : null}
    </div>
  );
}
