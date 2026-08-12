import { normalizeQuality, resolveKugouTrackURLs } from "./kugou-source.js";

const ref = (ctx, value) => typeof ctx?.vue?.ref === "function" ? ctx.vue.ref(value) : { value };
const taskList = (response) => Array.isArray(response) ? response : response?.tasks ?? [];

/** Polls helper state without overlapping requests and refreshes expired task URLs in place. */
export function createTaskStore(ctx, runtime, timers = globalThis) {
  const tasks = ref(ctx, []);
  const cacheEntries = ref(ctx, []);
  const cacheStats = ref(ctx, { cacheBytes: 0, cacheLimitBytes: 0 });
  const error = ref(ctx, null);
  let visible = false;
  let timer = null;
  let refreshing = null;
  let disposed = false;
  const refreshingKeys = new Map();
  const snapshotListeners = new Set();

  function client() { return runtime?.client ?? null; }
  function schedule() {
    if (disposed) return;
    if (timer) timers.clearInterval(timer);
    timer = timers.setInterval(() => void refresh(), visible ? 500 : 2_000);
  }
  function refreshableTask(task, state) {
    const hash = String(task?.track?.hash ?? "").trim().toLowerCase();
    const quality = normalizeQuality(task?.track?.quality);
    return task?.state === state
      && task?.kind === "cache"
      && /^[0-9a-f]{16,128}$/.test(hash)
      && task?.track?.key === `${hash}.${quality}.none`
      ? { hash, quality, key: task.track.key }
      : null;
  }
  function runKeyedRefresh(key, operation) {
    const active = refreshingKeys.get(key);
    if (active) return active;
    let current;
    current = Promise.resolve()
      .then(operation)
      .then(Boolean)
      .finally(() => { if (refreshingKeys.get(key) === current) refreshingKeys.delete(key); });
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
    const handled = new Set();
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
        // Keep last known state when the optional local helper is offline.
        if (!disposed) error.value = /cache_(unavailable|.*failed)|invalid_download_roots/.test(String(failure?.code ?? "")) ? "directory-error" : "helper-offline";
      } finally { refreshing = null; }
    })();
    return refreshing;
  }
  async function action(method, task) { if (disposed) return; await client()?.[method]?.(task?.id); if (!disposed) await refresh({ force: true }); }
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
  function setVisible(next) { if (disposed) return; visible = Boolean(next); schedule(); void refresh(); }
  schedule();
  return Object.freeze({
    tasks, cacheEntries, cacheStats, error,
    online: () => {
      const state = typeof runtime?.status === "function" ? runtime.status()?.state : runtime?.status?.state;
      return Boolean(client()) && (!state || state === "online" || state === "ready");
    }, setVisible, refresh, refreshNeedsURL,
    subscribe(listener) {
      if (disposed || typeof listener !== "function") return () => {};
      snapshotListeners.add(listener);
      listener(tasks.value);
      return () => snapshotListeners.delete(listener);
    },
    pause: (task) => action("pauseTask", task), resume: (task) => action("resumeTask", task),
    cancel: (task) => action("cancelTask", task), retry,
    reveal: (target) => client()?.revealFile?.(typeof target === "string" ? { kind: "select", path: target } : target),
    deleteCache: async (entry) => { if (disposed) return; await client()?.deleteCache?.(entry?.key ?? entry); if (!disposed) await refresh(); },
    clearCache: async () => { if (disposed) return; await client()?.clearCache?.(); if (!disposed) await refresh(); },
    dispose() { disposed = true; if (timer) timers.clearInterval(timer); timer = null; snapshotListeners.clear(); },
  });
}
