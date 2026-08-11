import { Link } from "react-router";

import { fileNumber, oneLineAddress, plural } from "../format";
import type { PropertyListItem } from "../api/types";
import { Stamp } from "./Stamp";

/**
 * One property in the drawer.
 *
 * It carries what you need to pick a record out of a stack and nothing more:
 * the name, where it is, how many units, and its standing. The file number is
 * the record's own id, which is the thing you would say out loud to name it.
 */
export function IndexCard({ property }: { property: PropertyListItem }) {
  return (
    <Link to={`/properties/${property.id}`} className="card card--index">
      <header className="index__head">
        <h2 className="index__name">{property.nickname}</h2>
        <span className="index__no mono">{fileNumber(property.id)}</span>
      </header>

      {/* No postal code: an index card carries what you need to pick a record
          out of a stack, and the ZIP is not part of that. */}
      <p className="index__address mono">
        {oneLineAddress({
          address_line1: property.address_line1,
          city: property.city,
          state: property.state,
        })}
      </p>

      <footer className="index__foot">
        <span className="index__units mono">{plural(property.unit_count, "unit", "units")}</span>
        <Stamp state={property.status} small />
      </footer>
    </Link>
  );
}
