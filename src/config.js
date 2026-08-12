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
  return {
    cacheRoot: normalized.cacheRoot || `${paths.pluginRoot}\\cache`,
    downloadRoot: normalized.downloadRoot || paths.defaultMusicRoot,
    managedDownloadRoots: normalizePaths(paths.managedDownloadRoots),
    completedDownloadPaths: normalizePaths(paths.completedDownloadPaths),
    cacheLimitBytes: normalized.cacheLimitBytes,
  };
}
