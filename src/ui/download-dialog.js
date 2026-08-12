import { normalizeQuality } from "../kugou-source.js";

const qualities = ["128", "320", "flac", "high", "super"];

function hostComponent(ctx, name, fallback) {
  const loader = ctx?.ui?.components?.[name];
  return typeof ctx?.vue?.defineAsyncComponent === "function" && typeof loader === "function"
    ? ctx.vue.defineAsyncComponent(loader)
    : fallback;
}

/** Quality chooser mounted by the host teleport API. */
export function createDownloadDialog(ctx, controller, track, options = {}) {
  const vue = ctx?.vue ?? {};
  const h = vue.h ?? ((type, props, children) => ({ type, props, children }));
  const selected = vue.ref?.(normalizeQuality(ctx?.stores?.player?.currentAudioQualityOverride ?? ctx?.stores?.settings?.defaultAudioQuality) ?? "320") ?? { value: "320" };
  const available = new Set((options.availableQualities ?? qualities).map(normalizeQuality).filter(Boolean));
  let close = () => {};
  const component = {
    name: "EchoMusicKeeperDownloadDialog",
    setup() {
      const Button = hostComponent(ctx, "Button", "button");
      const Select = hostComponent(ctx, "Select", "select");
      const Icon = vue.resolveComponent?.("Icon");
      const confirm = async () => {
        const task = await controller.request(track, selected.value);
        if (task) close();
      };
      return () => h("section", { class: "echo-music-keeper-dialog", role: "dialog", "aria-label": "下载歌曲" }, [
        h("header", { class: "echo-music-keeper-dialog__header" }, [h("h3", "下载歌曲"), Icon ? h(Icon, { icon: ctx?.icons?.iconArrowBarToDown ?? "tabler:download", width: 20, height: 20 }) : null]),
        h("p", `${track?.title ?? track?.songName ?? ""} ${track?.artist ?? track?.singerName ?? ""}`.trim()),
        h(Select, { modelValue: selected.value, "onUpdate:modelValue": (value) => { selected.value = normalizeQuality(value) ?? selected.value; }, options: qualities.map((quality) => ({ label: quality, value: quality, disabled: !available.has(quality) })) }),
        h("footer", { class: "echo-music-keeper-dialog__actions" }, [
          h(Button, { onClick: close }, () => "取消"),
          h(Button, { type: "primary", onClick: confirm }, () => "下载"),
        ]),
      ]);
    },
  };
  const dispose = ctx?.ui?.teleport?.(component, options.teleportOptions) ?? (() => {});
  close = () => { dispose?.(); };
  return Object.freeze({ close, component });
}
