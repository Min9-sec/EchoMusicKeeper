import { normalizeQuality } from "../kugou-source.js";

const qualities = ["128", "320", "flac", "high", "super"];

function hostComponent(ctx, name, fallback) {
  const loader = ctx?.ui?.components?.[name];
  return typeof ctx?.vue?.defineAsyncComponent === "function" && typeof loader === "function"
    ? ctx.vue.defineAsyncComponent(loader)
    : fallback;
}

const OPEN_FAILURE = "无法打开下载窗口。";

function warn(ctx, message) {
  ctx?.toast?.warning?.(message);
}

/**
 * Quality chooser mounted by the host teleport API.
 * The host appends the component to document.body. EchoMusic keeps
 * html/body/#app at 100vh with overflow hidden, so the chooser has to be a
 * fixed overlay or the click looks like it did nothing.
 */
export function createDownloadDialog(ctx, controller, track, options = {}) {
  const vue = ctx?.vue ?? {};
  const h = vue.h ?? ((type, props, children) => ({ type, props, children }));
  const selected = vue.ref?.(normalizeQuality(ctx?.stores?.player?.currentAudioQualityOverride ?? ctx?.stores?.settings?.defaultAudioQuality) ?? "320") ?? { value: "320" };
  const available = new Set((options.availableQualities ?? qualities).map(normalizeQuality).filter(Boolean));
  let close = () => {};
  let removeKeyListener = () => {};
  const component = {
    name: "EchoMusicKeeperDownloadDialog",
    setup() {
      const Button = hostComponent(ctx, "Button", "button");
      const Select = hostComponent(ctx, "Select", "select");
      const Icon = vue.resolveComponent?.("Icon");
      const submitting = vue.ref?.(false) ?? { value: false };
      const confirm = async () => {
        if (submitting.value) return;
        submitting.value = true;
        try {
          const task = await controller.request(track, selected.value);
          if (task) close();
        } catch {
          warn(ctx, "下载任务创建失败，请稍后重试。");
        } finally {
          submitting.value = false;
        }
      };
      const onKey = (event) => {
        if (event.key === "Escape" && !submitting.value) close();
      };
      if (typeof window !== "undefined") {
        window.addEventListener("keydown", onKey);
        removeKeyListener = () => window.removeEventListener("keydown", onKey);
        vue.onBeforeUnmount?.(removeKeyListener);
      }
      const dismiss = (event) => {
        if (event?.target === event?.currentTarget && !submitting.value) close();
      };
      return () => h("div", { class: "echo-music-keeper-dialog-layer", onClick: dismiss }, [
        h("section", { class: "echo-music-keeper-dialog", role: "dialog", "aria-label": "下载歌曲", "aria-busy": submitting.value, "aria-modal": "true" }, [
          h("header", { class: "echo-music-keeper-dialog__header" }, [h("h3", "下载歌曲"), Icon ? h(Icon, { icon: ctx?.icons?.iconArrowBarToDown ?? "tabler:download", width: 20, height: 20 }) : null]),
          h("p", `${track?.title ?? track?.songName ?? ""} ${track?.artist ?? track?.singerName ?? ""}`.trim()),
          h(Select, { modelValue: selected.value, disabled: submitting.value, "onUpdate:modelValue": (value) => { selected.value = normalizeQuality(value) ?? selected.value; }, options: qualities.map((quality) => ({ label: quality, value: quality, disabled: !available.has(quality) })) }),
          h("footer", { class: "echo-music-keeper-dialog__actions" }, [
            h(Button, { disabled: submitting.value, onClick: close }, () => "取消"),
            h(Button, { type: "primary", loading: submitting.value, disabled: submitting.value, onClick: confirm }, () => submitting.value ? "正在解析…" : "下载"),
          ]),
        ]),
      ]);
    },
  };
  const teleportOptions = { ...options.teleportOptions, className: ["echo-music-keeper-dialog-host", options.teleportOptions?.className].filter(Boolean).join(" ") };
  let dispose = () => {};
  let shown = false;
  try {
    const mounted = typeof ctx?.ui?.teleport === "function" ? ctx.ui.teleport(component, teleportOptions) : undefined;
    if (typeof mounted === "function") {
      dispose = mounted;
      shown = true;
    } else warn(ctx, OPEN_FAILURE);
  } catch {
    warn(ctx, OPEN_FAILURE);
  }
  close = () => {
    removeKeyListener();
    removeKeyListener = () => {};
    dispose?.();
  };
  return Object.freeze({ close, component, shown });
}
