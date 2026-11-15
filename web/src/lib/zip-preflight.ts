import type { RemovedEntry } from "./removed";

/**
 * Reading an archive before it is uploaded (ADR §60).
 *
 * The server's filter runs *after* the bytes have arrived. A 400 MB archive
 * that is 380 MB of `node_modules` is refused by the 100 MB wire cap, and no
 * amount of server-side filtering saves that person the wait or tells them what
 * to do about it. Only the browser can, and only before the POST — which is the
 * reason this file exists rather than a rule printed under the dropzone.
 *
 * A ZIP's central directory is at the *end* of the file and holds every entry's
 * name and both its sizes, so this needs the last 64 KiB plus the directory
 * itself rather than the 400 MB, and it is a `File.slice()` and a `DataView`
 * rather than a ZIP library shipped to the browser to read a header.
 *
 * **It never blocks and never refuses.** Every failure — an unusual writer, a
 * comment longer than the tail we read, a truncated file — returns null, and
 * the caller falls back to the static copy. The server is the enforcer; this is
 * advice, and advice that throws an error is worse than no advice.
 */

/** Mirrors `builder.ExcludedDirs`. Kept in step by the copy under the dropzone. */
const EXCLUDED_DIRS = new Set([
  "node_modules",
  "venv",
  ".venv",
  "__pycache__",
  ".git",
  ".hg",
  ".svn",
]);

/** Mirrors `builder.ExcludedFiles`. */
const EXCLUDED_FILES = new Set([".DS_Store"]);

/** The same names `toft deploy` mentions before a push — a warning, never a refusal. */
function secretish(base: string): boolean {
  return (
    base.startsWith(".env") ||
    base.startsWith("id_rsa") ||
    base.startsWith("id_ed25519") ||
    base.endsWith(".pem") ||
    base.endsWith(".key")
  );
}

/** Which component excludes this entry, or "" if none does. */
export function excludedBy(name: string, isDir: boolean): string {
  const trimmed = name.replace(/\/+$/, "");
  if (!trimmed) return "";
  const segs = trimmed.split("/");
  for (let i = 0; i < segs.length; i++) {
    const last = i === segs.length - 1;
    if (last && !isDir) return EXCLUDED_FILES.has(segs[i]) ? segs[i] : "";
    if (EXCLUDED_DIRS.has(segs[i])) return segs[i];
  }
  return "";
}

export interface ZipReport {
  /** What the platform will remove, largest first. `bytes` is uncompressed. */
  removed: RemovedEntry[];
  /** What those entries cost *in the archive* — the saving from re-zipping. */
  removedCompressedBytes: number;
  /** Files worth a mention, never a refusal: `.env`, keys, certificates. */
  secrets: string[];
}

const EOCD_SIG = 0x06054b50;
const EOCD64_LOCATOR_SIG = 0x07064b50;
const EOCD64_SIG = 0x06064b50;
const CENTRAL_SIG = 0x02014b50;
/** How far back the end-of-directory record is looked for. */
const TAIL_BYTES = 64 * 1024;

async function slice(file: File, start: number, end: number): Promise<DataView> {
  const buf = await file.slice(start, end).arrayBuffer();
  return new DataView(buf);
}

/** 64-bit read, little-endian, as a Number. Sizes here never approach 2^53. */
function u64(v: DataView, off: number): number {
  return v.getUint32(off, true) + v.getUint32(off + 4, true) * 0x100000000;
}

/** Where the central directory starts and how long it is, or null. */
async function locateCentralDirectory(
  file: File
): Promise<{ offset: number; size: number } | null> {
  const tailStart = Math.max(0, file.size - TAIL_BYTES);
  const tail = await slice(file, tailStart, file.size);

  // The record is 22 bytes plus a comment, so it is found by scanning back for
  // its signature rather than by arithmetic.
  let eocd = -1;
  for (let i = tail.byteLength - 22; i >= 0; i--) {
    if (tail.getUint32(i, true) === EOCD_SIG) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) return null;

  let size = tail.getUint32(eocd + 12, true);
  let offset = tail.getUint32(eocd + 16, true);

  // ZIP64: the 32-bit fields are saturated and the real ones live in a second
  // record, found through a locator immediately before this one.
  if (size === 0xffffffff || offset === 0xffffffff) {
    const loc = eocd - 20;
    if (loc < 0 || tail.getUint32(loc, true) !== EOCD64_LOCATOR_SIG) return null;
    const at = u64(tail, loc + 8);
    if (at < 0 || at + 56 > file.size) return null;
    const rec = await slice(file, at, at + 56);
    if (rec.getUint32(0, true) !== EOCD64_SIG) return null;
    size = u64(rec, 40);
    offset = u64(rec, 48);
  }

  if (offset < 0 || size <= 0 || offset + size > file.size) return null;
  return { offset, size };
}

/**
 * A central-directory entry's sizes, taking the ZIP64 extra field into account
 * when the 32-bit ones are saturated.
 */
function entrySizes(
  v: DataView,
  at: number,
  extraStart: number,
  extraLen: number
): { compressed: number; uncompressed: number } {
  let compressed = v.getUint32(at + 20, true);
  let uncompressed = v.getUint32(at + 24, true);
  if (compressed !== 0xffffffff && uncompressed !== 0xffffffff) {
    return { compressed, uncompressed };
  }
  // The 0x0001 field lists only the saturated members, in a fixed order.
  let p = extraStart;
  const end = extraStart + extraLen;
  while (p + 4 <= end) {
    const id = v.getUint16(p, true);
    const len = v.getUint16(p + 2, true);
    if (id === 0x0001) {
      let q = p + 4;
      if (uncompressed === 0xffffffff && q + 8 <= end) {
        uncompressed = u64(v, q);
        q += 8;
      }
      if (compressed === 0xffffffff && q + 8 <= end) {
        compressed = u64(v, q);
      }
      break;
    }
    p += 4 + len;
  }
  return { compressed, uncompressed };
}

export async function inspectZip(file: File): Promise<ZipReport | null> {
  try {
    const cd = await locateCentralDirectory(file);
    if (!cd) return null;

    const v = await slice(file, cd.offset, cd.offset + cd.size);
    const decoder = new TextDecoder("utf-8");
    const bytes = new Uint8Array(v.buffer);

    const byName = new Map<string, RemovedEntry>();
    const secrets: string[] = [];
    let removedCompressedBytes = 0;

    let at = 0;
    while (at + 46 <= v.byteLength) {
      if (v.getUint32(at, true) !== CENTRAL_SIG) return null;
      const nameLen = v.getUint16(at + 28, true);
      const extraLen = v.getUint16(at + 30, true);
      const commentLen = v.getUint16(at + 32, true);
      const nameStart = at + 46;
      if (nameStart + nameLen > v.byteLength) return null;

      const name = decoder.decode(bytes.subarray(nameStart, nameStart + nameLen));
      const isDir = name.endsWith("/");
      const { compressed, uncompressed } = entrySizes(
        v,
        at,
        nameStart + nameLen,
        extraLen
      );

      const by = excludedBy(name, isDir);
      if (by && !isDir) {
        const entry = byName.get(by) ?? { path: by, files: 0, bytes: 0 };
        entry.files += 1;
        entry.bytes += uncompressed;
        byName.set(by, entry);
        removedCompressedBytes += compressed;
      } else if (!by && !isDir) {
        const base = name.slice(name.lastIndexOf("/") + 1);
        if (secretish(base) && !secrets.includes(base)) secrets.push(base);
      }

      at = nameStart + nameLen + extraLen + commentLen;
    }
    if (at !== v.byteLength) return null;

    const removed = [...byName.values()].sort(
      (a, b) => b.bytes - a.bytes || a.path.localeCompare(b.path)
    );
    return { removed, removedCompressedBytes, secrets };
  } catch {
    // Every failure is the same failure: this archive cannot be read cheaply,
    // and the static copy under the dropzone already says the useful thing.
    return null;
  }
}
