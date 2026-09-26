/**
 * Platform detection for the bundled plugin runtime. The helper ships one
 * executable per supported operating system, so every helper interaction has to
 * agree on the platform detected here.
 */
export const DEFAULT_PLATFORM = "windows";

export const HELPER_EXECUTABLES = Object.freeze({
  windows: "bin/echo-music-keeper-helper.exe",
  macos: "bin/echo-music-keeper-helper-macos",
});

function platformHints(source) {
  const navigator = source?.navigator ?? {};
  return [
    source?.electron?.platform,
    source?.process?.platform,
    navigator.userAgentData?.platform,
    navigator.platform,
    navigator.userAgent,
  ]
    .map((value) => String(value ?? "").trim().toLowerCase())
    .filter(Boolean)
    .join(" ");
}

/**
 * Detects the host operating system from the first source that yields a
 * recognizable hint. The EchoMusic host API (`ctx.electron.platform`, one of
 * `darwin`, `win32`, `linux`) is authoritative, while `process.platform` and
 * the navigator fields keep detection working without a host context.
 */
export function detectPlatform(...sources) {
  for (const source of sources) {
    const platform = matchPlatform(platformHints(source));
    if (platform) return platform;
  }
  return DEFAULT_PLATFORM;
}

function matchPlatform(hints) {
  if (!hints) return null;
  if (/win(?:32|64|dows|nt)/.test(hints)) return "windows";
  if (/mac|darwin|iphone|ipad|ios/.test(hints)) return "macos";
  if (/linux|android|cros/.test(hints)) return "linux";
  return null;
}

/** Returns the packaged helper executable for a detected platform. */
export function helperExecutable(platform) {
  return HELPER_EXECUTABLES[platform] ?? HELPER_EXECUTABLES[DEFAULT_PLATFORM];
}

/** Returns the native path separator used for plugin-relative defaults. */
export function pathSeparator(platform) {
  return platform === "windows" ? "\\" : "/";
}
