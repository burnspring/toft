import { describe, it, expect } from "vitest";
import { deflateRawSync } from "node:zlib";

import { excludedBy, inspectZip } from "./zip-preflight";

/**
 * The archives are built here rather than fixtured, because what is being
 * tested is the byte layout: a fixture would only prove that this parser agrees
 * with whatever wrote the fixture.
 */

const enc = new TextEncoder();

function crc32(buf: Uint8Array): number {
  let c = ~0;
  for (let i = 0; i < buf.length; i++) {
    c ^= buf[i];
    for (let k = 0; k < 8; k++) c = (c >>> 1) ^ (0xedb88320 & -(c & 1));
  }
  return ~c >>> 0;
}

interface Entry {
  name: string;
  body: string;
}

/** A minimal but real ZIP: local headers, deflated data, central directory. */
function makeZip(entries: Entry[], opts: { comment?: string } = {}): Uint8Array {
  const chunks: Uint8Array[] = [];
  const central: Uint8Array[] = [];
  let offset = 0;

  for (const e of entries) {
    const name = enc.encode(e.name);
    const raw = enc.encode(e.body);
    const data = new Uint8Array(deflateRawSync(Buffer.from(raw)));
    const crc = crc32(raw);

    const local = new DataView(new ArrayBuffer(30));
    local.setUint32(0, 0x04034b50, true);
    local.setUint16(4, 20, true);
    local.setUint16(8, 8, true); // deflate
    local.setUint32(14, crc, true);
    local.setUint32(18, data.length, true);
    local.setUint32(22, raw.length, true);
    local.setUint16(26, name.length, true);

    chunks.push(new Uint8Array(local.buffer), name, data);

    const cen = new DataView(new ArrayBuffer(46));
    cen.setUint32(0, 0x02014b50, true);
    cen.setUint16(4, 20, true);
    cen.setUint16(6, 20, true);
    cen.setUint16(10, 8, true);
    cen.setUint32(16, crc, true);
    cen.setUint32(20, data.length, true);
    cen.setUint32(24, raw.length, true);
    cen.setUint16(28, name.length, true);
    cen.setUint32(42, offset, true);
    central.push(new Uint8Array(cen.buffer), name);

    offset += 30 + name.length + data.length;
  }

  const cdBytes = concat(central);
  const comment = enc.encode(opts.comment ?? "");
  const eocd = new DataView(new ArrayBuffer(22));
  eocd.setUint32(0, 0x06054b50, true);
  eocd.setUint16(8, entries.length, true);
  eocd.setUint16(10, entries.length, true);
  eocd.setUint32(12, cdBytes.length, true);
  eocd.setUint32(16, offset, true);
  eocd.setUint16(20, comment.length, true);

  return concat([...chunks, cdBytes, new Uint8Array(eocd.buffer), comment]);
}

function concat(parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}

function asFile(bytes: Uint8Array, name = "app.zip"): File {
  // `bytes.buffer` is ArrayBufferLike, and the DOM's BlobPart wants an
  // ArrayBuffer specifically — a distinction that only exists because a typed
  // array could in principle be backed by a SharedArrayBuffer, which nothing
  // here produces.
  return new File([bytes as Uint8Array<ArrayBuffer>], name, { type: "application/zip" });
}

describe("excludedBy", () => {
  it("matches any path segment, not the first", () => {
    expect(excludedBy("packages/web/node_modules/react/index.js", false)).toBe("node_modules");
    expect(excludedBy("my-app/.git/config", false)).toBe(".git");
    expect(excludedBy("assets/.DS_Store", false)).toBe(".DS_Store");
  });

  it("leaves a file named like an excluded directory alone", () => {
    expect(excludedBy("node_modules", false)).toBe("");
    expect(excludedBy("docs/.git", false)).toBe("");
  });

  it("leaves build output alone, as the server does", () => {
    for (const p of ["dist/index.html", "build/main.js", "out/x", ".next/page.js"]) {
      expect(excludedBy(p, false)).toBe("");
    }
  });
});

describe("inspectZip", () => {
  it("reads an ordinary archive", async () => {
    const zip = makeZip([
      { name: "package.json", body: "{}" },
      { name: "server.js", body: "listen()" },
      { name: "node_modules/left-pad/index.js", body: "x".repeat(4096) },
      { name: "node_modules/dep/index.js", body: "y".repeat(2048) },
      { name: ".git/config", body: "[core]" },
    ]);
    const report = await inspectZip(asFile(zip));
    expect(report).not.toBeNull();
    expect(report!.removed.map((r) => r.path)).toEqual(["node_modules", ".git"]);
    expect(report!.removed[0].files).toBe(2);
    expect(report!.removed[0].bytes).toBe(4096 + 2048);
    // The saving is what those entries cost *in the archive*, which is what
    // re-zipping actually buys back.
    expect(report!.removedCompressedBytes).toBeGreaterThan(0);
    expect(report!.removedCompressedBytes).toBeLessThan(4096 + 2048);
  });

  it("finds a nested tree under a common root prefix", async () => {
    const zip = makeZip([
      { name: "my-app/package.json", body: "{}" },
      { name: "my-app/node_modules/a/index.js", body: "z".repeat(100) },
    ]);
    const report = await inspectZip(asFile(zip));
    expect(report!.removed).toHaveLength(1);
    expect(report!.removed[0].path).toBe("node_modules");
  });

  it("mentions files worth mentioning, and never as a removal", async () => {
    const zip = makeZip([
      { name: "app.py", body: "print(1)" },
      { name: ".env", body: "TOKEN=1" },
      { name: "certs/server.pem", body: "----" },
    ]);
    const report = await inspectZip(asFile(zip));
    expect(report!.removed).toHaveLength(0);
    expect(report!.secrets).toEqual([".env", "server.pem"]);
  });

  it("says nothing about a clean archive", async () => {
    const zip = makeZip([
      { name: "index.html", body: "<h1>hi</h1>" },
      { name: "dist/app.js", body: "//" },
    ]);
    const report = await inspectZip(asFile(zip));
    expect(report!.removed).toHaveLength(0);
    expect(report!.secrets).toHaveLength(0);
  });

  // The end-of-directory record is found by scanning back for its signature,
  // so a trailing comment must not hide it.
  it("finds the directory behind an archive comment", async () => {
    const zip = makeZip([{ name: "node_modules/a/i.js", body: "q".repeat(500) }], {
      comment: "written by something with opinions",
    });
    const report = await inspectZip(asFile(zip));
    expect(report!.removed[0].path).toBe("node_modules");
  });

  // Every failure is the same failure, and the caller falls back to the static
  // copy under the dropzone. Advice that throws is worse than no advice.
  it("gives up quietly on a truncated file", async () => {
    const zip = makeZip([{ name: "app.py", body: "print(1)" }]);
    expect(await inspectZip(asFile(zip.slice(0, zip.length - 10)))).toBeNull();
  });

  it("gives up quietly on something that is not a ZIP at all", async () => {
    expect(await inspectZip(asFile(enc.encode("this is a tarball, honestly")))).toBeNull();
  });

  it("gives up quietly on an empty file", async () => {
    expect(await inspectZip(asFile(new Uint8Array(0)))).toBeNull();
  });

  // A ZIP64 archive whose 32-bit fields are saturated: the real offsets live in
  // a second record found through a locator. Handled where it can be, and
  // falling back where it cannot — either way, never an error.
  it("handles a ZIP64 end-of-directory record", async () => {
    const base = makeZip([
      { name: "app.py", body: "print(1)" },
      { name: "node_modules/a/i.js", body: "w".repeat(300) },
    ]);
    const upgraded = toZip64(base);
    const report = await inspectZip(asFile(upgraded));
    expect(report).not.toBeNull();
    expect(report!.removed[0].path).toBe("node_modules");
  });
});

/**
 * Rewrites a plain archive's tail as ZIP64: the 32-bit fields saturate and a
 * ZIP64 record plus its locator are appended in front of the original EOCD.
 */
function toZip64(zip: Uint8Array): Uint8Array {
  const v = new DataView(zip.buffer, zip.byteOffset, zip.byteLength);
  const eocdAt = zip.length - 22;
  const cdSize = v.getUint32(eocdAt + 12, true);
  const cdOffset = v.getUint32(eocdAt + 16, true);
  const entries = v.getUint16(eocdAt + 10, true);

  const body = zip.slice(0, eocdAt);

  const rec = new DataView(new ArrayBuffer(56));
  rec.setUint32(0, 0x06064b50, true);
  rec.setUint32(4, 44, true); // size of the record after this field
  rec.setUint32(24, entries, true);
  rec.setUint32(32, entries, true);
  rec.setUint32(40, cdSize, true);
  rec.setUint32(48, cdOffset, true);

  const loc = new DataView(new ArrayBuffer(20));
  loc.setUint32(0, 0x07064b50, true);
  loc.setUint32(8, body.length, true); // where the ZIP64 record starts
  loc.setUint32(16, 1, true);

  const eocd = new DataView(new ArrayBuffer(22));
  eocd.setUint32(0, 0x06054b50, true);
  eocd.setUint16(8, 0xffff, true);
  eocd.setUint16(10, 0xffff, true);
  eocd.setUint32(12, 0xffffffff, true);
  eocd.setUint32(16, 0xffffffff, true);

  return concat([
    body,
    new Uint8Array(rec.buffer),
    new Uint8Array(loc.buffer),
    new Uint8Array(eocd.buffer),
  ]);
}
