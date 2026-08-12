import { GIB, MAX_CACHE_BYTES, MIN_CACHE_BYTES, normalizeSettings, toHelperConfig } from "../config.js";

const STORAGE_KEY = "echo-music-keeper-settings";
const DOWNLOADS_KEY = "echo-music-keeper-downloads";
const parentPath = (path) => String(path ?? "").replace(/[\\/][^\\/]*$/, "");

function hostComponent(ctx, name, fallback) {
  const loader = ctx?.ui?.components?.[name];
  return typeof ctx?.vue?.defineAsyncComponent === "function" && typeof loader === "function"
    ? ctx.vue.defineAsyncComponent(loader)
    : fallback;
}

/** Host settings panel for cache lifecycle and download locations. */
export function createSettingsComponent(ctx, runtime, settingsRef) {
  const vue = ctx?.vue ?? {};
  const h = vue.h ?? ((type, props, children) => ({ type, props, children }));
  return {
    name: "EchoMusicKeeperSettings",
    setup() {
      const Switch = hostComponent(ctx, "Switch", "input");
      const Input = hostComponent(ctx, "Input", "input");
      const Button = hostComponent(ctx, "Button", "button");
      const save = async (next) => {
        const normalized = normalizeSettings(next);
        await ctx?.storage?.set?.(STORAGE_KEY, { ...normalized });
        settingsRef.value = normalized;
        const records = await ctx?.storage?.get?.(DOWNLOADS_KEY).catch?.(() => []) ?? [];
        const completedDownloadPaths = (Array.isArray(records) ? records : []).map((entry) => String(entry?.path ?? "").trim()).filter(Boolean);
        const managedDownloadRoots = [...new Set([normalized.downloadRoot, ...completedDownloadPaths.map(parentPath)].filter(Boolean))];
        await runtime?.client?.updateConfig?.(toHelperConfig(normalized, {
          pluginRoot: runtime?.pluginRoot ?? "", defaultMusicRoot: runtime?.defaultMusicRoot ?? "",
          managedDownloadRoots, completedDownloadPaths,
        }));
      };
      const changeCacheRoot = async (path) => {
        if (!path || path === settingsRef.value.cacheRoot) return;
        const migrate = await ctx?.dialog?.confirm?.({ title: "切换缓存目录", content: "迁移现有缓存？选择取消将从新目录开始。", confirmText: "迁移", cancelText: "重新开始" });
        await runtime?.client?.migrateCache?.({ cacheRoot: path, mode: migrate ? "migrate" : "fresh" });
        await save({ ...settingsRef.value, cacheRoot: path });
      };
      const choose = async (name) => {
        const selected = await ctx?.dialog?.selectDirectory?.();
        const path = typeof selected === "string" ? selected : selected?.path;
        if (!path) return;
        if (name === "cacheRoot") await changeCacheRoot(path);
        else await save({ ...settingsRef.value, [name]: path });
      };
      const cacheLocation = () => {
        const custom = String(settingsRef.value.cacheRoot ?? "").trim();
        if (custom) return custom;
        const pluginRoot = String(runtime?.pluginRoot ?? "").trim().replace(/[\\/]$/, "");
        return pluginRoot ? `${pluginRoot}\\cache` : "正在检测插件目录";
      };
      const downloadLocation = () => String(settingsRef.value.downloadRoot || runtime?.defaultMusicRoot || "正在检测默认音乐目录").trim();
      const changeCacheLimit = (event) => {
        const gib = Number(event?.target?.value);
        if (!Number.isFinite(gib) || gib <= 0) return;
        void save({ ...settingsRef.value, cacheLimitBytes: gib * GIB });
      };
      return () => h("section", { class: "echo-music-keeper-settings" }, [
        h("label", [h("span", "自动缓存"), h(Switch, { modelValue: settingsRef.value.autoCache, "onUpdate:modelValue": (value) => void save({ ...settingsRef.value, autoCache: value }) })]),
        h("label", [h("span", "缓存上限"), h("input", { class: "echo-music-keeper-limit-input", type: "number", min: MIN_CACHE_BYTES / GIB, max: MAX_CACHE_BYTES / GIB, step: 0.01, value: settingsRef.value.cacheLimitBytes / GIB, inputmode: "decimal", "aria-label": "缓存上限（GiB）", onChange: changeCacheLimit }), h("span", "GiB")]),
        h("label", [h("span", "缓存目录"), h(Input, { modelValue: settingsRef.value.cacheRoot, "onUpdate:modelValue": (value) => void changeCacheRoot(value) }), h(Button, { onClick: () => choose("cacheRoot") }, () => "选择文件夹")]),
        h("p", { class: "echo-music-keeper-settings__effective-path" }, `当前缓存目录：${cacheLocation()}`),
        h("label", [h("span", "下载目录"), h(Input, { modelValue: settingsRef.value.downloadRoot, "onUpdate:modelValue": (value) => void save({ ...settingsRef.value, downloadRoot: value }) }), h(Button, { onClick: () => choose("downloadRoot") }, () => "选择文件夹")]),
        h("p", { class: "echo-music-keeper-settings__effective-path" }, `当前下载目录：${downloadLocation()}`),
        h("p", { class: "echo-music-keeper-settings__notice" }, settingsRef.value.cacheRoot ? "自定义缓存目录不会随插件更新或卸载自动清理。" : "默认缓存目录会随插件更新、重装或卸载清理。"),
      ]);
    },
  };
}
