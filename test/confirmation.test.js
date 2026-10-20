import assert from "node:assert/strict";
import test from "node:test";
import { createConfirmation } from "../src/ui/confirmation.js";

function nodes(value) {
  if (Array.isArray(value)) return value.flatMap(nodes);
  if (!value || typeof value !== "object") return [];
  return [value, ...nodes(value.children)];
}

function harness() {
  let tree;
  let unmounted;
  let closes = 0;
  const warnings = [];
  const ctx = {
    // Mirrors beta.16: dialog has file pickers, no confirm.
    dialog: { selectDirectory() {} },
    vue: { onBeforeUnmount(callback) { unmounted = callback; } },
    ui: { teleport(component) { tree = component.setup()(); return () => { closes++; unmounted?.(); }; } },
    toast: { warning: (message) => warnings.push(message) },
  };
  return { ctx, warnings, closes: () => closes, tree: () => tree,
    click(label) { nodes(tree).find((node) => node.type === "button" && node.children === label).props.onClick(); },
    unmount() { unmounted(); },
  };
}

const message = { title: "删除下载", content: "确定删除此下载文件吗？" };

test("confirmation works with beta.16 API and requires explicit approval", async () => {
  const host = harness();
  const confirmation = createConfirmation(host.ctx);
  const result = confirmation.confirm(message);
  assert.equal(host.tree().props.class, "echo-music-keeper-dialog-layer");
  assert.equal(nodes(host.tree()).some((node) => node.props?.role === "alertdialog"), true);
  host.click("确定");
  assert.equal(await result, true);
  assert.equal(host.closes(), 1);
  confirmation.dispose();
  assert.equal(host.closes(), 1);
});

test("cancel, backdrop, unmount and disposal never approve a destructive operation", async () => {
  for (const action of ["cancel", "backdrop", "unmount", "dispose"]) {
    const host = harness();
    const confirmation = createConfirmation(host.ctx);
    const result = confirmation.confirm(message);
    if (action === "cancel") host.click("取消");
    if (action === "backdrop") host.tree().props.onClick({ target: 1, currentTarget: 1 });
    if (action === "unmount") host.unmount();
    if (action === "dispose") confirmation.dispose();
    assert.equal(await result, false, action);
    assert.equal(host.closes(), 1, action);
    confirmation.dispose();
    assert.equal(await confirmation.confirm(message), false);
  }
});

test("replacement settles the previous confirmation without approving it", async () => {
  const host = harness();
  const confirmation = createConfirmation(host.ctx);
  const previous = confirmation.confirm(message);
  const next = confirmation.confirm(message);
  assert.equal(await previous, false);
  host.click("确定");
  assert.equal(await next, true);
  assert.equal(host.closes(), 2);
});

test("missing, failed and throwing teleport cancel with a visible warning", async () => {
  for (const teleport of [undefined, () => undefined, () => { throw new Error("mount failed"); }]) {
    const host = harness();
    host.ctx.ui.teleport = teleport;
    assert.equal(await createConfirmation(host.ctx).confirm(message), false);
    assert.equal(host.warnings.length, 1);
  }
});
