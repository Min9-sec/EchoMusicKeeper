// src/config.js
var GIB = 1024 * 1024 * 1024;
var MIN_CACHE_BYTES = 256 * 1024 * 1024;
var MAX_CACHE_BYTES = 100 * GIB;
var DEFAULT_SETTINGS = Object.freeze({
  autoCache: true,
  cacheRoot: "",
  downloadRoot: "",
  cacheLimitBytes: GIB
});
var normalizePath = (value) => String(value ?? "").trim();
var clamp = (value, min, max) => Math.min(max, Math.max(min, value));
var normalizePaths = (value) => Array.isArray(value) ? value.map(normalizePath).filter(Boolean) : [];
function normalizeSettings(value) {
  const source = value && typeof value === "object" ? value : {};
  const rawLimit = Number(source.cacheLimitBytes ?? DEFAULT_SETTINGS.cacheLimitBytes);
  return {
    autoCache: Boolean(source.autoCache ?? DEFAULT_SETTINGS.autoCache),
    cacheRoot: normalizePath(source.cacheRoot),
    downloadRoot: normalizePath(source.downloadRoot),
    cacheLimitBytes: clamp(
      Number.isFinite(rawLimit) ? Math.round(rawLimit) : DEFAULT_SETTINGS.cacheLimitBytes,
      MIN_CACHE_BYTES,
      MAX_CACHE_BYTES
    )
  };
}
function toHelperConfig(settings, paths) {
  const normalized = normalizeSettings(settings);
  const separator = paths.separator === "/" ? "/" : "\\";
  return {
    cacheRoot: normalized.cacheRoot || `${paths.pluginRoot}${separator}cache`,
    downloadRoot: normalized.downloadRoot || paths.defaultMusicRoot,
    managedDownloadRoots: normalizePaths(paths.managedDownloadRoots),
    completedDownloadPaths: normalizePaths(paths.completedDownloadPaths),
    cacheLimitBytes: normalized.cacheLimitBytes
  };
}

// src/kugou-source.js
var HASH_PATTERN = /^[0-9a-f]{16,128}$/i;
var EXTENSIONS = /* @__PURE__ */ new Set(["mp3", "flac", "m4a", "aac", "ogg", "opus", "wav"]);
var QUALITY_ALIASES = /* @__PURE__ */ new Map([
  ["128", "128"],
  ["128k", "128"],
  ["128kbps", "128"],
  ["standard", "128"],
  ["normal", "128"],
  ["320", "320"],
  ["320k", "320"],
  ["320kbps", "320"],
  ["hq", "320"],
  ["flac", "flac"],
  ["lossless", "flac"],
  ["high", "high"],
  ["hires", "high"],
  ["hi-res", "high"],
  ["highres", "high"],
  ["high-res", "high"],
  ["super", "super"],
  ["master", "super"]
]);
var QUALITY_ORDER = ["128", "320", "flac", "high", "super"];
var MAX_CANDIDATE_NODES = 64;
var MAX_CANDIDATE_DEPTH = 8;
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
  const seen = /* @__PURE__ */ new Set();
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
      const names = quality === "128" ? ["hash", "fileHash", "file_hash"] : [`${quality}Hash`, `${quality}_hash`, `hash${quality}`];
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
  return ["eventId", "event_id", "ssaCode", "ssa_code"].some((key) => Object.prototype.hasOwnProperty.call(payload ?? {}, key) && (typeof payload[key] !== "string" || !payload[key].trim()));
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
  const ssaCode = typeof value?.ssaCode === "string" ? value.ssaCode.trim() : typeof value?.ssa_code === "string" ? value.ssa_code.trim() : "";
  return Boolean(
    ssaCode && !hasStatus(value) && (errorCode(value) === null || errorCode(value) === 0) && value?.success !== false && !value?.error
  );
}
function hasBusinessFailure(payload) {
  return responseObjects(payload).some((value) => {
    const code = errorCode(value);
    return failedStatus(value) || !successfulStatus(value) || code !== null && code !== 0 || value.success === false || typeof value.error === "string" && value.error.trim().length > 0 || value.error === true || hasInvalidChallengeId(value);
  });
}
function challengeDetails(payload) {
  return responseObjects(payload).find((value) => {
    const code = errorCode(value);
    return !hasInvalidChallengeId(value) && Boolean(challengeId(value)) && (code === 20028 || failedStatus(value) || missingStatusSsaChallenge(value));
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
function normalizeQuality(value) {
  const normalized = String(value ?? "").trim().toLowerCase().replace(/\s+/g, "");
  return QUALITY_ALIASES.get(normalized) ?? null;
}
function selectQualityHash(track, privilegePayload, requestedQuality) {
  const quality = normalizeQuality(requestedQuality);
  if (!quality) return null;
  const candidates = [
    ...collectCandidatePairs(privilegePayload, { includeCatalogHash: true, includeQualityPair: true }),
    ...collectCandidatePairs(track?.relateGoods ?? track?.relate_goods, { includeCatalogHash: true, includeQualityPair: true }),
    ...collectCandidatePairs(track, { includeCatalogHash: false, includeQualityPair: false })
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
function extractSongURLs(payload) {
  const urls = [];
  for (const value of [payload, payload?.data]) {
    if (!value || typeof value !== "object") continue;
    pushURL(urls, value.url);
    pushURL(urls, value.backup_url);
    pushURL(urls, value.backupUrl);
  }
  return urls;
}
async function resolveKugouTrackURLs(ctx, hash, quality) {
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
async function resolveKugouSource(ctx, track, requestedQuality) {
  const catalogHash = validHash(track?.hash);
  const quality = normalizeQuality(requestedQuality);
  const music = ctx?.kugou?.music;
  if (!catalogHash || !quality || typeof music?.getSongPrivilegeLite !== "function" || typeof music?.getSongUrl !== "function") return null;
  const privilege = await requestWithVerification(
    ctx,
    () => music.getSongPrivilegeLite(catalogHash, track?.albumId)
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
    extension: resolved.extension
  };
}

// src/ui/download-dialog.js
var qualities = ["128", "320", "flac", "high", "super"];
function hostComponent(ctx, name, fallback) {
  const loader = ctx?.ui?.components?.[name];
  return typeof ctx?.vue?.defineAsyncComponent === "function" && typeof loader === "function" ? ctx.vue.defineAsyncComponent(loader) : fallback;
}
function createDownloadDialog(ctx, controller, track, options = {}) {
  const vue = ctx?.vue ?? {};
  const h = vue.h ?? ((type, props, children) => ({ type, props, children }));
  const selected = vue.ref?.(normalizeQuality(ctx?.stores?.player?.currentAudioQualityOverride ?? ctx?.stores?.settings?.defaultAudioQuality) ?? "320") ?? { value: "320" };
  const available = new Set((options.availableQualities ?? qualities).map(normalizeQuality).filter(Boolean));
  let close = () => {
  };
  const component = {
    name: "EchoMusicKeeperDownloadDialog",
    setup() {
      const Button = hostComponent(ctx, "Button", "button");
      const Select = hostComponent(ctx, "Select", "select");
      const Icon = vue.resolveComponent?.("Icon");
      const submitting = vue.ref?.(false) ?? { value: false };
      const confirm = async () => {
        if (submitting.value) return;
        submitting.value = true;
        try {
          const task = await controller.request(track, selected.value);
          if (task) close();
        } finally {
          submitting.value = false;
        }
      };
      return () => h("section", { class: "echo-music-keeper-dialog", role: "dialog", "aria-label": "\u4E0B\u8F7D\u6B4C\u66F2", "aria-busy": submitting.value }, [
        h("header", { class: "echo-music-keeper-dialog__header" }, [h("h3", "\u4E0B\u8F7D\u6B4C\u66F2"), Icon ? h(Icon, { icon: ctx?.icons?.iconArrowBarToDown ?? "tabler:download", width: 20, height: 20 }) : null]),
        h("p", `${track?.title ?? track?.songName ?? ""} ${track?.artist ?? track?.singerName ?? ""}`.trim()),
        h(Select, { modelValue: selected.value, disabled: submitting.value, "onUpdate:modelValue": (value) => {
          selected.value = normalizeQuality(value) ?? selected.value;
        }, options: qualities.map((quality) => ({ label: quality, value: quality, disabled: !available.has(quality) })) }),
        h("footer", { class: "echo-music-keeper-dialog__actions" }, [
          h(Button, { disabled: submitting.value, onClick: close }, () => "\u53D6\u6D88"),
          h(Button, { type: "primary", loading: submitting.value, disabled: submitting.value, onClick: confirm }, () => submitting.value ? "\u6B63\u5728\u89E3\u6790\u2026" : "\u4E0B\u8F7D")
        ])
      ]);
    }
  };
  const dispose = ctx?.ui?.teleport?.(component, options.teleportOptions) ?? (() => {
  });
  close = () => {
    dispose?.();
  };
  return Object.freeze({ close, component });
}

// src/downloads.js
var STORAGE_KEY = "echo-music-keeper-downloads";
var PENDING_STORAGE_KEY = "echo-music-keeper-download-pending";
var BATCH_SIZE = 8;
var keyPattern = /^([0-9a-f]{16,128})\.(128|320|flac|high|super)\.none$/;
var ref = (ctx, value) => typeof ctx?.vue?.ref === "function" ? ctx.vue.ref(value) : { value };
var clientFor = (runtime) => runtime?.client ?? null;
var parentPath = (path) => String(path ?? "").replace(/[\\/][^\\/]*$/, "");
var trackId = (track) => String(track?.id ?? track?.trackId ?? track?.songId ?? "");
function normalizeRecord(value) {
  if (!value || typeof value !== "object" || !keyPattern.test(String(value.key))) return null;
  const path = String(value.path ?? "").trim();
  if (!path) return null;
  return {
    key: String(value.key),
    trackId: String(value.trackId ?? ""),
    title: String(value.title ?? ""),
    artist: String(value.artist ?? ""),
    quality: String(value.quality ?? ""),
    path,
    size: Math.max(0, Number(value.size) || 0),
    completedAt: Math.max(0, Number(value.completedAt) || 0),
    ...value.missing ? { missing: true } : {}
  };
}
function recordFromTask(task, fallback) {
  const outputPath = String(task?.outputPath ?? "").trim();
  const helperTrack2 = task?.track ?? {};
  if (!outputPath || !keyPattern.test(String(helperTrack2.key ?? fallback.key))) return null;
  return normalizeRecord({
    key: helperTrack2.key ?? fallback.key,
    trackId: fallback.trackId,
    title: helperTrack2.title ?? fallback.title,
    artist: helperTrack2.artist ?? fallback.artist,
    quality: helperTrack2.quality ?? fallback.quality,
    path: outputPath,
    size: task?.totalBytes ?? task?.downloadedBytes ?? 0,
    completedAt: Date.now()
  });
}
function normalizePending(value) {
  if (!value || typeof value !== "object" || !String(value.id ?? "").trim() || !keyPattern.test(String(value.key ?? ""))) return null;
  return {
    id: String(value.id),
    key: String(value.key),
    trackId: String(value.trackId ?? ""),
    title: String(value.title ?? ""),
    artist: String(value.artist ?? ""),
    quality: String(value.quality ?? "")
  };
}
function helperTrack(track, source) {
  return {
    key: `${source.actualHash}.${source.quality}.none`,
    catalogHash: source.catalogHash,
    hash: source.actualHash,
    requestedQuality: source.requestedQuality,
    quality: source.quality,
    effect: "none",
    title: String(track?.name ?? track?.songName ?? track?.title ?? ""),
    artist: String(track?.artist ?? track?.singerName ?? ""),
    album: String(track?.album ?? track?.albumName ?? ""),
    extension: source.extension,
    durationSeconds: Number(track?.durationSeconds ?? track?.duration ?? 0) || 0
  };
}
function createDownloadController(ctx, runtime, settingsRef, options = {}) {
  const records = ref(ctx, []);
  const pending = /* @__PURE__ */ new Map();
  let activeDialog = null;
  let disposed = false;
  let unsubscribeSnapshots = null;
  let recordOperation = Promise.resolve();
  function enqueueRecordOperation(work) {
    const next = recordOperation.then(async () => {
      if (!disposed) return work();
      return void 0;
    }, async () => {
      if (!disposed) return work();
      return void 0;
    });
    recordOperation = next.catch(() => void 0);
    return next;
  }
  const ready = Promise.all([
    Promise.resolve(ctx?.storage?.get?.(STORAGE_KEY)),
    Promise.resolve(ctx?.storage?.get?.(PENDING_STORAGE_KEY))
  ]).then(async ([stored, savedPending]) => {
    if (disposed) return;
    records.value = (Array.isArray(stored) ? stored : []).map(normalizeRecord).filter(Boolean);
    for (const entry of Array.isArray(savedPending) ? savedPending : []) {
      const normalized = normalizePending(entry);
      if (normalized) pending.set(normalized.id, normalized);
    }
    if (!disposed) await syncHelper();
    if (!disposed) await reconcileFiles();
  }).catch(() => void 0);
  function paths() {
    const configured = String(settingsRef?.value?.downloadRoot ?? "").trim();
    return [...new Set([...records.value.map((entry) => parentPath(entry.path)), configured].filter(Boolean))];
  }
  async function syncHelper() {
    if (disposed) return;
    const client = clientFor(runtime);
    if (!client?.updateConfig) return;
    const settings = settingsRef?.value ?? {};
    const config = toHelperConfig(settings, {
      pluginRoot: runtime?.pluginRoot ?? "",
      defaultMusicRoot: runtime?.defaultMusicRoot ?? "",
      separator: runtime?.pathSeparator ?? "\\",
      managedDownloadRoots: paths(),
      completedDownloadPaths: records.value.filter((entry) => !entry.missing).map((entry) => entry.path)
    });
    await client.updateConfig(config).catch(() => void 0);
  }
  async function persistRecords() {
    if (disposed) return;
    const plain = records.value.map(({ missing, ...entry }) => ({ ...entry }));
    await ctx?.storage?.set?.(STORAGE_KEY, plain);
    if (disposed) return;
    await syncHelper();
  }
  async function persistPending() {
    if (disposed) return;
    await ctx?.storage?.set?.(PENDING_STORAGE_KEY, [...pending.values()].map((entry) => ({ ...entry })));
  }
  async function saveCompleted(task, fallback, taskID) {
    if (disposed) return;
    const complete = recordFromTask(task, fallback);
    if (!complete) return;
    records.value = [...records.value.filter((entry) => entry.path !== complete.path), complete];
    if (taskID) pending.delete(taskID);
    await persistRecords();
    if (disposed) return;
    await persistPending();
  }
  async function reconcileTasks(currentTasks) {
    if (disposed || !pending.size || !Array.isArray(currentTasks)) return;
    let changed = false;
    for (const [id, fallback] of [...pending]) {
      const task = currentTasks.find((entry) => entry?.id === id);
      if (task?.state === "completed") {
        await saveCompleted(task, fallback, id);
        if (disposed) return;
        changed = true;
      } else if (task?.state === "canceled") {
        pending.delete(id);
        changed = true;
      }
    }
    if (changed && !disposed) await persistPending();
  }
  async function reconcileFiles() {
    const next = records.value.map((entry) => ({ ...entry }));
    for (let offset = 0; offset < next.length; offset += BATCH_SIZE) {
      await Promise.all(next.slice(offset, offset + BATCH_SIZE).map(async (entry) => {
        try {
          if ((await ctx?.fs?.getFileUrl?.(entry.path))?.ok) delete entry.missing;
          else entry.missing = true;
        } catch {
          entry.missing = true;
        }
      }));
      if (disposed) return;
    }
    if (!disposed) records.value = next;
  }
  async function trackPending(task, fallback) {
    if (disposed || !task?.id) return;
    pending.set(String(task.id), normalizePending({ id: task.id, ...fallback }));
    await persistPending();
  }
  function effectiveDownloadRoot() {
    return String(settingsRef?.value?.downloadRoot || runtime?.defaultMusicRoot || "").trim();
  }
  function reportFailure(message) {
    ctx?.toast?.warning?.(message);
    return null;
  }
  async function reportCreatedTask(task) {
    await Promise.resolve(options.taskStore?.refresh?.({ force: true })).catch(() => void 0);
    const root = effectiveDownloadRoot();
    ctx?.toast?.success?.(`\u5DF2\u52A0\u5165\u4E0B\u8F7D\u4EFB\u52A1\uFF0C\u53EF\u5728\u201C\u7F13\u5B58\u4E0E\u4E0B\u8F7D\u201D\u4E2D\u67E5\u770B\u8FDB\u5EA6${root ? `\u3002\u4E0B\u8F7D\u76EE\u5F55\uFF1A${root}` : "\u3002"}`);
    return task;
  }
  async function request(track, requestedQuality) {
    await ready;
    if (disposed) return null;
    const client = clientFor(runtime);
    const quality = normalizeQuality(requestedQuality);
    const catalogHash = String(track?.hash ?? "").trim().toLowerCase();
    if (!client?.cacheLookup || !client?.createDownload) return reportFailure("\u4E0B\u8F7D\u670D\u52A1\u672A\u542F\u52A8\uFF0C\u8BF7\u68C0\u67E5\u63D2\u4EF6\u662F\u5426\u5DF2\u83B7\u51C6\u542F\u52A8\u7F13\u5B58\u670D\u52A1\u3002");
    if (!quality || !/^[0-9a-f]{16,128}$/.test(catalogHash)) return reportFailure("\u5F53\u524D\u6B4C\u66F2\u4E0D\u652F\u6301\u4E0B\u8F7D\u3002");
    const fallback = { trackId: trackId(track), title: String(track?.title ?? track?.songName ?? ""), artist: String(track?.artist ?? track?.singerName ?? ""), quality };
    try {
      const alias = await client.cacheLookup({ catalogHash, requestedQuality: quality, effect: "none" }).catch(() => null);
      if (disposed) return null;
      if (alias?.key && (alias.status === "complete" || alias.status === "partial")) {
        const task2 = await client.createDownload({ key: alias.key }).catch(() => null);
        if (disposed) return null;
        if (!task2) return reportFailure("\u521B\u5EFA\u4E0B\u8F7D\u4EFB\u52A1\u5931\u8D25\uFF0C\u8BF7\u7A0D\u540E\u91CD\u8BD5\u3002");
        if (task2.state === "completed") await enqueueRecordOperation(() => saveCompleted(task2, { ...fallback, key: alias.key }, task2.id));
        else await trackPending(task2, { ...fallback, key: alias.key });
        return reportCreatedTask(task2);
      }
      const source = await resolveKugouSource(ctx, track, quality).catch(() => null);
      if (disposed) return null;
      if (!source) return reportFailure("\u65E0\u6CD5\u83B7\u53D6\u6B4C\u66F2\u4E0B\u8F7D\u5730\u5740\uFF0C\u8BF7\u68C0\u67E5\u767B\u5F55\u72B6\u6001\u6216\u7A0D\u540E\u91CD\u8BD5\u3002");
      const helper = helperTrack(track, source);
      const task = await client.createDownload({ track: helper, remoteUrl: source.urls[0] }).catch(() => null);
      if (disposed) return null;
      if (!task) return reportFailure("\u521B\u5EFA\u4E0B\u8F7D\u4EFB\u52A1\u5931\u8D25\uFF0C\u8BF7\u7A0D\u540E\u91CD\u8BD5\u3002");
      if (task.state === "completed") await enqueueRecordOperation(() => saveCompleted(task, { ...fallback, key: helper.key, quality: helper.quality }, task.id));
      else await trackPending(task, { ...fallback, key: helper.key, quality: helper.quality });
      return reportCreatedTask(task);
    } catch {
      return reportFailure("\u4E0B\u8F7D\u4EFB\u52A1\u521B\u5EFA\u5931\u8D25\uFF0C\u8BF7\u7A0D\u540E\u91CD\u8BD5\u3002");
    }
  }
  async function reconcile() {
    await ready;
    if (disposed) return records.value;
    await enqueueRecordOperation(reconcileFiles);
    return records.value;
  }
  async function reconcileTaskSnapshot(currentTasks) {
    await ready;
    if (disposed) return;
    await enqueueRecordOperation(() => reconcileTasks(currentTasks));
  }
  async function remove(entry) {
    await ready;
    if (disposed || !entry?.path || !await ctx?.dialog?.confirm?.({ title: "\u5220\u9664\u4E0B\u8F7D", content: "\u786E\u5B9A\u5220\u9664\u6B64\u4E0B\u8F7D\u6587\u4EF6\u5417\uFF1F" })) return false;
    if (disposed) return false;
    const client = clientFor(runtime);
    if (!client?.deleteDownload) return false;
    try {
      await client.deleteDownload(entry.path);
    } catch (error) {
      ctx?.toast?.warning?.(error instanceof Error && error.message ? error.message : "\u5220\u9664\u4E0B\u8F7D\u5931\u8D25\uFF0C\u8BF7\u7A0D\u540E\u91CD\u8BD5\u3002");
      return false;
    }
    if (disposed) return false;
    await enqueueRecordOperation(async () => {
      records.value = records.value.filter((record) => record.path !== entry.path);
      await persistRecords();
    });
    return true;
  }
  async function reveal(entry) {
    await ready;
    if (disposed || !entry?.path) return false;
    const client = clientFor(runtime);
    if (!client?.revealFile) return false;
    try {
      await client.revealFile({ kind: "select", path: entry.path });
      return true;
    } catch (error) {
      ctx?.toast?.warning?.(error instanceof Error && error.message ? error.message : "\u65E0\u6CD5\u6253\u5F00\u4E0B\u8F7D\u4F4D\u7F6E\u3002");
      return false;
    }
  }
  function supports(track) {
    return /^[0-9a-f]{16,128}$/i.test(String(track?.hash ?? "").trim()) && String(track?.source ?? "").toLowerCase() !== "cloud";
  }
  function registerContextMenus(onRegistered) {
    if (typeof ctx?.ui?.addSongContextMenuItem !== "function") return [];
    void reconcile();
    const find = (track) => records.value.find((entry) => entry.trackId === trackId(track) && !entry.missing);
    const availableQualities = (track) => {
      const values = Array.isArray(track?.relateGoods) ? track.relateGoods : Array.isArray(track?.relate_goods) ? track.relate_goods : [];
      const found = values.map((entry) => normalizeQuality(entry?.quality ?? entry?.bitrate)).filter(Boolean);
      return found.length ? found : [normalizeQuality(track?.quality) ?? "320"];
    };
    const openDialog = (track) => {
      activeDialog?.close?.();
      activeDialog = createDownloadDialog(ctx, { request }, track, { availableQualities: availableQualities(track) });
    };
    const disposers = [];
    const register = (item) => {
      const dispose = ctx.ui.addSongContextMenuItem(item);
      if (typeof dispose !== "function") return;
      disposers.push(dispose);
      onRegistered?.(dispose);
    };
    register({ id: "download", label: "\u4E0B\u8F7D\u6B4C\u66F2", visible: supports, onSelect: openDialog });
    register({ id: "reveal-download", label: "\u6253\u5F00\u4E0B\u8F7D\u4F4D\u7F6E", visible: (track) => Boolean(find(track)), onSelect: (track) => reveal(find(track)) });
    register({ id: "delete-download", label: "\u5220\u9664\u4E0B\u8F7D", visible: (track) => Boolean(find(track)), onSelect: (track) => remove(find(track)) });
    return disposers;
  }
  if (typeof options.taskStore?.subscribe === "function") {
    try {
      unsubscribeSnapshots = options.taskStore.subscribe((tasks) => {
        void reconcileTaskSnapshot(tasks).catch(() => void 0);
      });
    } catch {
      unsubscribeSnapshots = null;
    }
  }
  return Object.freeze({ request, remove, reveal, records, reconcile, reconcileTaskSnapshot, registerContextMenus, dispose() {
    disposed = true;
    activeDialog?.close?.();
    activeDialog = null;
    unsubscribeSnapshots?.();
    unsubscribeSnapshots = null;
    pending.clear();
  } });
}

// src/helper-client.js
var HelperClientError = class extends Error {
  constructor(message, { code = "helper_error", status = 0 } = {}) {
    super(message);
    this.name = "HelperClientError";
    this.code = code;
    this.status = status;
  }
};
var jsonHeaders = Object.freeze({ "Content-Type": "application/json" });
function endpoint(baseUrl, path) {
  return `${String(baseUrl).replace(/\/+$/, "")}${path}`;
}
async function responseBody(response) {
  if (response.status === 204) return void 0;
  const text = await response.text();
  if (!text) return void 0;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}
function createHelperClient({ baseUrl, token, fetchImpl = globalThis.fetch }) {
  if (typeof fetchImpl !== "function") throw new TypeError("fetchImpl must be a function");
  async function request(path, options = {}) {
    const method = options.method ?? "GET";
    const signal = options.signal ?? AbortSignal.timeout(options.timeoutMs ?? 1e4);
    const headers = { Authorization: `Bearer ${token}` };
    let body;
    if (options.json !== void 0) {
      Object.assign(headers, jsonHeaders);
      body = JSON.stringify(options.json);
    }
    const response = await fetchImpl(endpoint(baseUrl, path), { method, headers, body, signal });
    const payload = await responseBody(response);
    if (!response.ok) {
      const structured = payload && typeof payload === "object" ? payload : {};
      throw new HelperClientError(
        structured.message || typeof payload === "string" && payload || `helper request failed (${response.status})`,
        { code: structured.code, status: response.status }
      );
    }
    return payload;
  }
  const taskPath = (id, action) => `/v1/tasks/${encodeURIComponent(id)}/${action}`;
  return Object.freeze({
    health: (options) => request("/v1/health", options),
    updateConfig: (config, options) => request("/v1/config", { ...options, method: "PUT", json: config }),
    cacheList: (options) => request("/v1/cache", options),
    cacheLookup: (lookup, options) => request("/v1/cache/lookup", { ...options, method: "POST", json: lookup }),
    deleteCache: (key, options) => request(`/v1/cache/${encodeURIComponent(key)}`, { ...options, method: "DELETE" }),
    clearCache: (options) => request("/v1/cache/clear", { ...options, method: "POST" }),
    migrateCache: (migration, options) => request("/v1/cache/migrate", { ...options, method: "POST", json: migration }),
    createSession: (session, options) => request("/v1/proxy/sessions", { ...options, method: "POST", json: session }),
    createDownload: (download, options) => request("/v1/downloads", { ...options, method: "POST", json: download }),
    deleteDownload: (path, options) => request("/v1/downloads", { ...options, method: "DELETE", json: { path } }),
    listTasks: (options) => request("/v1/tasks", options),
    pauseTask: (id, options) => request(taskPath(id, "pause"), { ...options, method: "POST" }),
    resumeTask: (id, options) => request(taskPath(id, "resume"), { ...options, method: "POST" }),
    cancelTask: (id, options) => request(taskPath(id, "cancel"), { ...options, method: "POST" }),
    refreshTaskUrl: (id, remoteUrl, options) => request(taskPath(id, "url"), { ...options, method: "PUT", json: { remoteUrl } }),
    revealFile: (target, options) => request("/v1/files/reveal", { ...options, method: "POST", json: target }),
    shutdown: (options) => request("/v1/shutdown", { ...options, method: "POST" })
  });
}

// src/platform.js
var DEFAULT_PLATFORM = "windows";
var HELPER_EXECUTABLES = Object.freeze({
  windows: "bin/echo-music-keeper-helper.exe",
  macos: "bin/echo-music-keeper-helper-macos"
});
function platformHints(source) {
  const navigator = source?.navigator ?? {};
  return [
    source?.electron?.platform,
    source?.process?.platform,
    navigator.userAgentData?.platform,
    navigator.platform,
    navigator.userAgent
  ].map((value) => String(value ?? "").trim().toLowerCase()).filter(Boolean).join(" ");
}
function detectPlatform(...sources) {
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
function helperExecutable(platform) {
  return HELPER_EXECUTABLES[platform] ?? HELPER_EXECUTABLES[DEFAULT_PLATFORM];
}
function pathSeparator(platform) {
  return platform === "windows" ? "\\" : "/";
}

// src/helper-runtime.js
var PORT_MIN = 49152;
var PORT_COUNT = 65535 - PORT_MIN + 1;
var START_ATTEMPTS = 3;
var START_TIMEOUT_MS = 5e3;
var HEALTH_INTERVAL_MS = 100;
var sleepNormally = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));
function isAbsolutePath(path) {
  return /^(?:[A-Za-z]:[\\/]|\\\\[^\\/]+[\\/][^\\/]+|\/)/.test(path);
}
function randomToken() {
  const bytes2 = new Uint8Array(32);
  crypto.getRandomValues(bytes2);
  return Array.from(bytes2, (byte) => byte.toString(16).padStart(2, "0")).join("");
}
function waitForAbort(promise, signal) {
  if (!signal) return promise;
  if (signal.aborted) return Promise.resolve();
  return Promise.race([
    promise,
    new Promise((resolve) => signal.addEventListener("abort", resolve, { once: true }))
  ]);
}
function createHelperRuntime(ctx, options = {}) {
  const pluginRoot = String(ctx?.descriptor?.directory ?? "");
  const settings = options.settings ?? {};
  const platform = String(options.platform ?? detectPlatform(options.environment ?? ctx, globalThis));
  const executable = helperExecutable(platform);
  const separator = pathSeparator(platform);
  const clientFactory = options.clientFactory ?? ((clientOptions) => createHelperClient({
    ...clientOptions,
    fetchImpl: options.fetchImpl
  }));
  const sleep = options.sleep ?? sleepNormally;
  const choosePort = options.randomPort ?? (() => PORT_MIN + Math.floor(Math.random() * PORT_COUNT));
  const paths = {
    managedDownloadRoots: options.managedDownloadRoots ?? [],
    completedDownloadPaths: options.completedDownloadPaths ?? []
  };
  let operation = Promise.resolve();
  let pendingStart = null;
  let stopQueued = false;
  let tracked = null;
  let onlineClient = null;
  let defaultMusicRoot = String(options.defaultMusicRoot ?? "");
  let currentStatus = { state: "stopped", reason: null };
  const setStatus = (state, reason = null) => {
    currentStatus = { state, reason };
  };
  const fallback = (reason) => {
    onlineClient = null;
    setStatus("fallback", reason);
    return false;
  };
  function enqueue(work) {
    const next = operation.then(work, work);
    operation = next.catch(() => {
    });
    return next;
  }
  async function terminate(entry) {
    if (!entry || entry.terminated) return;
    entry.terminated = true;
    if (tracked === entry) tracked = null;
    try {
      await ctx.process.terminate(entry.pid);
    } catch {
    }
  }
  async function healthUntilReady(client, isCanceled) {
    const deadline = Date.now() + START_TIMEOUT_MS;
    const maximumProbes = Math.ceil(START_TIMEOUT_MS / HEALTH_INTERVAL_MS);
    for (let probe = 0; probe < maximumProbes && Date.now() < deadline && !isCanceled(); probe += 1) {
      try {
        const health = await client.health({ timeoutMs: 500 });
        if (!isCanceled()) return health;
      } catch {
      }
      if (Date.now() >= deadline || isCanceled()) break;
      await sleep(HEALTH_INTERVAL_MS);
    }
    return null;
  }
  async function startInternal(signal) {
    if (onlineClient) return true;
    if (!isAbsolutePath(pluginRoot)) return fallback("invalid-plugin-root");
    let canceled = Boolean(signal?.aborted);
    const markCanceled = () => {
      canceled = true;
    };
    signal?.addEventListener("abort", markCanceled, { once: true });
    const isCanceled = () => canceled;
    const initialConfig = toHelperConfig(settings, {
      pluginRoot,
      defaultMusicRoot: "",
      separator,
      ...paths
    });
    const seenPorts = /* @__PURE__ */ new Set();
    let launchedHelper = false;
    let launchFailed = false;
    setStatus("starting");
    try {
      for (let attempt = 0; attempt < START_ATTEMPTS && !isCanceled(); attempt += 1) {
        let port = Number(choosePort());
        if (!Number.isInteger(port) || port < PORT_MIN || port > 65535) {
          port = PORT_MIN + Math.floor(Math.random() * PORT_COUNT);
        }
        while (seenPorts.has(port)) port = PORT_MIN + (port - PORT_MIN + 1) % PORT_COUNT;
        seenPorts.add(port);
        const token = randomToken();
        const client = clientFactory({ baseUrl: `http://127.0.0.1:${port}`, token, fetchImpl: options.fetchImpl });
        let launchResult;
        try {
          launchResult = await ctx.process.launch({
            executable,
            args: [
              "--port",
              String(port),
              "--token",
              token,
              "--plugin-root",
              pluginRoot,
              "--cache-root",
              initialConfig.cacheRoot,
              "--download-root",
              initialConfig.downloadRoot,
              "--cache-limit-bytes",
              String(initialConfig.cacheLimitBytes)
            ]
          });
        } catch {
          launchFailed = true;
          continue;
        }
        if (launchResult?.canceled) return fallback("user-canceled");
        const pid = launchResult?.pid;
        if (launchResult?.ok !== true || !Number.isInteger(pid) || pid <= 0) {
          launchFailed = true;
          continue;
        }
        launchedHelper = true;
        const entry = { pid, client, terminated: false };
        tracked = entry;
        const health = await healthUntilReady(client, isCanceled);
        if (!health || isCanceled()) {
          await terminate(entry);
          continue;
        }
        defaultMusicRoot = String(health.defaultMusicPath ?? defaultMusicRoot);
        try {
          await client.updateConfig(toHelperConfig(settings, { pluginRoot, defaultMusicRoot, separator, ...paths }));
        } catch {
          await terminate(entry);
          continue;
        }
        if (isCanceled() || tracked !== entry) {
          await terminate(entry);
          continue;
        }
        onlineClient = client;
        setStatus("online");
        return true;
      }
      if (isCanceled()) return fallback("user-canceled");
      return fallback(!launchedHelper && launchFailed ? "launch-failed" : "health-check-failed");
    } finally {
      signal?.removeEventListener("abort", markCanceled);
    }
  }
  async function stopInternal() {
    onlineClient = null;
    const entry = tracked;
    if (entry) {
      const signal = AbortSignal.timeout(2e3);
      try {
        await waitForAbort(Promise.resolve().then(() => entry.client.shutdown({ signal })), signal);
      } catch {
      }
      if (tracked === entry) await terminate(entry);
    }
    setStatus("stopped");
  }
  function retainStart(promise) {
    pendingStart = promise;
    promise.finally(() => {
      if (pendingStart === promise) pendingStart = null;
    });
    return promise;
  }
  function start({ signal } = {}) {
    if (pendingStart) return pendingStart;
    if (onlineClient && !stopQueued) return Promise.resolve(true);
    return retainStart(enqueue(() => startInternal(signal)));
  }
  function stop() {
    stopQueued = true;
    pendingStart = null;
    return enqueue(async () => {
      await stopInternal();
      stopQueued = false;
    });
  }
  function restart() {
    stopQueued = true;
    pendingStart = null;
    return retainStart(enqueue(async () => {
      await stopInternal();
      stopQueued = false;
      return startInternal();
    }));
  }
  return Object.freeze({
    start,
    stop,
    restart,
    status: () => ({ ...currentStatus }),
    get client() {
      return onlineClient;
    },
    get pluginRoot() {
      return pluginRoot;
    },
    get platform() {
      return platform;
    },
    get pathSeparator() {
      return separator;
    },
    get executable() {
      return executable;
    },
    get defaultMusicRoot() {
      return defaultMusicRoot;
    }
  });
}

// src/resolver.js
var HASH_PATTERN2 = /^[0-9a-f]{16,128}$/i;
var CANONICAL_HASH_PATTERN = /^[0-9a-f]{16,128}$/;
var CACHE_KEY_PATTERN = /^([0-9a-f]{16,128})\.(128|320|flac|high|super)\.none$/;
var ABSOLUTE_PATH_PATTERN = /^(?:[A-Za-z]:[\\/]|\\\\[^\\/]+[\\/][^\\/]+|\/)/;
var CANONICAL_QUALITIES = /* @__PURE__ */ new Set(["128", "320", "flac", "high", "super"]);
function runtimeState(runtime) {
  const status = typeof runtime?.status === "function" ? runtime.status() : runtime?.status;
  const value = status?.value ?? status?.state ?? status;
  return value === "online" || value === "ready";
}
function validTrack(track) {
  return Boolean(
    track && String(track.source ?? "").toLowerCase() !== "cloud" && typeof track.hash === "string" && HASH_PATTERN2.test(track.hash.trim())
  );
}
function cachedQuality(lookup, fallback) {
  return normalizeQuality(lookup?.track?.quality) ?? normalizeQuality(lookup?.track?.requestedQuality) ?? fallback;
}
function cacheKeyDetails(key) {
  if (typeof key !== "string") return null;
  const match = CACHE_KEY_PATTERN.exec(key);
  return match ? { hash: match[1], quality: match[2] } : null;
}
function validLookupTrack(track, key, allowEmptyAlias) {
  const details = cacheKeyDetails(key);
  const recoveryAlias = track?.catalogHash === "" && track?.requestedQuality === "";
  const canonicalAlias = typeof track?.catalogHash === "string" && CANONICAL_HASH_PATTERN.test(track.catalogHash) && typeof track?.requestedQuality === "string" && CANONICAL_QUALITIES.has(track.requestedQuality);
  return Boolean(
    details && track && typeof track.key === "string" && track.key === key && (canonicalAlias || allowEmptyAlias && recoveryAlias) && typeof track.hash === "string" && track.hash === details.hash && typeof track.quality === "string" && track.quality === details.quality && track.effect === "none"
  );
}
function validLookup(lookup, expected = {}) {
  if (!lookup || typeof lookup !== "object") return null;
  if (lookup.status === "miss") return lookup;
  if (lookup.status !== "partial" && lookup.status !== "complete") return null;
  const isAliasLookup = expected.catalogHash !== void 0 || expected.requestedQuality !== void 0;
  if (!cacheKeyDetails(lookup.key) || !validLookupTrack(lookup.track, lookup.key, !isAliasLookup)) return null;
  if (expected.key !== void 0 && (typeof expected.key !== "string" || lookup.key !== expected.key)) return null;
  if (expected.catalogHash !== void 0 && (typeof expected.catalogHash !== "string" || !CANONICAL_HASH_PATTERN.test(expected.catalogHash) || lookup.track.catalogHash !== expected.catalogHash)) return null;
  if (expected.requestedQuality !== void 0 && (typeof expected.requestedQuality !== "string" || !CANONICAL_QUALITIES.has(expected.requestedQuality) || lookup.track.requestedQuality !== expected.requestedQuality)) return null;
  if (lookup.status === "complete" && !ABSOLUTE_PATH_PATTERN.test(String(lookup.path ?? ""))) return null;
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
    durationSeconds: Number(track?.durationSeconds ?? track?.duration ?? 0) || 0
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
async function resolveCachedOrProxy(options) {
  const { ctx, runtime, taskStore, settings = {}, track, effect = "none", forceReload = false, warn } = options ?? {};
  const quality = normalizeQuality(options?.quality);
  const client = runtime?.client;
  try {
    if (!runtimeState(runtime) || !client || typeof client.cacheLookup !== "function" || effect !== "none" || !quality || !validTrack(track)) return null;
    const catalogHash = track.hash.trim().toLowerCase();
    const alias = validLookup(
      await client.cacheLookup({ catalogHash, requestedQuality: quality, effect: "none" }),
      { catalogHash, requestedQuality: quality }
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
    const helperTrack2 = sourceTrack(track, source);
    const actual = validLookup(await client.cacheLookup({ key: helperTrack2.key }), { key: helperTrack2.key });
    if (!actual) {
      warn?.();
      return null;
    }
    const localActual = await completeResult(ctx, actual, source.quality);
    if (localActual) return localActual;
    if (actual?.status === "complete") return null;
    const remoteURL = source.urls[0];
    if (forceReload && await taskStore?.refreshNeedsURL?.(helperTrack2.key, remoteURL)) {
      return null;
    }
    if (typeof client.createSession !== "function") return null;
    const session = await client.createSession({ track: helperTrack2, remoteUrl: remoteURL });
    if (!session?.playUrl) return null;
    return { url: session.playUrl, quality: source.quality, effect: "none" };
  } catch {
    warn?.();
    return null;
  }
}
function createAudioResolver(ctx, runtime, settingsRef, taskStore) {
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
        warn
      });
    }
  };
}

// src/task-store.js
var ref2 = (ctx, value) => typeof ctx?.vue?.ref === "function" ? ctx.vue.ref(value) : { value };
var taskList = (response) => Array.isArray(response) ? response : response?.tasks ?? [];
function createTaskStore(ctx, runtime, timers = globalThis) {
  const tasks = ref2(ctx, []);
  const cacheEntries = ref2(ctx, []);
  const cacheStats = ref2(ctx, { cacheBytes: 0, cacheLimitBytes: 0 });
  const error = ref2(ctx, null);
  let visible = false;
  let timer = null;
  let refreshing = null;
  let disposed = false;
  const refreshingKeys = /* @__PURE__ */ new Map();
  const snapshotListeners = /* @__PURE__ */ new Set();
  function client() {
    return runtime?.client ?? null;
  }
  function schedule() {
    if (disposed) return;
    if (timer) timers.clearInterval(timer);
    timer = timers.setInterval(() => void refresh(), visible ? 500 : 2e3);
  }
  function refreshableTask(task, state) {
    const hash = String(task?.track?.hash ?? "").trim().toLowerCase();
    const quality = normalizeQuality(task?.track?.quality);
    return task?.state === state && task?.kind === "cache" && /^[0-9a-f]{16,128}$/.test(hash) && task?.track?.key === `${hash}.${quality}.none` ? { hash, quality, key: task.track.key } : null;
  }
  function runKeyedRefresh(key, operation) {
    const active = refreshingKeys.get(key);
    if (active) return active;
    let current;
    current = Promise.resolve().then(operation).then(Boolean).finally(() => {
      if (refreshingKeys.get(key) === current) refreshingKeys.delete(key);
    });
    refreshingKeys.set(key, current);
    return current;
  }
  async function renewTaskURL(task, state) {
    const details = refreshableTask(task, state);
    const active = client();
    if (!details || !task?.id || !active?.refreshTaskUrl) return false;
    return runKeyedRefresh(details.key, async () => {
      const source = await resolveKugouTrackURLs(ctx, details.hash, details.quality);
      if (disposed || !source?.urls?.[0]) return false;
      await active.refreshTaskUrl(task.id, source.urls[0]);
      return true;
    }).catch(() => false);
  }
  async function renewURLs(list) {
    const handled = /* @__PURE__ */ new Set();
    for (const task of list) {
      if (disposed) return;
      const details = refreshableTask(task, "needs-url");
      if (!details || handled.has(details.key)) continue;
      handled.add(details.key);
      await renewTaskURL(task, "needs-url");
    }
  }
  function publishTasks(list) {
    if (disposed) return;
    for (const listener of snapshotListeners) listener(list);
  }
  async function refresh({ force = false } = {}) {
    if (disposed) return Promise.resolve();
    if (refreshing) {
      const active = refreshing;
      await active;
      if (disposed || !force) return;
    }
    if (refreshing) return refresh({ force });
    refreshing = (async () => {
      const active = client();
      if (!active?.listTasks || !active?.cacheList) {
        if (!disposed) error.value = "helper-offline";
        return;
      }
      try {
        const [taskResponse, cacheResponse] = await Promise.all([active.listTasks(), active.cacheList()]);
        if (disposed) return;
        const list = taskList(taskResponse);
        tasks.value = list;
        publishTasks(list);
        cacheEntries.value = Array.isArray(cacheResponse?.items) ? cacheResponse.items : Array.isArray(cacheResponse?.entries) ? cacheResponse.entries : [];
        cacheStats.value = { cacheBytes: Number(cacheResponse?.cacheBytes ?? 0), cacheLimitBytes: Number(cacheResponse?.cacheLimitBytes ?? 0) };
        error.value = null;
        await renewURLs(list);
      } catch (failure) {
        if (!disposed) error.value = /cache_(unavailable|.*failed)|invalid_download_roots/.test(String(failure?.code ?? "")) ? "directory-error" : "helper-offline";
      } finally {
        refreshing = null;
      }
    })();
    return refreshing;
  }
  async function action(method, task) {
    if (disposed) return;
    await client()?.[method]?.(task?.id);
    if (!disposed) await refresh({ force: true });
  }
  async function retry(task) {
    if (task?.kind !== "cache") return action("resumeTask", task);
    if (disposed) return;
    await renewTaskURL(task, task?.state);
    if (!disposed) await refresh({ force: true });
  }
  async function refreshNeedsURL(key, remoteURL) {
    if (disposed || typeof remoteURL !== "string" || !remoteURL) return false;
    const task = tasks.value.find((entry) => entry?.id && refreshableTask(entry, "needs-url")?.key === key);
    const active = client();
    if (!task || !active?.refreshTaskUrl) return false;
    return runKeyedRefresh(key, async () => {
      if (disposed || !tasks.value.some((entry) => entry?.id === task.id && refreshableTask(entry, "needs-url")?.key === key)) return false;
      await active.refreshTaskUrl(task.id, remoteURL);
      return true;
    }).catch(() => false);
  }
  function setVisible(next) {
    if (disposed) return;
    visible = Boolean(next);
    schedule();
    void refresh();
  }
  schedule();
  return Object.freeze({
    tasks,
    cacheEntries,
    cacheStats,
    error,
    online: () => {
      const state = typeof runtime?.status === "function" ? runtime.status()?.state : runtime?.status?.state;
      return Boolean(client()) && (!state || state === "online" || state === "ready");
    },
    setVisible,
    refresh,
    refreshNeedsURL,
    subscribe(listener) {
      if (disposed || typeof listener !== "function") return () => {
      };
      snapshotListeners.add(listener);
      listener(tasks.value);
      return () => snapshotListeners.delete(listener);
    },
    pause: (task) => action("pauseTask", task),
    resume: (task) => action("resumeTask", task),
    cancel: (task) => action("cancelTask", task),
    retry,
    reveal: (target) => client()?.revealFile?.(typeof target === "string" ? { kind: "select", path: target } : target),
    deleteCache: async (entry) => {
      if (disposed) return;
      await client()?.deleteCache?.(entry?.key ?? entry);
      if (!disposed) await refresh();
    },
    clearCache: async () => {
      if (disposed) return;
      await client()?.clearCache?.();
      if (!disposed) await refresh();
    },
    dispose() {
      disposed = true;
      if (timer) timers.clearInterval(timer);
      timer = null;
      snapshotListeners.clear();
    }
  });
}

// src/ui/page.js
var bytes = (value) => {
  const amount = Number(value) || 0;
  if (amount < 1024) return `${amount} B`;
  const units = ["KiB", "MiB", "GiB"];
  let unit = -1;
  let current = amount;
  while (current >= 1024 && unit < units.length - 1) {
    current /= 1024;
    unit += 1;
  }
  return `${current.toFixed(current >= 10 ? 0 : 1)} ${units[unit]}`;
};
var seconds = (value) => !Number.isFinite(value) || value < 0 ? "--" : `${Math.ceil(value / 60)} \u5206`;
function hostComponent2(ctx, name, fallback) {
  const loader = ctx?.ui?.components?.[name];
  return typeof ctx?.vue?.defineAsyncComponent === "function" && typeof loader === "function" ? ctx.vue.defineAsyncComponent(loader) : fallback;
}
function createManagementPage(ctx, taskStore, downloads) {
  const vue = ctx?.vue ?? {};
  const h = vue.h ?? ((type, props, children) => ({ type, props, children }));
  return {
    name: "EchoMusicKeeperManagementPage",
    setup() {
      const Button = hostComponent2(ctx, "Button", "button");
      const tab = vue.ref?.("tasks") ?? { value: "tasks" };
      const loading = vue.ref?.(true) ?? { value: true };
      const Icon = vue.resolveComponent?.("Icon");
      const icon = (keys, fallback) => keys.map((key) => ctx?.icons?.[key]).find(Boolean) ?? fallback;
      const icons = {
        play: icon(["iconPlayerPlay", "iconPlay"], "tabler:player-play"),
        pause: icon(["iconPlayerPause", "iconPause"], "tabler:player-pause"),
        cancel: icon(["iconX", "iconCancel"], "tabler:x"),
        retry: icon(["iconRefresh", "iconReload"], "tabler:refresh"),
        reveal: icon(["iconFolderOpen", "iconFolder"], "tabler:folder-open"),
        remove: icon(["iconTrash", "iconX"], "tabler:trash")
      };
      const confirm = async (message) => ctx?.dialog?.confirm?.({ title: "\u7F13\u5B58\u7BA1\u7406", content: message });
      const runAction = (action) => async () => {
        try {
          await action();
        } catch (error) {
          ctx?.toast?.warning?.(error instanceof Error && error.message ? error.message : "\u64CD\u4F5C\u5931\u8D25\uFF0C\u8BF7\u7A0D\u540E\u91CD\u8BD5\u3002");
        }
      };
      const refresh = async () => {
        loading.value = true;
        await taskStore.refresh();
        await downloads.reconcile();
        loading.value = false;
      };
      vue.onMounted?.(() => {
        taskStore.setVisible(true);
        void refresh().catch(() => {
          loading.value = false;
        });
      });
      vue.onUnmounted?.(() => taskStore.setVisible(false));
      const command = (title, action, iconValue) => h(Button, { class: "echo-music-keeper-icon-button", title, "aria-label": title, onClick: runAction(action) }, () => Icon ? h(Icon, { icon: iconValue, width: 16, height: 16 }) : title);
      const tabButton = (value, title) => h(Button, { class: { active: tab.value === value }, onClick: () => {
        tab.value = value;
      } }, () => title);
      const taskRow = (task) => {
        const progress = task.totalBytes > 0 ? Math.min(100, Math.round((task.downloadedBytes || 0) / task.totalBytes * 100)) : 0;
        const remaining = task.bytesPerSecond > 0 && task.totalBytes > 0 ? (task.totalBytes - task.downloadedBytes) / task.bytesPerSecond : NaN;
        const completedDownload = task.outputPath ? downloads.records.value.find((entry) => entry.path === task.outputPath && !entry.missing) : null;
        return h("li", { class: "echo-music-keeper-task", key: task.id }, [
          h("div", { class: "echo-music-keeper-task__song" }, [h("strong", task.track?.title || "\u672A\u77E5\u6B4C\u66F2"), h("span", `${task.track?.artist || "\u672A\u77E5\u827A\u4EBA"} \xB7 ${task.track?.quality || "--"}`)]),
          h("div", { class: "echo-music-keeper-task__progress" }, [h("div", { class: "echo-music-keeper-progress", style: { "--progress": `${progress}%` } }), h("span", `${progress}%`)]),
          h("span", `${bytes(task.bytesPerSecond)}/s`),
          h("span", seconds(remaining)),
          h("span", { class: `echo-music-keeper-state is-${task.state}` }, task.state),
          h("div", { class: "echo-music-keeper-task__actions" }, [
            task.pausable ? command(task.state === "paused" ? "\u7EE7\u7EED" : "\u6682\u505C", () => task.state === "paused" ? taskStore.resume(task) : taskStore.pause(task), task.state === "paused" ? icons.play : icons.pause) : null,
            task.cancelable ? command("\u53D6\u6D88", () => taskStore.cancel(task), icons.cancel) : null,
            task.retryable || task.state === "failed" ? command("\u91CD\u8BD5", () => taskStore.retry(task), icons.retry) : null,
            completedDownload ? command("\u6253\u5F00\u4F4D\u7F6E", () => downloads.reveal(completedDownload), icons.reveal) : null
          ])
        ]);
      };
      const downloadRow = (entry) => h("li", { class: "echo-music-keeper-entry", key: entry.path }, [
        h("div", [h("strong", entry.title || "\u672A\u77E5\u6B4C\u66F2"), h("span", `${entry.artist || "\u672A\u77E5\u827A\u4EBA"} \xB7 ${entry.quality || "--"} \xB7 ${bytes(entry.size)}`)]),
        h("div", { class: "echo-music-keeper-task__actions" }, [
          command("\u6253\u5F00\u4F4D\u7F6E", () => downloads.reveal(entry), icons.reveal),
          command("\u5220\u9664\u4E0B\u8F7D", () => downloads.remove(entry), icons.remove)
        ])
      ]);
      return () => h("main", { class: "echo-music-keeper-page" }, [
        h("nav", { class: "echo-music-keeper-tabs", "aria-label": "\u7F13\u5B58\u7BA1\u7406" }, [tabButton("tasks", "\u4E0B\u8F7D\u4EFB\u52A1"), tabButton("cache", "\u7F13\u5B58\u7BA1\u7406")]),
        loading.value ? h("p", { class: "echo-music-keeper-empty" }, "\u6B63\u5728\u8BFB\u53D6\u7F13\u5B58\u72B6\u6001\u2026") : null,
        taskStore.error.value === "helper-offline" ? h("p", { class: "echo-music-keeper-empty" }, "\u7F13\u5B58\u670D\u52A1\u672A\u8FDE\u63A5") : null,
        taskStore.error.value === "directory-error" ? h("p", { class: "echo-music-keeper-empty" }, "\u7F13\u5B58\u76EE\u5F55\u4E0D\u53EF\u7528") : null,
        tab.value === "tasks" ? h("section", { class: "echo-music-keeper-section" }, [
          h("ul", { class: "echo-music-keeper-task-list" }, taskStore.tasks.value.map(taskRow)),
          !taskStore.tasks.value.length && !loading.value ? h("p", { class: "echo-music-keeper-empty" }, "\u6682\u65E0\u4E0B\u8F7D\u4EFB\u52A1") : null,
          h("h3", { class: "echo-music-keeper-subheading" }, "\u5DF2\u5B8C\u6210\u4E0B\u8F7D"),
          h("ul", { class: "echo-music-keeper-entry-list" }, downloads.records.value.filter((entry) => !entry.missing).map(downloadRow)),
          !downloads.records.value.some((entry) => !entry.missing) && !loading.value ? h("p", { class: "echo-music-keeper-empty" }, "\u6682\u65E0\u5DF2\u5B8C\u6210\u4E0B\u8F7D") : null
        ]) : h("section", { class: "echo-music-keeper-section" }, [
          h("div", { class: "echo-music-keeper-cache-summary" }, [
            h("div", { class: "echo-music-keeper-progress", style: { "--progress": `${Math.min(100, taskStore.cacheStats.value.cacheBytes / Math.max(1, taskStore.cacheStats.value.cacheLimitBytes) * 100)}%` } }),
            h("span", `${bytes(taskStore.cacheStats.value.cacheBytes)} / ${bytes(taskStore.cacheStats.value.cacheLimitBytes)}`),
            h(Button, { onClick: runAction(async () => {
              if (await confirm("\u786E\u5B9A\u6E05\u7A7A\u6240\u6709\u672A\u4F7F\u7528\u7F13\u5B58\u5417\uFF1F")) await taskStore.clearCache();
            }) }, () => "\u6E05\u7A7A\u7F13\u5B58")
          ]),
          h("ul", { class: "echo-music-keeper-entry-list" }, taskStore.cacheEntries.value.map((entry) => h("li", { class: "echo-music-keeper-entry", key: entry.key }, [
            h("div", [h("strong", entry.track?.title || entry.key), h("span", `${entry.track?.artist || ""} \xB7 ${bytes(entry.size)}`)]),
            h("div", { class: "echo-music-keeper-task__actions" }, [command("\u5220\u9664\u7F13\u5B58", async () => {
              if (await confirm("\u786E\u5B9A\u5220\u9664\u6B64\u7F13\u5B58\u5417\uFF1F")) await taskStore.deleteCache(entry);
            }, icons.remove)])
          ]))),
          !taskStore.cacheEntries.value.length && !loading.value ? h("p", { class: "echo-music-keeper-empty" }, "\u6682\u65E0\u5B8C\u6574\u7F13\u5B58") : null
        ])
      ]);
    }
  };
}

// src/ui/settings.js
var STORAGE_KEY2 = "echo-music-keeper-settings";
var DOWNLOADS_KEY = "echo-music-keeper-downloads";
var parentPath2 = (path) => String(path ?? "").replace(/[\\/][^\\/]*$/, "");
function hostComponent3(ctx, name, fallback) {
  const loader = ctx?.ui?.components?.[name];
  return typeof ctx?.vue?.defineAsyncComponent === "function" && typeof loader === "function" ? ctx.vue.defineAsyncComponent(loader) : fallback;
}
function createSettingsComponent(ctx, runtime, settingsRef) {
  const vue = ctx?.vue ?? {};
  const h = vue.h ?? ((type, props, children) => ({ type, props, children }));
  return {
    name: "EchoMusicKeeperSettings",
    setup() {
      const Switch = hostComponent3(ctx, "Switch", "input");
      const Input = hostComponent3(ctx, "Input", "input");
      const Button = hostComponent3(ctx, "Button", "button");
      const save = async (next) => {
        const normalized = normalizeSettings(next);
        await ctx?.storage?.set?.(STORAGE_KEY2, { ...normalized });
        settingsRef.value = normalized;
        const records = await ctx?.storage?.get?.(DOWNLOADS_KEY).catch?.(() => []) ?? [];
        const completedDownloadPaths = (Array.isArray(records) ? records : []).map((entry) => String(entry?.path ?? "").trim()).filter(Boolean);
        const managedDownloadRoots = [...new Set([normalized.downloadRoot, ...completedDownloadPaths.map(parentPath2)].filter(Boolean))];
        await runtime?.client?.updateConfig?.(toHelperConfig(normalized, {
          pluginRoot: runtime?.pluginRoot ?? "",
          defaultMusicRoot: runtime?.defaultMusicRoot ?? "",
          separator: runtime?.pathSeparator ?? "\\",
          managedDownloadRoots,
          completedDownloadPaths
        }));
      };
      const changeCacheRoot = async (path) => {
        if (!path || path === settingsRef.value.cacheRoot) return;
        const migrate = await ctx?.dialog?.confirm?.({ title: "\u5207\u6362\u7F13\u5B58\u76EE\u5F55", content: "\u8FC1\u79FB\u73B0\u6709\u7F13\u5B58\uFF1F\u9009\u62E9\u53D6\u6D88\u5C06\u4ECE\u65B0\u76EE\u5F55\u5F00\u59CB\u3002", confirmText: "\u8FC1\u79FB", cancelText: "\u91CD\u65B0\u5F00\u59CB" });
        await runtime?.client?.migrateCache?.({ cacheRoot: path, mode: migrate ? "migrate" : "fresh" });
        await save({ ...settingsRef.value, cacheRoot: path });
      };
      const choose = async (name) => {
        const selected = await ctx?.dialog?.selectDirectory?.();
        const path = typeof selected === "string" ? selected : selected?.path;
        if (!path) return;
        if (name === "cacheRoot") await changeCacheRoot(path);
        else await save({ ...settingsRef.value, [name]: path });
      };
      const cacheLocation = () => {
        const custom = String(settingsRef.value.cacheRoot ?? "").trim();
        if (custom) return custom;
        const pluginRoot = String(runtime?.pluginRoot ?? "").trim().replace(/[\\/]$/, "");
        return pluginRoot ? `${pluginRoot}${runtime?.pathSeparator ?? "\\"}cache` : "\u6B63\u5728\u68C0\u6D4B\u63D2\u4EF6\u76EE\u5F55";
      };
      const downloadLocation = () => String(settingsRef.value.downloadRoot || runtime?.defaultMusicRoot || "\u6B63\u5728\u68C0\u6D4B\u9ED8\u8BA4\u97F3\u4E50\u76EE\u5F55").trim();
      const changeCacheLimit = (event) => {
        const gib = Number(event?.target?.value);
        if (!Number.isFinite(gib) || gib <= 0) return;
        void save({ ...settingsRef.value, cacheLimitBytes: gib * GIB });
      };
      return () => h("section", { class: "echo-music-keeper-settings" }, [
        h("label", [h("span", "\u81EA\u52A8\u7F13\u5B58"), h(Switch, { modelValue: settingsRef.value.autoCache, "onUpdate:modelValue": (value) => void save({ ...settingsRef.value, autoCache: value }) })]),
        h("label", [h("span", "\u7F13\u5B58\u4E0A\u9650"), h("input", { class: "echo-music-keeper-limit-input", type: "number", min: MIN_CACHE_BYTES / GIB, max: MAX_CACHE_BYTES / GIB, step: 0.01, value: settingsRef.value.cacheLimitBytes / GIB, inputmode: "decimal", "aria-label": "\u7F13\u5B58\u4E0A\u9650\uFF08GiB\uFF09", onChange: changeCacheLimit }), h("span", "GiB")]),
        h("label", [h("span", "\u7F13\u5B58\u76EE\u5F55"), h(Input, { modelValue: settingsRef.value.cacheRoot, "onUpdate:modelValue": (value) => void changeCacheRoot(value) }), h(Button, { onClick: () => choose("cacheRoot") }, () => "\u9009\u62E9\u6587\u4EF6\u5939")]),
        h("p", { class: "echo-music-keeper-settings__effective-path" }, `\u5F53\u524D\u7F13\u5B58\u76EE\u5F55\uFF1A${cacheLocation()}`),
        h("label", [h("span", "\u4E0B\u8F7D\u76EE\u5F55"), h(Input, { modelValue: settingsRef.value.downloadRoot, "onUpdate:modelValue": (value) => void save({ ...settingsRef.value, downloadRoot: value }) }), h(Button, { onClick: () => choose("downloadRoot") }, () => "\u9009\u62E9\u6587\u4EF6\u5939")]),
        h("p", { class: "echo-music-keeper-settings__effective-path" }, `\u5F53\u524D\u4E0B\u8F7D\u76EE\u5F55\uFF1A${downloadLocation()}`),
        h("p", { class: "echo-music-keeper-settings__notice" }, settingsRef.value.cacheRoot ? "\u81EA\u5B9A\u4E49\u7F13\u5B58\u76EE\u5F55\u4E0D\u4F1A\u968F\u63D2\u4EF6\u66F4\u65B0\u6216\u5378\u8F7D\u81EA\u52A8\u6E05\u7406\u3002" : "\u9ED8\u8BA4\u7F13\u5B58\u76EE\u5F55\u4F1A\u968F\u63D2\u4EF6\u66F4\u65B0\u3001\u91CD\u88C5\u6216\u5378\u8F7D\u6E05\u7406\u3002")
      ]);
    }
  };
}

// src/ui/styles.js
var ECHO_MUSIC_KEEPER_STYLES = `
.echo-music-keeper-page{box-sizing:border-box;min-height:100%;padding:20px 24px 32px;color:var(--color-text-main,var(--text-color,#222))}.echo-music-keeper-tabs{display:flex;gap:4px;border-bottom:1px solid var(--control-border,var(--border-color,#ddd))}.echo-music-keeper-tabs button{height:36px;padding:0 14px;border:0;border-radius:4px 4px 0 0;color:inherit;background:transparent;font-size:14px;font-weight:600}.echo-music-keeper-tabs button.active{color:var(--color-primary,var(--primary-color,#3b82f6));border-bottom:2px solid currentColor}.echo-music-keeper-section{max-width:1240px;padding-top:20px}.echo-music-keeper-task-list,.echo-music-keeper-entry-list{list-style:none;margin:0;padding:0;border-top:1px solid var(--control-border,var(--border-color,#ddd))}.echo-music-keeper-task{min-height:72px;display:grid;grid-template-columns:minmax(220px,2fr) minmax(150px,1fr) 88px 56px 90px 132px;align-items:center;gap:12px;border-bottom:1px solid var(--control-border,var(--border-color,#ddd))}.echo-music-keeper-entry{min-height:64px;display:flex;align-items:center;justify-content:space-between;gap:16px;padding:8px 0;border-bottom:1px solid var(--control-border,var(--border-color,#ddd))}.echo-music-keeper-task__song,.echo-music-keeper-entry>div:first-child{min-width:0;display:grid;gap:4px}.echo-music-keeper-task__song strong,.echo-music-keeper-entry strong{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:14px}.echo-music-keeper-task__song span,.echo-music-keeper-entry span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:12px;opacity:.72}.echo-music-keeper-progress{height:6px;min-width:80px;background:linear-gradient(90deg,var(--color-primary,var(--primary-color,#3b82f6)) var(--progress),rgba(127,127,127,.18) var(--progress));border-radius:3px}.echo-music-keeper-task__progress{display:grid;grid-template-columns:minmax(80px,1fr) 40px;align-items:center;gap:8px;font-size:12px}.echo-music-keeper-task__actions{display:flex;justify-content:flex-end;gap:6px}.echo-music-keeper-icon-button{width:32px;height:32px;padding:0;border:1px solid var(--control-border,var(--border-color,#ddd));border-radius:4px;color:inherit;background:transparent;display:inline-flex;align-items:center;justify-content:center}.echo-music-keeper-icon-button svg{display:block}.echo-music-keeper-empty{padding:28px 0;margin:0;text-align:center;opacity:.72}.echo-music-keeper-subheading{margin:24px 0 10px;font-size:14px}.echo-music-keeper-cache-summary{display:grid;grid-template-columns:minmax(140px,1fr) auto auto;gap:14px;align-items:center;margin-bottom:18px}.echo-music-keeper-dialog{width:min(360px,calc(100vw - 32px));padding:20px;background:var(--color-bg-elevated,var(--background-color,#fff));border-radius:6px}.echo-music-keeper-dialog__header,.echo-music-keeper-dialog__actions{display:flex;align-items:center;justify-content:space-between}.echo-music-keeper-settings{display:grid;gap:16px}.echo-music-keeper-settings label{display:grid;grid-template-columns:120px minmax(160px,1fr) auto;align-items:center;gap:10px}.echo-music-keeper-limit-input{box-sizing:border-box;width:100%;min-width:0;height:32px;padding:0 10px;border:1px solid var(--control-border,var(--border-color,#ddd));border-radius:4px;color:inherit;background:transparent}.echo-music-keeper-settings__effective-path,.echo-music-keeper-settings__notice{margin:0 0 0 130px;font-size:12px;line-height:1.5;opacity:.72;overflow-wrap:anywhere}.echo-music-keeper-settings__notice{margin-top:-6px}.echo-music-keeper-state.is-failed{color:#c2410c}.echo-music-keeper-state.is-needs-url{color:#b45309}@media(max-width:900px){.echo-music-keeper-page{padding:16px}.echo-music-keeper-task{grid-template-columns:minmax(180px,1fr) minmax(120px,1fr) 80px 112px}.echo-music-keeper-task>span:nth-of-type(2),.echo-music-keeper-task>span:nth-of-type(3){display:none}.echo-music-keeper-settings label{grid-template-columns:1fr;gap:6px}.echo-music-keeper-settings__effective-path,.echo-music-keeper-settings__notice{margin-left:0}.echo-music-keeper-cache-summary{grid-template-columns:1fr auto}.echo-music-keeper-cache-summary button{grid-column:1/-1;justify-self:start}}
`;

// src/index.js
var SETTINGS_KEY = "echo-music-keeper-settings";
var FeatureRegistrationError = class extends Error {
  constructor(cause, disposers) {
    super(cause instanceof Error ? cause.message : "echo-music-keeper feature registration failed", { cause });
    this.disposers = disposers;
  }
};
async function disposeAll(disposers) {
  let firstError;
  for (const dispose of disposers.reverse()) {
    try {
      await dispose?.();
    } catch (error) {
      firstError ??= error;
    }
  }
  return firstError;
}
function registerEchoMusicKeeperFeatures(ctx, runtime, settingsRef) {
  let taskStore;
  let downloads;
  const registrations = [];
  try {
    taskStore = createTaskStore(ctx, runtime);
    downloads = createDownloadController(ctx, runtime, settingsRef, { taskStore });
    registrations.push(ctx.player.audioSource.register(createAudioResolver(ctx, runtime, settingsRef, taskStore)));
    downloads.registerContextMenus((dispose) => registrations.push(dispose));
    registrations.push(ctx.ui.addPage({
      id: "echo-music-keeper-manager",
      title: "\u7F13\u5B58\u4E0E\u4E0B\u8F7D",
      component: createManagementPage(ctx, taskStore, downloads),
      sidebar: { title: "\u7F13\u5B58\u4E0E\u4E0B\u8F7D", icon: ctx.icons.iconArrowBarToDown }
    }));
    registrations.push(ctx.ui.settings.define({
      title: "EchoMusicKeeper",
      component: createSettingsComponent(ctx, runtime, settingsRef)
    }));
    registrations.push(ctx.css.inject(ECHO_MUSIC_KEEPER_STYLES, { id: "echo-music-keeper-runtime" }));
    return [
      ...registrations,
      () => downloads.dispose(),
      () => taskStore.dispose()
    ].filter((dispose) => typeof dispose === "function");
  } catch (cause) {
    const partialDisposers = [
      ...registrations,
      ...downloads ? [() => downloads.dispose()] : [],
      ...taskStore ? [() => taskStore.dispose()] : []
    ].filter((dispose) => typeof dispose === "function");
    throw new FeatureRegistrationError(cause, partialDisposers);
  }
}
var disposeRuntime = null;
async function activate(ctx) {
  const stored = await ctx.storage.get(SETTINGS_KEY).catch(() => null);
  const settings = ctx.vue.ref(normalizeSettings(stored));
  const runtime = createHelperRuntime(ctx, { settings });
  await runtime.start().catch(() => void 0);
  let disposers;
  try {
    disposers = registerEchoMusicKeeperFeatures(ctx, runtime, settings);
  } catch (error) {
    await disposeAll(error instanceof FeatureRegistrationError ? error.disposers : []);
    await runtime.stop();
    throw error instanceof FeatureRegistrationError ? error.cause : error;
  }
  let disposed = false;
  disposeRuntime = async () => {
    if (disposed) return;
    disposed = true;
    const disposeError = await disposeAll(disposers);
    let stopError;
    try {
      await runtime.stop();
    } catch (error) {
      stopError = error;
    }
    if (disposeError) throw disposeError;
    if (stopError) throw stopError;
  };
  ctx.dispose(() => {
    void disposeRuntime?.().catch(() => void 0);
  });
}
async function deactivate() {
  const dispose = disposeRuntime;
  disposeRuntime = null;
  await dispose?.();
}
export {
  activate,
  deactivate,
  registerEchoMusicKeeperFeatures
};
