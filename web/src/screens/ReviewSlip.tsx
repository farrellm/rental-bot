import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router";

import {
  describeError,
  useApproveProposal,
  useProposal,
  useRejectProposal,
  useUpdateProposal,
  type ProposalDetail,
  type ProposalEnclosure,
  type ProposalKind,
} from "../api";
import { AmendableRow, FieldRow } from "../components/FieldRow";
import { Stamp, type StampState } from "../components/Stamp";
import {
  bytes,
  calendarDate,
  DASH,
  fileNumber,
  isCalendarDate,
  money,
  parseMoney,
  timestamp,
} from "../format";

/**
 * One field of one kind's form.
 *
 * The shapes differ per kind and the API does not flatten them, so the slip
 * carries the table instead. It is the same table the Go schema declares, in
 * the same order, and it is the only thing this screen knows about what a
 * receipt is.
 */
interface FieldSpec {
  key: string;
  label: string;
  as: "text" | "money" | "date" | "count" | "note";
}

const FIELDS: Record<ProposalKind, FieldSpec[]> = {
  receipt: [
    { key: "vendor_name", label: "Paid to", as: "text" },
    { key: "date_iso", label: "Date", as: "date" },
    { key: "total_cents", label: "Total", as: "money" },
    { key: "category", label: "Category", as: "text" },
    { key: "payment_method", label: "Paid by", as: "text" },
    { key: "notes", label: "Notes", as: "note" },
  ],
  lease: [
    { key: "unit_label", label: "Unit", as: "text" },
    { key: "start_date_iso", label: "Starts", as: "date" },
    { key: "end_date_iso", label: "Ends", as: "date" },
    { key: "rent_cents", label: "Rent", as: "money" },
    { key: "deposit_cents", label: "Deposit", as: "money" },
    { key: "due_day", label: "Due day", as: "count" },
    { key: "late_fee_cents", label: "Late fee", as: "money" },
    { key: "notes", label: "Notes", as: "note" },
  ],
  insurance: [
    { key: "carrier", label: "Carrier", as: "text" },
    { key: "policy_number", label: "Policy no.", as: "text" },
    { key: "type", label: "Type", as: "text" },
    { key: "effective_date_iso", label: "Effective", as: "date" },
    { key: "expiration_date_iso", label: "Expires", as: "date" },
    { key: "annual_premium_cents", label: "Premium", as: "money" },
    { key: "dwelling_coverage_cents", label: "Dwelling", as: "money" },
    { key: "liability_coverage_cents", label: "Liability", as: "money" },
    { key: "deductible_cents", label: "Deductible", as: "money" },
    { key: "agent_name", label: "Agent", as: "text" },
    { key: "notes", label: "Notes", as: "note" },
  ],
  mortgage_statement: [
    { key: "lender", label: "Lender", as: "text" },
    { key: "loan_number", label: "Loan no.", as: "text" },
    { key: "statement_date_iso", label: "Statement", as: "date" },
    { key: "principal_balance_cents", label: "Balance", as: "money" },
    { key: "payment_cents", label: "Payment", as: "money" },
    { key: "principal_paid_cents", label: "Principal", as: "money" },
    { key: "interest_paid_cents", label: "Interest", as: "money" },
    { key: "escrow_paid_cents", label: "Escrow", as: "money" },
    { key: "notes", label: "Notes", as: "note" },
  ],
  // Read and filed, with no form to fill in. The slip still shows the
  // enclosure and what the classifier made of it, which is the point: "we
  // could not tell what this is" is a fact worth putting in front of somebody.
  repair: [],
  valuation: [],
  note: [],
  unknown: [],
};

const KIND_WORD: Record<ProposalKind, string> = {
  receipt: "Receipt",
  lease: "Lease",
  insurance: "Insurance",
  mortgage_statement: "Mortgage statement",
  repair: "Repair",
  valuation: "Valuation",
  note: "Note",
  unknown: "Unread",
};

/** The same mapping the register uses, and for the same reason. */
const STATUS_STAMP: Record<ProposalDetail["status"], StampState> = {
  pending: "pending",
  approved: "applied",
  rejected: "rejected",
  auto_applied: "needs-review",
};

/**
 * The slip: what was read, beside what it was read off.
 *
 * Every value the model typed is set in carbon; the moment the operator
 * changes one it goes graphite. Nothing that came off a model passes for
 * something a person wrote down, which is the whole of what the proposal gate
 * is for.
 */
export function ReviewSlip() {
  const params = useParams();
  const id = Number(params.id ?? 0);
  const navigate = useNavigate();

  const proposal = useProposal(id);
  const save = useUpdateProposal(id);
  const approve = useApproveProposal(id);
  const reject = useRejectProposal(id);

  // Every draft value is a string, because that is what an input holds.
  const [draft, setDraft] = useState<Record<string, string> | null>(null);
  const [seeded, setSeeded] = useState(0);
  const [property, setProperty] = useState<number | null>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);

  const data = proposal.data ?? null;

  // Seeded during render rather than in an effect, so the first paint already
  // has the values -- unless the operator is part way through correcting one.
  if (data && seeded !== data.id) {
    setSeeded(data.id);
    setDraft(seedDraft(data));
    setProperty(data.property_id);
    setProblem(null);
  }

  if (proposal.isPending) {
    return (
      <main className="shell__main shell__main--single">
        <p className="waiting">Reading the slip</p>
      </main>
    );
  }
  if (proposal.isError || !data || !draft) {
    return (
      <main className="shell__main shell__main--single">
        <p className="empty__line">
          {proposal.isError ? describeError(proposal.error) : "No such proposal."}
        </p>
      </main>
    );
  }

  const fields = FIELDS[data.kind];
  const settled = data.status !== "pending";
  const dirty = fields.some((f) => touched(data, draft, f)) || property !== data.property_id;
  const busy = save.isPending || approve.isPending || reject.isPending;

  function set(key: string, value: string) {
    setDraft((was) => ({ ...(was ?? {}), [key]: value }));
    setProblem(null);
  }

  /** Turns the draft back into the payload shape, or reports what will not go. */
  function corrected(): Record<string, unknown> | null {
    const payload: Record<string, unknown> = { ...(data?.payload ?? {}) };
    for (const field of fields) {
      const raw = (draft ?? {})[field.key] ?? "";
      switch (field.as) {
        case "money": {
          const cents = parseMoney(raw);
          if (cents === undefined) {
            setProblem(`Write ${field.label.toLowerCase()} as a figure, like 482.19.`);
            return null;
          }
          payload[field.key] = cents ?? 0;
          break;
        }
        case "date": {
          if (raw !== "" && !isCalendarDate(raw)) {
            setProblem(`Write ${field.label.toLowerCase()} as YYYY-MM-DD.`);
            return null;
          }
          payload[field.key] = raw;
          break;
        }
        case "count": {
          const n = Number(raw.trim());
          if (raw.trim() !== "" && !Number.isInteger(n)) {
            setProblem(`Write ${field.label.toLowerCase()} as a whole number.`);
            return null;
          }
          payload[field.key] = raw.trim() === "" ? 0 : n;
          break;
        }
        default:
          payload[field.key] = raw;
      }
    }
    return payload;
  }

  async function persist(): Promise<boolean> {
    const payload = corrected();
    if (!payload) return false;
    try {
      await save.mutateAsync({ payload, property_id: property });
      return true;
    } catch (err) {
      setProblem(describeError(err));
      return false;
    }
  }

  async function file() {
    if (dirty && !(await persist())) return;
    try {
      await approve.mutateAsync();
      void navigate("/review");
    } catch (err) {
      setProblem(describeError(err));
    }
  }

  async function refuse() {
    try {
      await reject.mutateAsync();
      void navigate("/review");
    } catch (err) {
      setProblem(describeError(err));
      setConfirming(false);
    }
  }

  return (
    <main className="shell__main shell__main--single">
      <section className="card card--plate">
        <header className="card__head">
          <div className="card__title">
            <h1 className="card__mark stamped">{mark(data)}</h1>
            <p className="card__origin mono">
              {KIND_WORD[data.kind]} · {data.subject || "no subject"}
            </p>
          </div>
          <div className="card__file">
            <p className="card__eyebrow stamped">Proposal</p>
            <p className="card__read mono">{fileNumber(data.id)}</p>
          </div>
        </header>

        {problem && <p className="card__notice">{problem}</p>}
        {!problem && data.error && <p className="card__notice">{data.error}</p>}

        <div className="slip">
          <Enclosures enclosures={data.enclosures} />

          <div className="slip__fields">
            <p className="slip__eyebrow stamped">Read from the enclosure</p>
            <dl className="card__fields">
              <PropertyRow
                data={data}
                property={property}
                editing={!settled}
                onChange={setProperty}
              />

              {fields.map((field) => (
                <AmendableRow
                  key={field.key}
                  label={field.label}
                  editing={!settled}
                  htmlFor={`slip-${field.key}`}
                  block={field.as === "note"}
                  value={
                    <span className={inkClass(touched(data, draft, field))}>
                      {read(data, field)}
                    </span>
                  }
                >
                  {field.as === "note" ? (
                    <textarea
                      id={`slip-${field.key}`}
                      className={`entry ${inkClass(touched(data, draft, field))}`}
                      rows={2}
                      value={draft[field.key] ?? ""}
                      onChange={(e) => set(field.key, e.target.value)}
                    />
                  ) : (
                    <input
                      id={`slip-${field.key}`}
                      className={`entry ${inkClass(touched(data, draft, field))}`}
                      value={draft[field.key] ?? ""}
                      onChange={(e) => set(field.key, e.target.value)}
                      inputMode={field.as === "text" ? undefined : "numeric"}
                      placeholder={field.as === "date" ? "YYYY-MM-DD" : undefined}
                      autoComplete="off"
                    />
                  )}
                </AmendableRow>
              ))}

              {fields.length === 0 && (
                <FieldRow label="Reading">
                  {data.reasoning || "Nothing was taken off this document."}
                </FieldRow>
              )}

              {settled && (
                <FieldRow label="Filed">
                  {data.status === "auto_applied" ? "automatically" : "by hand"}
                  {data.reviewed_at ? ` · ${timestamp(data.reviewed_at)}` : ""}
                </FieldRow>
              )}
            </dl>
          </div>
        </div>

        <footer className="card__foot">
          <div className="register__foot">
            <Reading data={data} />
            <div className="actions">
              <Link className="button" to="/review">
                Back
              </Link>
              {!settled && dirty && (
                <button
                  type="button"
                  className="button"
                  disabled={busy}
                  onClick={() => void persist()}
                >
                  {save.isPending ? "Saving…" : "Save changes"}
                </button>
              )}
              {!settled && !confirming && (
                <button
                  type="button"
                  className="button button--danger"
                  disabled={busy}
                  onClick={() => setConfirming(true)}
                >
                  Reject
                </button>
              )}
              {!settled && confirming && (
                <>
                  <button
                    type="button"
                    className="button button--danger"
                    disabled={busy}
                    onClick={() => void refuse()}
                  >
                    Reject for good
                  </button>
                  <button type="button" className="button" onClick={() => setConfirming(false)}>
                    Keep
                  </button>
                </>
              )}
              {!settled && (
                <button
                  type="button"
                  className="button button--primary"
                  disabled={busy}
                  onClick={() => void file()}
                >
                  {approve.isPending ? "Filing…" : "Approve"}
                </button>
              )}
            </div>
          </div>
          <Stamp state={STATUS_STAMP[data.status]} />
        </footer>
      </section>
    </main>
  );
}

/**
 * The property this is filed against, and the matcher's account of itself.
 *
 * The match is deterministic Go over the folded address, and it is sometimes
 * wrong. When it is, the operator says which building this is; when it found
 * nothing, the row is where they say so for the first time.
 */
function PropertyRow({
  data,
  property,
  editing,
  onChange,
}: {
  data: ProposalDetail;
  property: number | null;
  editing: boolean;
  onChange: (id: number | null) => void;
}) {
  const named = data.properties.find((p) => p.id === property);
  const unmatched = property === null;

  // The account of the match reads the same in both states, because it is
  // about how the record got here rather than about what it currently says.
  // AmendableRow shows either its value or its entry, so this sits inside both
  // rather than only in the one the operator is not looking at.
  const note = data.property_hint && (
    <span className="slip__match">
      read as “{data.property_hint}” · {data.reasoning || "no note"}
    </span>
  );

  return (
    <AmendableRow
      label="Property"
      editing={editing}
      htmlFor="slip-property"
      block
      value={
        <>
          <span className={unmatched ? "field__value--fault" : inkClass(false)}>
            {named?.nickname ?? "no property matched"}
          </span>
          {note}
        </>
      }
    >
      {/* An unmatched proposal is the one thing on this slip the operator has
          to fix before it can be filed, so the rule under it says so — the
          same red rule an invalid entry wears anywhere else in the app. */}
      <select
        id="slip-property"
        className={unmatched ? "entry entry--invalid" : "entry"}
        value={property ?? ""}
        onChange={(e) => onChange(e.target.value === "" ? null : Number(e.target.value))}
      >
        <option value="">no property matched</option>
        {data.properties.map((p) => (
          <option key={p.id} value={p.id}>
            {p.nickname} — {p.address}
          </option>
        ))}
      </select>
      {note}
    </AmendableRow>
  );
}

/**
 * The enclosure pane.
 *
 * A facsimile on a laptop, a tap on a phone. Loading a three-megabyte PDF into
 * an iframe the moment the screen opens is a slow screen, and the fields are
 * what the operator came to read.
 */
function Enclosures({ enclosures }: { enclosures: ProposalEnclosure[] }) {
  const wide = typeof window !== "undefined" && window.matchMedia("(min-width: 52rem)").matches;
  const [showing, setShowing] = useState(wide);

  const filed = enclosures.filter((e) => e.document_id !== null);

  return (
    <div className="slip__enclosure">
      {filed.length === 0 && (
        <p className="slip__encl-note">
          Nothing came attached. The reading is off the message itself.
        </p>
      )}

      {filed.map((enclosure) => {
        const href = `/api/v1/documents/${enclosure.document_id}/content`;
        return (
          <div key={enclosure.id} className="slip__encl">
            <div className="slip__encl-head">
              <span className="slip__encl-name">{enclosure.filename || "unnamed"}</span>
              <span className="slip__encl-size mono">{bytes(enclosure.size_bytes)}</span>
            </div>

            {showing ? (
              <Facsimile href={href} mime={enclosure.mime} name={enclosure.filename} />
            ) : (
              <button type="button" className="button slip__open" onClick={() => setShowing(true)}>
                Show it here
              </button>
            )}

            {/* Not a quiet button. On a laptop this is how a scanned lease
                gets read at a size you can check a figure against, and a word
                that only appears on hover is a word nobody finds. */}
            <a className="button slip__open" href={href} target="_blank" rel="noreferrer">
              Open the enclosure
            </a>
          </div>
        );
      })}
    </div>
  );
}

/** The document itself, rendered the way its type wants to be rendered. */
function Facsimile({ href, mime, name }: { href: string; mime: string; name: string }) {
  if (mime.startsWith("image/")) {
    return (
      <div className="slip__facsimile">
        <img src={href} alt={name || "the enclosure"} />
      </div>
    );
  }
  // The handler serves this sandboxed, with nosniff and a CSP that allows
  // nothing, so an uploaded HTML or SVG cannot run script on this origin.
  return <iframe className="slip__facsimile" src={href} title={name || "the enclosure"} />;
}

/**
 * How sure the model was, and what read it.
 *
 * A margin mark rather than a stamp: the stamp says where a thing stands, and
 * this is a property of the reading. Giving it a stamp would make the one bold
 * mark on a card stop meaning one thing.
 */
function Reading({ data }: { data: ProposalDetail }) {
  const confidence = data.confidence;
  const weak = confidence !== null && confidence < 0.7;

  return (
    <>
      <p className={weak ? "slip__reading slip__reading--weak" : "slip__reading"}>
        read {confidence === null ? DASH : <span className="mono">{confidence.toFixed(2)}</span>}
        {confidence !== null && " confidence"}
      </p>
      <p className="slip__provenance">
        {data.llm_model || "unknown model"} ·{" "}
        <span className="mono">{data.prompt_tokens + data.completion_tokens}</span> tokens
      </p>
    </>
  );
}

/** The bold word at the top: who this document is about. */
function mark(data: ProposalDetail): string {
  for (const field of ["vendor_name", "carrier", "lender"]) {
    const value = data.payload[field];
    if (typeof value === "string" && value.trim() !== "") return value;
  }
  return data.from_addr || "Unread";
}

/** The read face of one field, formatted the way its type is read. */
function read(data: ProposalDetail, field: FieldSpec): string {
  const value = data.payload[field.key];
  switch (field.as) {
    case "money":
      return typeof value === "number" && value !== 0 ? money(value) : DASH;
    case "date":
      return typeof value === "string" && value !== "" ? calendarDate(value) : DASH;
    case "count":
      return typeof value === "number" && value !== 0 ? String(value) : DASH;
    default:
      return typeof value === "string" && value !== "" ? value : DASH;
  }
}

/** The draft face of one field: a string, because that is what an input holds. */
function seedDraft(data: ProposalDetail): Record<string, string> {
  const draft: Record<string, string> = {};
  for (const field of FIELDS[data.kind]) {
    const value = data.payload[field.key];
    if (field.as === "money") {
      draft[field.key] = typeof value === "number" && value !== 0 ? (value / 100).toFixed(2) : "";
    } else if (field.as === "count") {
      draft[field.key] = typeof value === "number" && value !== 0 ? String(value) : "";
    } else {
      draft[field.key] = typeof value === "string" ? value : "";
    }
  }
  return draft;
}

/** Whether a person has changed this field since the model typed it. */
function touched(
  data: ProposalDetail,
  draft: Record<string, string> | null,
  field: FieldSpec,
): boolean {
  if (!draft) return false;
  return (draft[field.key] ?? "") !== (seedDraft(data)[field.key] ?? "");
}

/**
 * Carbon for what the machine typed, graphite for what a person has vouched
 * for. One declaration each, the discipline the stamp keeps.
 */
function inkClass(changed: boolean): string {
  return changed ? "field__value--touched" : "field__value--carbon";
}
