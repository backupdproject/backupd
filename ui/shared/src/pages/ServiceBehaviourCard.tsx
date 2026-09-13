import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { BackupdError } from "@shared/api/contracts";
import type { ApiError, AppSettings } from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { Banner } from "@shared/components/Banner";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { ErrorState } from "@shared/components/EmptyState";
import { Icon } from "@shared/design-system/icons";
import { isNotConfigured } from "@shared/api/failure";
import { InfoTooltip } from "@shared/tooltips/InfoTooltip";

/**
 * Issue #845 — how often Backupd looks at a source, as a control rather
 * than a config-file edit.
 *
 * # This is the card #299 deleted, built properly
 *
 * A "Service" card with a "Polling interval" picklist used to sit on this
 * page: 15/30/60 SECONDS, `defaultValue`, no handler, saving nowhere. It
 * was removed rather than wired, because the real key is a duration in
 * minutes and there was no write path for it at all. Both halves now
 * exist: the engine reads its cadence from the running configuration on
 * every wake, and this writes it.
 *
 * # Minutes here, seconds on the wire
 *
 * The API carries durations as seconds (`stale_after_seconds`,
 * `stable_for_seconds`, and now `poll_interval_seconds`), and a backup
 * cadence is a thing operators think about in minutes. This component is
 * the one place the two meet, exactly as CapacityCard is the one place a
 * byte count becomes an amount-plus-unit.
 *
 * # The floor comes from the server
 *
 * `schema.service.minPollIntervalSeconds` is what disables Save, not a
 * number written here. A local copy of a server-side bound is one that
 * eventually refuses a value the engine would accept, and the refusal an
 * operator would see for it is this form's own opinion rather than the
 * product's.
 */
export function ServiceBehaviourCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const settings = useAsync<AppSettings>(() => api.getSettings(), [api]);

  return (
    <section className="card">
      <div className="card__header">
        <InfoTooltip id="service.card" block>
          <h2 className="eyebrow">Service behaviour</h2>
        </InfoTooltip>
      </div>
      <div className="card__body">
        {isNotConfigured(settings.error) ? (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
            The polling interval is part of a configuration this instance has not been given
            yet. It becomes editable once the first backup set has been added.
          </p>
        ) : settings.error ? (
          <ErrorState
            message={settings.error.message}
            remediation="The service settings could not be read, so they cannot be edited here yet."
            correlationId={settings.error.correlationId}
            onRetry={settings.reload}
          />
        ) : settings.data ? (
          <ServiceBehaviourEditor
            key={String(settings.data.service.pollIntervalSeconds)}
            loadedSeconds={settings.data.service.pollIntervalSeconds}
            minSeconds={settings.data.schema.service.minPollIntervalSeconds}
            readOnly={readOnly}
          />
        ) : (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
            Loading service settings…
          </p>
        )}
      </div>
    </section>
  );
}

/** Minutes as the box holds them: text, not a number, so a cleared or
 *  half-typed field stays exactly what was typed rather than being
 *  coerced into something nobody entered — the same reason CapacityCard's
 *  byte fields hold strings. */
function minutesDraft(seconds: number): string {
  return String(Math.round(seconds / 60));
}

function ServiceBehaviourEditor({
  loadedSeconds,
  minSeconds,
  readOnly
}: {
  loadedSeconds: number;
  minSeconds: number;
  readOnly: boolean;
}) {
  const api = useApi();

  const [baselineSeconds, setBaselineSeconds] = useState(loadedSeconds);
  const [minutes, setMinutes] = useState(() => minutesDraft(loadedSeconds));
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<ApiError | null>(null);
  const [saved, setSaved] = useState(false);

  const typed = Number(minutes.trim());
  const seconds = Math.round(typed * 60);
  const minMinutes = Math.max(1, Math.round(minSeconds / 60));
  const invalid =
    minutes.trim() === "" || !Number.isInteger(typed) || seconds < minSeconds;
  const error = invalid
    ? "Enter a whole number of minutes, at least " +
      minMinutes +
      (minMinutes === 1 ? " minute." : " minutes.")
    : undefined;
  const dirty = !invalid && seconds !== baselineSeconds;

  function onSave() {
    if (readOnly || invalid || !dirty || busy) return;
    setBusy(true);
    setSaveError(null);
    setSaved(false);
    api
      .updateSettings({ service: { pollIntervalSeconds: seconds } })
      .then((next) => {
        // Re-baselined against what the server says is now running, not
        // against the draft, exactly as CapacityCard does: the value in
        // effect is the answer, and the next "has anything changed"
        // comparison has to be made against it.
        setBaselineSeconds(next.service.pollIntervalSeconds);
        setMinutes(minutesDraft(next.service.pollIntervalSeconds));
        setSaved(true);
      })
      .catch((e: unknown) => {
        setSaveError(
          e instanceof BackupdError
            ? e.api
            : {
                code: "unknown",
                message: "Backupd could not save the service settings.",
                correlationId: "unavailable"
              }
        );
      })
      .finally(() => setBusy(false));
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "78ch" }}>
        How often Backupd checks each source for new backup files. This is the default every
        backup set follows; a set can be given its own interval on its own page.
      </p>

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))", gap: "15px 18px" }}>
        <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
          <HelpField label="Source polling interval (minutes)" help={FIELD_HELP.pollInterval}>
            {(helpId) => (
              <input
                className="input"
                type="number"
                min={minMinutes}
                step={1}
                aria-describedby={helpId}
                value={minutes}
                disabled={readOnly}
                onChange={(e) => {
                  setSaved(false);
                  setMinutes(e.target.value);
                }}
              />
            )}
          </HelpField>
          {error ? (
            <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>{error}</span>
          ) : null}
        </div>
      </div>

      {saveError ? (
        <ErrorState
          message={saveError.message}
          remediation="Nothing was saved. The polling interval on disk is unchanged."
          correlationId={saveError.correlationId}
        />
      ) : null}

      {saved ? (
        <Banner tone="ok" style={{ fontSize: "var(--text-sm)" }}>
          <span aria-hidden="true" style={{ color: "var(--ok)", lineHeight: 1.5 }}>
            <Icon name="success" />
          </span>
          <span>Polling interval saved. It is in effect now, with no restart.</span>
        </Banner>
      ) : null}

      <div>
        <InfoTooltip id="service.save">
          <button
            className="btn btn--primary"
            type="button"
            style={{ height: 40 }}
            disabled={readOnly || invalid || !dirty || busy}
            onClick={onSave}
          >
            {busy ? "Saving…" : "Save service behaviour"}
          </button>
        </InfoTooltip>
      </div>
    </div>
  );
}
