import {
  useQuery,
  type QueryKey,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import { useInvalidating } from "./mutations";
import type { Repair, RepairEvent, RepairList, RepairWrite } from "./types";

export function useRepairs(propertyId: number): UseQueryResult<RepairList, Error> {
  return useQuery({
    queryKey: keys.repairs(propertyId),
    queryFn: ({ signal }) =>
      request<RepairList>(`/api/v1/properties/${propertyId}/repairs`, { signal }),
    enabled: propertyId > 0,
  });
}

export function useRepair(id: number): UseQueryResult<Repair, Error> {
  return useQuery({
    queryKey: keys.repair(id),
    queryFn: ({ signal }) => request<Repair>(`/api/v1/repairs/${id}`, { signal }),
    enabled: id > 0,
  });
}

/** A write against one repair touches the docket list and the docket itself. */
function docketKeys(propertyId: number, repairId?: number): QueryKey[] {
  const stale: QueryKey[] = [keys.repairs(propertyId)];
  if (repairId !== undefined) stale.push(keys.repair(repairId));
  return stale;
}

export function useCreateRepair(propertyId: number): UseMutationResult<Repair, Error, RepairWrite> {
  return useInvalidating(
    (body: RepairWrite) =>
      request<Repair>(`/api/v1/properties/${propertyId}/repairs`, { method: "POST", body }),
    docketKeys(propertyId),
  );
}

export function useUpdateRepair(
  propertyId: number,
  repairId: number,
): UseMutationResult<Repair, Error, RepairWrite> {
  return useInvalidating(
    (body: RepairWrite) =>
      request<Repair>(`/api/v1/repairs/${repairId}`, { method: "PATCH", body }),
    docketKeys(propertyId, repairId),
  );
}

export function useDeleteRepair(propertyId: number): UseMutationResult<void, Error, number> {
  return useInvalidating(
    (id: number) => request<void>(`/api/v1/repairs/${id}`, { method: "DELETE" }),
    docketKeys(propertyId),
  );
}

export function useAddRepairEvent(
  propertyId: number,
  repairId: number,
): UseMutationResult<RepairEvent, Error, { at?: string; note: string }> {
  return useInvalidating(
    (body: { at?: string; note: string }) =>
      request<RepairEvent>(`/api/v1/repairs/${repairId}/events`, { method: "POST", body }),
    docketKeys(propertyId, repairId),
  );
}
