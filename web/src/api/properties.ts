import {
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import { useInvalidating } from "./mutations";
import type { PropertyDetail, PropertyPage, PropertyWrite, Unit, UnitWrite } from "./types";

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
  return useInvalidating(
    (body: PropertyWrite) =>
      request<PropertyDetail>("/api/v1/properties", { method: "POST", body }),
    [keys.properties],
    (created) => client.setQueryData(keys.property(created.id), created),
  );
}

export function useUpdateProperty(
  id: number,
): UseMutationResult<PropertyDetail, Error, PropertyWrite> {
  const client = useQueryClient();
  return useInvalidating(
    (body: PropertyWrite) =>
      request<PropertyDetail>(`/api/v1/properties/${id}`, { method: "PATCH", body }),
    // The index shows the nickname, address, and status, any of which this
    // may have changed.
    [keys.properties],
    (updated) => client.setQueryData(keys.property(id), updated),
  );
}

export function useDeleteProperty(): UseMutationResult<void, Error, number> {
  const client = useQueryClient();
  return useInvalidating(
    (id: number) => request<void>(`/api/v1/properties/${id}`, { method: "DELETE" }),
    [keys.properties],
    (_result, id) => client.removeQueries({ queryKey: keys.property(id) }),
  );
}

/* Units -------------------------------------------------------------------- */

/**
 * Units live on the property detail, so every unit write invalidates it — and
 * the index too, because the index card shows a unit count.
 */
function unitKeys(propertyId: number) {
  return [keys.property(propertyId), keys.properties];
}

export function useCreateUnit(propertyId: number): UseMutationResult<Unit, Error, UnitWrite> {
  return useInvalidating(
    (body: UnitWrite) =>
      request<Unit>(`/api/v1/properties/${propertyId}/units`, { method: "POST", body }),
    unitKeys(propertyId),
  );
}

export function useUpdateUnit(
  propertyId: number,
): UseMutationResult<Unit, Error, { id: number; body: UnitWrite }> {
  return useInvalidating(
    ({ id, body }: { id: number; body: UnitWrite }) =>
      request<Unit>(`/api/v1/units/${id}`, { method: "PATCH", body }),
    unitKeys(propertyId),
  );
}

export function useDeleteUnit(propertyId: number): UseMutationResult<void, Error, number> {
  return useInvalidating(
    (id: number) => request<void>(`/api/v1/units/${id}`, { method: "DELETE" }),
    unitKeys(propertyId),
  );
}
