import { readFile, readdir, lstat } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const EXPECTED_FILES = Object.freeze([
  "LICENSE",
  "bin/echo-music-keeper-helper.exe",
  "icon.svg",
  "index.js",
  "manifest.json",
]);
const VERSION_PATTERN = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;

async function readJSON(filename) {
  return JSON.parse(await readFile(filename, "utf8"));
}

async function packageFiles(directory, relative = "") {
  const entries = await readdir(path.join(directory, relative), { withFileTypes: true });
  const files = [];
  for (const entry of entries) {
    const item = path.join(relative, entry.name);
    const info = await lstat(path.join(directory, item));
    if (info.isSymbolicLink()) throw new Error(`plugin package must not contain symlinks: ${item}`);
    if (info.isDirectory()) files.push(...await packageFiles(directory, item));
    else if (info.isFile()) files.push(item.split(path.sep).join("/"));
    else throw new Error(`plugin package contains an unsupported entry: ${item}`);
  }
  return files.sort();
}

function assertEqual(actual, expected, message) {
  if (actual !== expected) throw new Error(`${message}: expected ${expected}, received ${actual}`);
}

export async function verifyPackage(root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..")) {
  const pluginRoot = path.join(root, "plugin");
  const files = await packageFiles(pluginRoot);
  assertEqual(JSON.stringify(files), JSON.stringify(EXPECTED_FILES), "plugin file list mismatch");

  const [packageJSON, manifest, source] = await Promise.all([
    readJSON(path.join(root, "package.json")),
    readJSON(path.join(pluginRoot, "manifest.json")),
    readJSON(path.join(root, "echo-plugins.json")),
  ]);
  const sourceEntry = source?.plugins?.find((entry) => entry?.id === manifest.id);
  assertEqual(manifest.id, "echo-music-keeper", "plugin id mismatch");
  assertEqual(sourceEntry?.id, manifest.id, "plugin source id mismatch");
  assertEqual(sourceEntry?.path, "plugin", "plugin source path mismatch");
  if (typeof packageJSON.version !== "string" || !VERSION_PATTERN.test(packageJSON.version)) {
    throw new Error(`invalid package version: ${packageJSON.version}`);
  }
  assertEqual(manifest.version, packageJSON.version, "plugin version mismatch");
  assertEqual(manifest.main, "index.js", "plugin entry mismatch");
  assertEqual(manifest.icon, "icon.svg", "plugin icon mismatch");

  const [repositoryLicense, pluginLicense] = await Promise.all([
    readFile(path.join(root, "LICENSE")),
    readFile(path.join(pluginRoot, "LICENSE")),
  ]);
  assertEqual(pluginLicense.equals(repositoryLicense), true, "plugin license mismatch");

  const executable = await readFile(path.join(pluginRoot, "bin", "echo-music-keeper-helper.exe"));
  if (executable.length < 2 || executable[0] !== 0x4d || executable[1] !== 0x5a) {
    throw new Error("plugin helper is not a PE executable");
  }
  return { files, version: manifest.version };
}

const invokedPath = process.argv[1] ? pathToFileURL(path.resolve(process.argv[1])).href : "";
if (import.meta.url === invokedPath) {
  const result = await verifyPackage();
  console.log(`verified plugin ${result.version}: ${result.files.join(", ")}`);
}
