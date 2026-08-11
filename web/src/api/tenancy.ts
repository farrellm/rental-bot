import {
  useQuery,
  type QueryKey,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import { useInvalidating } from "./mutations";
import type { Lease, LeaseList, LeaseWrite, Tenant, TenantList, TenantRole } from "./types";

export function useLeases(propertyId: number): UseQueryResult<LeaseList, Error> {
  return useQuery({
    queryKey: keys.leases(propertyId),
    queryFn: ({ signal }) =>
      request<LeaseList>(`/api/v1/properties/${propertyId}/leases`, { signal }),
    enabled: propertyId > 0,
  });
}

export function useLease(id: number): UseQueryResult<Lease, Error> {
  return useQuery({
    queryKey: keys.lease(id),
    queryFn: ({ signal }) => request<Lease>(`/api/v1/leases/${id}`, { signal }),
    enabled: id > 0,
  });
}

/**
 * A lease write also invalidates the property: occupancy is derived from these
 * dates, and the Overview tab reads it off the property detail.
 */
function tenancyKeys(propertyId: number, leaseId?: number): QueryKey[] {
  const stale: QueryKey[] = [keys.leases(propertyId), keys.property(propertyId)];
  if (leaseId !== undefined) stale.push(keys.lease(leaseId));
  return stale;
}

export function useCreateLease(propertyId: number): UseMutationResult<Lease, Error, LeaseWrite> {
  return useInvalidating(
    (body: LeaseWrite) =>
      request<Lease>(`/api/v1/properties/${propertyId}/leases`, { method: "POST", body }),
    tenancyKeys(propertyId),
  );
}

export function useUpdateLease(
  propertyId: number,
  leaseId: number,
): UseMutationResult<Lease, Error, LeaseWrite> {
  return useInvalidating(
    (body: LeaseWrite) => request<Lease>(`/api/v1/leases/${leaseId}`, { method: "PATCH", body }),
    tenancyKeys(propertyId, leaseId),
  );
}

export function useDeleteLease(propertyId: number): UseMutationResult<void, Error, number> {
  return useInvalidating(
    (id: number) => request<void>(`/api/v1/leases/${id}`, { method: "DELETE" }),
    tenancyKeys(propertyId),
  );
}

export function useTenants(): UseQueryResult<TenantList, Error> {
  return useQuery({
    queryKey: keys.tenants,
    queryFn: ({ signal }) => request<TenantList>("/api/v1/tenants", { signal }),
  });
}

export function useCreateTenant(): UseMutationResult<
  Tenant,
  Error,
  { name: string; email?: string; phone?: string }
> {
  return useInvalidating(
    (body: { name: string; email?: string; phone?: string }) =>
      request<Tenant>("/api/v1/tenants", { method: "POST", body }),
    [keys.tenants],
  );
}

export function useAddLeaseTenant(
  propertyId: number,
  leaseId: number,
): UseMutationResult<void, Error, { tenant_id: number; role: TenantRole }> {
  return useInvalidating(
    (body: { tenant_id: number; role: TenantRole }) =>
      request<void>(`/api/v1/leases/${leaseId}/tenants`, { method: "POST", body }),
    tenancyKeys(propertyId, leaseId),
  );
}

export function useRemoveLeaseTenant(
  propertyId: number,
  leaseId: number,
): UseMutationResult<void, Error, number> {
  return useInvalidating(
    (tenantId: number) =>
      request<void>(`/api/v1/leases/${leaseId}/tenants`, {
        method: "DELETE",
        body: { tenant_id: tenantId },
      }),
    tenancyKeys(propertyId, leaseId),
  );
}
