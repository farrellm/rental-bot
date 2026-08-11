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
import type { ChannelStanding, NoticePage, PairingCode } from "./types";

/**
 * The alert channel's standing.
 *
 * Polled, like the mailbox's, because everything it reports can change without
 * the operator doing anything: a pairing lands, a delivery fails, a condition
 * goes out. A stale answer here says alerts are arriving when they are not.
 */
export function useChannelStanding(): UseQueryResult<ChannelStanding, Error> {
  return useQuery({
    queryKey: keys.channel,
    queryFn: ({ signal }) => request<ChannelStanding>("/api/v1/telegram", { signal }),
    refetchInterval: 15_000,
  });
}

export function useNotices(): UseQueryResult<NoticePage, Error> {
  return useQuery({
    queryKey: keys.notices,
    queryFn: ({ signal }) => request<NoticePage>("/api/v1/notifications", { signal }),
    refetchInterval: 15_000,
  });
}

/**
 * Mints a pairing code.
 *
 * The code is in this response and nowhere else — only its hash is stored — so
 * the caller holds on to what it returns rather than expecting to read it back
 * off the standing.
 */
export function useIssuePairingCode(): UseMutationResult<PairingCode, Error, void> {
  return useInvalidating(
    () => request<PairingCode>("/api/v1/telegram/pairing-code", { method: "POST" }),
    [keys.channel],
  );
}

/**
 * Sends one notice, to prove the channel works.
 *
 * The register is invalidated after a beat: routine delivery goes through the
 * job queue, so refetching the instant this returns shows the same page back.
 * The same wait the sync button takes, for the same reason.
 */
export function useSendTestNotice(): UseMutationResult<{ sent: boolean }, Error, void> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<{ sent: boolean }>("/api/v1/telegram/test", { method: "POST" }),
    onSuccess: () => {
      window.setTimeout(() => {
        void client.invalidateQueries({ queryKey: keys.channel });
        void client.invalidateQueries({ queryKey: keys.notices });
      }, 1_500);
    },
  });
}
