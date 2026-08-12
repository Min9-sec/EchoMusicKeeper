import assert from "node:assert/strict";
import test from "node:test";

import { createDownloadController } from "../src/downloads.js";
import { createTaskStore } from "../src/task-store.js";

const hash = "0123456789abcdef0123456789abcdef";
const key = `${hash}.320.none`;
const record = {
  key,
  trackId: "123",
  title: "Song",
  artist: "Artist",
  quality: "320",
  path: "D:\\Music\\Artist - Song [320].mp3",
  size: 123456,
  completedAt: 1770000000000,
};

function context(overrides = {}) {
  const stored = overrides.stored ?? [];
  return {
    storage: { get: async () => stored, set: async (name, value) => { overrides.saved?.push([name, value]); } },
    fs: { getFileUrl: async () => ({ ok: true, url: "file:///song.mp3" }) },
    dialog: { confirm: async () => true },
    kugou: { music: {
      getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "320", hash }] }] }),
      getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
    } },
    ...overrides,
  };
}

test("download exports a completed cache hit and persists normalized plain records", async () => {
  const calls = [];
  const saved = [];
  const controller = createDownloadController(context({ saved }), {
    status: () => ({ state: "online" }),
    client: {
      cacheLookup: async () => ({ status: "complete", key, path: "D:\\cache\\song.mp3", size: 123456, track: { key, quality: "320", hash, title: "Song", artist: "Artist" } }),
      createDownload: async (body) => { calls.push(body); return { state: "completed", outputPath: record.path, downloadedBytes: 123456, track: { key, quality: "320", title: "Song", artist: "Artist" } }; },
      updateConfig: async (value) => { calls.push(value); },
    },
  }, { value: { downloadRoot: "D:\\Music" } });

  await controller.request({ id: 123, hash, title: "Song", artist: "Artist" }, "320");
  assert.deepEqual(calls.find((call) => call.key === key), { key });
  assert.deepEqual(controller.records.value, [{ ...record, completedAt: controller.records.value[0].completedAt }]);
  assert.equal(typeof controller.records.value[0].completedAt, "number");
  assert.equal(saved[0][0], "echo-music-keeper-downloads");
  assert.deepEqual(saved[0][1], [{ ...record, completedAt: saved[0][1][0].completedAt }]);
  const config = calls.find((call) => call.completedDownloadPaths?.includes(record.path));
  assert.deepEqual(config.completedDownloadPaths, [record.path]);
  assert.deepEqual(config.managedDownloadRoots, ["D:\\Music"]);
});

test("completed exports retain distinct same-key paths and restore every helper allowlist entry", async () => {
  const oldPath = "D:\\Old Music\\Artist - Song [320].mp3";
  const newPath = "E:\\New Music\\Artist - Song [320].mp3";
  const storage = new Map([
    ["echo-music-keeper-downloads", []],
    ["echo-music-keeper-download-pending", [
      { id: "task-old", key, trackId: "123", title: "Song", artist: "Artist", quality: "320" },
      { id: "task-new", key, trackId: "123", title: "Song", artist: "Artist", quality: "320" },
    ]],
  ]);
  const configs = [];
  const controller = createDownloadController({
    ...context(),
    storage: { get: async (name) => storage.get(name), set: async (name, value) => storage.set(name, value) },
  }, { client: { updateConfig: async (config) => configs.push(config) } }, { value: { downloadRoot: "E:\\New Music" } });
  await controller.reconcileTaskSnapshot([
    { id: "task-old", state: "completed", outputPath: oldPath, totalBytes: 100, track: { key, quality: "320" } },
    { id: "task-new", state: "completed", outputPath: newPath, totalBytes: 100, track: { key, quality: "320" } },
  ]);
  assert.deepEqual(controller.records.value.map((entry) => entry.path).sort(), [oldPath, newPath].sort());
  assert.deepEqual(configs.at(-1).completedDownloadPaths.sort(), [oldPath, newPath].sort());
  assert.deepEqual(configs.at(-1).managedDownloadRoots.sort(), ["D:\\Old Music", "E:\\New Music"].sort());
  controller.dispose();
});

test("deleting one same-key export retains the other exact path", async () => {
  const oldRecord = { ...record, path: "D:\\Old Music\\Artist - Song [320].mp3" };
  const newRecord = { ...record, path: "E:\\New Music\\Artist - Song [320].mp3" };
  const saved = [];
  const deleted = [];
  const configs = [];
  const controller = createDownloadController(context({ stored: [oldRecord, newRecord], saved }), { client: {
    deleteDownload: async (path) => deleted.push(path),
    updateConfig: async (config) => configs.push(config),
  } }, { value: { downloadRoot: "E:\\New Music" } });
  assert.equal(await controller.remove(oldRecord), true);
  assert.deepEqual(deleted, [oldRecord.path]);
  assert.deepEqual(controller.records.value, [newRecord]);
  assert.deepEqual(saved.filter(([name]) => name === "echo-music-keeper-downloads").at(-1)[1], [newRecord]);
  assert.deepEqual(configs.at(-1).completedDownloadPaths, [newRecord.path]);
  controller.dispose();
});

test("download reveal selects the completed file through the real handler contract", async () => {
  const targets = [];
  const controller = createDownloadController(context({ stored: [record] }), {
    client: { revealFile: async (target) => targets.push(target) },
  }, { value: {} });
  await controller.reveal(record);
  assert.deepEqual(targets, [{ kind: "select", path: record.path }]);
  controller.dispose();
});

test("download attaches an export to an active cache task before resolving a second source", async () => {
  const calls = [];
  const controller = createDownloadController(context(), {
    status: () => ({ state: "online" }),
    client: {
      cacheLookup: async () => ({ status: "partial", key, track: { key, quality: "320", hash } }),
      createDownload: async (body) => { calls.push(body); return { state: "queued" }; },
    },
  }, { value: {} });
  await controller.request({ id: "123", hash, title: "Song", artist: "Artist" }, "320");
  assert.deepEqual(calls, [{ key }]);
});

test("download creates a standalone task when its cache key is absent", async () => {
  const calls = [];
  const controller = createDownloadController(context(), {
    status: () => ({ state: "online" }),
    client: {
      cacheLookup: async () => ({ status: "miss" }),
      createDownload: async (body) => { calls.push(body); return { state: "queued" }; },
    },
  }, { value: {} });
  await controller.request({ id: "123", hash, title: "Song", artist: "Artist" }, "320");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].track.key, key);
  assert.equal(calls[0].remoteUrl, "https://cdn.example/song.mp3");
});

test("download reconciliation marks missing files without dropping persisted records", async () => {
  const controller = createDownloadController(context({ stored: [record], fs: { getFileUrl: async () => ({ ok: false }) } }), { client: null }, { value: {} });
  await controller.reconcile();
  assert.deepEqual(controller.records.value, [{ ...record, missing: true }]);
});

test("download deletion requires confirmation before calling the approved delete endpoint", async () => {
  let deleted = 0;
  const controller = createDownloadController(context({ stored: [record], dialog: { confirm: async () => false } }), {
    client: { deleteDownload: async () => { deleted += 1; } },
  }, { value: {} });
  await controller.remove(record);
  assert.equal(deleted, 0);
  assert.equal(controller.records.value.length, 1);
});

test("downloads use task-store snapshots instead of an independent listTasks scheduler", async () => {
  const saved = [];
  let nextID = 0;
  let publishSnapshot;
  const controller = createDownloadController(context({ saved }), { client: {
    cacheLookup: async () => ({ status: "partial", key, track: { key, quality: "320", hash } }),
    createDownload: async () => ({ id: `task-${++nextID}`, state: "queued" }),
  } }, { value: {} }, { taskStore: { subscribe(listener) { publishSnapshot = listener; return () => {}; } } });
  await controller.request({ id: "123", hash, title: "Song", artist: "Artist" }, "320");
  await controller.request({ id: "124", hash, title: "Song 2", artist: "Artist" }, "320");
  const pending = saved.filter(([name]) => name === "echo-music-keeper-download-pending").at(-1)[1];
  assert.deepEqual(pending.map((entry) => entry.id), ["task-1", "task-2"]);
  publishSnapshot([{ id: "task-1", state: "completed", outputPath: record.path, totalBytes: record.size, track: { key, quality: "320", title: "Song", artist: "Artist" } }]);
  await controller.reconcileTaskSnapshot([{ id: "task-1", state: "completed", outputPath: record.path, totalBytes: record.size, track: { key, quality: "320", title: "Song", artist: "Artist" } }]);
  assert.equal(controller.records.value.length, 1);
  controller.dispose();
});

test("pending downloads survive reload and finalize helper tasks into one persisted record", async () => {
  const storage = new Map([["echo-music-keeper-downloads", []], ["echo-music-keeper-download-pending", [{ id: "task-1", key, trackId: "123", title: "Song", artist: "Artist", quality: "320" }]]]);
  const calls = [];
  const controller = createDownloadController({
    ...context(), storage: { get: async (name) => storage.get(name), set: async (name, value) => storage.set(name, value) },
  }, { client: {
    updateConfig: async (config) => calls.push(config),
  } }, { value: { downloadRoot: "D:\\Music" } });
  await controller.reconcileTaskSnapshot([{ id: "task-1", state: "completed", outputPath: record.path, totalBytes: record.size, track: { key, quality: "320", title: "Song", artist: "Artist" } }]);
  assert.deepEqual(controller.records.value, [{ ...record, completedAt: controller.records.value[0].completedAt }]);
  assert.deepEqual(storage.get("echo-music-keeper-download-pending"), []);
  assert.deepEqual(calls.at(-1).completedDownloadPaths, [record.path]);
  controller.dispose();
});

test("pending metadata survives failed resume retry and completed task reconciliation", async () => {
  const storage = new Map([["echo-music-keeper-downloads", []], ["echo-music-keeper-download-pending", []]]);
  const calls = [];
  const resumed = [];
  let state = "queued";
  const task = () => ({ id: "task-1", state, outputPath: state === "completed" ? record.path : "", totalBytes: record.size, retryable: state === "failed", track: { key, quality: "320", title: "Song", artist: "Artist" } });
  const client = {
    cacheLookup: async () => ({ status: "partial", key, track: { key, quality: "320", hash } }),
    createDownload: async () => ({ id: "task-1", state: "queued" }),
    listTasks: async () => ({ tasks: [task()] }), cacheList: async () => ({ items: [] }),
    resumeTask: async (id) => { resumed.push(id); state = "completed"; },
    updateConfig: async (config) => calls.push(config),
  };
  const taskStore = createTaskStore({}, { client });
  const controller = createDownloadController({
    ...context(), storage: { get: async (name) => storage.get(name), set: async (name, value) => storage.set(name, value) },
  }, { client }, { value: { downloadRoot: "D:\\Music" } }, { taskStore });
  await controller.request({ id: "123", hash, title: "Song", artist: "Artist" }, "320");
  state = "failed";
  await taskStore.refresh();
  await controller.reconcileTaskSnapshot(taskStore.tasks.value);
  assert.equal(storage.get("echo-music-keeper-download-pending").length, 1);
  await taskStore.retry(taskStore.tasks.value[0]);
  await controller.reconcileTaskSnapshot(taskStore.tasks.value);
  assert.deepEqual(resumed, ["task-1"]);
  assert.deepEqual(storage.get("echo-music-keeper-download-pending"), []);
  assert.equal(controller.records.value.length, 1);
  assert.equal(controller.records.value[0].path, record.path);
  assert.deepEqual(calls.at(-1).completedDownloadPaths, [record.path]);
  controller.dispose();
  taskStore.dispose();
});

test("task snapshots do not scan files and serialize completion behind an explicit file reconciliation", async () => {
  const otherHash = "abcdef0123456789abcdef0123456789";
  const otherKey = `${otherHash}.320.none`;
  const otherRecord = { ...record, key: otherKey, path: "D:\\Music\\Artist - Other [320].mp3", title: "Other" };
  const storage = new Map([["echo-music-keeper-downloads", [record]], ["echo-music-keeper-download-pending", [{ id: "task-2", key: otherKey, trackId: "124", title: "Other", artist: "Artist", quality: "320" }]]]);
  let fileCalls = 0;
  let releaseScan;
  let scanStarted;
  const scanStartedPromise = new Promise((resolve) => { scanStarted = resolve; });
  const controller = createDownloadController({
    ...context(), storage: { get: async (name) => storage.get(name), set: async (name, value) => storage.set(name, value) },
    fs: { getFileUrl: async () => {
      fileCalls += 1;
      if (fileCalls <= 2) return { ok: true, url: "file:///initial.mp3" };
      scanStarted();
      return new Promise((resolve) => { releaseScan = () => resolve({ ok: true, url: "file:///scan.mp3" }); });
    } },
  }, { client: { updateConfig: async () => {} } }, { value: {} });
  await controller.reconcile();
  const baselineCalls = fileCalls;
  const scan = controller.reconcile();
  const completion = controller.reconcileTaskSnapshot([{ id: "task-2", state: "completed", outputPath: otherRecord.path, totalBytes: otherRecord.size, track: { key: otherKey, quality: "320", title: "Other", artist: "Artist" } }]);
  await scanStartedPromise;
  assert.equal(fileCalls, baselineCalls + 1);
  releaseScan();
  await Promise.all([scan, completion]);
  assert.deepEqual(controller.records.value.map((entry) => entry.path).sort(), [record.path, otherRecord.path].sort());
  controller.dispose();
});

test("task snapshot subscription contains asynchronous persistence failures", async () => {
  const storage = new Map([["echo-music-keeper-downloads", []], ["echo-music-keeper-download-pending", []]]);
  let publish;
  let unhandled = null;
  const onUnhandled = (failure) => { unhandled = failure; };
  process.once("unhandledRejection", onUnhandled);
  const controller = createDownloadController({
    ...context(), storage: {
      get: async (name) => storage.get(name),
      set: async (name, value) => {
        if (name === "echo-music-keeper-downloads") throw new Error("storage unavailable");
        storage.set(name, value);
      },
    },
  }, { client: {
    cacheLookup: async () => ({ status: "partial", key, track: { key, quality: "320", hash } }),
    createDownload: async () => ({ id: "task-1", state: "queued" }),
  } }, { value: {} }, { taskStore: { subscribe(listener) { publish = listener; return () => {}; } } });
  await controller.request({ id: "123", hash, title: "Song", artist: "Artist" }, "320");
  publish([{ id: "task-1", state: "completed", outputPath: record.path, totalBytes: record.size, track: { key, quality: "320", title: "Song", artist: "Artist" } }]);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  process.removeListener("unhandledRejection", onUnhandled);
  assert.equal(unhandled, null);
  controller.dispose();
});
