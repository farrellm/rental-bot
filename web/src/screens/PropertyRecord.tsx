import { useEffect, useRef } from "react";
import { Link, NavLink, Outlet, useLocation, useParams } from "react-router";

import { describeError, useProperty } from "../api";
import { fileNumber, oneLineAddress } from "../format";
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
  const location = useLocation();
  const id = isNew ? 0 : Number(params.id ?? 0);
  const property = useProperty(id);

  // The strip is one scrolling row, so a section further along it can be off
  // the edge on arrival -- following a link straight to Documents would
  // otherwise show a strip that starts at Overview and says nothing about
  // where you are. Instant rather than smooth: this is the initial position of
  // the row, not a movement the reader asked for.
  //
  // Twice, because the first pass runs against the wrong geometry: on a cold
  // load it fires before the display face has loaded, the tabs are narrower
  // than they will be, and a tab that measured as visible slides back off the
  // edge the moment the real font arrives.
  //
  // `mounted` is in the dependencies because the strip does not exist on the
  // first render — the record is still loading and this component returns early
  // — so an effect keyed on the path alone found no nav to scroll and never ran
  // again once one appeared. That is the whole reason a deep link landed on a
  // strip showing Overview.
  const tabs = useRef<HTMLElement>(null);
  const mounted = isNew || Boolean(property.data);
  useEffect(() => {
    // Centred rather than merely on-screen: flush against an edge gives no sign
    // that the row continues past it.
    const reveal = () =>
      tabs.current
        ?.querySelector('[aria-current="page"]')
        ?.scrollIntoView({ block: "nearest", inline: "center" });

    reveal();
    let cancelled = false;
    void document.fonts.ready.then(() => {
      if (!cancelled) reveal();
    });
    return () => {
      cancelled = true;
    };
  }, [location.pathname, mounted]);

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
          <nav className="tabs" aria-label="Sections of this record" ref={tabs}>
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
            <NavLink to={`/properties/${id}/documents`} className={tabClass}>
              Documents
            </NavLink>
          </nav>
        )}

        <article className="card">
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
