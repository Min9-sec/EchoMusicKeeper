import assert from "node:assert/strict";
import test from "node:test";
import { buildOptions } from "../scripts/build.js";

test("build configuration targets the installable plugin bundle", () => {
  assert.deepEqual(buildOptions, {
    entryPoints: ["src/index.js"],
    bundle: true,
    format: "esm",
    target: "chrome120",
    outfile: "plugin/index.js",
  });
});
