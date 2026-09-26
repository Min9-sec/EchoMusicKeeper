import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { verifyPackage } from "../scripts/verify-package.js";

test("installable package contains only the required runtime files", async () => {
  const packageJSON = JSON.parse(await readFile(new URL("../package.json", import.meta.url), "utf8"));
  const result = await verifyPackage();
  assert.deepEqual(result, {
    files: ["LICENSE", "bin/echo-music-keeper-helper-macos", "bin/echo-music-keeper-helper.exe", "icon.svg", "index.js", "manifest.json"],
    version: packageJSON.version,
  });
});
