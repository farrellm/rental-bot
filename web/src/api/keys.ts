/**
 * Every query key, declared once.
 *
 * A mutation has to invalidate exactly what it changed. A property write
 * touches both the index and the detail, and getting that wrong shows the
 * operator a card that disagrees with what they just saved — so the keys are
 * spelled here and nowhere else, and a call site that wants one asks for it by
 * name.
 */

/** The ledger's filter is part of its key, so each view caches separately. */
export interface LedgerFilter {
  from?: string;
  to?: string;
  category?: string;
}

export const keys = {
  me: ["me"] as const,
  status: ["status"] as const,
  properties: ["properties"] as const,
  property: (id: number) => ["properties", id] as const,

  // Per-property collections hang under the property's key, so a filtered
  // ledger view and the property it belongs to invalidate together.
  documents: (id: number) => ["properties", id, "documents"] as const,
  transactions: (id: number, filter: LedgerFilter) =>
    ["properties", id, "transactions", filter] as const,
  /**
   * Every filtered view of one property's ledger.
   *
   * A ledger write invalidates this rather than one filter's key, because an
   * entry dated last March belongs to a range the operator may be looking at
   * right now.
   */
  allTransactions: (id: number) => ["properties", id, "transactions"] as const,
  repairs: (id: number) => ["properties", id, "repairs"] as const,
  repair: (id: number) => ["repairs", id] as const,
  leases: (id: number) => ["properties", id, "leases"] as const,
  lease: (id: number) => ["leases", id] as const,

  // Portfolio-wide, because one plumber works on several houses.
  tenants: ["tenants"] as const,
  vendors: ["vendors"] as const,

  // The mail room. Both are polled, because both change without the operator
  // doing anything.
  intake: ["intake"] as const,
  emailMessages: ["intake", "messages"] as const,

  // The channel alerts go out on, and the register of what went out. Polled
  // for the same reason the mail room is: both change without the operator
  // doing anything.
  channel: ["channel"] as const,
  notices: ["channel", "notices"] as const,

  // The review queue. The status is part of the key, because the register and
  // the "what was decided" view are different pages of different rows;
  // allReview is what a settlement invalidates, since a proposal leaving
  // pending changes both.
  allReview: ["review"] as const,
  review: (status: string) => ["review", status] as const,
  proposal: (id: number) => ["review", "proposal", id] as const,

  // What an applied proposal wrote. Per-property, under the property's key,
  // like every other collection that hangs off one.
  insurance: (id: number) => ["properties", id, "insurance"] as const,
  mortgages: (id: number) => ["properties", id, "mortgage"] as const,
};
