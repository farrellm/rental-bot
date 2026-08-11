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
  ChannelStanding,
  ConnectResponse,
  Document,
  DocumentKind,
  DocumentLink,
  DocumentPage,
  EmailMessagePage,
  IntakeStanding,
  Lease,
  LeaseList,
  LeaseWrite,
  NoticePage,
  PairingCode,
  PropertyDetail,
  PropertyPage,
  PropertyWrite,
  Repair,
  RepairEvent,
  RepairList,
  RepairWrite,
  Status,
  Tenant,
  TenantList,
  TenantRole,
  Transaction,
  TransactionPage,
  TransactionWrite,
  Unit,
  UnitWrite,
  UploadedDocument,
  User,
  Vendor,
  VendorList,
} from "./types";

export const keys = {
  me: ["me"] as const,
  status: ["status"] as const,
  properties: ["properties"] as const,
  property: (id: number) => ["properties", id] as const,

  // Per-property collections hang under the property's key, so a filtered
  // ledger view and the property it belongs to invalidate together.
  documents: (id: number) => ["properties", id, "documents"] as const,
  transactions: (id: number, filter: LedgerFilter) =>
    ["properties", id, "transactions", filter] as const,
  repairs: (id: number) => ["properties", id, "repairs"] as const,
  repair: (id: number) => ["repairs", id] as const,
  leases: (id: number) => ["properties", id, "leases"] as const,
  lease: (id: number) => ["leases", id] as const,

  // Portfolio-wide, because one plumber works on several houses.
  tenants: ["tenants"] as const,
  vendors: ["vendors"] as const,

  // The mail room. Both are polled, because both change without the operator
  // doing anything.
  intake: ["intake"] as const,
  emailMessages: ["intake", "messages"] as const,

  // The channel alerts go out on, and the register of what went out. Polled
  // for the same reason the mail room is: both change without the operator
  // doing anything.
  channel: ["channel"] as const,
  notices: ["channel", "notices"] as const,
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

/* Documents ---------------------------------------------------------------- */

/**
 * Uploads one document.
 *
 * FormData rather than JSON, and no Content-Type header: the browser sets it
 * with the multipart boundary, and naming it ourselves would corrupt the body.
 * The CSRF header still applies, so this goes through `request` like the rest.
 */
export function useUploadDocument(
  propertyId: number,
): UseMutationResult<UploadedDocument, Error, DocumentUpload> {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (upload: DocumentUpload) => {
      const form = new FormData();
      form.append("file", upload.file);
      form.append("kind", upload.kind);
      if (upload.title) form.append("title", upload.title);
      if (upload.propertyId) form.append("property_id", String(upload.propertyId));
      if (upload.link) {
        form.append("entity_type", upload.link.entity_type);
        form.append("entity_id", String(upload.link.entity_id));
      }
      return request<UploadedDocument>("/api/v1/documents", { method: "POST", body: form });
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.documents(propertyId) });
    },
  });
}

export interface DocumentUpload {
  file: File;
  kind: DocumentKind;
  title?: string;
  propertyId?: number;
  link?: DocumentLink;
}

export function useDocuments(propertyId: number): UseQueryResult<DocumentPage, Error> {
  return useQuery({
    queryKey: keys.documents(propertyId),
    queryFn: ({ signal }) =>
      request<DocumentPage>(`/api/v1/properties/${propertyId}/documents`, { signal }),
    enabled: propertyId > 0,
  });
}

export function useUpdateDocument(propertyId: number) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ id, body }: { id: number; body: Partial<Document> }) =>
      request<Document>(`/api/v1/documents/${id}`, { method: "PATCH", body }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.documents(propertyId) });
    },
  });
}

export function useDeleteDocument(propertyId: number) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => request<void>(`/api/v1/documents/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.documents(propertyId) });
    },
  });
}

/* The ledger --------------------------------------------------------------- */

export interface LedgerFilter {
  from?: string;
  to?: string;
  category?: string;
}

function ledgerQuery(filter: LedgerFilter): string {
  const params = new URLSearchParams();
  if (filter.from) params.set("from", filter.from);
  if (filter.to) params.set("to", filter.to);
  if (filter.category) params.set("category", filter.category);
  const query = params.toString();
  return query ? `?${query}` : "";
}

export function useTransactions(
  propertyId: number,
  filter: LedgerFilter,
): UseQueryResult<TransactionPage, Error> {
  return useQuery({
    queryKey: keys.transactions(propertyId, filter),
    queryFn: ({ signal }) =>
      request<TransactionPage>(
        `/api/v1/properties/${propertyId}/transactions${ledgerQuery(filter)}`,
        { signal },
      ),
    enabled: propertyId > 0,
  });
}

/**
 * A ledger write invalidates every filtered view of that property's ledger,
 * because an entry dated last March belongs to a range the operator may be
 * looking at right now.
 */
function useLedgerMutation<TArgs>(propertyId: number, call: (args: TArgs) => Promise<unknown>) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: call,
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["properties", propertyId, "transactions"] });
    },
  });
}

export function useCreateTransaction(propertyId: number) {
  return useLedgerMutation(propertyId, (body: TransactionWrite) =>
    request<Transaction>(`/api/v1/properties/${propertyId}/transactions`, {
      method: "POST",
      body,
    }),
  );
}

export function useUpdateTransaction(propertyId: number) {
  return useLedgerMutation(propertyId, ({ id, body }: { id: number; body: TransactionWrite }) =>
    request<Transaction>(`/api/v1/transactions/${id}`, { method: "PATCH", body }),
  );
}

export function useDeleteTransaction(propertyId: number) {
  return useLedgerMutation(propertyId, (id: number) =>
    request<void>(`/api/v1/transactions/${id}`, { method: "DELETE" }),
  );
}

/* Repairs ------------------------------------------------------------------ */

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

/** A repair write touches the docket list and the docket itself. */
function useRepairMutation<TArgs>(
  propertyId: number,
  repairId: number | null,
  call: (args: TArgs) => Promise<unknown>,
) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: call,
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.repairs(propertyId) });
      if (repairId !== null) {
        void client.invalidateQueries({ queryKey: keys.repair(repairId) });
      }
    },
  });
}

export function useCreateRepair(propertyId: number) {
  return useRepairMutation(propertyId, null, (body: RepairWrite) =>
    request<Repair>(`/api/v1/properties/${propertyId}/repairs`, { method: "POST", body }),
  );
}

export function useUpdateRepair(propertyId: number, repairId: number) {
  return useRepairMutation(propertyId, repairId, (body: RepairWrite) =>
    request<Repair>(`/api/v1/repairs/${repairId}`, { method: "PATCH", body }),
  );
}

export function useDeleteRepair(propertyId: number) {
  return useRepairMutation(propertyId, null, (id: number) =>
    request<void>(`/api/v1/repairs/${id}`, { method: "DELETE" }),
  );
}

export function useAddRepairEvent(propertyId: number, repairId: number) {
  return useRepairMutation(propertyId, repairId, (body: { at?: string; note: string }) =>
    request<RepairEvent>(`/api/v1/repairs/${repairId}/events`, { method: "POST", body }),
  );
}

/* Tenancy ------------------------------------------------------------------ */

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
function useLeaseMutation<TArgs>(
  propertyId: number,
  leaseId: number | null,
  call: (args: TArgs) => Promise<unknown>,
) {
  const client = useQueryClient();
  return useMutation({
    mutationFn: call,
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.leases(propertyId) });
      void client.invalidateQueries({ queryKey: keys.property(propertyId) });
      if (leaseId !== null) {
        void client.invalidateQueries({ queryKey: keys.lease(leaseId) });
      }
    },
  });
}

export function useCreateLease(propertyId: number) {
  return useLeaseMutation(propertyId, null, (body: LeaseWrite) =>
    request<Lease>(`/api/v1/properties/${propertyId}/leases`, { method: "POST", body }),
  );
}

export function useUpdateLease(propertyId: number, leaseId: number) {
  return useLeaseMutation(propertyId, leaseId, (body: LeaseWrite) =>
    request<Lease>(`/api/v1/leases/${leaseId}`, { method: "PATCH", body }),
  );
}

export function useDeleteLease(propertyId: number) {
  return useLeaseMutation(propertyId, null, (id: number) =>
    request<void>(`/api/v1/leases/${id}`, { method: "DELETE" }),
  );
}

export function useTenants(): UseQueryResult<TenantList, Error> {
  return useQuery({
    queryKey: keys.tenants,
    queryFn: ({ signal }) => request<TenantList>("/api/v1/tenants", { signal }),
  });
}

export function useCreateTenant() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: { name: string; email?: string; phone?: string }) =>
      request<Tenant>("/api/v1/tenants", { method: "POST", body }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.tenants });
    },
  });
}

export function useAddLeaseTenant(propertyId: number, leaseId: number) {
  return useLeaseMutation(propertyId, leaseId, (body: { tenant_id: number; role: TenantRole }) =>
    request<void>(`/api/v1/leases/${leaseId}/tenants`, { method: "POST", body }),
  );
}

export function useRemoveLeaseTenant(propertyId: number, leaseId: number) {
  return useLeaseMutation(propertyId, leaseId, (tenantId: number) =>
    request<void>(`/api/v1/leases/${leaseId}/tenants`, {
      method: "DELETE",
      body: { tenant_id: tenantId },
    }),
  );
}

/* Vendors ------------------------------------------------------------------ */

export function useVendors(): UseQueryResult<VendorList, Error> {
  return useQuery({
    queryKey: keys.vendors,
    queryFn: ({ signal }) => request<VendorList>("/api/v1/vendors", { signal }),
  });
}

export function useCreateVendor() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (body: { name: string; trade?: string; phone?: string }) =>
      request<Vendor>("/api/v1/vendors", { method: "POST", body }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.vendors });
    },
  });
}

/* Intake ------------------------------------------------------------------- */

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
    queryFn: ({ signal }) =>
      request<EmailMessagePage>("/api/v1/email-messages", { signal }),
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
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<void>("/api/v1/gmail", { method: "DELETE" }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.intake });
    },
  });
}

/**
 * Queues a sync now.
 *
 * The register is invalidated after a beat rather than immediately: the request
 * only queues the work, and refetching the instant it returns shows the same
 * page back. This is the one place in the app that waits on purpose.
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
  const client = useQueryClient();
  return useMutation({
    mutationFn: () => request<PairingCode>("/api/v1/telegram/pairing-code", { method: "POST" }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: keys.channel });
    },
  });
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
