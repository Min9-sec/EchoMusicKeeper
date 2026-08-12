import assert from "node:assert/strict";
import test from "node:test";
import { GIB, normalizeSettings, toHelperConfig } from "../src/config.js";

test("defaults to automatic caching and a one GiB limit", () => {
  assert.deepEqual(normalizeSettings(null), {
    autoCache: true,
    cacheRoot: "",
    downloadRoot: "",
    cacheLimitBytes: GIB,
  });
});

test("clamps cache size and normalizes paths", () => {
  assert.deepEqual(
    normalizeSettings({
      autoCache: 0,
      cacheRoot: "  D:\\Echo Cache  ",
      downloadRoot: "  E:\\Music  ",
      cacheLimitBytes: 10,
    }),
    {
      autoCache: false,
      cacheRoot: "D:\\Echo Cache",
      downloadRoot: "E:\\Music",
      cacheLimitBytes: 256 * 1024 * 1024,
    },
  );
});

test("serializes exact helper configuration keys", () => {
  assert.deepEqual(
    toHelperConfig(normalizeSettings(null), {
      pluginRoot: "C:\\Plugins\\echo-music-keeper",
      defaultMusicRoot: "C:\\Users\\me\\Music",
      managedDownloadRoots: ["C:\\Archive"],
      completedDownloadPaths: ["C:\\Archive\\saved.mp3"],
    }),
    {
      cacheRoot: "C:\\Plugins\\echo-music-keeper\\cache",
      downloadRoot: "C:\\Users\\me\\Music",
      managedDownloadRoots: ["C:\\Archive"],
      completedDownloadPaths: ["C:\\Archive\\saved.mp3"],
      cacheLimitBytes: GIB,
    },
  );
});
