import { build } from "esbuild";
import path from "node:path";
import { pathToFileURL } from "node:url";

export const buildOptions = Object.freeze({
  entryPoints: ["src/index.js"],
  bundle: true,
  format: "esm",
  target: "chrome120",
  outfile: "plugin/index.js",
});

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) await build(buildOptions);
