import { useQuery, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";

import { request } from "./client";
import { keys } from "./keys";
import { useInvalidating } from "./mutations";
import type { Document, DocumentKind, DocumentLink, DocumentPage, UploadedDocument } from "./types";

export interface DocumentUpload {
  file: File;
  kind: DocumentKind;
  title?: string;
  propertyId?: number;
  link?: DocumentLink;
}

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
  return useInvalidating(
    (upload: DocumentUpload) => {
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
    [keys.documents(propertyId)],
  );
}

export function useDocuments(propertyId: number): UseQueryResult<DocumentPage, Error> {
  return useQuery({
    queryKey: keys.documents(propertyId),
    queryFn: ({ signal }) =>
      request<DocumentPage>(`/api/v1/properties/${propertyId}/documents`, { signal }),
    enabled: propertyId > 0,
  });
}

export function useUpdateDocument(
  propertyId: number,
): UseMutationResult<Document, Error, { id: number; body: Partial<Document> }> {
  return useInvalidating(
    ({ id, body }: { id: number; body: Partial<Document> }) =>
      request<Document>(`/api/v1/documents/${id}`, { method: "PATCH", body }),
    [keys.documents(propertyId)],
  );
}

export function useDeleteDocument(propertyId: number): UseMutationResult<void, Error, number> {
  return useInvalidating(
    (id: number) => request<void>(`/api/v1/documents/${id}`, { method: "DELETE" }),
    [keys.documents(propertyId)],
  );
}
