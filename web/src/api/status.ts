import { useQuery, type UseQueryResult } from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import type { Status } from "./types";

export function useStatus(): UseQueryResult<Status, Error> {
  return useQuery({
    queryKey: keys.status,
    queryFn: ({ signal }) => request<Status>("/api/v1/status", { signal }),
    refetchInterval: 10_000,
  });
}
