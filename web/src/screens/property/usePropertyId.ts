import { useParams } from "react-router";

/**
 * The property this section belongs to.
 *
 * Every section of a record reads the same id off the same route parameter,
 * and every query built on it is guarded by `id > 0` — so a missing or
 * unparseable segment has to come back as 0 rather than NaN, which compares
 * false against everything and would leave a query neither enabled nor
 * disabled for a reason anyone could see.
 */
export function usePropertyId(): number {
  const params = useParams();
  const id = Number(params.id ?? 0);
  return Number.isFinite(id) ? id : 0;
}
