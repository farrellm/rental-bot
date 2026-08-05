/**
 * Typed calls and their query keys.
 *
 * Keys are declared once, here, so a mutation invalidates exactly what it
 * changed. A property write touches both the index and the detail, and getting
 * that wrong shows the operator a card that disagrees with what they just
 * saved.
 */

import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";

import { request } from "./client";
import type {
  PropertyDetail,
  PropertyPage,
  PropertyWrite,
  Status,
  Unit,
  UnitWrite,
  User,
} from "./types";

export const keys = {
  me: ["me"] as const,
  status: ["status"] as const,
  properties: ["properties"] as const,
  property: (id: number) => ["properties", id] as const,
};

/* Session ------------------------------------------------------------------ */

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

/* Status ------------------------------------------------------------------- */

export function useStatus(): UseQueryResult<Status, Error> {
  return useQuery({
    queryKey: keys.status,
    queryFn: ({ signal }) => request<Status>("/api/v1/status", { signal }),
    refetchInterval: 10_000,
  });
}

/* Properties --------------------------------------------------------------- */

export function useProperties(): UseQueryResult<PropertyPage, Error> {
  return useQuery({
    queryKey: keys.properties,
    queryFn: ({ signal }) => request<PropertyPage>("/api/v1/properties", { signal }),
  });
}

export function useProperty(id: number): UseQueryResult<PropertyDetail, Error> {
  return useQuery({
    queryKey: keys.property(id),
    queryFn: ({ signal }) => request<PropertyDetail>(`/api/v1/properties/${id}`, { signal }),
    enabled: Number.isFinite(id) && id > 0,
  });
}

export function useCreateProperty(): UseMutationResult<PropertyDetail, Error, PropertyWrite> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: PropertyWrite) =>
      request<PropertyDetail>("/api/v1/properties", { method: "POST", body }),
    onSuccess: (created) => {
      client.setQueryData(keys.property(created.id), created);
      void client.invalidateQueries({ queryKey: keys.properties });
    },
  });
}

export function useUpdateProperty(id: number): UseMutationResult<PropertyDetail, Error, PropertyWrite> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: PropertyWrite) =>
      request<PropertyDetail>(`/api/v1/properties/${id}`, { method: "PATCH", body }),
    onSuccess: (updated) => {
      client.setQueryData(keys.property(id), updated);
      // The index shows the nickname, address, and status, any of which this
      // may have changed.
      void client.invalidateQueries({ queryKey: keys.properties });
    },
  });
}

export function useDeleteProperty(): UseMutationResult<void, Error, number> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => request<void>(`/api/v1/properties/${id}`, { method: "DELETE" }),
    onSuccess: (_void, id) => {
      client.removeQueries({ queryKey: keys.property(id) });
      void client.invalidateQueries({ queryKey: keys.properties });
    },
  });
}

/* Units -------------------------------------------------------------------- */

/** Units live on the property detail, so every unit write invalidates it. */
function useUnitMutation<TArgs>(
  propertyId: number,
  call: (args: TArgs) => Promise<unknown>,
): UseMutationResult<unknown, Error, TArgs> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: call,
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.property(propertyId) });
      // The index card shows a unit count.
      void client.invalidateQueries({ queryKey: keys.properties });
    },
  });
}

export function useCreateUnit(propertyId: number) {
  return useUnitMutation(propertyId, (body: UnitWrite) =>
    request<Unit>(`/api/v1/properties/${propertyId}/units`, { method: "POST", body }),
  );
}

export function useUpdateUnit(propertyId: number) {
  return useUnitMutation(propertyId, ({ id, body }: { id: number; body: UnitWrite }) =>
    request<Unit>(`/api/v1/units/${id}`, { method: "PATCH", body }),
  );
}

export function useDeleteUnit(propertyId: number) {
  return useUnitMutation(propertyId, (id: number) =>
    request<void>(`/api/v1/units/${id}`, { method: "DELETE" }),
  );
}
