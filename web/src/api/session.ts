import { useQuery, type UseQueryResult } from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import type { User } from "./types";

export function fetchMe(signal?: AbortSignal): Promise<User> {
  return request<User>("/api/v1/auth/me", { signal });
}

export function signIn(username: string, password: string): Promise<User> {
  return request<User>("/api/v1/auth/login", {
    method: "POST",
    body: { username, password },
  });
}

export function signOut(): Promise<void> {
  return request<void>("/api/v1/auth/logout", { method: "POST" });
}

export function useMe(): UseQueryResult<User, Error> {
  return useQuery({
    queryKey: keys.me,
    queryFn: ({ signal }) => fetchMe(signal),
    // A 401 here is the answer, not a failure worth retrying.
    retry: false,
    staleTime: 5 * 60 * 1000,
  });
}
