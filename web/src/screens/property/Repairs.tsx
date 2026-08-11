import { useState } from "react";
import { useParams } from "react-router";

import {
  describeError,
  useAddRepairEvent,
  useCreateRepair,
  useRepair,
  useRepairs,
  useUpdateRepair,
  type Repair,
  type RepairStatus,
} from "../../api";
import { FieldRow } from "../../components/FieldRow";
import { Select } from "../../components/Select";
import { SheetField, SheetFilter } from "../../components/SheetField";
import { SheetForm } from "../../components/SheetForm";
import { Stamp, type StampState } from "../../components/Stamp";
import {
  calendarDate,
  DASH,
  isCalendarDate,
  money,
  parseMoney,
  timestamp,
  today,
} from "../../format";

const STATUSES: RepairStatus[] = ["open", "scheduled", "in_progress", "done", "wontfix"];

/** The stamp says these words; the wire keeps the underscored name. */
const STATUS_STAMP: Record<RepairStatus, StampState> = {
  open: "open",
  scheduled: "scheduled",
  in_progress: "in-progress",
  done: "done",
  wontfix: "wontfix",
};

const STATUS_LABEL: Record<RepairStatus, string> = {
  open: "Open",
  scheduled: "Scheduled",
  in_progress: "In progress",
  done: "Done",
  wontfix: "Won't fix",
};

/**
 * The repairs docket.
 *
 * A repair is a job ticket: what is wrong, who is on it, what it cost, and a
 * dated history of what has happened to it. The history is numbered because it
 * genuinely is a sequence — quoted, then scheduled, then done, then paid — and
 * the order carries information the reader needs.
 */
export function Repairs() {
  const params = useParams();
  const propertyId = Number(params.id ?? 0);

  const repairs = useRepairs(propertyId);
  const createRepair = useCreateRepair(propertyId);

  const [openDocket, setOpenDocket] = useState<number | null>(null);
  const [adding, setAdding] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  return (
    <>
      {notice && <p className="card__notice">{notice}</p>}

      <div className="sheet">
        {repairs.isPending && <p className="waiting waiting--ink">Reading the docket</p>}
        {repairs.isError && <p className="hint hint--fault">{describeError(repairs.error)}</p>}

        {repairs.data &&
          (repairs.data.items.length === 0 ? (
            <p className="sheet__empty">
              Nothing on the docket. Open the first job when there is one.
            </p>
          ) : (
            <div className="dockets">
              {repairs.data.items.map((repair) => (
                <Docket
                  key={repair.id}
                  repair={repair}
                  propertyId={propertyId}
                  open={openDocket === repair.id}
                  onToggle={() => setOpenDocket(openDocket === repair.id ? null : repair.id)}
                  onError={setNotice}
                />
              ))}
            </div>
          ))}

        {adding ? (
          <RepairForm
            onCancel={() => setAdding(false)}
            onSubmit={async (body) => {
              setNotice(null);
              try {
                await createRepair.mutateAsync(body);
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
              Open a repair
            </button>
          </div>
        )}
      </div>
    </>
  );
}

interface DocketProps {
  repair: Repair;
  propertyId: number;
  open: boolean;
  onToggle: () => void;
  onError: (message: string) => void;
}

/**
 * One job ticket.
 *
 * Closed until you look at it: the docket line says what a person scanning the
 * list needs — the fault, its standing, and what it cost — and the history is
 * behind it, because a list of every event on every repair is not a list you
 * can scan.
 */
function Docket({ repair, propertyId, open, onToggle, onError }: DocketProps) {
  const detail = useRepair(open ? repair.id : 0);
  const update = useUpdateRepair(propertyId, repair.id);
  const addEvent = useAddRepairEvent(propertyId, repair.id);

  const [note, setNote] = useState("");
  const [saving, setSaving] = useState(false);

  async function recordEvent() {
    if (!note.trim() || saving) return;
    setSaving(true);
    try {
      await addEvent.mutateAsync({ note: note.trim() });
      setNote("");
    } catch (err) {
      onError(describeError(err));
    } finally {
      setSaving(false);
    }
  }

  async function setStatus(status: RepairStatus) {
    try {
      await update.mutateAsync({ status });
    } catch (err) {
      onError(describeError(err));
    }
  }

  const events = detail.data?.events ?? [];

  return (
    <article className="docket">
      <button
        type="button"
        className="docket__line"
        onClick={onToggle}
        aria-expanded={open}
        aria-controls={`docket-${repair.id}`}
      >
        <span className="docket__no mono">No. {String(repair.id).padStart(3, "0")}</span>
        <span className="docket__fault">{repair.description}</span>
        <span className="docket__cost mono">
          {money(repair.actual_cents ?? repair.estimate_cents)}
          {repair.actual_cents === null && repair.estimate_cents !== null && (
            <span className="docket__estimate stamped"> est</span>
          )}
        </span>
        <Stamp state={STATUS_STAMP[repair.status]} small />
      </button>

      {open && (
        <div className="docket__body" id={`docket-${repair.id}`}>
          <dl className="docket__facts">
            <FieldRow label="Opened">{calendarDate(repair.opened_on)}</FieldRow>
            <FieldRow label="Closed">{calendarDate(repair.closed_on)}</FieldRow>
            <FieldRow label="Trade">{repair.category || DASH}</FieldRow>
            <FieldRow label="Estimate">{money(repair.estimate_cents)}</FieldRow>
            <FieldRow label="Actual">{money(repair.actual_cents)}</FieldRow>
            <FieldRow label="Capital">{repair.is_capex ? "Yes" : "No"}</FieldRow>
          </dl>

          <div className="docket__standing">
            <SheetFilter label="Standing">
              <Select
                value={repair.status}
                onChange={(status) => void setStatus(status)}
                options={STATUSES}
                labels={STATUS_LABEL}
                short
              />
            </SheetFilter>
          </div>

          <h3 className="docket__eyebrow stamped">History</h3>
          {detail.isPending && <p className="waiting waiting--ink">Reading the history</p>}
          {events.length === 0 && !detail.isPending && (
            <p className="sheet__empty">Nothing recorded yet.</p>
          )}

          {events.length > 0 && (
            <ol className="timeline">
              {events.map((event, i) => (
                <li key={event.id} className="timeline__step">
                  {/* The number is the position in the sequence, which is what
                      a repair's history actually is. */}
                  <span className="timeline__no mono">{String(i + 1).padStart(2, "0")}</span>
                  <span className="timeline__at mono">{timestamp(event.at)}</span>
                  <span className="timeline__note">{event.note}</span>
                </li>
              ))}
            </ol>
          )}

          <div className="docket__record">
            <input
              className="entry"
              value={note}
              onChange={(e) => setNote(e.target.value)}
              placeholder="Quoted 285.00 by Ace Plumbing"
              aria-label="What happened"
              autoComplete="off"
            />
            <button
              type="button"
              className="button"
              onClick={() => void recordEvent()}
              disabled={saving || !note.trim()}
            >
              Record it
            </button>
          </div>
        </div>
      )}
    </article>
  );
}

interface RepairFormProps {
  onCancel: () => void;
  onSubmit: (body: {
    opened_on: string;
    description: string;
    category: string;
    estimate_cents: number | null;
  }) => Promise<void>;
}

function RepairForm({ onCancel, onSubmit }: RepairFormProps) {
  const [openedOn, setOpenedOn] = useState(today());
  const [description, setDescription] = useState("");
  const [category, setCategory] = useState("");
  const [estimate, setEstimate] = useState("");
  const [problem, setProblem] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  async function submit() {
    if (saving) return;

    if (!description.trim()) {
      setProblem("Say what is wrong.");
      return;
    }
    if (!isCalendarDate(openedOn)) {
      setProblem("Write the date as YYYY-MM-DD.");
      return;
    }
    const cents = parseMoney(estimate);
    if (cents === undefined) {
      setProblem("Write the estimate as a figure, like 285.00.");
      return;
    }

    setProblem(null);
    setSaving(true);
    try {
      // What a job costs is a magnitude, not a ledger entry. The signed
      // convention belongs to transactions.amount_cents, where the sign is the
      // only thing separating income from expense; a repair has no income side,
      // and "Estimate: -$650.00" would just read as a mistake.
      await onSubmit({
        opened_on: openedOn.trim(),
        description: description.trim(),
        category: category.trim(),
        estimate_cents: cents === null ? null : Math.abs(cents),
      });
    } finally {
      setSaving(false);
    }
  }

  return (
    <SheetForm
      title="New repair"
      problem={problem}
      saving={saving}
      submitLabel="Open the repair"
      onSubmit={() => void submit()}
      onCancel={onCancel}
    >
      <SheetField label="Opened">
        <input
          className="entry"
          value={openedOn}
          onChange={(e) => setOpenedOn(e.target.value)}
          placeholder="YYYY-MM-DD"
          inputMode="numeric"
          autoComplete="off"
        />
      </SheetField>

      <SheetField label="Trade">
        <input
          className="entry"
          value={category}
          onChange={(e) => setCategory(e.target.value)}
          placeholder="plumbing"
          autoComplete="off"
        />
      </SheetField>

      <SheetField label="Estimate">
        <input
          className="entry"
          value={estimate}
          onChange={(e) => setEstimate(e.target.value)}
          placeholder="285.00"
          inputMode="decimal"
          autoComplete="off"
        />
      </SheetField>

      <SheetField label="What is wrong" wide>
        <input
          className="entry"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          placeholder="Kitchen tap drips at the base"
          autoComplete="off"
        />
      </SheetField>
    </SheetForm>
  );
}
