import { useQuery, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import { useInvalidating } from "./mutations";
import type {
  InsuranceList,
  MortgageList,
  Proposal,
  ProposalDetail,
  ProposalPage,
  ProposalWrite,
} from "./types";

/**
 * The review queue.
 *
 * Polled, like the mail room and the dispatch book, because it fills without
 * the operator doing anything: a forwarded receipt arrives, a worker reads it,
 * and a line appears. A queue that only refreshes when you reload is a queue
 * you stop trusting.
 */
export function useReviewQueue(status = "pending"): UseQueryResult<ProposalPage, Error> {
  return useQuery({
    queryKey: keys.review(status),
    queryFn: ({ signal }) =>
      request<ProposalPage>(`/api/v1/review?status=${encodeURIComponent(status)}`, { signal }),
    refetchInterval: 15_000,
  });
}

export function useProposal(id: number): UseQueryResult<ProposalDetail, Error> {
  return useQuery({
    queryKey: keys.proposal(id),
    queryFn: ({ signal }) => request<ProposalDetail>(`/api/v1/review/${id}`, { signal }),
    enabled: id > 0,
  });
}

/** Corrects what the model got wrong, before anybody agrees to it. */
export function useUpdateProposal(id: number): UseMutationResult<Proposal, Error, ProposalWrite> {
  return useInvalidating(
    (body: ProposalWrite) => request<Proposal>(`/api/v1/review/${id}`, { method: "PATCH", body }),
    [keys.proposal(id), keys.allReview],
  );
}

/**
 * Files the proposal.
 *
 * The property the entry lands on is invalidated too, and by its whole key
 * rather than one collection's: an approval can write a ledger entry, a lease,
 * a policy or a mortgage statement, and which of those it was is the server's
 * answer rather than something the call site knows in advance.
 */
export function useApproveProposal(id: number): UseMutationResult<Proposal, Error, void> {
  return useInvalidating(
    () => request<Proposal>(`/api/v1/review/${id}/approve`, { method: "POST" }),
    [keys.proposal(id), keys.allReview, keys.properties, keys.intake, keys.emailMessages],
  );
}

/** Records that somebody looked and said no. The row stays. */
export function useRejectProposal(id: number): UseMutationResult<Proposal, Error, void> {
  return useInvalidating(
    () => request<Proposal>(`/api/v1/review/${id}/reject`, { method: "POST" }),
    [keys.proposal(id), keys.allReview, keys.intake, keys.emailMessages],
  );
}

/**
 * How many proposals are waiting, for the count beside the nav link.
 *
 * It shares the queue's key rather than asking a second endpoint: the list
 * already carries the tally, and a badge that disagrees with the screen it
 * points at is worse than no badge.
 */
export function usePendingCount(): number {
  const queue = useReviewQueue();
  return queue.data?.counts.pending ?? 0;
}

export function useInsurance(propertyId: number): UseQueryResult<InsuranceList, Error> {
  return useQuery({
    queryKey: keys.insurance(propertyId),
    queryFn: ({ signal }) =>
      request<InsuranceList>(`/api/v1/properties/${propertyId}/insurance`, { signal }),
    enabled: propertyId > 0,
  });
}

export function useMortgages(propertyId: number): UseQueryResult<MortgageList, Error> {
  return useQuery({
    queryKey: keys.mortgages(propertyId),
    queryFn: ({ signal }) =>
      request<MortgageList>(`/api/v1/properties/${propertyId}/mortgage`, { signal }),
    enabled: propertyId > 0,
  });
}
