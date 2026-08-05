import { Link, NavLink, Outlet, useParams } from "react-router";

import { describeError } from "../api/client";
import { useProperty } from "../api/queries";
import { fileNumber } from "../format";
import { Overview } from "./property/Overview";

/**
 * One property record, with its divider tabs.
 *
 * The head band and the tabs belong to the record; each section fills the
 * card's body. It is one folder — the same stock, the same width — and turning
 * to another section does not change the shape of what you are holding.
 *
 * A property that does not exist yet has nothing to hang tabs off, so
 * /properties/new renders the Overview card alone.
 */
export function PropertyRecord({ isNew = false }: { isNew?: boolean }) {
  const params = useParams();
  const id = isNew ? 0 : Number(params.id ?? 0);
  const property = useProperty(id);

  if (!isNew && property.isPending) {
    return (
      <main className="shell__main">
        <p className="waiting">Reading the record</p>
      </main>
    );
  }
  if (!isNew && property.isError) {
    return (
      <main className="shell__main">
        <div className="empty">
          <p className="empty__line">{describeError(property.error)}</p>
          <p className="empty__action">
            <Link to="/properties" className="button button--ground">
              Back to properties
            </Link>
          </p>
        </div>
      </main>
    );
  }

  const record = property.data;

  return (
    <main className="shell__main shell__main--single">
      <div className="record">
        {!isNew && (
          <nav className="tabs" aria-label="Sections of this record">
            <NavLink to={`/properties/${id}`} end className={tabClass}>
              Overview
            </NavLink>
            <NavLink to={`/properties/${id}/cash-flow`} className={tabClass}>
              Cash flow
            </NavLink>
            <NavLink to={`/properties/${id}/repairs`} className={tabClass}>
              Repairs
            </NavLink>
            <NavLink to={`/properties/${id}/leases`} className={tabClass}>
              Leases
            </NavLink>
          </nav>
        )}

        <article className="card stock">
          <header className="card__head">
            <div>
              {/* The head is what is on file. While a section is open for
                  amendment the entries show the draft and this does not:
                  the record and the change to it are different claims. */}
              <h1 className="card__mark stamped">
                {record?.nickname || (isNew ? "New property" : "Untitled")}
              </h1>
              <p className="record__address mono">{oneLineAddress(record)}</p>
            </div>
            <div className="card__file">
              <p className="card__eyebrow stamped">Property record</p>
              {!isNew && <p className="card__read mono">{fileNumber(id)}</p>}
            </div>
          </header>

          {isNew ? <Overview isNew /> : <Outlet />}
        </article>
      </div>
    </main>
  );
}

function tabClass({ isActive }: { isActive: boolean }): string {
  return isActive ? "tab stock" : "tab";
}

interface Addressable {
  address_line1: string;
  city: string;
  state: string;
  postal_code: string;
}

/** The address on one line, skipping the parts that were never filled in. */
function oneLineAddress(record: Addressable | undefined): string {
  if (!record) return "—";
  const region = [record.city, record.state, record.postal_code].filter(Boolean).join(" ");
  return [record.address_line1, region].filter(Boolean).join(", ") || "—";
}
