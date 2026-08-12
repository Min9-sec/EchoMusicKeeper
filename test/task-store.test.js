import assert from "node:assert/strict";
import test from "node:test";

import { createTaskStore } from "../src/task-store.js";
import { createManagementPage } from "../src/ui/page.js";

const hash = "0123456789abcdef0123456789abcdef";
const record = { key: `${hash}.320.none`, trackId: "123", title: "Song", artist: "Artist", quality: "320", path: "D:\\Music\\Song.mp3", size: 123456, completedAt: 1770000000000 };

function clock() {
  const entries = [];
  return {
    setInterval(fn, delay) { const entry = { fn, delay, cleared: false }; entries.push(entry); return entry; },
    clearInterval(entry) { entry.cleared = true; },
    entries,
  };
}

test("task store polls every 500ms while visible, every 2s while hidden, and disposes cleanly", async () => {
  const timers = clock();
  let listCalls = 0;
  const store = createTaskStore({}, { client: { listTasks: async () => { listCalls += 1; return { tasks: [] }; }, cacheList: async () => ({ entries: [] }) } }, timers);
  store.setVisible(true);
  assert.equal(timers.entries.at(-1).delay, 500);
  await timers.entries.at(-1).fn();
  store.setVisible(false);
  assert.equal(timers.entries.at(-1).delay, 2000);
  store.dispose();
  assert.ok(timers.entries.every((entry) => entry.cleared));
  assert.ok(listCalls >= 1);
});

test("task store prevents overlapping refreshes", async () => {
  let resolve;
  let calls = 0;
  const store = createTaskStore({}, { client: { listTasks: () => { calls += 1; return new Promise((done) => { resolve = done; }); }, cacheList: async () => ({ entries: [] }) } });
  const first = store.refresh();
  const second = store.refresh();
  assert.equal(calls, 1);
  resolve({ tasks: [] });
  await Promise.all([first, second]);
  store.dispose();
});

test("task store consumes the helper's LRU items response", async () => {
  const store = createTaskStore({}, { client: { listTasks: async () => ({ tasks: [] }), cacheList: async () => ({ items: [{ key: "oldest" }], cacheBytes: 1, cacheLimitBytes: 2 }) } });
  await store.refresh();
  assert.deepEqual(store.cacheEntries.value, [{ key: "oldest" }]);
  assert.deepEqual(store.cacheStats.value, { cacheBytes: 1, cacheLimitBytes: 2 });
  store.dispose();
});

test("task store exposes helper-offline state for unavailable clients and request failures", async () => {
  const unavailable = createTaskStore({}, { client: null });
  await unavailable.refresh();
  assert.equal(unavailable.error.value, "helper-offline");
  unavailable.dispose();
  const failed = createTaskStore({}, { client: { listTasks: async () => { throw new Error("offline"); }, cacheList: async () => ({ items: [] }) } });
  await failed.refresh();
  assert.equal(failed.error.value, "helper-offline");
  failed.dispose();
});

test("task store retries failed downloads and resumes paused downloads through the approved resume route", async () => {
  const resumed = [];
  const store = createTaskStore({}, { client: {
    listTasks: async () => ({ tasks: [] }), cacheList: async () => ({ entries: [] }),
    resumeTask: async (id) => resumed.push(id),
  } });
  await store.retry({ id: "failed", kind: "download", state: "failed" });
  await store.resume({ id: "paused", kind: "download", state: "paused" });
  assert.deepEqual(resumed, ["failed", "paused"]);
  store.dispose();
});

test("failed cache retry verifies a challenge and refreshes one normalized nested URL", async () => {
  const calls = { urls: 0, verification: [], refresh: [], resume: 0 };
  const task = {
    id: "cache-failed", kind: "cache", state: "failed",
    track: { key: `${hash}.320.none`, hash, quality: "320" },
  };
  const store = createTaskStore({
    kugou: { music: { getSongUrl: async () => {
      calls.urls += 1;
      return calls.urls === 1
        ? { status: 0, error_code: 20028, eventId: "challenge-cache" }
        : { data: { backup_url: "https://backup.example/song.mp3?token=ephemeral" } };
    } } },
    kugouVerification: { request: async (id) => { calls.verification.push(id); return { ok: true }; } },
  }, { client: {
    listTasks: async () => ({ tasks: [task] }), cacheList: async () => ({ entries: [] }),
    refreshTaskUrl: async (...args) => calls.refresh.push(args),
    resumeTask: async () => { calls.resume += 1; },
  } });
  await store.retry(task);
  assert.deepEqual(calls, {
    urls: 2,
    verification: ["challenge-cache"],
    refresh: [["cache-failed", "https://backup.example/song.mp3?token=ephemeral"]],
    resume: 0,
  });
  store.dispose();
});

test("task actions force a fresh task snapshot after an in-flight stale poll", async () => {
  let resolveFirst;
  let resolveSecond;
  let secondStarted;
  const secondStartedPromise = new Promise((resolve) => { secondStarted = resolve; });
  let listCalls = 0;
  const store = createTaskStore({}, { client: {
    listTasks: () => {
      listCalls += 1;
      return new Promise((resolve) => { if (listCalls === 1) resolveFirst = resolve; else { resolveSecond = resolve; secondStarted(); } });
    },
    cacheList: async () => ({ items: [] }), pauseTask: async () => {},
  } });
  const poll = store.refresh();
  const action = store.pause({ id: "task-1" });
  resolveFirst({ tasks: [{ id: "task-1", state: "running" }] });
  await secondStartedPromise;
  assert.equal(listCalls, 2);
  resolveSecond({ tasks: [{ id: "task-1", state: "paused" }] });
  await Promise.all([poll, action]);
  assert.deepEqual(store.tasks.value, [{ id: "task-1", state: "paused" }]);
  store.dispose();
});

test("task store refreshes a needs-url task once per cache key without creating a task", async () => {
  const updates = [];
  const taskHash = "a".repeat(32);
  const task = { id: "task-1", kind: "cache", state: "needs-url", track: { key: `${taskHash}.320.none`, hash: taskHash, quality: "320" } };
  const store = createTaskStore({ kugou: { music: { getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }) } } }, { client: {
    listTasks: async () => ({ tasks: [task, { ...task, id: "task-2" }] }), cacheList: async () => ({ entries: [] }),
    refreshTaskUrl: async (...args) => updates.push(args),
  } });
  await store.refresh();
  assert.deepEqual(updates, [["task-1", "https://cdn.example/song.mp3"]]);
  store.dispose();
});

test("snapshot URL refresh shares the poller's per-key update", async () => {
  const taskHash = "b".repeat(32);
  const key = `${taskHash}.320.none`;
  const task = { id: "task-shared", kind: "cache", state: "needs-url", track: { key, hash: taskHash, quality: "320" } };
  let releaseURL;
  let startedURL;
  const urlStarted = new Promise((resolve) => { startedURL = resolve; });
  const updates = [];
  const store = createTaskStore({ kugou: { music: { getSongUrl: async () => {
    startedURL();
    return new Promise((resolve) => { releaseURL = resolve; });
  } } } }, { client: {
    listTasks: async () => ({ tasks: [task] }), cacheList: async () => ({ entries: [] }),
    refreshTaskUrl: async (...args) => updates.push(args),
  } });
  const poll = store.refresh();
  await urlStarted;
  const forced = store.refreshNeedsURL(key, "https://cdn.example/forced.mp3?token=unused");
  releaseURL({ url: "https://cdn.example/polled.mp3?token=ephemeral" });
  assert.equal(await forced, true);
  await poll;
  assert.deepEqual(updates, [["task-shared", "https://cdn.example/polled.mp3?token=ephemeral"]]);
  store.dispose();
});

test("snapshot URL refresh rejects non-cache and non-canonical task metadata", async () => {
  const validHash = "c".repeat(32);
  const key = `${validHash}.320.none`;
  const tasks = [
    { id: "download", kind: "download", state: "needs-url", track: { key, hash: validHash, quality: "320" } },
    { id: "malformed", kind: "cache", state: "needs-url", track: { key, hash: "d".repeat(32), quality: "320" } },
  ];
  const updates = [];
  const store = createTaskStore({}, { client: {
    listTasks: async () => ({ tasks }), cacheList: async () => ({ entries: [] }),
    refreshTaskUrl: async (...args) => updates.push(args),
  } });
  await store.refresh();
  assert.equal(await store.refreshNeedsURL(key, "https://cdn.example/song.mp3"), false);
  assert.deepEqual(updates, []);
  store.dispose();
});

test("task store reveal uses the real handler's tagged file and directory contract", async () => {
  const targets = [];
  const store = createTaskStore({}, { client: {
    listTasks: async () => ({ tasks: [] }), cacheList: async () => ({ entries: [] }),
    revealFile: async (target) => targets.push(target),
  } });
  await store.reveal("D:\\Music\\Song.mp3");
  await store.reveal({ kind: "open-directory", path: "D:\\Music" });
  assert.deepEqual(targets, [
    { kind: "select", path: "D:\\Music\\Song.mp3" },
    { kind: "open-directory", path: "D:\\Music" },
  ]);
  store.dispose();
});

test("task store leaves a needs-url task recoverable when URL refresh is canceled", async () => {
  let created = 0;
  const taskHash = "a".repeat(32);
  const task = { id: "task-1", kind: "cache", state: "needs-url", track: { key: `${taskHash}.320.none`, hash: taskHash, quality: "320" } };
  const store = createTaskStore({ kugou: { music: { getSongUrl: async () => { throw new Error("verification canceled"); } } } }, { client: {
    listTasks: async () => ({ tasks: [task] }), cacheList: async () => ({ items: [] }),
    createDownload: async () => { created += 1; }, refreshTaskUrl: async () => assert.fail("must not update without a URL"),
  } });
  await store.refresh();
  assert.deepEqual(store.tasks.value, [task]);
  assert.equal(created, 0);
  store.dispose();
});

test("task store disposal prevents in-flight refresh mutations and later rescheduling", async () => {
  const timers = clock();
  let resolveTasks;
  let resolveCache;
  const store = createTaskStore({ kugou: { music: { getSongUrl: async () => assert.fail("must not refresh after disposal") } } }, { client: {
    listTasks: () => new Promise((resolve) => { resolveTasks = resolve; }), cacheList: () => new Promise((resolve) => { resolveCache = resolve; }),
  } }, timers);
  const refresh = store.refresh();
  const timerCount = timers.entries.length;
  store.dispose();
  store.setVisible(true);
  resolveTasks({ tasks: [{ id: "task", state: "needs-url", track: { key: "key", hash, quality: "320" } }] });
  resolveCache({ items: [{ key: "cache" }] });
  await refresh;
  assert.deepEqual(store.tasks.value, []);
  assert.deepEqual(store.cacheEntries.value, []);
  assert.equal(timers.entries.length, timerCount);
});

test("management page wraps host component loaders and renders completed download controls", () => {
  const h = (type, props, children) => ({ type, props, children });
  const loaders = [];
  const component = createManagementPage({ vue: { h, ref: (value) => ({ value }), resolveComponent: () => "HostIcon", defineAsyncComponent(loader) { loaders.push(loader); return "HostButton"; } }, ui: { components: { Button: async () => "HostButton" } } }, {
    tasks: { value: [] }, cacheEntries: { value: [] }, cacheStats: { value: { cacheBytes: 0, cacheLimitBytes: 1 } }, error: { value: "helper-offline" },
    refresh: async () => {}, setVisible: () => {}, online: () => true,
  }, { records: { value: [record] }, reconcile: async () => {}, reveal: async () => {}, remove: async () => {} });
  const render = component.setup();
  const tree = render();
  const serialized = JSON.stringify(tree);
  assert.match(serialized, /已完成下载/);
  assert.match(serialized, /打开位置/);
  assert.match(serialized, /删除下载/);
  assert.match(serialized, /缓存服务未连接/);
  assert.match(serialized, /HostButton/);
  assert.equal(loaders.length, 1);
});

test("management page only exposes download locations recorded by the helper", () => {
  const h = (type, props, children) => ({ type, props, children });
  const nodes = (value) => {
    if (typeof value === "function") return nodes(value());
    if (Array.isArray(value)) return value.flatMap(nodes);
    if (!value || typeof value !== "object") return [];
    return [value, ...nodes(value.children)];
  };
  const outputPath = "D:\\Music\\Song.mp3";
  const task = {
    id: "download-1", kind: "download", state: "completed", outputPath,
    pausable: true, track: { title: "Song", artist: "Artist", quality: "320" },
  };
  const records = { value: [] };
  const component = createManagementPage({
    icons: {},
    vue: { h, ref: (value) => ({ value }), resolveComponent: () => "HostIcon", defineAsyncComponent: () => "HostButton" },
    ui: { components: { Button: async () => "HostButton" } },
  }, {
    tasks: { value: [task] }, cacheEntries: { value: [] }, cacheStats: { value: { cacheBytes: 0, cacheLimitBytes: 1 } }, error: { value: null },
    refresh: async () => {}, setVisible: () => {}, online: () => true,
  }, { records, reconcile: async () => {}, reveal: async () => {}, remove: async () => {} });

  const render = component.setup();
  assert.doesNotMatch(JSON.stringify(render()), /打开位置/);
  records.value = [{ ...record, path: outputPath, missing: false }];
  const tree = render();
  assert.match(JSON.stringify(tree), /打开位置/);
  const icons = nodes(tree).filter((node) => node.type === "HostIcon").map((node) => node.props);
  assert.equal(icons.some((props) => props.icon === "tabler:player-pause"), true);
  assert.equal(icons.some((props) => "name" in props), false);
});
