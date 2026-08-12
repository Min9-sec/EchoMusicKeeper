import { normalizeQuality, resolveKugouSource } from "./kugou-source.js";
import { toHelperConfig } from "./config.js";
import { createDownloadDialog } from "./ui/download-dialog.js";

const STORAGE_KEY = "echo-music-keeper-downloads";
const PENDING_STORAGE_KEY = "echo-music-keeper-download-pending";
const BATCH_SIZE = 8;
const keyPattern = /^([0-9a-f]{16,128})\.(128|320|flac|high|super)\.none$/;

const ref = (ctx, value) => typeof ctx?.vue?.ref === "function" ? ctx.vue.ref(value) : { value };
const clientFor = (runtime) => runtime?.client ?? null;
const parentPath = (path) => String(path ?? "").replace(/[\\/][^\\/]*$/, "");
const trackId = (track) => String(track?.id ?? track?.trackId ?? track?.songId ?? "");

function normalizeRecord(value) {
  if (!value || typeof value !== "object" || !keyPattern.test(String(value.key))) return null;
  const path = String(value.path ?? "").trim();
  if (!path) return null;
  return {
    key: String(value.key), trackId: String(value.trackId ?? ""), title: String(value.title ?? ""),
    artist: String(value.artist ?? ""), quality: String(value.quality ?? ""), path,
    size: Math.max(0, Number(value.size) || 0), completedAt: Math.max(0, Number(value.completedAt) || 0),
    ...(value.missing ? { missing: true } : {}),
  };
}

function recordFromTask(task, fallback) {
  const outputPath = String(task?.outputPath ?? "").trim();
  const helperTrack = task?.track ?? {};
  if (!outputPath || !keyPattern.test(String(helperTrack.key ?? fallback.key))) return null;
  return normalizeRecord({
    key: helperTrack.key ?? fallback.key, trackId: fallback.trackId,
    title: helperTrack.title ?? fallback.title, artist: helperTrack.artist ?? fallback.artist,
    quality: helperTrack.quality ?? fallback.quality, path: outputPath,
    size: task?.totalBytes ?? task?.downloadedBytes ?? 0, completedAt: Date.now(),
  });
}

function normalizePending(value) {
  if (!value || typeof value !== "object" || !String(value.id ?? "").trim() || !keyPattern.test(String(value.key ?? ""))) return null;
  return {
    id: String(value.id), key: String(value.key), trackId: String(value.trackId ?? ""),
    title: String(value.title ?? ""), artist: String(value.artist ?? ""), quality: String(value.quality ?? ""),
  };
}

function helperTrack(track, source) {
  return {
    key: `${source.actualHash}.${source.quality}.none`, catalogHash: source.catalogHash,
    hash: source.actualHash, requestedQuality: source.requestedQuality, quality: source.quality,
    effect: "none", title: String(track?.title ?? track?.songName ?? ""),
    artist: String(track?.artist ?? track?.singerName ?? ""), album: String(track?.album ?? track?.albumName ?? ""),
    extension: source.extension, durationSeconds: Number(track?.durationSeconds ?? track?.duration ?? 0) || 0,
  };
}

/** Owns persisted user-download records and routes exports through the helper. */
export function createDownloadController(ctx, runtime, settingsRef, options = {}) {
  const records = ref(ctx, []);
  const pending = new Map();
  let activeDialog = null;
  let disposed = false;
  let unsubscribeSnapshots = null;
  let recordOperation = Promise.resolve();

  function enqueueRecordOperation(work) {
    const next = recordOperation.then(async () => {
      if (!disposed) return work();
      return undefined;
    }, async () => {
      if (!disposed) return work();
      return undefined;
    });
    recordOperation = next.catch(() => undefined);
    return next;
  }

  const ready = Promise.all([
    Promise.resolve(ctx?.storage?.get?.(STORAGE_KEY)),
    Promise.resolve(ctx?.storage?.get?.(PENDING_STORAGE_KEY)),
  ]).then(async ([stored, savedPending]) => {
    if (disposed) return;
    records.value = (Array.isArray(stored) ? stored : []).map(normalizeRecord).filter(Boolean);
    for (const entry of Array.isArray(savedPending) ? savedPending : []) {
      const normalized = normalizePending(entry);
      if (normalized) pending.set(normalized.id, normalized);
    }
    if (!disposed) await syncHelper();
    if (!disposed) await reconcileFiles();
  }).catch(() => undefined);

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
      pluginRoot: runtime?.pluginRoot ?? "", defaultMusicRoot: runtime?.defaultMusicRoot ?? "",
      managedDownloadRoots: paths(), completedDownloadPaths: records.value.filter((entry) => !entry.missing).map((entry) => entry.path),
    });
    await client.updateConfig(config).catch(() => undefined);
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
        } catch { entry.missing = true; }
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
    await Promise.resolve(options.taskStore?.refresh?.({ force: true })).catch(() => undefined);
    const root = effectiveDownloadRoot();
    ctx?.toast?.success?.(`已加入下载任务，可在“缓存与下载”中查看进度${root ? `。下载目录：${root}` : "。"}`);
    return task;
  }

  async function request(track, requestedQuality) {
    await ready;
    if (disposed) return null;
    const client = clientFor(runtime);
    const quality = normalizeQuality(requestedQuality);
    const catalogHash = String(track?.hash ?? "").trim().toLowerCase();
    if (!client?.cacheLookup || !client?.createDownload) return reportFailure("下载服务未启动，请检查插件是否已获准启动缓存服务。");
    if (!quality || !/^[0-9a-f]{16,128}$/.test(catalogHash)) return reportFailure("当前歌曲不支持下载。");
    const fallback = { trackId: trackId(track), title: String(track?.title ?? track?.songName ?? ""), artist: String(track?.artist ?? track?.singerName ?? ""), quality };
    try {
      const alias = await client.cacheLookup({ catalogHash, requestedQuality: quality, effect: "none" }).catch(() => null);
      if (disposed) return null;
      if (alias?.key && (alias.status === "complete" || alias.status === "partial")) {
        const task = await client.createDownload({ key: alias.key }).catch(() => null);
        if (disposed) return null;
        if (!task) return reportFailure("创建下载任务失败，请稍后重试。");
        if (task.state === "completed") await enqueueRecordOperation(() => saveCompleted(task, { ...fallback, key: alias.key }, task.id));
        else await trackPending(task, { ...fallback, key: alias.key });
        return reportCreatedTask(task);
      }
      const source = await resolveKugouSource(ctx, track, quality).catch(() => null);
      if (disposed) return null;
      if (!source) return reportFailure("无法获取歌曲下载地址，请检查登录状态或稍后重试。");
      const helper = helperTrack(track, source);
      const task = await client.createDownload({ track: helper, remoteUrl: source.urls[0] }).catch(() => null);
      if (disposed) return null;
      if (!task) return reportFailure("创建下载任务失败，请稍后重试。");
      if (task.state === "completed") await enqueueRecordOperation(() => saveCompleted(task, { ...fallback, key: helper.key, quality: helper.quality }, task.id));
      else await trackPending(task, { ...fallback, key: helper.key, quality: helper.quality });
      return reportCreatedTask(task);
    } catch {
      return reportFailure("下载任务创建失败，请稍后重试。");
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
    if (disposed || !entry?.path || !(await ctx?.dialog?.confirm?.({ title: "删除下载", content: "确定删除此下载文件吗？" }))) return false;
    if (disposed) return false;
    const client = clientFor(runtime);
    if (!client?.deleteDownload) return false;
    try {
      await client.deleteDownload(entry.path);
    } catch (error) {
      ctx?.toast?.warning?.(error instanceof Error && error.message ? error.message : "删除下载失败，请稍后重试。");
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
      ctx?.toast?.warning?.(error instanceof Error && error.message ? error.message : "无法打开下载位置。");
      return false;
    }
  }

  function supports(track) { return /^[0-9a-f]{16,128}$/i.test(String(track?.hash ?? "").trim()) && String(track?.source ?? "").toLowerCase() !== "cloud"; }
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
    register({ id: "download", label: "下载歌曲", visible: supports, onSelect: openDialog });
    register({ id: "reveal-download", label: "打开下载位置", visible: (track) => Boolean(find(track)), onSelect: (track) => reveal(find(track)) });
    register({ id: "delete-download", label: "删除下载", visible: (track) => Boolean(find(track)), onSelect: (track) => remove(find(track)) });
    return disposers;
  }

  if (typeof options.taskStore?.subscribe === "function") {
    try {
      unsubscribeSnapshots = options.taskStore.subscribe((tasks) => { void reconcileTaskSnapshot(tasks).catch(() => undefined); });
    } catch {
      unsubscribeSnapshots = null;
    }
  }

  return Object.freeze({ request, remove, reveal, records, reconcile, reconcileTaskSnapshot, registerContextMenus, dispose() { disposed = true; activeDialog?.close?.(); activeDialog = null; unsubscribeSnapshots?.(); unsubscribeSnapshots = null; pending.clear(); } });
}
