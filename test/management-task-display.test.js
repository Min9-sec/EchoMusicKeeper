import assert from "node:assert/strict";
import test from "node:test";
import { createManagementPage } from "../src/ui/page.js";

function nodes(value) {
  if (typeof value === "function") return nodes(value());
  if (Array.isArray(value)) return value.flatMap(nodes);
  if (!value || typeof value !== "object") return [];
  return [value, ...nodes(value.children)];
}

async function pageWith(tasks, records = []) {
  const calls = [];
  const store = {
    tasks: { value: tasks }, cacheEntries: { value: [] },
    cacheStats: { value: { cacheBytes: 0, cacheLimitBytes: 1024 } }, error: { value: null },
    refresh: async () => {}, setVisible() {},
    pause: async (task) => calls.push(["pause", task.id]),
    retry: async (task) => calls.push(["retry", task.id]),
  };
  const downloads = { records: { value: records }, reconcile: async () => {} };
  let mounted;
  const page = createManagementPage({ vue: { onMounted(callback) { mounted = callback; } } }, store, downloads);
  const render = page.setup();
  mounted();
  await new Promise(setImmediate);
  return { render, store, downloads, calls,
    cache() { nodes(render()).find((node) => typeof node.children === "function" && node.children() === "缓存管理").props.onClick(); },
  };
}

const task = (id, kind, state, extra = {}) => ({ id, kind, state, track: { title: id }, ...extra });
const rows = (render) => nodes(render()).filter((node) => node.props?.class === "echo-music-keeper-task").map((node) => node.props.key);

test("management tabs separate cache and download tasks while retaining recoverable failures", async () => {
  const tasks = [
    task("download-running", "download", "running"), task("download-queued", "download", "queued"),
    task("download-paused", "download", "paused"), task("download-failed", "download", "failed"),
    task("download-complete", "download", "completed"), task("download-canceled", "download", "canceled"),
    task("cache-running", "cache", "running", { pausable: true }), task("cache-needs-url", "cache", "needs-url"),
    task("cache-failed", "cache", "failed"), task("cache-complete", "cache", "completed"), task("cache-canceled", "cache", "canceled"),
  ];
  const page = await pageWith(tasks);
  assert.deepEqual(rows(page.render), ["download-running", "download-queued", "download-paused", "download-failed"]);
  page.cache();
  assert.deepEqual(rows(page.render), ["cache-running", "cache-needs-url", "cache-failed"]);
  const controls = nodes(page.render());
  await controls.find((node) => node.props?.title === "暂停").props.onClick();
  await controls.find((node) => node.props?.title === "重试").props.onClick();
  assert.deepEqual(page.calls, [["pause", "cache-running"], ["retry", "cache-failed"]]);
  assert.equal(page.store.tasks.value.length, tasks.length);
});

test("finished downloads disappear from progress rows but remain manageable in completed downloads", async () => {
  const download = task("download", "download", "running");
  const page = await pageWith([download, task("cache", "cache", "running")]);
  assert.deepEqual(rows(page.render), ["download"]);
  download.state = "completed";
  page.downloads.records.value = [{ title: "Saved song", path: "/Music/saved.mp3", size: 12 }];
  assert.deepEqual(rows(page.render), []);
  const tree = page.render();
  assert.match(JSON.stringify(tree), /暂无下载任务/);
  assert.match(JSON.stringify(tree), /Saved song/);
  assert.equal(nodes(tree).filter((node) => node.props?.title === "打开位置").length, 1);
  assert.equal(nodes(tree).filter((node) => node.props?.title === "删除下载").length, 1);
  assert.equal(page.store.tasks.value.length, 2);
});
