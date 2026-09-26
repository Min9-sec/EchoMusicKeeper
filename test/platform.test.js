import assert from "node:assert/strict";
import test from "node:test";

import { DEFAULT_PLATFORM, HELPER_EXECUTABLES, detectPlatform, helperExecutable, pathSeparator } from "../src/platform.js";

test("detects Windows, macOS, and Linux hosts from runtime hints", () => {
  const cases = [
    ["host electron windows", { electron: { platform: "win32" } }, "windows"],
    ["host electron darwin", { electron: { platform: "darwin" } }, "macos"],
    ["host electron linux", { electron: { platform: "linux" } }, "linux"],
    ["node windows", { process: { platform: "win32" } }, "windows"],
    ["user agent windows", { navigator: { userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36" } }, "windows"],
    ["node darwin", { process: { platform: "darwin" } }, "macos"],
    ["user agent macintosh", { navigator: { userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36" } }, "macos"],
    ["userAgentData macos", { navigator: { userAgentData: { platform: "macOS" } } }, "macos"],
    ["platform macintel", { navigator: { platform: "MacIntel" } }, "macos"],
    ["node linux", { process: { platform: "linux" } }, "linux"],
    ["unknown host", {}, DEFAULT_PLATFORM],
    ["empty hints", { navigator: { platform: "", userAgent: "" } }, DEFAULT_PLATFORM],
  ];
  for (const [name, source, expected] of cases) {
    assert.equal(detectPlatform(source), expected, name);
  }
});

test("prefers the first source that reports a recognized platform", () => {
  assert.equal(detectPlatform({ electron: { platform: "darwin" } }, { process: { platform: "win32" } }), "macos");
  assert.equal(detectPlatform({}, { navigator: { userAgent: "Mozilla/5.0 (X11; Linux x86_64)" } }), "linux");
  assert.equal(detectPlatform({ electron: {} }, { navigator: {} }), DEFAULT_PLATFORM);
  assert.equal(detectPlatform(), DEFAULT_PLATFORM);
});

test("selects the packaged helper executable per platform", () => {
  assert.equal(helperExecutable("windows"), "bin/echo-music-keeper-helper.exe");
  assert.equal(helperExecutable("macos"), "bin/echo-music-keeper-helper-macos");
  assert.deepEqual(HELPER_EXECUTABLES, {
    windows: "bin/echo-music-keeper-helper.exe",
    macos: "bin/echo-music-keeper-helper-macos",
  });
});

test("falls back to the Windows helper for unsupported platforms", () => {
  assert.equal(helperExecutable("linux"), "bin/echo-music-keeper-helper.exe");
  assert.equal(helperExecutable(undefined), "bin/echo-music-keeper-helper.exe");
});

test("uses the native path separator per platform", () => {
  assert.equal(pathSeparator("windows"), "\\");
  assert.equal(pathSeparator("macos"), "/");
  assert.equal(pathSeparator("linux"), "/");
});
