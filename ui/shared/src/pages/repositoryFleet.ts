/**
 * Every repository domain this deployment declares, with the maintenance
 * record that belongs to it (EPIC K, issue #788).
 *
 * # Why the two reads are joined here
 *
 * The contract splits them: GET /repositories answers one health record
 * per domain, and maintenance ownership is a sub-resource read one domain
 * at a time. Two screens need them together — the domains list names each
 * domain's owner, and the deployment's backup defaults carry the ownership
 * table — and a page that fetched the fleet and then fanned out inside its
 * own render would give the two of them different join logic and different
 * failure behaviour.
 *
 * # A maintenance read that fails does not fail the fleet
 *
 * Each record is fetched independently and a rejection is KEPT, per
 * domain, rather than thrown. A domain whose maintenance sub-resource is
 * unreadable is still a domain that exists, is still storing snapshots and
 * still has health worth showing; collapsing the whole list because one
 * sub-read refused would hide four working domains to report one that did
 * not answer. The row says what it could not read, which is the honest
 * version of an empty owner cell — "nobody owns this" is a real answer
 * this product draws, and it must never be what a failed read looks like.
 */
import type { RetndApi } from "@shared/api/contracts";
import { describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import type { RepositoryHealth, RepositoryMaintenance } from "@shared/types/snapshot";

/** One domain, as the deployment screens read it. */
export interface DomainRecord {
  health: RepositoryHealth;
  /** Null when the maintenance sub-resource could not be read; see
   *  `maintenanceError` for what happened. Never null for "nobody owns
   *  it", which is `maintenance.owner === ""`. */
  maintenance: RepositoryMaintenance | null;
  /** The whole translated refusal rather than its sentence: a list draws
   *  the message inline, and the maintenance screen — which is where an
   *  operator goes to act on it — owes the correlation id that response
   *  was logged under (#274). One shape serves both. */
  maintenanceError: OperatorFailure | null;
}

export interface RepositoryFleetView {
  generatedAt: string;
  domains: DomainRecord[];
}

export async function loadRepositoryFleet(api: RetndApi): Promise<RepositoryFleetView> {
  const fleet = await api.listRepositories();
  const domains = await Promise.all(
    fleet.repositories.map(async (health): Promise<DomainRecord> => {
      try {
        return { health, maintenance: await api.getRepositoryMaintenance(health.domain), maintenanceError: null };
      } catch (e) {
        return {
          health,
          maintenance: null,
          maintenanceError: describeFailure(
            e,
            "retnd could not read who maintains this repository domain."
          )
        };
      }
    })
  );
  return { generatedAt: fleet.generatedAt, domains };
}

/**
 * The probes a domain is currently failing, as words.
 *
 * Each probe is reported separately rather than reduced to the state
 * badge, because the remedies differ and the badge cannot say which one is
 * wanted: unreachable is a mount, unwritable is a permission, invalid
 * credentials is a passphrase, and overdue maintenance is a schedule. An
 * empty array is a domain passing all of them.
 */
export function failingProbes(health: RepositoryHealth): string[] {
  const failures: string[] = [];
  if (!health.reachable) failures.push("unreachable");
  if (!health.readable) failures.push("not readable");
  if (!health.writable) failures.push("not writable");
  if (!health.credentialsValid) failures.push("credentials rejected");
  if (!health.clockSane) failures.push("clock out of step");
  if (health.maintenanceOverdue) failures.push("maintenance overdue");
  return failures;
}

/**
 * A clock skew as an operator reads one.
 *
 * Null is "not measured" and never a perfect zero, for the reason every
 * nullable counter in this epic is: an unmeasured skew drawn as 0 s is a
 * claim that the two clocks agree exactly. The SIGN is kept because it is
 * the dangerous half — a repository clock BEHIND this deployment dates a
 * new snapshot before one already stored.
 */
export function clockSkew(seconds: number | null): string {
  if (seconds === null) return "not measured";
  if (seconds === 0) return "0 s (in step)";
  const magnitude = Math.abs(seconds);
  const size = magnitude >= 3600
    ? Math.round(magnitude / 360) / 10 + " h"
    : magnitude >= 60
      ? Math.round(magnitude / 6) / 10 + " min"
      : magnitude + " s";
  return size + (seconds > 0 ? " ahead of this deployment" : " behind this deployment");
}
