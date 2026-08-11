import { useState } from "react";
import { useParams } from "react-router";

import { describeError } from "../../api/client";
import {
  useAddLeaseTenant,
  useCreateLease,
  useCreateTenant,
  useLease,
  useLeases,
  useProperty,
  useTenants,
  useUpdateLease,
} from "../../api/queries";
import type { Lease, LeaseStatus } from "../../api/types";
import { Stamp, type StampState } from "../../components/Stamp";
import { calendarDate, DASH, isCalendarDate, money, parseMoney, today } from "../../format";
import { TermRule } from "./TermRule";

const STATUSES: LeaseStatus[] = ["pending", "active", "ended", "terminated"];

const STATUS_LABEL: Record<LeaseStatus, string> = {
  pending: "Pending",
  active: "Active",
  ended: "Ended",
  terminated: "Terminated",
};

/**
 * The lease abstracts.
 *
 * Not the lease itself — that is a PDF in Documents — but the one-page summary
 * a manager keeps clipped to the front of it: who, which unit, for how long, at
 * what rent. The term is drawn rather than written, because a lease's most
 * characteristic fact is that it runs out, and a date does not say how soon.
 */
export function Leases() {
  const params = useParams();
  const propertyId = Number(params.id ?? 0);

  const leases = useLeases(propertyId);
  const property = useProperty(propertyId);
  const createLease = useCreateLease(propertyId);

  const [adding, setAdding] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  const units = property.data?.units ?? [];

  return (
    <>
      {notice && <p className="card__notice">{notice}</p>}

      <div className="sheet">
        {leases.isPending && <p className="waiting waiting--ink">Reading the leases</p>}
        {leases.isError && <p className="hint hint--fault">{describeError(leases.error)}</p>}

        {leases.data &&
          (leases.data.items.length === 0 ? (
            <p className="sheet__empty">No leases on file. Enter one when a unit is let.</p>
          ) : (
            <div className="abstracts">
              {leases.data.items.map((lease) => (
                <Abstract
                  key={lease.id}
                  lease={lease}
                  propertyId={propertyId}
                  onError={setNotice}
                />
              ))}
            </div>
          ))}

        {adding ? (
          <LeaseForm
            units={units.map((u) => ({ id: u.id, label: u.label }))}
            onCancel={() => setAdding(false)}
            onSubmit={async (body) => {
              setNotice(null);
              try {
                await createLease.mutateAsync(body);
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
              disabled={units.length === 0}
            >
              Enter a lease
            </button>
          </div>
        )}
      </div>
    </>
  );
}

interface AbstractProps {
  lease: Lease;
  propertyId: number;
  onError: (message: string) => void;
}

function Abstract({ lease, propertyId, onError }: AbstractProps) {
  const [open, setOpen] = useState(false);
  const detail = useLease(open ? lease.id : 0);
  const update = useUpdateLease(propertyId, lease.id);

  async function setStatus(status: LeaseStatus) {
    try {
      await update.mutateAsync({ status });
    } catch (err) {
      onError(describeError(err));
    }
  }

  const tenants = detail.data?.tenants ?? [];

  return (
    <article className="abstract">
      <header className="abstract__head">
        <div className="abstract__who">
          <h3 className="abstract__unit">{lease.unit_label}</h3>
          <p className="abstract__rent mono">
            {money(lease.rent_cents)}
            <span className="abstract__per stamped"> a month</span>
          </p>
        </div>
        <Stamp state={lease.status as StampState} small />
      </header>

      <TermRule start={lease.start_date} end={lease.end_date} status={lease.status} />

      <button
        type="button"
        className="abstract__more"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
      >
        {open ? "Less" : "More"}
      </button>

      {open && (
        <div className="abstract__body">
          <dl className="docket__facts">
            <Fact label="Term">
              {calendarDate(lease.start_date)} to {lease.end_date ?? "month to month"}
            </Fact>
            <Fact label="Deposit">{money(lease.deposit_cents)}</Fact>
            <Fact label="Rent due">{lease.due_day ? `Day ${lease.due_day}` : DASH}</Fact>
            <Fact label="Late fee">{money(lease.late_fee_cents)}</Fact>
            <Fact label="Tenants">
              {detail.isPending
                ? "…"
                : tenants.length === 0
                  ? "None recorded"
                  : tenants.map((t) => `${t.name} (${t.role})`).join(", ")}
            </Fact>
          </dl>

          <div className="abstract__actions">
            <label className="sheet__filter">
              <span className="sheet__filter-label stamped">Standing</span>
              <select
                className="entry entry--short"
                value={lease.status}
                onChange={(e) => void setStatus(e.target.value as LeaseStatus)}
              >
                {STATUSES.map((s) => (
                  <option key={s} value={s}>
                    {STATUS_LABEL[s]}
                  </option>
                ))}
              </select>
            </label>

            <AddTenant propertyId={propertyId} leaseId={lease.id} onError={onError} />
          </div>
        </div>
      )}
    </article>
  );
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="field">
      <dt className="field__label stamped">{label}</dt>
      <dd className="field__value mono">{children}</dd>
    </div>
  );
}

/**
 * Putting a name on a lease.
 *
 * Tenants are portfolio-wide, so this picks an existing one or makes a new one
 * in the same control: a person who moves between units is the same person, and
 * asking the operator to go and create them elsewhere first would guarantee
 * duplicates.
 */
function AddTenant({
  propertyId,
  leaseId,
  onError,
}: {
  propertyId: number;
  leaseId: number;
  onError: (message: string) => void;
}) {
  const tenants = useTenants();
  const createTenant = useCreateTenant();
  const addTenant = useAddLeaseTenant(propertyId, leaseId);

  const [name, setName] = useState("");
  const [saving, setSaving] = useState(false);

  async function add() {
    const trimmed = name.trim();
    if (!trimmed || saving) return;

    setSaving(true);
    try {
      const existing = tenants.data?.items.find(
        (t) => t.name.toLowerCase() === trimmed.toLowerCase(),
      );
      const tenant = existing ?? (await createTenant.mutateAsync({ name: trimmed }));
      await addTenant.mutateAsync({ tenant_id: tenant.id, role: "primary" });
      setName("");
    } catch (err) {
      onError(describeError(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="abstract__tenant">
      <label className="sheet__filter">
        <span className="sheet__filter-label stamped">Add a tenant</span>
        <input
          className="entry"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="Dana Reyes"
          list="tenant-names"
          autoComplete="off"
        />
      </label>
      <datalist id="tenant-names">
        {(tenants.data?.items ?? []).map((t) => (
          <option key={t.id} value={t.name} />
        ))}
      </datalist>
      <button
        type="button"
        className="button"
        onClick={() => void add()}
        disabled={saving || !name.trim()}
      >
        Add
      </button>
    </div>
  );
}

interface LeaseFormProps {
  units: { id: number; label: string }[];
  onCancel: () => void;
  onSubmit: (body: {
    unit_id: number;
    start_date: string;
    end_date: string | null;
    rent_cents: number;
    deposit_cents: number | null;
    due_day: number | null;
    status: LeaseStatus;
  }) => Promise<void>;
}

function LeaseForm({ units, onCancel, onSubmit }: LeaseFormProps) {
  const [unitId, setUnitId] = useState(units[0]?.id ?? 0);
  const [startDate, setStartDate] = useState(today());
  const [endDate, setEndDate] = useState("");
  const [rent, setRent] = useState("");
  const [deposit, setDeposit] = useState("");
  const [dueDay, setDueDay] = useState("1");
  const [status, setStatus] = useState<LeaseStatus>("active");
  const [problem, setProblem] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  async function submit() {
    if (saving) return;

    if (!isCalendarDate(startDate)) {
      setProblem("Write the start date as YYYY-MM-DD.");
      return;
    }
    if (endDate.trim() && !isCalendarDate(endDate)) {
      setProblem("Write the end date as YYYY-MM-DD, or leave it empty for month to month.");
      return;
    }
    const rentCents = parseMoney(rent);
    if (rentCents === undefined || rentCents === null) {
      setProblem("Write the rent as a figure, like 1450.00.");
      return;
    }
    const depositCents = parseMoney(deposit);
    if (depositCents === undefined) {
      setProblem("Write the deposit as a figure, like 1450.00.");
      return;
    }
    const day = dueDay.trim() === "" ? null : Number(dueDay);
    if (day !== null && (!Number.isInteger(day) || day < 1 || day > 31)) {
      setProblem("The rent due day is a day of the month, 1 to 31.");
      return;
    }

    setProblem(null);
    setSaving(true);
    try {
      await onSubmit({
        unit_id: unitId,
        start_date: startDate.trim(),
        // Empty is month to month, which is an open end and not a missing one.
        end_date: endDate.trim() || null,
        rent_cents: rentCents,
        deposit_cents: depositCents,
        due_day: day,
        status,
      });
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="sheet__form">
      <h3 className="sheet__eyebrow stamped">New lease</h3>

      <div className="sheet__form-rows">
        <label className="sheet__field">
          <span className="field__label stamped">Unit</span>
          <select
            className="entry"
            value={unitId}
            onChange={(e) => setUnitId(Number(e.target.value))}
          >
            {units.map((u) => (
              <option key={u.id} value={u.id}>
                {u.label}
              </option>
            ))}
          </select>
        </label>

        <label className="sheet__field">
          <span className="field__label stamped">Starts</span>
          <input
            className="entry"
            value={startDate}
            onChange={(e) => setStartDate(e.target.value)}
            placeholder="YYYY-MM-DD"
            inputMode="numeric"
            autoComplete="off"
          />
        </label>

        <label className="sheet__field">
          <span className="field__label stamped">Ends</span>
          <input
            className="entry"
            value={endDate}
            onChange={(e) => setEndDate(e.target.value)}
            placeholder="Month to month"
            inputMode="numeric"
            autoComplete="off"
          />
        </label>

        <label className="sheet__field">
          <span className="field__label stamped">Rent</span>
          <input
            className="entry"
            value={rent}
            onChange={(e) => setRent(e.target.value)}
            placeholder="1450.00"
            inputMode="decimal"
            autoComplete="off"
          />
        </label>

        <label className="sheet__field">
          <span className="field__label stamped">Deposit</span>
          <input
            className="entry"
            value={deposit}
            onChange={(e) => setDeposit(e.target.value)}
            placeholder="1450.00"
            inputMode="decimal"
            autoComplete="off"
          />
        </label>

        <label className="sheet__field">
          <span className="field__label stamped">Rent due</span>
          <input
            className="entry"
            value={dueDay}
            onChange={(e) => setDueDay(e.target.value)}
            placeholder="1"
            inputMode="numeric"
            autoComplete="off"
          />
        </label>

        <label className="sheet__field">
          <span className="field__label stamped">Standing</span>
          <select
            className="entry"
            value={status}
            onChange={(e) => setStatus(e.target.value as LeaseStatus)}
          >
            {STATUSES.map((s) => (
              <option key={s} value={s}>
                {STATUS_LABEL[s]}
              </option>
            ))}
          </select>
        </label>
      </div>

      {problem && <p className="hint hint--fault">{problem}</p>}

      <div className="actions">
        <button
          type="button"
          className="button button--primary"
          onClick={() => void submit()}
          disabled={saving}
        >
          {saving ? "Saving" : "Enter the lease"}
        </button>
        <button type="button" className="button" onClick={onCancel} disabled={saving}>
          Cancel
        </button>
      </div>
    </div>
  );
}
