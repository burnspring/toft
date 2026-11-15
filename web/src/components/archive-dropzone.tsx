import { useRef, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/notice";
import { Upload, FileArchive } from "lucide-react";
import { formatBytes } from "@/lib/bytes";
import { inspectZip, type ZipReport } from "@/lib/zip-preflight";

/** The wire cap, and the client-side check that saves somebody the wait. */
export const MAX_ARCHIVE_MB = 100;

/** What the two upload routes accept, in the order the copy names them. The
 *  server's list is `uploadArchiveSuffixes` in `internal/api/app_handlers.go`
 *  and this is the client-side half of the same sentence. */
const ACCEPTED = [".zip", ".tar.gz", ".tgz"];

/**
 * The one dropzone, on both surfaces that take an uploaded archive: the New App
 * wizard and the app detail page's new-version dialog. Both formats the server
 * unpacks are offered here — a ZIP, which is what a folder becomes on either
 * desktop, and a gzipped tar, which is what a Unix toolchain already has.
 *
 * It is shared because the *copy* is the point. The platform removes dependency
 * trees, virtualenvs and VCS metadata from an archive before installing it
 * (ADR §60), and guidance that only appears on one of the two doors is guidance
 * half the uploads never see. The New App page had a single line — "Max 100 MB"
 * — and the app detail page had a bare hidden `<input type="file">` with no
 * copy at all.
 *
 * The size check stays exactly what it was, and gains the sentence naming what
 * to remove when it fires: a refusal that does not say what would have worked
 * sends somebody to Slack.
 *
 * **Two shapes, because one of them has to be a real button.** With nothing
 * picked the whole zone *is* a `<button>`, so Tab reaches it and Enter opens
 * the file picker; with a file picked it is a panel holding a Remove button,
 * and nested buttons are not a thing. Before this the zone was a `<div>` with
 * an `onClick`, which meant there was no keyboard route to attaching an archive
 * on either of the product's two deploy doors — not a rough edge but a flow
 * with no way through it.
 */
export function ArchiveDropzone({
  file,
  onFile,
  disabled = false,
}: {
  file: File | null;
  onFile: (file: File | null) => void;
  disabled?: boolean;
}) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [over, setOver] = useState(false);
  // What this archive actually holds, when it could be read. Null covers both
  // "not looked yet" and "could not be read", which are the same thing to
  // everything downstream: the static copy below is what gets said.
  const [report, setReport] = useState<ZipReport | null>(null);

  function accept(picked: File | undefined) {
    if (!picked) return;
    const name = picked.name.toLowerCase();
    if (!ACCEPTED.some((ext) => name.endsWith(ext))) {
      toast.error(
        "Only .zip, .tar.gz and .tgz archives are accepted."
      );
      return;
    }
    if (picked.size > MAX_ARCHIVE_MB * 1024 * 1024) {
      toast.error(
        `That file is ${(picked.size / 1024 / 1024).toFixed(0)} MB — the limit is ${MAX_ARCHIVE_MB} MB. ` +
          `Leave out node_modules, virtualenvs and .git.`
      );
      return;
    }
    setReport(null);
    onFile(picked);
    // Only a ZIP can be read here: its directory is at the end of the file and
    // four ranged reads answer the question, while a tarball is a gzip stream
    // with no index — reading one in the browser means decompressing the whole
    // upload to say something the server will say anyway. So a tar.gz gets the
    // static copy below, which is the same fallback a ZIP that will not parse
    // already gets.
    //
    // Deliberately not awaited and deliberately not caught: inspectZip resolves
    // to null on every failure, and nothing about picking a file waits on it.
    if (name.endsWith(".zip")) void inspectZip(picked).then(setReport);
  }

  function clear() {
    setReport(null);
    onFile(null);
    if (inputRef.current) inputRef.current.value = "";
  }

  const removed = file ? (report?.removed ?? []) : [];
  const secrets = file ? (report?.secrets ?? []) : [];

  const zoneClass =
    "flex flex-col items-center justify-center rounded-lg border-2 border-dashed px-6 py-8 transition-colors";

  /** A drop is a mouse gesture on either shape; the button above is what makes
   *  the same act reachable without one. */
  const dropHandlers = {
    onDragOver: (e: React.DragEvent) => {
      e.preventDefault();
      if (!disabled) setOver(true);
    },
    onDragLeave: () => setOver(false),
    onDrop: (e: React.DragEvent) => {
      e.preventDefault();
      setOver(false);
      if (!disabled) accept(e.dataTransfer.files?.[0]);
    },
  };

  return (
    <div className="space-y-2">
      {file ? (
        <div className={`${zoneClass} border-primary/40 bg-primary/5`} {...dropHandlers}>
          <FileArchive className="h-8 w-8 text-primary" />
          <p className="mt-2 text-sm font-medium">{file.name}</p>
          <p className="text-xs text-muted-foreground">
            {(file.size / 1024 / 1024).toFixed(1)} MB
          </p>
          <Button
            variant="ghost"
            size="sm"
            className="mt-2 h-7 text-xs text-muted-foreground"
            disabled={disabled}
            onClick={clear}
          >
            Remove
          </Button>
        </div>
      ) : (
        <button
          type="button"
          disabled={disabled}
          onClick={() => inputRef.current?.click()}
          className={`w-full ${zoneClass} ${
            disabled
              ? "cursor-not-allowed border-border opacity-60"
              : `cursor-pointer focus-visible:ring-[3px] focus-visible:ring-ring/50 ${
                  over
                    ? "border-primary/40 bg-primary/5"
                    : "border-border hover:border-primary/30 hover:bg-muted/30"
                }`
          }`}
          {...dropHandlers}
        >
          <Upload className="h-8 w-8 text-muted-foreground" />
          <p className="mt-2 text-sm font-medium">Drop a .zip or .tar.gz here, or choose one</p>
          <p className="text-xs text-muted-foreground">Max {MAX_ARCHIVE_MB} MB</p>
        </button>
      )}
      <input
        ref={inputRef}
        type="file"
        accept=".zip,.tar.gz,.tgz"
        // Hidden and out of the tab order: the button above is the control, and
        // a second stop on an invisible input is a Tab that appears to do
        // nothing.
        className="hidden"
        tabIndex={-1}
        aria-hidden="true"
        disabled={disabled}
        onChange={(e) => accept(e.target.files?.[0])}
      />
      {removed.length > 0 && file ? (
        <Notice
          tone="info"
          size="sm"
          live
          title="The platform will remove these before installing"
        >
          <p>
            {removed.map((e, i) => (
              <span key={e.path}>
                {i > 0 ? (i === removed.length - 1 ? " and " : ", ") : ""}
                <code className="font-mono">{e.path}</code> ({e.files.toLocaleString()}{" "}
                {e.files === 1 ? "file" : "files"}, {formatBytes(e.bytes)})
              </span>
            ))}
            {report && report.removedCompressedBytes > 0
              ? `. Without ${removed.length === 1 ? "it" : "them"} this upload is ${formatBytes(
                  Math.max(0, file.size - report.removedCompressedBytes)
                )}, not ${formatBytes(file.size)}.`
              : "."}
          </p>
        </Notice>
      ) : null}

      {/* The rule, for as long as nobody has been told the answer about their
          own file. The notice above says the same thing better and about this
          archive, so the two are never shown together. */}
      {removed.length === 0 ? (
        <p className="text-xs text-muted-foreground">
          Leave out <code className="font-mono">node_modules</code>, virtualenvs and{" "}
          <code className="font-mono">.git</code>. The platform removes them before installing.
        </p>
      ) : null}

      {/* A warning, never a refusal: `.env` is genuinely how some apps read
          their configuration, and the platform supports that. But it is the
          one line in this flow that can stop a credential leaking, so it is
          not set in the quietest style on the page. */}
      {secrets.length > 0 ? (
        <Notice
          tone="attention"
          size="sm"
          live
          title="This archive contains files that look like secrets"
        >
          {secrets.map((s, i) => (
            <span key={s}>
              {i > 0 ? ", " : ""}
              <code className="font-mono">{s}</code>
            </span>
          ))}{" "}
          {secrets.length === 1 ? "is" : "are"} deployed with the app. Remove{" "}
          {secrets.length === 1 ? "it" : "them"} and use environment variables instead.
        </Notice>
      ) : null}
    </div>
  );
}
