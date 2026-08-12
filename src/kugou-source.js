const HASH_PATTERN = /^[0-9a-f]{16,128}$/i;
const EXTENSIONS = new Set(["mp3", "flac", "m4a", "aac", "ogg", "opus", "wav"]);

const QUALITY_ALIASES = new Map([
  ["128", "128"], ["128k", "128"], ["128kbps", "128"], ["standard", "128"], ["normal", "128"],
  ["320", "320"], ["320k", "320"], ["320kbps", "320"], ["hq", "320"],
  ["flac", "flac"], ["lossless", "flac"],
  ["high", "high"], ["hires", "high"], ["hi-res", "high"], ["highres", "high"], ["high-res", "high"],
  ["super", "super"], ["master", "super"],
]);
const QUALITY_ORDER = ["128", "320", "flac", "high", "super"];
const MAX_CANDIDATE_NODES = 64;
const MAX_CANDIDATE_DEPTH = 8;

function validHash(value) {
  const hash = String(value ?? "").trim();
  return HASH_PATTERN.test(hash) ? hash.toLowerCase() : "";
}

function qualityPair(value) {
  const quality = normalizeQuality(value?.quality ?? value?.bitrate ?? value?.audioQuality ?? value?.audio_quality);
  const hash = validHash(value?.hash ?? value?.fileHash ?? value?.file_hash);
  return quality && hash ? { hash, quality } : null;
}

function collectCandidatePairs(value, { includeCatalogHash, includeQualityPair }) {
  const pairs = [];
  const queue = [];
  const seen = new Set();
  const add = (candidate, depth) => {
    if (!candidate || typeof candidate !== "object" || seen.has(candidate) || queue.length >= MAX_CANDIDATE_NODES) return;
    seen.add(candidate);
    queue.push({ candidate, depth });
  };
  const addPair = (candidate) => {
    if (candidate && !pairs.some((pair) => pair.hash === candidate.hash && pair.quality === candidate.quality)) pairs.push(candidate);
  };
  add(value, 0);
  for (let index = 0; index < queue.length; index += 1) {
    const { candidate, depth } = queue[index];
    if (Array.isArray(candidate)) {
      if (depth < MAX_CANDIDATE_DEPTH) {
        const limit = Math.min(candidate.length, MAX_CANDIDATE_NODES - queue.length);
        for (let child = 0; child < limit; child += 1) add(candidate[child], depth + 1);
      }
      continue;
    }
    if (includeQualityPair) addPair(qualityPair(candidate));
    for (const quality of QUALITY_ORDER) {
      if (!includeCatalogHash && quality === "128") continue;
      const names = quality === "128"
        ? ["hash", "fileHash", "file_hash"]
        : [`${quality}Hash`, `${quality}_hash`, `hash${quality}`];
      for (const name of names) {
        const hash = validHash(candidate[name]);
        if (hash) addPair({ hash, quality });
      }
    }
    if (depth < MAX_CANDIDATE_DEPTH) {
      for (const key of ["data", "relateGoods", "relate_goods"]) add(candidate[key], depth + 1);
    }
  }
  return pairs;
}

function challengeId(payload) {
  for (const key of ["eventId", "event_id", "ssaCode", "ssa_code"]) {
    const value = payload?.[key];
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

function hasInvalidChallengeId(payload) {
  return ["eventId", "event_id", "ssaCode", "ssa_code"].some((key) => (
    Object.prototype.hasOwnProperty.call(payload ?? {}, key)
    && (typeof payload[key] !== "string" || !payload[key].trim())
  ));
}

function responseObjects(payload) {
  if (!payload || typeof payload !== "object") return [];
  const values = [payload];
  if (payload.data && typeof payload.data === "object" && !Array.isArray(payload.data)) values.push(payload.data);
  return values;
}

function errorCode(value) {
  const code = Number(value?.error_code ?? value?.errorCode ?? value?.errcode);
  return Number.isFinite(code) ? code : null;
}

function hasStatus(value) {
  return Object.prototype.hasOwnProperty.call(value ?? {}, "status");
}

function failedStatus(value) {
  return value?.status === 0 || value?.status === "0";
}

function successfulStatus(value) {
  return !hasStatus(value) || value?.status === 1 || value?.status === "1";
}

function missingStatusSsaChallenge(value) {
  const ssaCode = typeof value?.ssaCode === "string" ? value.ssaCode.trim()
    : typeof value?.ssa_code === "string" ? value.ssa_code.trim() : "";
  return Boolean(
    ssaCode
    && !hasStatus(value)
    && (errorCode(value) === null || errorCode(value) === 0)
    && value?.success !== false
    && !value?.error,
  );
}

function hasBusinessFailure(payload) {
  return responseObjects(payload).some((value) => {
    const code = errorCode(value);
    return failedStatus(value)
      || !successfulStatus(value)
      || (code !== null && code !== 0)
      || value.success === false
      || (typeof value.error === "string" && value.error.trim().length > 0)
      || value.error === true
      || hasInvalidChallengeId(value);
  });
}

function challengeDetails(payload) {
  return responseObjects(payload).find((value) => {
    const code = errorCode(value);
    return !hasInvalidChallengeId(value)
      && Boolean(challengeId(value))
      && (code === 20028 || failedStatus(value) || missingStatusSsaChallenge(value));
  }) ?? null;
}

async function requestWithVerification(ctx, requestOnce) {
  const first = await requestOnce();
  const challenge = challengeDetails(first);
  if (!challenge) return hasBusinessFailure(first) ? null : first;
  const verify = ctx?.kugouVerification?.request;
  if (typeof verify !== "function") return null;
  const verified = await verify(challengeId(challenge));
  if (!verified?.ok) return null;
  const retry = await requestOnce();
  return challengeDetails(retry) || hasBusinessFailure(retry) ? null : retry;
}

function isHttpURL(value) {
  if (typeof value !== "string" || !value.trim()) return "";
  try {
    const url = new URL(value);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : "";
  } catch {
    return "";
  }
}

function pushURL(target, value) {
  const values = Array.isArray(value) ? value : [value];
  for (const item of values) {
    const url = isHttpURL(item);
    if (url && !target.includes(url)) target.push(url);
  }
}

function extension(value) {
  const text = String(value ?? "").trim().toLowerCase().replace(/^\./, "");
  return EXTENSIONS.has(text) ? text : "";
}

function extensionFromURL(value) {
  try {
    const pathname = new URL(value).pathname;
    const match = /\.([a-z0-9]+)$/i.exec(pathname);
    return extension(match?.[1]);
  } catch {
    return "";
  }
}

function sourceExtension(payload, urls) {
  const candidates = [payload, payload?.data];
  for (const value of candidates) {
    if (!value || typeof value !== "object") continue;
    for (const key of ["extension", "ext", "fileExt", "file_ext", "audioExt", "audio_ext"]) {
      const result = extension(value[key]);
      if (result) return result;
    }
  }
  for (const url of urls) {
    const result = extensionFromURL(url);
    if (result) return result;
  }
  return "mp3";
}

/** Convert public and legacy host quality labels to the helper's cache key set. */
export function normalizeQuality(value) {
  const normalized = String(value ?? "").trim().toLowerCase().replace(/\s+/g, "");
  return QUALITY_ALIASES.get(normalized) ?? null;
}

/** Select a validated actual hash/quality pair, preferring current privilege metadata. */
export function selectQualityHash(track, privilegePayload, requestedQuality) {
  const quality = normalizeQuality(requestedQuality);
  if (!quality) return null;
  const candidates = [
    ...collectCandidatePairs(privilegePayload, { includeCatalogHash: true, includeQualityPair: true }),
    ...collectCandidatePairs(track?.relateGoods ?? track?.relate_goods, { includeCatalogHash: true, includeQualityPair: true }),
    ...collectCandidatePairs(track, { includeCatalogHash: false, includeQualityPair: false }),
  ];
  const exact = candidates.find((candidate) => candidate.quality === quality);
  if (exact) return exact;
  const requestedIndex = QUALITY_ORDER.indexOf(quality);
  for (let index = requestedIndex - 1; index >= 0; index -= 1) {
    const downgraded = candidates.find((candidate) => candidate.quality === QUALITY_ORDER[index]);
    if (downgraded) return downgraded;
  }
  const catalogHash = validHash(track?.hash);
  if (catalogHash) {
    return { hash: catalogHash, quality: normalizeQuality(track?.quality ?? track?.audioQuality ?? track?.audio_quality) ?? "128" };
  }
  return candidates[0] ?? null;
}

/** Pull usable Kugou stream endpoints from the host response without accepting non-HTTP schemes. */
export function extractSongURLs(payload) {
  const urls = [];
  for (const value of [payload, payload?.data]) {
    if (!value || typeof value !== "object") continue;
    pushURL(urls, value.url);
    pushURL(urls, value.backup_url);
    pushURL(urls, value.backupUrl);
  }
  return urls;
}

/** Resolve validated URLs for one exact Kugou hash/quality pair. */
export async function resolveKugouTrackURLs(ctx, hash, quality) {
  const actualHash = validHash(hash);
  const actualQuality = normalizeQuality(quality);
  const music = ctx?.kugou?.music;
  if (!actualHash || !actualQuality || typeof music?.getSongUrl !== "function") return null;
  const payload = await requestWithVerification(ctx, () => music.getSongUrl(actualHash, actualQuality));
  if (!payload) return null;
  const urls = extractSongURLs(payload);
  if (!urls.length) return null;
  return { urls, extension: sourceExtension(payload, urls) };
}

/** Resolve one validated Kugou playback source, retrying one verified security challenge at most once. */
export async function resolveKugouSource(ctx, track, requestedQuality) {
  const catalogHash = validHash(track?.hash);
  const quality = normalizeQuality(requestedQuality);
  const music = ctx?.kugou?.music;
  if (!catalogHash || !quality || typeof music?.getSongPrivilegeLite !== "function" || typeof music?.getSongUrl !== "function") return null;

  const privilege = await requestWithVerification(
    ctx,
    () => music.getSongPrivilegeLite(catalogHash, track?.albumId),
  );
  if (!privilege) return null;
  const actual = selectQualityHash(track, privilege, quality);
  if (!actual) return null;
  const resolved = await resolveKugouTrackURLs(ctx, actual.hash, actual.quality);
  if (!resolved) return null;
  return {
    catalogHash,
    actualHash: actual.hash,
    requestedQuality: quality,
    quality: actual.quality,
    urls: resolved.urls,
    extension: resolved.extension,
  };
}
