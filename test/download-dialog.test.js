import assert from "node:assert/strict";
import test from "node:test";

import { createDownloadDialog } from "../src/ui/download-dialog.js";
import { ECHO_MUSIC_KEEPER_STYLES } from "../src/ui/styles.js";

function nodes(value) {
  if (Array.isArray(value)) return value.flatMap(nodes);
  if (!value || typeof value !== "object") return [];
  return [value, ...nodes(value.children)];
}

test("download dialog wraps lazy host components and stays open after a failed request", async () => {
  const h = (type, props, children) => ({ type, props, children });
  let mounted;
  let closed = 0;
  let loaderCalls = 0;
  const wrappers = [];
  const buttonLoader = async () => { loaderCalls += 1; return "unused"; };
  const selectLoader = async () => { loaderCalls += 1; return "unused"; };
  const ctx = {
    vue: {
      h, ref: (value) => ({ value }), resolveComponent: () => null,
      defineAsyncComponent(loader) {
        wrappers.push(loader);
        return loader === buttonLoader ? "HostButton" : "HostSelect";
      },
    },
    ui: {
      components: { Button: buttonLoader, Select: selectLoader },
      teleport(component) {
        mounted = component;
        return () => { closed += 1; };
      },
    },
  };
  const requests = [];
  createDownloadDialog(ctx, { request: async (...args) => { requests.push(args); return null; } }, { hash: "0123456789abcdef", title: "Song" });

  assert.equal(loaderCalls, 0);
  const tree = mounted.setup()();
  assert.deepEqual(wrappers, [buttonLoader, selectLoader]);
  const buttons = nodes(tree).filter((node) => node.type === "HostButton");
  await buttons.at(-1).props.onClick();
  assert.deepEqual(requests, [[{ hash: "0123456789abcdef", title: "Song" }, "320"]]);
  assert.equal(closed, 0);
  buttons[0].props.onClick();
  assert.equal(closed, 1);
});

test("download dialog cancel closes after a synchronous host mount", () => {
  const h = (type, props, children) => ({ type, props, children });
  let tree;
  let closed = 0;
  createDownloadDialog({
    vue: { h, ref: (value) => ({ value }), resolveComponent: () => null },
    ui: {
      teleport(component) {
        tree = component.setup()();
        return () => { closed += 1; };
      },
    },
  }, { request: async () => null }, { hash: "0123456789abcdef", title: "Song" });

  const cancel = nodes(tree).find((node) => node.type === "button");
  cancel.props.onClick();
  assert.equal(closed, 1);
});

test("download dialog exposes a pending state and suppresses duplicate submissions", async () => {
  const h = (type, props, children) => ({ type, props, children });
  let mounted;
  let releaseRequest;
  const calls = [];
  const pending = new Promise((resolve) => { releaseRequest = resolve; });
  const ctx = {
    vue: {
      h, ref: (value) => ({ value }), resolveComponent: () => null,
      defineAsyncComponent(loader) { return loader.name === "loadButton" ? "HostButton" : "HostSelect"; },
    },
    ui: {
      components: {
        Button: async function loadButton() {},
        Select: async function loadSelect() {},
      },
      teleport(component) { mounted = component; return () => {}; },
    },
  };
  createDownloadDialog(ctx, { request: (...args) => { calls.push(args); return pending; } }, { hash: "0123456789abcdef", title: "Song" });

  const render = mounted.setup();
  let tree = render();
  let buttons = nodes(tree).filter((node) => node.type === "HostButton");
  const first = buttons.at(-1).props.onClick();
  const duplicate = buttons.at(-1).props.onClick();

  tree = render();
  buttons = nodes(tree).filter((node) => node.type === "HostButton");
  const dialog = nodes(tree).find((node) => node.props?.role === "dialog");
  const select = nodes(tree).find((node) => node.type === "HostSelect");
  assert.equal(dialog.props["aria-busy"], true);
  assert.equal(buttons[0].props.disabled, true);
  assert.equal(buttons[1].props.loading, true);
  assert.equal(buttons[1].props.disabled, true);
  assert.equal(buttons[1].children(), "正在解析…");
  assert.equal(select.props.disabled, true);
  assert.equal(calls.length, 1);

  releaseRequest(null);
  await Promise.all([first, duplicate]);
  tree = render();
  buttons = nodes(tree).filter((node) => node.type === "HostButton");
  assert.equal(nodes(tree).find((node) => node.props?.role === "dialog").props["aria-busy"], false);
  assert.equal(buttons[1].props.loading, false);
  assert.equal(buttons[1].props.disabled, false);
  assert.equal(buttons[1].children(), "下载");
});

test("download dialog is a fixed overlay and toasts when the host cannot mount it", () => {
  assert.match(ECHO_MUSIC_KEEPER_STYLES, /\.echo-music-keeper-dialog-host\{[^}]*position:fixed[^}]*z-index:1800/);
  assert.match(ECHO_MUSIC_KEEPER_STYLES, /\.echo-music-keeper-dialog-layer\{[^}]*position:fixed[^}]*z-index:1800/);
  assert.match(ECHO_MUSIC_KEEPER_STYLES, /\.echo-music-keeper-dialog\{[^}]*width:min\(440px,calc\(100vw - 32px\)\)[^}]*padding:24px/);
  assert.match(ECHO_MUSIC_KEEPER_STYLES, /\.echo-music-keeper-dialog__quality\{[^}]*width:100%[^}]*min-height:40px/);
  const h = (type, props, children) => ({ type, props, children });
  const warnings = [];
  const vue = { h, ref: (value) => ({ value }), resolveComponent: () => null };
  const track = { hash: "0123456789abcdef", title: "Song", artist: "Artist" };
  let teleportOptions;
  let mounted;
  let closed = 0;
  const shown = createDownloadDialog({
    vue,
    toast: { warning: (message) => warnings.push(message) },
    ui: { teleport(component, options) { mounted = component; teleportOptions = options; return () => { closed += 1; }; } },
  }, { request: async () => null }, track, { teleportOptions: { className: "extra", id: "download-dialog" } });

  assert.equal(shown.shown, true);
  assert.deepEqual(warnings, []);
  assert.equal(teleportOptions.className, "echo-music-keeper-dialog-host extra");
  assert.equal(teleportOptions.id, "download-dialog");
  const tree = mounted.setup()();
  assert.equal(tree.props.class, "echo-music-keeper-dialog-layer");
  const dialog = nodes(tree).find((node) => node.props?.role === "dialog");
  assert.equal(dialog.props.class, "echo-music-keeper-dialog");
  assert.equal(dialog.props["aria-modal"], "true");
  const select = nodes(tree).find((node) => node.type === "select");
  assert.equal(select.props.class, "echo-music-keeper-dialog__quality");
  assert.deepEqual(select.props.style, { width: "100%" });
  const backdrop = {};
  tree.props.onClick({ target: backdrop, currentTarget: backdrop });
  assert.equal(closed, 1);
  tree.props.onClick({ target: {}, currentTarget: backdrop });
  assert.equal(closed, 1);

  for (const teleport of [undefined, () => undefined, () => { throw new Error("mount failed"); }]) {
    warnings.length = 0;
    const failed = createDownloadDialog({
      vue,
      toast: { warning: (message) => warnings.push(message) },
      ui: teleport ? { teleport } : {},
    }, { request: async () => null }, track);
    assert.equal(failed.shown, false);
    assert.deepEqual(warnings, ["无法打开下载窗口。"]);
    assert.doesNotThrow(() => failed.close());
  }
});
