import assert from "node:assert/strict";
import test from "node:test";

import { createManagementPage } from "../src/ui/page.js";

const hash = "0123456789abcdef0123456789abcdef";
const saved = {
  key: `${hash}.320.none`, trackId: "123", title: "Saved Song", artist: "Artist", quality: "320",
  path: "D:\\Music\\Saved Song.mp3", size: 4096, completedAt: 1770000000000,
};

function nodes(value) {
  if (typeof value === "function") return nodes(value());
  if (Array.isArray(value)) return value.flatMap(nodes);
  if (!value || typeof value !== "object") return [];
  return [value, ...nodes(value.children)];
}

function serialized(tree) {
  return JSON.stringify(tree);
}

function labels(tree) {
  return nodes(tree).flatMap((node) => {
    if (typeof node.children === "function") {
      const rendered = node.children();
      return typeof rendered === "string" ? [rendered] : [];
    }
    return typeof node.children === "string" ? [node.children] : [];
  });
}

function task(overrides) {
  return {
    downloadedBytes: 0, totalBytes: 100, bytesPerSecond: 0,
    track: { title: overrides.title, artist: "Artist", quality: overrides.quality ?? "320" },
    ...overrides,
  };
}

function mount(tasks, { records = [], cacheEntries = [], error = null } = {}) {
  const hooks = [];
  const actions = [];
  const taskStore = {
    tasks: { value: tasks },
    cacheEntries: { value: cacheEntries },
    cacheStats: { value: { cacheBytes: 10, cacheLimitBytes: 100 } },
    error: { value: error },
    refresh: async () => {},
    setVisible: () => {},
    pause: async (entry) => actions.push(["pause", entry?.id]),
    resume: async (entry) => actions.push(["resume", entry?.id]),
    cancel: async (entry) => actions.push(["cancel", entry?.id]),
    retry: async (entry) => actions.push(["retry", entry?.id]),
    deleteCache: async () => {},
    clearCache: async () => {},
  };
  const component = createManagementPage({
    icons: {},
    vue: {
      h: (type, props, children) => ({ type, props, children }),
      ref: (value) => ({ value }),
      onMounted: (fn) => hooks.push(fn),
      onUnmounted: () => {},
      resolveComponent: () => "HostIcon",
      defineAsyncComponent: () => "HostButton",
    },
    ui: { components: { Button: async () => "HostButton" } },
  }, taskStore, {
    records: { value: records },
    reconcile: async () => {},
    reveal: async () => {},
    remove: async () => {},
  });
  const render = component.setup();
  const clickTab = (title) => {
    const tab = nodes(render()).find((node) => typeof node.children === "function" && node.children() === title);
    assert.ok(tab, `missing tab ${title}`);
    tab.props.onClick();
  };
  return {
    actions,
    render,
    async loaded() {
      for (const hook of hooks) hook();
      await new Promise((resolve) => setImmediate(resolve));
      return render();
    },
    downloadTab() {
      clickTab("下载任务");
      return render();
    },
    cacheTab() {
      clickTab("缓存管理");
      return render();
    },
  };
}

const titles = {
  running: "Running Download",
  paused: "Paused Download",
  failed: "Failed Download",
  needsUrl: "Needs URL Download",
  queued: "Queued Download",
  buffering: "Buffering Download",
  completed: "Completed Download",
  canceled: "Canceled Download",
  unspecified: "Unspecified Download",
  cacheRunning: "Running Cache",
  cacheFailed: "Failed Cache",
  cacheNeedsUrl: "Needs URL Cache",
  cacheCompleted: "Completed Cache",
  cacheCanceled: "Canceled Cache",
  other: "Other Task",
};

function mixedTasks() {
  return [
    null,
    task({ id: "dl-run", kind: "download", state: "running", downloadedBytes: 50, pausable: true, cancelable: true, title: titles.running }),
    task({ id: "dl-pause", kind: "download", state: "paused", pausable: true, title: titles.paused }),
    task({ id: "dl-fail", kind: "download", state: "failed", retryable: true, title: titles.failed }),
    task({ id: "dl-url", kind: "download", state: "needs-url", title: titles.needsUrl }),
    task({ id: "dl-queue", kind: "download", state: "queued", title: titles.queued }),
    task({ id: "dl-buffer", kind: "download", state: "buffering", title: titles.buffering }),
    task({ id: "dl-done", kind: "download", state: "completed", downloadedBytes: 100, outputPath: saved.path, title: titles.completed }),
    task({ id: "dl-cancel", kind: "download", state: "canceled", title: titles.canceled }),
    task({ id: "dl-open", kind: "download", title: titles.unspecified }),
    task({ id: "cache-run", kind: "cache", state: "running", downloadedBytes: 40, cancelable: true, title: titles.cacheRunning }),
    task({ id: "cache-fail", kind: "cache", state: "failed", retryable: true, title: titles.cacheFailed }),
    task({ id: "cache-url", kind: "cache", state: "needs-url", retryable: true, title: titles.cacheNeedsUrl }),
    task({ id: "cache-done", kind: "cache", state: "completed", downloadedBytes: 100, title: titles.cacheCompleted }),
    task({ id: "cache-cancel", kind: "cache", state: "canceled", title: titles.cacheCanceled }),
    task({ id: "other", kind: "preview", state: "running", title: titles.other }),
  ];
}

test("download tab lists active downloads and omits cache progress and finished downloads", () => {
  const view = mount(mixedTasks());
  const tree = serialized(view.render());
  for (const title of [titles.running, titles.paused, titles.failed, titles.needsUrl, titles.queued, titles.buffering, titles.unspecified]) {
    assert.match(tree, new RegExp(title));
  }
  for (const title of [titles.completed, titles.canceled, titles.cacheRunning, titles.cacheFailed, titles.cacheNeedsUrl, titles.cacheCompleted, titles.cacheCanceled, titles.other]) {
    assert.doesNotMatch(tree, new RegExp(title));
  }
  assert.match(tree, /is-running/);
  assert.match(tree, /is-paused/);
  assert.match(tree, /is-failed/);
  assert.match(tree, /is-needs-url/);
  assert.match(tree, /50%/);
  assert.doesNotMatch(tree, /is-completed/);
  assert.doesNotMatch(tree, /is-canceled/);
  assert.doesNotMatch(tree, /100%/);
  assert.doesNotMatch(tree, /缓存进度/);
  assert.doesNotMatch(tree, /echo-music-keeper-cache-progress/);
  const buttons = nodes(view.render()).map((node) => node.props?.title).filter(Boolean);
  assert.ok(buttons.includes("暂停"));
  assert.ok(buttons.includes("继续"));
  assert.ok(buttons.includes("重试"));
  assert.equal(buttons.includes("取消"), true);
});

test("cache tab shows in-progress cache tasks and hides finished cache tasks and downloads", async () => {
  const view = mount(mixedTasks(), {
    cacheEntries: [{ key: `${hash}.128.none`, size: 2048, track: { title: "Stored Cache", artist: "Artist" } }],
  });
  const tree = serialized(view.cacheTab());
  for (const title of [titles.cacheRunning, titles.cacheFailed, titles.cacheNeedsUrl, "Stored Cache"]) {
    assert.match(tree, new RegExp(title));
  }
  for (const title of [titles.running, titles.paused, titles.failed, titles.completed, titles.canceled, titles.cacheCompleted, titles.cacheCanceled, titles.other, titles.unspecified]) {
    assert.doesNotMatch(tree, new RegExp(title));
  }
  assert.match(tree, /缓存进度/);
  assert.match(tree, /echo-music-keeper-cache-progress/);
  assert.match(tree, /40%/);
  assert.ok(labels(view.cacheTab()).includes("清空缓存"));
  assert.doesNotMatch(tree, /100%/);
  assert.doesNotMatch(tree, /is-completed/);
  const loaded = serialized(await view.loaded());
  assert.doesNotMatch(loaded, /暂无完整缓存/);
  const buttons = nodes(view.cacheTab()).map((node) => node.props?.title).filter(Boolean);
  assert.ok(buttons.includes("取消"));
  assert.ok(buttons.includes("重试"));
  assert.ok(buttons.includes("删除缓存"));
  assert.equal(buttons.includes("暂停"), false);
  assert.equal(buttons.includes("继续"), false);
});

test("finished downloads leave the progress list while saved files stay under completed downloads", async () => {
  const view = mount([
    task({ id: "done", kind: "download", state: "completed", downloadedBytes: 100, outputPath: saved.path, pausable: true, title: titles.completed }),
    task({ id: "cancel", kind: "download", state: "canceled", title: titles.canceled }),
  ], { records: [saved, { ...saved, path: "D:\\Music\\Missing.mp3", title: "Missing File", missing: true }] });
  const loading = serialized(view.render());
  assert.doesNotMatch(loading, /暂无进行中的下载/);
  assert.doesNotMatch(loading, new RegExp(titles.completed));
  assert.doesNotMatch(loading, new RegExp(titles.canceled));
  const tree = serialized(await view.loaded());
  assert.match(tree, /暂无进行中的下载/);
  assert.match(tree, /已完成下载/);
  assert.match(tree, /Saved Song/);
  assert.doesNotMatch(tree, /Missing File/);
  assert.doesNotMatch(tree, new RegExp(titles.completed));
  assert.doesNotMatch(tree, new RegExp(titles.canceled));
  assert.doesNotMatch(tree, /is-completed/);
  const buttons = nodes(view.render()).map((node) => node.props?.title).filter(Boolean);
  assert.deepEqual(buttons, ["打开位置", "删除下载"]);
});

test("cache-only and empty task lists leave the download tab empty and omit idle cache progress", async () => {
  const cacheOnly = mount([
    task({ id: "cache-run", kind: "cache", state: "running", downloadedBytes: 40, cancelable: true, title: titles.cacheRunning }),
    task({ id: "cache-done", kind: "cache", state: "completed", downloadedBytes: 100, title: titles.cacheCompleted }),
  ]);
  const download = serialized(await cacheOnly.loaded());
  assert.match(download, /暂无进行中的下载/);
  assert.match(download, /暂无已完成下载/);
  assert.doesNotMatch(download, new RegExp(titles.cacheRunning));
  assert.doesNotMatch(download, new RegExp(titles.cacheCompleted));
  const cache = serialized(cacheOnly.cacheTab());
  assert.match(cache, new RegExp(titles.cacheRunning));
  assert.doesNotMatch(cache, new RegExp(titles.cacheCompleted));
  assert.match(cache, /缓存进度/);
  assert.match(cache, /暂无完整缓存/);

  const empty = mount([]);
  const emptyDownload = serialized(await empty.loaded());
  assert.match(emptyDownload, /暂无进行中的下载/);
  assert.match(emptyDownload, /暂无已完成下载/);
  assert.doesNotMatch(emptyDownload, /echo-music-keeper-task\"/);
  const emptyCache = serialized(empty.cacheTab());
  assert.doesNotMatch(emptyCache, /缓存进度/);
  assert.doesNotMatch(emptyCache, /echo-music-keeper-cache-progress/);
  assert.match(emptyCache, /暂无完整缓存/);
  assert.ok(labels(empty.cacheTab()).includes("清空缓存"));

  const missing = mount(null);
  assert.doesNotThrow(() => missing.render());
  const missingDownload = serialized(await missing.loaded());
  assert.match(missingDownload, /暂无进行中的下载/);
  assert.doesNotMatch(serialized(missing.cacheTab()), /缓存进度/);
});

test("download and cache progress actions invoke the matching task", async () => {
  const view = mount([
    task({ id: "dl-run", kind: "download", state: "running", pausable: true, cancelable: true, title: titles.running }),
    task({ id: "cache-fail", kind: "cache", state: "failed", retryable: true, title: titles.cacheFailed }),
  ]);
  const pause = nodes(view.render()).find((node) => node.props?.title === "暂停");
  await pause.props.onClick();
  view.cacheTab();
  const retry = nodes(view.render()).find((node) => node.props?.title === "重试");
  await retry.props.onClick();
  assert.deepEqual(view.actions, [["pause", "dl-run"], ["retry", "cache-fail"]]);
});
