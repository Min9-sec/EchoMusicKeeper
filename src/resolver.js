import { normalizeQuality, resolveKugouSource } from "./kugou-source.js";

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

async function completeResult(ctx, lookup, quality) {
  if (lookup?.status !== "complete" || !lookup.path || typeof ctx?.fs?.getFileUrl !== "function") return null;
  const result = await ctx.fs.getFileUrl(lookup.path);
  if (!result?.ok || !result.url) return null;
  return { url: result.url, quality: cachedQuality(lookup, quality), effect: "none" };
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

/** Resolve cache aliases before contacting Kugou, then proxy exactly one actual cache key. */
export async function resolveCachedOrProxy(options) {
  const { ctx, runtime, taskStore, settings = {}, track, effect = "none", forceReload = false, warn } = options ?? {};
  const quality = normalizeQuality(options?.quality);
  const client = runtime?.client;
  try {
    if (!runtimeState(runtime) || !client || typeof client.cacheLookup !== "function" || effect !== "none" || !quality || !validTrack(track)) return null;

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
    if (alias?.status === "complete") return null;
    if (settings.autoCache === false) return null;

    const source = await resolveKugouSource(ctx, track, quality);
    if (!source) return null;
    const helperTrack = sourceTrack(track, source);
    const actual = validLookup(await client.cacheLookup({ key: helperTrack.key }), { key: helperTrack.key });
    if (!actual) {
      warn?.();
      return null;
    }
    const localActual = await completeResult(ctx, actual, source.quality);
    if (localActual) return localActual;
    if (actual?.status === "complete") return null;

    const remoteURL = source.urls[0];
    if (forceReload && await taskStore?.refreshNeedsURL?.(helperTrack.key, remoteURL)) {
      return null;
    }
    if (typeof client.createSession !== "function") return null;
    const session = await client.createSession({ track: helperTrack, remoteUrl: remoteURL });
    if (!session?.playUrl) return null;
    return { url: session.playUrl, quality: source.quality, effect: "none" };
  } catch {
    warn?.();
    return null;
  }
}

/** Build the EchoMusic audio-source contribution around the current helper runtime. */
export function createAudioResolver(ctx, runtime, settingsRef, taskStore) {
  const warn = warnOnceFactory(ctx);
  return {
    id: "echo-music-keeper",
    order: 10,
    match({ track, effect } = {}) {
      return runtimeState(runtime) && validTrack(track) && effect === "none";
    },
    async resolve({ track, quality, effect, forceReload } = {}) {
      return resolveCachedOrProxy({
        ctx,
        runtime,
        taskStore,
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
