import { describeError, useInsurance, type InsurancePolicy, type PolicyType } from "../../api";
import { FieldRow } from "../../components/FieldRow";
import { calendarDate, DASH, money } from "../../format";
import { usePropertyId } from "./usePropertyId";

const TYPE_WORD: Record<PolicyType, string> = {
  hazard: "Hazard",
  flood: "Flood",
  umbrella: "Umbrella",
  liability: "Liability",
};

/**
 * The policies on a property.
 *
 * Read-only, and that is not an oversight: at M4 a policy reaches this record
 * one way, by forwarding the declaration page, and the review slip is where
 * its figures are corrected. Entering one by hand arrives with the milestone
 * that needs it.
 *
 * The policy number is not shown, because it is not sent. It is encrypted at
 * rest, and a screen that lists policies has no use for it worth decrypting it
 * for.
 */
export function Insurance() {
  const propertyId = usePropertyId();
  const policies = useInsurance(propertyId);

  return (
    <div className="sheet">
      {policies.isPending && <p className="waiting waiting--ink">Reading the policies</p>}
      {policies.isError && <p className="hint hint--fault">{describeError(policies.error)}</p>}

      {policies.data &&
        (policies.data.items.length === 0 ? (
          <p className="sheet__empty">
            No policy on file. Forward a declaration page to the connected mailbox and it lands in
            Review.
          </p>
        ) : (
          policies.data.items.map((policy) => <Policy key={policy.id} policy={policy} />)
        ))}
    </div>
  );
}

function Policy({ policy }: { policy: InsurancePolicy }) {
  return (
    <article className="policy">
      <header className="policy__head">
        <h2 className="policy__carrier">{policy.carrier || "unnamed carrier"}</h2>
        <span className="policy__type stamped">{TYPE_WORD[policy.type]}</span>
      </header>

      <dl className="card__fields">
        <FieldRow label="Term">
          {calendarDate(policy.effective_date)} → {calendarDate(policy.expiration_date)}
        </FieldRow>
        <FieldRow label="Premium">{money(policy.annual_premium_cents)}</FieldRow>
        <FieldRow label="Dwelling">{money(policy.dwelling_coverage_cents)}</FieldRow>
        <FieldRow label="Liability">{money(policy.liability_coverage_cents)}</FieldRow>
        <FieldRow label="Deductible">{money(policy.deductible_cents)}</FieldRow>
        <FieldRow label="Agent">{agent(policy)}</FieldRow>
        {policy.document_id !== null && (
          <FieldRow label="Enclosure">
            <a
              className="button button--quiet"
              href={`/api/v1/documents/${policy.document_id}/content`}
              target="_blank"
              rel="noreferrer"
            >
              Open the declaration
            </a>
          </FieldRow>
        )}
        {policy.notes && <FieldRow label="Notes">{policy.notes}</FieldRow>}
      </dl>
    </article>
  );
}

/** The agent, as one line, or a dash when nothing about them is on file. */
function agent(policy: InsurancePolicy): string {
  const parts = [policy.agent_name, policy.agent_phone, policy.agent_email].filter(
    (part) => part !== "",
  );
  return parts.length > 0 ? parts.join(" · ") : DASH;
}
