import { useQuery, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import { useInvalidating } from "./mutations";
import type { Vendor, VendorList } from "./types";

export function useVendors(): UseQueryResult<VendorList, Error> {
  return useQuery({
    queryKey: keys.vendors,
    queryFn: ({ signal }) => request<VendorList>("/api/v1/vendors", { signal }),
  });
}

export function useCreateVendor(): UseMutationResult<
  Vendor,
  Error,
  { name: string; trade?: string; phone?: string }
> {
  return useInvalidating(
    (body: { name: string; trade?: string; phone?: string }) =>
      request<Vendor>("/api/v1/vendors", { method: "POST", body }),
    [keys.vendors],
  );
}
