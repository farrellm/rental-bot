import { useQuery, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";

import { request } from "./client";
import { keys, type LedgerFilter } from "./keys";
import { useInvalidating } from "./mutations";
import type { Transaction, TransactionPage, TransactionWrite } from "./types";

function ledgerQuery(filter: LedgerFilter): string {
  const params = new URLSearchParams();
  if (filter.from) params.set("from", filter.from);
  if (filter.to) params.set("to", filter.to);
  if (filter.category) params.set("category", filter.category);
  const query = params.toString();
  return query ? `?${query}` : "";
}

export function useTransactions(
  propertyId: number,
  filter: LedgerFilter,
): UseQueryResult<TransactionPage, Error> {
  return useQuery({
    queryKey: keys.transactions(propertyId, filter),
    queryFn: ({ signal }) =>
      request<TransactionPage>(
        `/api/v1/properties/${propertyId}/transactions${ledgerQuery(filter)}`,
        { signal },
      ),
    enabled: propertyId > 0,
  });
}

export function useCreateTransaction(
  propertyId: number,
): UseMutationResult<Transaction, Error, TransactionWrite> {
  return useInvalidating(
    (body: TransactionWrite) =>
      request<Transaction>(`/api/v1/properties/${propertyId}/transactions`, {
        method: "POST",
        body,
      }),
    [keys.allTransactions(propertyId)],
  );
}

export function useUpdateTransaction(
  propertyId: number,
): UseMutationResult<Transaction, Error, { id: number; body: TransactionWrite }> {
  return useInvalidating(
    ({ id, body }: { id: number; body: TransactionWrite }) =>
      request<Transaction>(`/api/v1/transactions/${id}`, { method: "PATCH", body }),
    [keys.allTransactions(propertyId)],
  );
}

export function useDeleteTransaction(propertyId: number): UseMutationResult<void, Error, number> {
  return useInvalidating(
    (id: number) => request<void>(`/api/v1/transactions/${id}`, { method: "DELETE" }),
    [keys.allTransactions(propertyId)],
  );
}
