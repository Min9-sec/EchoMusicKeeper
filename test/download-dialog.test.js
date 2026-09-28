import assert from "node:assert/strict";
import test from "node:test";

import { createDownloadDialog } from "../src/ui/download-dialog.js";

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
  const select = nodes(tree).find((node) => node.type === "HostSelect");
  assert.equal(tree.props["aria-busy"], true);
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
  assert.equal(tree.props["aria-busy"], false);
  assert.equal(buttons[1].props.loading, false);
  assert.equal(buttons[1].props.disabled, false);
  assert.equal(buttons[1].children(), "下载");
});
