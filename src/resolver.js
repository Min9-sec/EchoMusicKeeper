import { normalizeQuality, QUALITY_ORDER, resolveKugouSource } from "./kugou-source.js";

const HASH_PATTERN = /^[0-9a-f]{16,128}$/i;
const CANONICAL_HASH_PATTERN = /^[0-9a-f]{16,128}$/;
const CACHE_KEY_PATTERN = /^([0-9a-f]{16,128})\.(128|320|flac|high|super)\.none$/;
const ABSOLUTE_PATH_PATTERN = /^(?:[A-Za-z]:[\\/]|\\\\[^\\/]+[\\/][^\\/]+|\/)/;
const CANONICAL_QUALITIES = new Set(["128", "320", "flac", "high", "super"]);

function runtimeState(runtime) {
  const status = typeof runtime?.status === "function" ? runtime.status() : runtime?.status;
  const value = status?.value ?? status?.state ?? status;
  return value === "online" || value === "ready";
}

function validTrack(track) {
  return Boolean(
    track
    && String(track.source ?? "").toLowerCase() !== "cloud"
    && typeof track.hash === "string"
    && HASH_PATTERN.test(track.hash.trim()),
  );
}

function cachedQuality(lookup, fallback) {
  return normalizeQuality(lookup?.track?.quality)
    ?? normalizeQuality(lookup?.track?.requestedQuality)
    ?? fallback;
}

function cacheKeyDetails(key) {
  if (typeof key !== "string") return null;
  const match = CACHE_KEY_PATTERN.exec(key);
  return match ? { hash: match[1], quality: match[2] } : null;
}

function validLookupTrack(track, key, allowEmptyAlias) {
  const details = cacheKeyDetails(key);
  const recoveryAlias = track?.catalogHash === "" && track?.requestedQuality === "";
  const canonicalAlias = typeof track?.catalogHash === "string"
    && CANONICAL_HASH_PATTERN.test(track.catalogHash)
    && typeof track?.requestedQuality === "string"
    && CANONICAL_QUALITIES.has(track.requestedQuality);
  return Boolean(
    details
    && track
    && typeof track.key === "string"
    && track.key === key
    && (canonicalAlias || (allowEmptyAlias && recoveryAlias))
    && typeof track.hash === "string"
    && track.hash === details.hash
    && typeof track.quality === "string"
    && track.quality === details.quality
    && track.effect === "none",
  );
}

function validLookup(lookup, expected = {}) {
  if (!lookup || typeof lookup !== "object") return null;
  if (lookup.status === "miss") return lookup;
  if (lookup.status !== "partial" && lookup.status !== "complete") return null;
  const isAliasLookup = expected.catalogHash !== undefined || expected.requestedQuality !== undefined;
  if (!cacheKeyDetails(lookup.key) || !validLookupTrack(lookup.track, lookup.key, !isAliasLookup)) return null;
  if (expected.key !== undefined && (typeof expected.key !== "string" || lookup.key !== expected.key)) return null;
  if (expected.catalogHash !== undefined && (
    typeof expected.catalogHash !== "string"
    || !CANONICAL_HASH_PATTERN.test(expected.catalogHash)
    || lookup.track.catalogHash !== expected.catalogHash
  )) return null;
  if (expected.requestedQuality !== undefined && (
    typeof expected.requestedQuality !== "string"
    || !CANONICAL_QUALITIES.has(expected.requestedQuality)
    || lookup.track.requestedQuality !== expected.requestedQuality
  )) return null;
  if (lookup.status === "complete" && (!ABSOLUTE_PATH_PATTERN.test(String(lookup.path ?? "")))) return null;
  return lookup;
}

async function fileResult(ctx, path, quality) {
  if (!ABSOLUTE_PATH_PATTERN.test(String(path ?? "")) || typeof ctx?.fs?.getFileUrl !== "function") return null;
  const result = await ctx.fs.getFileUrl(path);
  if (!result?.ok || !result.url) return null;
  return { url: result.url, quality, effect: "none" };
}

async function completeResult(ctx, lookup, quality) {
  if (lookup?.status !== "complete" || !lookup.path) return null;
  return fileResult(ctx, lookup.path, cachedQuality(lookup, quality));
}

function downloadRecordList(downloads) {
  const records = downloads?.records;
  const value = records && typeof records === "object" && Object.prototype.hasOwnProperty.call(records, "value")
    ? records.value
    : records;
  return Array.isArray(value) ? value.filter((entry) => entry && !entry.missing && cacheKeyDetails(entry.key)) : [];
}

function trackIdentity(track) {
  return String(track?.id ?? track?.trackId ?? track?.songId ?? "").trim();
}

function sameDownload(record, track) {
  if (!ABSOLUTE_PATH_PATTERN.test(String(record?.path ?? ""))) return false;
  const id = trackIdentity(track);
  if (id && String(record.trackId ?? "") === id) return true;
  const hash = String(track?.hash ?? "").trim().toLowerCase();
  const details = cacheKeyDetails(record.key);
  return Boolean(hash && details && details.hash === hash);
}

function qualityDistance(candidate, requested) {
  if (!candidate || !requested) return QUALITY_ORDER.length + 1;
  if (candidate === requested) return 0;
  const left = QUALITY_ORDER.indexOf(candidate);
  const right = QUALITY_ORDER.indexOf(requested);
  if (left < 0 || right < 0) return QUALITY_ORDER.length + 1;
  return Math.abs(left - right);
}

function itemKey(item) {
  if (typeof item?.key === "string") return item.key;
  return typeof item?.track?.key === "string" ? item.track.key : "";
}

function itemMatchesTrack(item, catalogHash) {
  const details = cacheKeyDetails(itemKey(item));
  const hashes = [item?.track?.catalogHash, item?.track?.hash, details?.hash]
    .map((value) => String(value ?? "").trim().toLowerCase())
    .filter(Boolean);
  return hashes.includes(catalogHash);
}

async function listCache(client) {
  if (typeof client?.cacheList !== "function") return [];
  try {
    const view = await client.cacheList();
    const items = Array.isArray(view) ? view : view?.items;
    return Array.isArray(items) ? items : [];
  } catch {
    return [];
  }
}

async function downloadFallback(ctx, downloads, track, quality) {
  const records = downloadRecordList(downloads)
    .filter((record) => sameDownload(record, track))
    .sort((left, right) => qualityDistance(normalizeQuality(left.quality) ?? cacheKeyDetails(left.key)?.quality, quality)
      - qualityDistance(normalizeQuality(right.quality) ?? cacheKeyDetails(right.key)?.quality, quality));
  for (const record of records) {
    const local = await fileResult(ctx, record.path, normalizeQuality(record.quality) ?? cacheKeyDetails(record.key)?.quality ?? quality);
    if (local) return local;
  }
  return null;
}

async function localFileFallback(ctx, client, downloads, track, quality, excludeKeys) {
  const catalogHash = track.hash.trim().toLowerCase();
  const cached = (await listCache(client))
    .filter((item) => item?.status === "complete" && itemMatchesTrack(item, catalogHash) && !excludeKeys?.has(itemKey(item)))
    .map((item) => ({ kind: "cache", key: itemKey(item), quality: cacheKeyDetails(itemKey(item))?.quality }));
  const saved = downloadRecordList(downloads)
    .filter((record) => sameDownload(record, track))
    .map((record) => ({ kind: "download", record, quality: normalizeQuality(record.quality) ?? cacheKeyDetails(record.key)?.quality }));
  const candidates = [...cached, ...saved].sort((left, right) => qualityDistance(left.quality, quality) - qualityDistance(right.quality, quality));
  for (const candidate of candidates) {
    if (candidate.kind === "download") {
      const local = await fileResult(ctx, candidate.record.path, candidate.quality ?? quality);
      if (local) return local;
    } else {
      const lookup = validLookup(await client.cacheLookup({ key: candidate.key }), { key: candidate.key });
      const local = await completeResult(ctx, lookup, quality);
      if (local) return local;
    }
  }
  return null;
}

function playablePartialItem(item) {
  if (item?.status !== "partial") return false;
  const ranges = Array.isArray(item.ranges) ? item.ranges : [];
  return ranges.some((range) => Number(range?.start) === 0 && Number(range?.end) > 0);
}

async function partialFromLookup(ctx, lookup, quality) {
  if (lookup?.status !== "partial" || !ABSOLUTE_PATH_PATTERN.test(String(lookup.path ?? ""))) return null;
  const ranges = Array.isArray(lookup.ranges) ? lookup.ranges : [];
  if (ranges.length > 0 && !ranges.some((range) => Number(range?.start) === 0 && Number(range?.end) > 0)) return null;
  return fileResult(ctx, lookup.path, cachedQuality(lookup, quality));
}

async function partialFallback(ctx, client, track, quality) {
  const catalogHash = track.hash.trim().toLowerCase();
  const items = (await listCache(client))
    .filter((item) => playablePartialItem(item) && itemMatchesTrack(item, catalogHash))
    .sort((left, right) => qualityDistance(cacheKeyDetails(itemKey(left))?.quality, quality) - qualityDistance(cacheKeyDetails(itemKey(right))?.quality, quality));
  for (const item of items) {
    const key = itemKey(item);
    const lookup = validLookup(await client.cacheLookup({ key }), { key });
    const local = await partialFromLookup(ctx, lookup, quality);
    if (local) return local;
  }
  return null;
}

function resolveSourceWithTimeout(ctx, track, quality, timeoutMs) {
  const requested = Number(timeoutMs);
  const limit = Number.isFinite(requested) && requested > 0 ? requested : 8000;
  let timer;
  const sourcePromise = Promise.resolve()
    .then(() => resolveKugouSource(ctx, track, quality))
    .catch(() => null);
  const timeoutPromise = new Promise((resolve) => {
    timer = setTimeout(() => resolve(null), limit);
  });
  return Promise.race([sourcePromise, timeoutPromise]).finally(() => clearTimeout(timer));
}

function sourceTrack(track, source) {
  return {
    key: `${source.actualHash}.${source.quality}.none`,
    catalogHash: source.catalogHash,
    hash: source.actualHash,
    requestedQuality: source.requestedQuality,
    quality: source.quality,
    effect: "none",
    title: String(track?.title ?? track?.songName ?? track?.songname ?? ""),
    artist: String(track?.artist ?? track?.singerName ?? track?.singername ?? ""),
    album: String(track?.album ?? track?.albumName ?? track?.albumname ?? ""),
    extension: source.extension,
    durationSeconds: Number(track?.durationSeconds ?? track?.duration ?? 0) || 0,
  };
}

function warnOnceFactory(ctx) {
  let warned = false;
  return () => {
    if (warned) return;
    warned = true;
    ctx?.toast?.warning?.("Music cache could not resolve this song.");
  };
}

async function partialAfterFailure(ctx, client, track, quality, lookups) {
  for (const lookup of lookups) {
    const local = await partialFromLookup(ctx, lookup, quality);
    if (local) return local;
  }
  return partialFallback(ctx, client, track, quality);
}

/** Resolve cache aliases before contacting Kugou, then proxy exactly one actual cache key. */
export async function resolveCachedOrProxy(options) {
  const { ctx, runtime, taskStore, settings = {}, track, effect = "none", forceReload = false, warn, downloads, sourceTimeoutMs } = options ?? {};
  const quality = normalizeQuality(options?.quality);
  const client = runtime?.client;
  try {
    if (effect !== "none" || !quality || !validTrack(track)) return null;
    if (!runtimeState(runtime) || !client || typeof client.cacheLookup !== "function") {
      return await downloadFallback(ctx, downloads, track, quality);
    }

    const catalogHash = track.hash.trim().toLowerCase();
    const alias = validLookup(
      await client.cacheLookup({ catalogHash, requestedQuality: quality, effect: "none" }),
      { catalogHash, requestedQuality: quality },
    );
    if (!alias) {
      warn?.();
      return null;
    }
    const localAlias = await completeResult(ctx, alias, quality);
    if (localAlias) return localAlias;
    if (alias.status === "complete") {
      return await localFileFallback(ctx, client, downloads, track, quality, new Set([alias.key]));
    }

    const localBeforeNetwork = await localFileFallback(ctx, client, downloads, track, quality);
    if (localBeforeNetwork) return localBeforeNetwork;
    if (settings.autoCache === false) {
      return await partialAfterFailure(ctx, client, track, quality, [alias]);
    }

    const source = await resolveSourceWithTimeout(ctx, track, quality, sourceTimeoutMs);
    if (!source) {
      return await partialAfterFailure(ctx, client, track, quality, [alias]);
    }
    const helperTrack = sourceTrack(track, source);
    const actual = validLookup(await client.cacheLookup({ key: helperTrack.key }), { key: helperTrack.key });
    if (!actual) {
      warn?.();
      return null;
    }
    const localActual = await completeResult(ctx, actual, source.quality);
    if (localActual) return localActual;
    if (actual.status === "complete") return null;

    const remoteURL = source.urls[0];
    if (forceReload && await taskStore?.refreshNeedsURL?.(helperTrack.key, remoteURL)) {
      return null;
    }
    if (typeof client.createSession !== "function") {
      return await partialAfterFailure(ctx, client, track, quality, [actual, alias]);
    }
    const session = await client.createSession({ track: helperTrack, remoteUrl: remoteURL });
    if (!session?.playUrl) {
      return await partialAfterFailure(ctx, client, track, quality, [actual, alias]);
    }
    return { url: session.playUrl, quality: source.quality, effect: "none" };
  } catch {
    warn?.();
    return null;
  }
}

/** Build the EchoMusic audio-source contribution around the current helper runtime. */
export function createAudioResolver(ctx, runtime, settingsRef, taskStore, downloads) {
  const warn = warnOnceFactory(ctx);
  return {
    id: "echo-music-keeper",
    order: 10,
    match({ track, effect } = {}) {
      if (effect !== "none" || !validTrack(track)) return false;
      if (runtimeState(runtime)) return true;
      return downloadRecordList(downloads).some((record) => sameDownload(record, track));
    },
    async resolve({ track, quality, effect, forceReload } = {}) {
      return resolveCachedOrProxy({
        ctx,
        runtime,
        taskStore,
        downloads,
        settings: settingsRef?.value ?? {},
        track,
        quality,
        effect,
        forceReload,
        warn,
      });
    },
  };
}
