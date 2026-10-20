import assert from "node:assert/strict";
import test from "node:test";
import { createManagementPage } from "../src/ui/page.js";
import { createTaskStore } from "../src/task-store.js";

function nodes(value) {
  if (typeof value === "function") return nodes(value());
  if (Array.isArray(value)) return value.flatMap(nodes);
  if (!value || typeof value !== "object") return [];
  return [value, ...nodes(value.children)];
}

test("cache controls use plugin confirmation and invoke the helper only after approval", async () => {
  for (const action of ["删除缓存", "清空缓存"]) {
    for (const accept of [false, true]) {
      let dialog;
      const calls = [];
      const warnings = [];
      const entry = { key: "a".repeat(32) + ".320.none", size: 12 };
      const store = createTaskStore({}, { client: {
        listTasks: async () => ({ tasks: [] }), cacheList: async () => ({ items: [entry] }),
        deleteCache: async (key) => calls.push(["delete", key]), clearCache: async () => calls.push(["clear"]),
      } });
      await store.refresh();
      const page = createManagementPage({
        dialog: { selectDirectory() {} },
        ui: { teleport(component) { dialog = component.setup()(); return () => {}; } },
        toast: { warning: (message) => warnings.push(message) },
      }, store, { records: { value: [] } });
      const render = page.setup();
      const cacheTab = nodes(render()).find((node) => typeof node.children === "function" && node.children() === "缓存管理");
      cacheTab.props.onClick();
      const button = nodes(render()).find((node) => node.props?.title === action || (typeof node.children === "function" && node.children() === action));
      const pending = button.props.onClick();
      assert.deepEqual(calls, []);
      nodes(dialog).find((node) => node.type === "button" && node.children === (accept ? "确定" : "取消")).props.onClick();
      await pending;
      assert.deepEqual(calls, accept ? (action === "删除缓存" ? [["delete", entry.key]] : [["clear"]]) : []);
      assert.deepEqual(warnings, []);
      store.dispose();
    }
  }
});

test("cache mutations report a disconnected helper instead of silently succeeding", async () => {
  const store = createTaskStore({}, { client: null });
  await assert.rejects(store.deleteCache({ key: "cache" }), /缓存服务未连接/);
  await assert.rejects(store.clearCache(), /缓存服务未连接/);
  store.dispose();
});
