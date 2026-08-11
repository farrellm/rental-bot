import { useRef, useState } from "react";
import { useParams } from "react-router";

import {
  describeError,
  useDeleteDocument,
  useDocuments,
  useUploadDocument,
  type Document,
  type DocumentKind,
} from "../../api";
import { Select } from "../../components/Select";
import { SheetFilter } from "../../components/SheetField";
import { bytes, timestamp } from "../../format";

const KINDS: DocumentKind[] = [
  "lease",
  "insurance",
  "receipt",
  "statement",
  "tax",
  "photo",
  "correspondence",
  "other",
];

const KIND_LABEL: Record<DocumentKind, string> = {
  lease: "Lease",
  insurance: "Insurance",
  receipt: "Receipt",
  statement: "Statement",
  tax: "Tax",
  photo: "Photo",
  correspondence: "Letter",
  other: "Other",
};

/**
 * The document jacket.
 *
 * One slip per document: what it is, what it was called, how big, and its
 * accession number — the first bytes of the SHA-256 the file is stored under.
 * That is not decoration. It is the document's actual identity in this system:
 * two files with the same accession are the same bytes, which is why
 * forwarding the same receipt twice files it once.
 */
export function Documents() {
  const params = useParams();
  const propertyId = Number(params.id ?? 0);

  const documents = useDocuments(propertyId);
  const remove = useDeleteDocument(propertyId);

  const [notice, setNotice] = useState<string | null>(null);

  async function unfile(id: number) {
    setNotice(null);
    try {
      await remove.mutateAsync(id);
    } catch (err) {
      setNotice(describeError(err));
    }
  }

  return (
    <>
      {notice && <p className="card__notice">{notice}</p>}

      <div className="sheet">
        <Attach propertyId={propertyId} onNotice={setNotice} />

        {documents.isPending && <p className="waiting waiting--ink">Reading the file</p>}
        {documents.isError && <p className="hint hint--fault">{describeError(documents.error)}</p>}

        {documents.data &&
          (documents.data.items.length === 0 ? (
            <p className="sheet__empty">
              Nothing filed against this property yet. Attach a lease, a policy, or a receipt.
            </p>
          ) : (
            <div className="jackets">
              {documents.data.items.map((doc) => (
                <Jacket key={doc.id} doc={doc} onRemove={() => void unfile(doc.id)} />
              ))}
            </div>
          ))}
      </div>
    </>
  );
}

function Jacket({ doc, onRemove }: { doc: Document; onRemove: () => void }) {
  return (
    <article className="jacket">
      <div className="jacket__body">
        <h3 className="jacket__title">
          {/* The content route is the only way to these bytes, and it needs a
              session. Opening in a new tab keeps the record on screen. */}
          <a
            className="jacket__link"
            href={`/api/v1/documents/${doc.id}/content`}
            target="_blank"
            rel="noreferrer"
          >
            {doc.title || doc.original_filename}
          </a>
        </h3>
        <p className="jacket__facts mono">
          <span className="jacket__kind stamped">{KIND_LABEL[doc.kind]}</span>
          {/* An upload with no title of its own is titled with its filename.
              Printing it again underneath says nothing twice. */}
          {doc.title && doc.title !== doc.original_filename && (
            <>
              <span className="jacket__sep" aria-hidden="true">
                ·
              </span>
              {doc.original_filename}
            </>
          )}
          <span className="jacket__sep" aria-hidden="true">
            ·
          </span>
          {bytes(doc.size_bytes)}
        </p>
        <p className="jacket__accession mono">
          <span className="stamped">Acc.</span> {doc.sha256.slice(0, 8)}
          <span className="jacket__filed"> filed {timestamp(doc.created_at)}</span>
        </p>
      </div>

      <button
        type="button"
        className="button button--danger button--quiet jacket__remove"
        onClick={onRemove}
        aria-label={`Remove ${doc.title || doc.original_filename} from the file`}
      >
        Remove
      </button>
    </article>
  );
}

/**
 * Attaching a document.
 *
 * A file input is a browser control nobody can restyle usefully, so the real
 * one is hidden and a word on a rule stands in for it — ink on stock like every
 * other control here. It still is the input: the label wraps it, so a click and
 * a keyboard both reach it and focus lands where it should.
 */
function Attach({
  propertyId,
  onNotice,
}: {
  propertyId: number;
  onNotice: (message: string) => void;
}) {
  const upload = useUploadDocument(propertyId);
  const inputRef = useRef<HTMLInputElement>(null);

  const [kind, setKind] = useState<DocumentKind>("receipt");
  const [state, setState] = useState<"idle" | "filing" | "filed" | "already">("idle");
  const [filename, setFilename] = useState("");

  async function send(file: File) {
    setFilename(file.name);
    setState("filing");
    try {
      const filed = await upload.mutateAsync({
        file,
        kind,
        propertyId,
        link: { entity_type: "property", entity_id: propertyId },
      });
      setState(filed.deduplicated ? "already" : "filed");
    } catch (err) {
      setState("idle");
      onNotice(describeError(err));
    } finally {
      // The same file can be chosen again once it has been dealt with.
      if (inputRef.current) inputRef.current.value = "";
    }
  }

  return (
    <div className="attach">
      <SheetFilter label="Kind">
        <Select value={kind} onChange={setKind} options={KINDS} labels={KIND_LABEL} short />
      </SheetFilter>

      <label className="attach__choose">
        <span className="attach__word stamped">Attach a document</span>
        <input
          ref={inputRef}
          className="sr-only"
          type="file"
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file) void send(file);
          }}
        />
      </label>

      {state !== "idle" && (
        <p className="attach__state" role="status">
          <span className="attach__filename mono">{filename}</span>
          {state === "filing" && <span className="attach__word stamped">filing</span>}
          {state === "filed" && (
            <span className="attach__word attach__word--done stamped">filed</span>
          )}
          {state === "already" && (
            <span className="attach__word attach__word--already stamped">already on file</span>
          )}
        </p>
      )}
    </div>
  );
}
