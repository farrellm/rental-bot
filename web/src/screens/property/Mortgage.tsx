import { describeError, useMortgages, type Mortgage as Loan } from "../../api";
import { FieldRow } from "../../components/FieldRow";
import { calendarDate, DASH, money } from "../../format";
import { usePropertyId } from "./usePropertyId";

/**
 * The mortgages on a property, and the statements that moved them.
 *
 * Read-only at M4, like Insurance and for the same reason: a mortgage reaches
 * this record by forwarding a statement, and the review slip is where its
 * figures are corrected.
 *
 * The statements are append-only, which is what makes the amortization history
 * below a consequence of the write path rather than a feature. Nothing
 * overwrites a month; a new one is a new line.
 */
export function Mortgage() {
  const propertyId = usePropertyId();
  const mortgages = useMortgages(propertyId);

  return (
    <div className="sheet">
      {mortgages.isPending && <p className="waiting waiting--ink">Reading the loan file</p>}
      {mortgages.isError && <p className="hint hint--fault">{describeError(mortgages.error)}</p>}

      {mortgages.data &&
        (mortgages.data.items.length === 0 ? (
          <p className="sheet__empty">
            No mortgage on file. Forward a statement to the connected mailbox and it lands in
            Review.
          </p>
        ) : (
          mortgages.data.items.map((loan) => <Loan key={loan.id} loan={loan} />)
        ))}
    </div>
  );
}

function Loan({ loan }: { loan: Loan }) {
  return (
    <article className="loan">
      <header className="loan__head">
        <h2 className="loan__lender">{loan.lender || "unnamed lender"}</h2>
        <span className="policy__type stamped">{rate(loan)}</span>
      </header>

      <dl className="card__fields">
        <FieldRow label="Balance">
          {money(loan.current_balance_cents)}
          {loan.balance_as_of ? ` as of ${calendarDate(loan.balance_as_of)}` : ""}
        </FieldRow>
        <FieldRow label="Original">{money(loan.original_principal_cents)}</FieldRow>
        <FieldRow label="Payment">{money(loan.monthly_pi_cents)}</FieldRow>
        <FieldRow label="Escrow">{money(loan.escrow_monthly_cents)}</FieldRow>
        <FieldRow label="Originated">{calendarDate(loan.origination_date)}</FieldRow>
      </dl>

      <p className="register__eyebrow stamped">Statements</p>
      {loan.statements.length === 0 ? (
        <p className="sheet__empty">No statement has been filed against this loan yet.</p>
      ) : (
        <div className="statements">
          {loan.statements.map((statement) => (
            <div key={statement.id} className="statements__row">
              <span className="statements__date mono">
                {calendarDate(statement.statement_date)}
              </span>
              <span className="mono">
                {money(statement.principal_paid_cents)} principal ·{" "}
                {money(statement.interest_paid_cents)} interest
              </span>
              <span className="statements__balance mono">
                {money(statement.principal_balance_cents)}
              </span>
            </div>
          ))}
        </div>
      )}
    </article>
  );
}

/**
 * The rate, in the margin.
 *
 * Basis points on the wire, because 6.375% is 637 and integer arithmetic over
 * it is exact. This is the one place it becomes a percentage.
 */
function rate(loan: Loan): string {
  if (loan.interest_rate_bps === null) return DASH;
  return `${(loan.interest_rate_bps / 100).toFixed(3).replace(/0+$/, "").replace(/\.$/, "")}%`;
}
