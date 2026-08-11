import { Link } from "react-router";

import { describeError, useProperties } from "../api";
import { IndexCard } from "../components/IndexCard";
import { plural } from "../format";

/** The drawer: every property on file, as a stack of index cards. */
export function Properties() {
  const properties = useProperties();

  if (properties.isPending) {
    return (
      <main className="shell__main">
        <p className="waiting">Reading the file</p>
      </main>
    );
  }

  if (properties.isError) {
    return (
      <main className="shell__main">
        <div className="empty">
          <p className="empty__line">{describeError(properties.error)}</p>
        </div>
      </main>
    );
  }

  const items = properties.data.items;

  return (
    <main className="shell__main">
      <div className="screen-head">
        <h1 className="screen-head__title">
          Properties
          {items.length > 0 && (
            <span className="screen-head__count mono">
              {" "}
              {plural(items.length, "record", "records")}
            </span>
          )}
        </h1>
        {items.length > 0 && (
          <Link to="/properties/new" className="button button--ground">
            Add property
          </Link>
        )}
      </div>

      {items.length === 0 ? (
        <div className="empty">
          <p className="empty__line">No properties on file yet.</p>
          <p className="empty__action">
            <Link to="/properties/new" className="button button--ground">
              Add the first one
            </Link>
          </p>
        </div>
      ) : (
        <div className="stack">
          {items.map((property) => (
            <IndexCard key={property.id} property={property} />
          ))}
        </div>
      )}
    </main>
  );
}
