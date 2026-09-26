import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { universalMachOArchitectures } from "../scripts/verify-package.js";

const CPU_TYPE_X86_64 = 0x01000007;
const CPU_TYPE_ARM64 = 0x0100000c;

function fatMachO(architectures) {
  const sliceSize = 64;
  const header = Buffer.alloc(8 + architectures.length * 20);
  header.writeUInt32BE(0xcafebabe, 0);
  header.writeUInt32BE(architectures.length, 4);
  const slices = [];
  architectures.forEach((architecture, index) => {
    const entry = 8 + index * 20;
    const offset = header.length + slices.length * sliceSize;
    header.writeUInt32BE(architecture, entry);
    header.writeUInt32BE(0, entry + 4);
    header.writeUInt32BE(offset, entry + 8);
    header.writeUInt32BE(sliceSize, entry + 12);
    header.writeUInt32BE(14, entry + 16);
    slices.push(Buffer.alloc(sliceSize, index + 1));
  });
  return Buffer.concat([header, ...slices]);
}

test("reads every architecture of a universal Mach-O binary", () => {
  assert.deepEqual(universalMachOArchitectures(fatMachO([CPU_TYPE_X86_64, CPU_TYPE_ARM64])), [CPU_TYPE_X86_64, CPU_TYPE_ARM64]);
  assert.deepEqual(universalMachOArchitectures(fatMachO([CPU_TYPE_ARM64])), [CPU_TYPE_ARM64]);
});

test("rejects binaries that are not universal Mach-O images", () => {
  const truncated = fatMachO([CPU_TYPE_ARM64]).subarray(0, 12);
  const outOfRange = fatMachO([CPU_TYPE_ARM64]);
  outOfRange.writeUInt32BE(outOfRange.length, 8 + 8);
  const empty = Buffer.alloc(8);
  empty.writeUInt32BE(0xcafebabe, 0);
  empty.writeUInt32BE(0, 4);
  const windowsExecutable = Buffer.concat([Buffer.from("MZ"), Buffer.alloc(64)]);
  for (const [name, buffer] of Object.entries({ truncated, outOfRange, empty, windowsExecutable, short: Buffer.alloc(4) })) {
    assert.equal(universalMachOArchitectures(buffer), null, name);
  }
});

test("packaged macOS helper contains both macOS architectures", async () => {
  const executable = await readFile(new URL("../plugin/bin/echo-music-keeper-helper-macos", import.meta.url));
  const architectures = universalMachOArchitectures(executable);
  assert.notEqual(architectures, null);
  assert.equal(architectures.includes(CPU_TYPE_ARM64), true);
  assert.equal(architectures.includes(CPU_TYPE_X86_64), true);
});
