export const GIB = 1024 * 1024 * 1024;
export const MIN_CACHE_BYTES = 256 * 1024 * 1024;
export const MAX_CACHE_BYTES = 100 * GIB;

export const DEFAULT_SETTINGS = Object.freeze({
  autoCache: true,
  cacheRoot: "",
  downloadRoot: "",
  cacheLimitBytes: GIB,
});

const normalizePath = (value) => String(value ?? "").trim();
const clamp = (value, min, max) => Math.min(max, Math.max(min, value));
const normalizePaths = (value) => Array.isArray(value)
  ? value.map(normalizePath).filter(Boolean)
  : [];

function homeDirectory(paths) {
  const explicit = String(paths?.home ?? "").trim().replace(/[\\/]+$/, "");
  if (explicit) return explicit;
  const music = String(paths?.defaultMusicRoot ?? "").trim().replace(/[\\/]+$/, "");
  const match = /^(.*)[\\/]Music$/i.exec(music);
  return match?.[1] ?? "";
}

/** Expand a leading ~ using a known home directory. Absolute paths stay unchanged. */
export function expandHomePath(value, home) {
  const path = String(value ?? "").trim();
  if (path !== "~" && !path.startsWith("~/") && !path.startsWith("~\\")) return path;
  const root = String(home ?? "").trim().replace(/[\\/]+$/, "");
  if (!root) return path;
  if (path === "~") return root;
  const rest = path.slice(2).replace(/^[\\/]+/, "").replace(/[\\/]/g, root.includes("\\") ? "\\" : "/");
  const separator = root.includes("\\") ? "\\" : "/";
  return rest ? `${root}${separator}${rest}` : root;
}

export function normalizeSettings(value) {
  const source = value && typeof value === "object" ? value : {};
  const rawLimit = Number(source.cacheLimitBytes ?? DEFAULT_SETTINGS.cacheLimitBytes);
  return {
    autoCache: Boolean(source.autoCache ?? DEFAULT_SETTINGS.autoCache),
    cacheRoot: normalizePath(source.cacheRoot),
    downloadRoot: normalizePath(source.downloadRoot),
    cacheLimitBytes: clamp(
      Number.isFinite(rawLimit) ? Math.round(rawLimit) : DEFAULT_SETTINGS.cacheLimitBytes,
      MIN_CACHE_BYTES,
      MAX_CACHE_BYTES,
    ),
  };
}

export function toHelperConfig(settings, paths) {
  const normalized = normalizeSettings(settings);
  const separator = paths.separator === "/" ? "/" : "\\";
  const home = homeDirectory(paths);
  const expand = (value) => expandHomePath(value, home);
  return {
    cacheRoot: expand(normalized.cacheRoot) || `${paths.pluginRoot}${separator}cache`,
    downloadRoot: expand(normalized.downloadRoot || paths.defaultMusicRoot),
    managedDownloadRoots: normalizePaths(paths.managedDownloadRoots).map(expand),
    completedDownloadPaths: normalizePaths(paths.completedDownloadPaths).map(expand),
    cacheLimitBytes: normalized.cacheLimitBytes,
  };
}
