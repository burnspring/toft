import { describe, expect, it } from "vitest";

import { SLICE_BYTES, digestBlob } from "./store-hash";

/**
 * These four fixtures are the contract between three implementations of the
 * same number: this file, `hash-wasm` under it, and the Go that verifies what
 * S3 was told. The same table is asserted in
 * `internal/api/store_handlers_test.go` — a digest computed here and refused
 * there is the failure this pins, and it is the one an upload cannot recover
 * from, because by then the URL is signed and the bytes are on the wire.
 *
 * `abc` and the empty string are the published vectors for both algorithms.
 * `toft` is arbitrary. The 20 MiB pattern is the only one that matters
 * structurally: it is longer than two slices, so it proves the chunked path
 * agrees with a one-shot digest of the same bytes rather than hashing each
 * slice separately.
 */
const VECTORS = [
  {
    name: "the empty file",
    bytes: new Uint8Array(new ArrayBuffer(0)),
    sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    md5: "d41d8cd98f00b204e9800998ecf8427e",
  },
  {
    name: "abc",
    bytes: new TextEncoder().encode("abc"),
    sha256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
    md5: "900150983cd24fb0d6963f7d28e17f72",
  },
  {
    name: "toft",
    bytes: new TextEncoder().encode("toft"),
    sha256: "6a6f9028c099184750c56aa6f1655f97aeae171bd6a7d384b56b95c28b5a6179",
    md5: "b4f445a2e2c16f4b8ab6496bb54ef265",
  },
];

/** byte i = (7i + 3) mod 251, 20 MiB of it — deterministic, not compressible
 * into a run of one value, and longer than two 8 MiB slices. */
function pattern(size: number): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(new ArrayBuffer(size));
  for (let i = 0; i < size; i++) out[i] = (i * 7 + 3) % 251;
  return out;
}

const PATTERN_20MIB = {
  sha256: "fc8299d1434a2b792cd13a13d546a73d9351033fe7bcff5a6f342ff865cbabc9",
  md5: "eb9c339d686a68906f8496c456e45169",
};

describe("digestBlob", () => {
  for (const v of VECTORS) {
    it(`matches the published vectors for ${v.name}`, async () => {
      const got = await digestBlob(new Blob([v.bytes]));
      expect(got.sha256).toBe(v.sha256);
      expect(got.md5).toBe(v.md5);
    });
  }

  it("agrees with a one-shot digest across slice boundaries", async () => {
    const bytes = pattern(20 * 1024 * 1024);
    expect(bytes.byteLength).toBeGreaterThan(2 * SLICE_BYTES);

    const got = await digestBlob(new Blob([bytes]));
    expect(got.sha256).toBe(PATTERN_20MIB.sha256);
    expect(got.md5).toBe(PATTERN_20MIB.md5);
  });

  it("reports every byte exactly once, in order", async () => {
    const bytes = pattern(20 * 1024 * 1024);
    const seen: number[] = [];
    await digestBlob(new Blob([bytes]), (read) => seen.push(read));

    // One report per slice, monotonic, ending on the file's own size — a
    // progress bar that jumps backwards or stops short is how a finished
    // transfer looks stuck.
    expect(seen).toEqual([SLICE_BYTES, 2 * SLICE_BYTES, bytes.byteLength]);
  });

  it("stops when the transfer is cancelled", async () => {
    const control = new AbortController();
    control.abort();
    await expect(digestBlob(new Blob([pattern(1024)]), undefined, control.signal)).rejects.toThrow(
      /abort/i,
    );
  });
});
