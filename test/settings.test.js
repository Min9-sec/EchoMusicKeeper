import assert from "node:assert/strict";
import test from "node:test";

import { GIB } from "../src/config.js";
import { createSettingsComponent } from "../src/ui/settings.js";

const nodes = (value) => {
  if (typeof value === "function") return nodes(value());
  if (Array.isArray(value)) return value.flatMap(nodes);
  if (!value || typeof value !== "object") return [];
  return [value, ...nodes(Array.isArray(value.props) ? value.props : value.children)];
};

test("settings shows effective default paths and saves a numeric cache limit", async () => {
  const saved = [];
  const configs = [];
  const settingsRef = { value: { autoCache: true, cacheRoot: "", downloadRoot: "", cacheLimitBytes: GIB } };
  const component = createSettingsComponent({
    storage: { get: async () => [], set: async (key, value) => saved.push([key, value]) },
    vue: { h: (type, props, children) => ({ type, props, children }), defineAsyncComponent: () => "HostComponent" },
    ui: { components: { Switch: async () => {}, Input: async () => {}, Button: async () => {} } },
  }, {
    pluginRoot: "C:\\Plugins\\echo-music-keeper",
    defaultMusicRoot: "C:\\Users\\me\\Music",
    client: { updateConfig: async (config) => configs.push(config) },
  }, settingsRef);

  const render = component.setup();
  const tree = render();
  const visibleText = nodes(tree).filter((node) => node.type === "p").map((node) => node.children);
  assert.deepEqual(visibleText, [
    "当前缓存目录：C:\\Plugins\\echo-music-keeper\\cache",
    "当前下载目录：C:\\Users\\me\\Music",
    "默认缓存目录会随插件更新、重装或卸载清理。",
  ]);

  const cacheLimit = nodes(tree).find((node) => node.type === "input" && node.props?.type === "number");
  assert.equal(cacheLimit.props.value, 1);
  assert.equal(cacheLimit.props.step, 0.01);
  cacheLimit.props.onChange({ target: { value: "1.5" } });
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(settingsRef.value.cacheLimitBytes, 1.5 * GIB);
  assert.equal(saved.at(-1)[1].cacheLimitBytes, 1.5 * GIB);
  assert.equal(configs.at(-1).cacheRoot, "C:\\Plugins\\echo-music-keeper\\cache");
  assert.equal(configs.at(-1).downloadRoot, "C:\\Users\\me\\Music");
});

test("settings ignores an invalid cache-limit value", () => {
  const settingsRef = { value: { autoCache: true, cacheRoot: "", downloadRoot: "", cacheLimitBytes: GIB } };
  const component = createSettingsComponent({
    vue: { h: (type, props, children) => ({ type, props, children }) },
  }, { pluginRoot: "C:\\Plugins\\echo-music-keeper", defaultMusicRoot: "C:\\Users\\me\\Music" }, settingsRef);
  const input = nodes(component.setup()()).find((node) => node.type === "input" && node.props?.type === "number");
  input.props.onChange({ target: { value: "" } });
  assert.equal(settingsRef.value.cacheLimitBytes, GIB);
});
