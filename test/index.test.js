import assert from "node:assert/strict";
import test from "node:test";

import { activate, deactivate, registerEchoMusicKeeperFeatures } from "../src/index.js";

function registrationContext(events = []) {
  return {
    icons: { iconArrowBarToDown: "download-icon" },
    player: { audioSource: { register(resolver) {
      events.push(["register", "resolver", resolver]);
      return () => events.push(["dispose", "resolver"]);
    } } },
    ui: {
      addSongContextMenuItem(item) {
        events.push(["register", `menu:${item.id}`, item]);
        return () => events.push(["dispose", `menu:${item.id}`]);
      },
      addPage(page) {
        events.push(["register", "page", page]);
        return () => events.push(["dispose", "page"]);
      },
      settings: { define(settings) {
        events.push(["register", "settings", settings]);
        return () => events.push(["dispose", "settings"]);
      } },
    },
    css: { inject(styles, options) {
      events.push(["register", "css", { styles, options }]);
      return () => events.push(["dispose", "css"]);
    } },
    storage: { get: async () => [], set: async () => {} },
    vue: { ref: (value) => ({ value }) },
  };
}

test("registers every feature once and returns one disposer per owned resource", async () => {
  const events = [];
  const ctx = registrationContext(events);
  const runtime = { client: null, status: () => ({ state: "fallback" }) };
  const settings = { value: { autoCache: true, cacheRoot: "", downloadRoot: "", cacheLimitBytes: 1024 ** 3 } };

  const disposers = registerEchoMusicKeeperFeatures(ctx, runtime, settings);
  const registrations = events.filter(([event]) => event === "register");
  assert.deepEqual(registrations.map(([, name]) => name), [
    "resolver", "menu:download", "menu:reveal-download", "menu:delete-download", "page", "settings", "css",
  ]);
  assert.equal(disposers.length, 9);
  assert.equal(registrations[0][2].id, "echo-music-keeper");
  for (const [, name, item] of registrations.filter(([, name]) => name.startsWith("menu:"))) {
    assert.equal(typeof item.label, "string");
    assert.equal(typeof item.onSelect, "function");
    assert.equal(item.title, undefined);
    assert.equal(item.onClick, undefined);
  }
  assert.deepEqual(registrations[4][2], {
    id: "echo-music-keeper-manager",
    title: "缓存与下载",
    component: registrations[4][2].component,
    sidebar: { title: "缓存与下载", icon: "download-icon" },
  });
  assert.equal(registrations[5][2].title, "EchoMusicKeeper");
  assert.equal(registrations[6][2].options.id, "echo-music-keeper-runtime");

  for (const dispose of disposers.reverse()) await dispose();
  assert.deepEqual(events.filter(([event]) => event === "dispose").map(([, name]) => name), [
    "css", "settings", "page", "menu:delete-download", "menu:reveal-download", "menu:download", "resolver",
  ]);
});

test("activation normalizes settings, contains helper launch failure, and deactivates twice safely", async () => {
  await deactivate();
  const events = [];
  const refs = [];
  const ctx = {
    ...registrationContext(events),
    descriptor: { directory: "C:\\Plugins\\echo-music-keeper" },
    storage: { get: async (key) => {
      events.push(["storage", key]);
      if (key === "echo-music-keeper-settings") {
        return { autoCache: 0, cacheRoot: "  D:\\Cache  ", downloadRoot: null, cacheLimitBytes: 1 };
      }
      return [];
    }, set: async () => {} },
    vue: { ref(value) { const result = { value }; refs.push(result); return result; } },
    process: {
      launch: async () => { events.push(["helper", "launch"]); throw new Error("consent denied"); },
      terminate: async (pid) => events.push(["helper", `terminate:${pid}`]),
    },
    dispose(dispose) { events.push(["register", "host-dispose", dispose]); },
  };

  await assert.doesNotReject(activate(ctx));
  assert.deepEqual(refs[0].value, {
    autoCache: false,
    cacheRoot: "D:\\Cache",
    downloadRoot: "",
    cacheLimitBytes: 256 * 1024 * 1024,
  });
  assert.equal(events.filter((event) => event[0] === "helper" && event[1] === "launch").length, 3);
  assert.equal(events.filter((event) => event[0] === "register" && event[1] === "host-dispose").length, 1);

  await deactivate();
  await deactivate();
  assert.equal(events.filter((event) => event[0] === "dispose" && event[1] === "resolver").length, 1);
  assert.equal(events.some((event) => event[0] === "helper" && event[1].startsWith("terminate:")), false);
});

test("deactivation disposes registrations before shutting down and terminating the helper", async (t) => {
  await deactivate();
  const events = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (rawURL, options = {}) => {
    const path = new URL(rawURL).pathname;
    events.push(["helper", `${options.method ?? "GET"} ${path}`]);
    if (path === "/v1/health") {
      return Response.json({ defaultMusicPath: "C:\\Users\\listener\\Music" });
    }
    return Response.json({ ok: true });
  };
  t.after(() => { globalThis.fetch = originalFetch; });
  const ctx = {
    ...registrationContext(events),
    descriptor: { directory: "C:\\Plugins\\echo-music-keeper" },
    process: {
      launch: async () => { events.push(["helper", "launch"]); return { ok: true, pid: 42 }; },
      terminate: async (pid) => events.push(["helper", `terminate:${pid}`]),
    },
    dispose: () => {},
  };

  await activate(ctx);
  await deactivate();
  const resolverDisposed = events.findIndex((event) => event[0] === "dispose" && event[1] === "resolver");
  const shutdown = events.findIndex((event) => event[0] === "helper" && event[1] === "POST /v1/shutdown");
  const terminated = events.findIndex((event) => event[0] === "helper" && event[1] === "terminate:42");
  assert.ok(resolverDisposed >= 0 && resolverDisposed < shutdown && shutdown < terminated, events);
});

test("partial activation unwinds acquired resources and stops the helper", async (t) => {
  await deactivate();
  const events = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (rawURL) => {
    const path = new URL(rawURL).pathname;
    events.push(["helper", path]);
    return Response.json(path === "/v1/health" ? { defaultMusicPath: "C:\\Users\\listener\\Music" } : { ok: true });
  };
  t.after(() => { globalThis.fetch = originalFetch; });
  const base = registrationContext(events);
  const ctx = {
    ...base,
    descriptor: { directory: "C:\\Plugins\\echo-music-keeper" },
    ui: { ...base.ui, addPage() { throw new Error("page registration failed"); } },
    process: {
      launch: async () => ({ ok: true, pid: 84 }),
      terminate: async (pid) => events.push(["helper", `terminate:${pid}`]),
    },
    dispose: () => assert.fail("host disposer must not be registered after partial activation"),
  };

  await assert.rejects(activate(ctx), /page registration failed/);
  await assert.doesNotReject(deactivate());
  assert.deepEqual(events.filter(([event]) => event === "dispose").map(([, name]) => name), [
    "menu:delete-download", "menu:reveal-download", "menu:download", "resolver",
  ]);
  assert.equal(events.some((event) => event[0] === "helper" && event[1] === "terminate:84"), true);
});

test("later context-menu failure disposes every earlier acquisition and permits reactivation", async (t) => {
  await deactivate();
  const events = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (rawURL) => {
    const path = new URL(rawURL).pathname;
    return Response.json(path === "/v1/health" ? { defaultMusicPath: "C:\\Users\\listener\\Music" } : { ok: true });
  };
  t.after(() => { globalThis.fetch = originalFetch; });
  const firstBase = registrationContext(events);
  let menuRegistrations = 0;
  const failingContext = {
    ...firstBase,
    descriptor: { directory: "C:\\Plugins\\echo-music-keeper" },
    ui: { ...firstBase.ui, addSongContextMenuItem(item) {
      menuRegistrations += 1;
      events.push(["register", `menu:${item.id}`, item]);
      if (menuRegistrations === 3) throw new Error("third menu registration failed");
      return () => events.push(["dispose", `menu:${item.id}`]);
    } },
    process: {
      launch: async () => ({ ok: true, pid: 91 }),
      terminate: async (pid) => events.push(["helper", `terminate:${pid}`]),
    },
    dispose: () => assert.fail("failed activation must not install a host disposer"),
  };

  await assert.rejects(activate(failingContext), /third menu registration failed/);
  assert.deepEqual(events.filter(([event]) => event === "dispose").map(([, name]) => name), [
    "menu:reveal-download", "menu:download", "resolver",
  ]);
  assert.equal(events.some((event) => event[0] === "helper" && event[1] === "terminate:91"), true);

  const secondBase = registrationContext(events);
  const healthyContext = {
    ...secondBase,
    descriptor: { directory: "C:\\Plugins\\echo-music-keeper" },
    process: {
      launch: async () => ({ ok: true, pid: 92 }),
      terminate: async (pid) => events.push(["helper", `terminate:${pid}`]),
    },
    dispose: () => {},
  };
  await assert.doesNotReject(activate(healthyContext));
  await assert.doesNotReject(deactivate());
  assert.equal(events.some((event) => event[0] === "helper" && event[1] === "terminate:92"), true);
});
