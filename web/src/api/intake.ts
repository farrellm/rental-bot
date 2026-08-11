import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import { useInvalidating } from "./mutations";
import type { ConnectResponse, EmailMessagePage, IntakeStanding } from "./types";

/**
 * The mailbox's standing.
 *
 * Polled, like the service record, because everything it reports can change
 * without the operator doing anything: a watch lapses, a grant is revoked, a
 * sync lands. A stale answer here says mail is arriving when it is not.
 */
export function useIntakeStanding(): UseQueryResult<IntakeStanding, Error> {
  return useQuery({
    queryKey: keys.intake,
    queryFn: ({ signal }) => request<IntakeStanding>("/api/v1/gmail", { signal }),
    refetchInterval: 15_000,
  });
}

export function useEmailMessages(): UseQueryResult<EmailMessagePage, Error> {
  return useQuery({
    queryKey: keys.emailMessages,
    queryFn: ({ signal }) => request<EmailMessagePage>("/api/v1/email-messages", { signal }),
    refetchInterval: 15_000,
  });
}

/** Starts the OAuth flow. The caller navigates to the URL this returns. */
export function useConnectGmail(): UseMutationResult<ConnectResponse, Error, void> {
  return useMutation({
    mutationFn: () => request<ConnectResponse>("/api/v1/gmail/connect", { method: "POST" }),
  });
}

export function useDisconnectGmail(): UseMutationResult<void, Error, void> {
  return useInvalidating(() => request<void>("/api/v1/gmail", { method: "DELETE" }), [keys.intake]);
}

/**
 * Queues a sync now.
 *
 * The register is invalidated after a beat rather than immediately: the request
 * only queues the work, and refetching the instant it returns shows the same
 * page back. This is one of the two places in the app that waits on purpose.
 */
export function useSyncNow(): UseMutationResult<{ queued: boolean }, Error, void> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<{ queued: boolean }>("/api/v1/gmail/sync", { method: "POST" }),
    onSuccess: () => {
      window.setTimeout(() => {
        void client.invalidateQueries({ queryKey: keys.intake });
        void client.invalidateQueries({ queryKey: keys.emailMessages });
      }, 1_500);
    },
  });
}
