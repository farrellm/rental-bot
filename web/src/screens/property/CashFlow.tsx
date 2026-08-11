import { useState } from "react";
import { useParams } from "react-router";

import { describeError } from "../../api/client";
import {
  useCreateTransaction,
  useDeleteTransaction,
  useTransactions,
  type LedgerFilter,
} from "../../api/queries";
import type { TransactionCategory } from "../../api/types";
import { Select } from "../../components/Select";
import { SheetField, SheetFilter } from "../../components/SheetField";
import { SheetForm } from "../../components/SheetForm";
import { calendarDate, DASH, isCalendarDate, money, parseMoney, today } from "../../format";

/** The categories, in the order the ledger's own CHECK lists them. */
const CATEGORIES: TransactionCategory[] = [
  "rent_income",
  "other_income",
  "mortgage_payment",
  "insurance",
  "property_tax",
  "hoa",
  "mgmt_fee",
  "repair",
  "capex",
  "utilities",
  "legal",
  "other",
];

/** Written out for a reader; the wire keeps the underscored name. */
const CATEGORY_LABEL: Record<TransactionCategory, string> = {
  rent_income: "Rent",
  other_income: "Other income",
  mortgage_payment: "Mortgage",
  insurance: "Insurance",
  property_tax: "Property tax",
  hoa: "HOA",
  mgmt_fee: "Management",
  repair: "Repair",
  capex: "Capital",
  utilities: "Utilities",
  legal: "Legal",
  other: "Other",
};

/**
 * The sign, chosen as a word.
 *
 * "-285.00" is a thing people mistype and "Money out" is not. It becomes
 * signed cents at the form's boundary and nowhere else.
 */
type Direction = "in" | "out";

const DIRECTIONS: Direction[] = ["out", "in"];

const DIRECTION_LABEL: Record<Direction, string> = {
  out: "Money out",
  in: "Money in",
};

type Range = "month" | "year" | "all";

const RANGES: Range[] = ["month", "year", "all"];

const RANGE_LABEL: Record<Range, string> = {
  month: "This month",
  year: "This year",
  all: "Everything",
};

/**
 * The cash-flow ledger.
 *
 * One signed amount column: income and expense are the same figure with
 * different signs, in the database and here, and a second column would be this
 * screen inventing a distinction the data does not make. Both are set in
 * graphite — an expense is not a fault, and colouring every one of them red
 * would make an ordinary month look like an emergency. The net is the one
 * figure allowed to raise its voice, and only when it is negative.
 */
export function CashFlow() {
  const params = useParams();
  const propertyId = Number(params.id ?? 0);

  const [range, setRange] = useState<Range>("year");
  const [category, setCategory] = useState<TransactionCategory | "">("");
  const [adding, setAdding] = useState(false);

  const filter: LedgerFilter = { ...rangeDates(range), category: category || undefined };
  const ledger = useTransactions(propertyId, filter);
  const createEntry = useCreateTransaction(propertyId);
  const deleteEntry = useDeleteTransaction(propertyId);

  const [notice, setNotice] = useState<string | null>(null);

  async function remove(id: number) {
    setNotice(null);
    try {
      await deleteEntry.mutateAsync(id);
    } catch (err) {
      setNotice(describeError(err));
    }
  }

  return (
    <>
      {notice && <p className="card__notice">{notice}</p>}

      <div className="sheet">
        {/* The tab overhead already says which section this is. */}
        <div className="sheet__head">
          <div className="sheet__filters">
            <SheetFilter label="Range">
              <Select
                value={range}
                onChange={setRange}
                options={RANGES}
                labels={RANGE_LABEL}
                short
              />
            </SheetFilter>

            <SheetFilter label="Category">
              <Select
                value={category}
                onChange={setCategory}
                options={CATEGORIES}
                labels={CATEGORY_LABEL}
                anyLabel="All"
                short
              />
            </SheetFilter>
          </div>
        </div>

        {ledger.isPending && <p className="waiting waiting--ink">Reading the ledger</p>}
        {ledger.isError && <p className="hint hint--fault">{describeError(ledger.error)}</p>}

        {ledger.data && (
          <>
            {ledger.data.items.length === 0 ? (
              <p className="sheet__empty">
                No entries in this range.
                {range !== "all" && " Widen the range, or add the first one."}
              </p>
            ) : (
              <div className="ledger-sheet">
                {/* Pre-printed column heads, the way a ruled book has them. */}
                <div className="ledger-sheet__head" aria-hidden="true">
                  <span className="stamped">Date</span>
                  <span className="stamped">Entry</span>
                  <span className="stamped">Category</span>
                  <span className="stamped ledger-sheet__figure">Amount</span>
                </div>

                {ledger.data.items.map((entry) => (
                  <div key={entry.id} className="ledger-sheet__row">
                    <span className="ledger-sheet__date mono">
                      {calendarDate(entry.occurred_on)}
                    </span>
                    <span className="ledger-sheet__entry">
                      {entry.description || entry.counterparty || DASH}
                      {entry.needs_review && (
                        <span className="ledger-sheet__flag stamped"> needs review</span>
                      )}
                    </span>
                    <span className="ledger-sheet__category mono">
                      {CATEGORY_LABEL[entry.category]}
                    </span>
                    <span className="ledger-sheet__figure mono">{money(entry.amount_cents)}</span>
                    <button
                      type="button"
                      className="button button--danger button--quiet ledger-sheet__strike"
                      onClick={() => void remove(entry.id)}
                      aria-label={`Remove the entry of ${money(entry.amount_cents)} on ${entry.occurred_on}`}
                    >
                      Remove
                    </button>
                  </div>
                ))}

                {/* The foot is the server's arithmetic over the whole filtered
                    set, not a sum of the rows that fitted on this page. */}
                <div className="ledger-sheet__foot">
                  <span className="ledger-sheet__total-label stamped">Income</span>
                  <span className="ledger-sheet__figure mono">
                    {money(ledger.data.totals.income_cents)}
                  </span>
                  <span className="ledger-sheet__total-label stamped">Expense</span>
                  <span className="ledger-sheet__figure mono">
                    {money(ledger.data.totals.expense_cents)}
                  </span>
                  <span className="ledger-sheet__total-label ledger-sheet__net stamped">Net</span>
                  <span
                    className={
                      ledger.data.totals.net_cents < 0
                        ? "ledger-sheet__figure ledger-sheet__net mono ledger-sheet__figure--short"
                        : "ledger-sheet__figure ledger-sheet__net mono"
                    }
                  >
                    {money(ledger.data.totals.net_cents)}
                  </span>
                </div>
              </div>
            )}
          </>
        )}

        {adding ? (
          <EntryForm
            onCancel={() => setAdding(false)}
            onSubmit={async (body) => {
              setNotice(null);
              try {
                await createEntry.mutateAsync(body);
                setAdding(false);
              } catch (err) {
                setNotice(describeError(err));
              }
            }}
          />
        ) : (
          <div className="sheet__actions">
            <button
              type="button"
              className="button button--primary"
              onClick={() => setAdding(true)}
            >
              Add an entry
            </button>
          </div>
        )}
      </div>
    </>
  );
}

interface EntryFormProps {
  onCancel: () => void;
  onSubmit: (body: {
    occurred_on: string;
    amount_cents: number;
    category: TransactionCategory;
    description: string;
  }) => Promise<void>;
}

/**
 * A new line on the sheet.
 *
 * The amount is typed as an amount and the sign is chosen as a word, because
 * "-285.00" is a thing people mistype and "Money out" is not. It becomes
 * signed cents at this boundary and nowhere else.
 */
function EntryForm({ onCancel, onSubmit }: EntryFormProps) {
  const [occurredOn, setOccurredOn] = useState(today());
  const [amount, setAmount] = useState("");
  const [direction, setDirection] = useState<Direction>("out");
  const [category, setCategory] = useState<TransactionCategory>("repair");
  const [description, setDescription] = useState("");
  const [problem, setProblem] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  async function submit() {
    if (saving) return;

    if (!isCalendarDate(occurredOn)) {
      setProblem("Write the date as YYYY-MM-DD.");
      return;
    }
    const cents = parseMoney(amount);
    if (cents === undefined || cents === null) {
      setProblem("Write the amount as a figure, like 285.00.");
      return;
    }

    setProblem(null);
    setSaving(true);
    try {
      await onSubmit({
        occurred_on: occurredOn.trim(),
        amount_cents: direction === "out" ? -Math.abs(cents) : Math.abs(cents),
        category,
        description: description.trim(),
      });
    } finally {
      setSaving(false);
    }
  }

  return (
    <SheetForm
      title="New entry"
      problem={problem}
      saving={saving}
      submitLabel="Save entry"
      onSubmit={() => void submit()}
      onCancel={onCancel}
    >
      <SheetField label="Date">
        <input
          className="entry"
          value={occurredOn}
          onChange={(e) => setOccurredOn(e.target.value)}
          placeholder="YYYY-MM-DD"
          inputMode="numeric"
          autoComplete="off"
        />
      </SheetField>

      <SheetField label="Direction">
        <Select
          value={direction}
          onChange={setDirection}
          options={DIRECTIONS}
          labels={DIRECTION_LABEL}
        />
      </SheetField>

      <SheetField label="Amount">
        <input
          className="entry"
          value={amount}
          onChange={(e) => setAmount(e.target.value)}
          placeholder="285.00"
          inputMode="decimal"
          autoComplete="off"
        />
      </SheetField>

      <SheetField label="Category">
        <Select
          value={category}
          onChange={setCategory}
          options={CATEGORIES}
          labels={CATEGORY_LABEL}
        />
      </SheetField>

      <SheetField label="Entry" wide>
        <input
          className="entry"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          placeholder="Ace Plumbing, kitchen tap"
          autoComplete="off"
        />
      </SheetField>
    </SheetForm>
  );
}

/**
 * The dates a named range covers.
 *
 * These are calendar dates in the reader's own year and month, formatted the
 * way the column is stored: no timezone is invented for a date that never had
 * one.
 */
function rangeDates(range: Range): { from?: string; to?: string } {
  if (range === "all") return {};

  const now = new Date();
  const year = now.getFullYear();
  if (range === "year") {
    return { from: `${year}-01-01`, to: `${year}-12-31` };
  }

  const month = String(now.getMonth() + 1).padStart(2, "0");
  const lastDay = new Date(year, now.getMonth() + 1, 0).getDate();
  return {
    from: `${year}-${month}-01`,
    to: `${year}-${month}-${String(lastDay).padStart(2, "0")}`,
  };
}
