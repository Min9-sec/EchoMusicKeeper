import { normalizeSettings } from "./config.js";
import { createDownloadController } from "./downloads.js";
import { createHelperRuntime } from "./helper-runtime.js";
import { createAudioResolver } from "./resolver.js";
import { createTaskStore } from "./task-store.js";
import { createManagementPage } from "./ui/page.js";
import { createSettingsComponent } from "./ui/settings.js";
import { ECHO_MUSIC_KEEPER_STYLES } from "./ui/styles.js";

const SETTINGS_KEY = "echo-music-keeper-settings";

class FeatureRegistrationError extends Error {
  constructor(cause, disposers) {
    super(cause instanceof Error ? cause.message : "echo-music-keeper feature registration failed", { cause });
    this.disposers = disposers;
  }
}

async function disposeAll(disposers) {
  let firstError;
  for (const dispose of disposers.reverse()) {
    try {
      await dispose?.();
    } catch (error) {
      firstError ??= error;
    }
  }
  return firstError;
}

export function registerEchoMusicKeeperFeatures(ctx, runtime, settingsRef) {
  let taskStore;
  let downloads;
  const registrations = [];
  try {
    taskStore = createTaskStore(ctx, runtime);
    downloads = createDownloadController(ctx, runtime, settingsRef, { taskStore });
    registrations.push(ctx.player.audioSource.register(createAudioResolver(ctx, runtime, settingsRef, taskStore)));
    downloads.registerContextMenus((dispose) => registrations.push(dispose));
    registrations.push(ctx.ui.addPage({
      id: "echo-music-keeper-manager",
      title: "缓存与下载",
      component: createManagementPage(ctx, taskStore, downloads),
      sidebar: { title: "缓存与下载", icon: ctx.icons.iconArrowBarToDown },
    }));
    registrations.push(ctx.ui.settings.define({
      title: "EchoMusicKeeper",
      component: createSettingsComponent(ctx, runtime, settingsRef),
    }));
    registrations.push(ctx.css.inject(ECHO_MUSIC_KEEPER_STYLES, { id: "echo-music-keeper-runtime" }));
    return [
      ...registrations,
      () => downloads.dispose(),
      () => taskStore.dispose(),
    ].filter((dispose) => typeof dispose === "function");
  } catch (cause) {
    const partialDisposers = [
      ...registrations,
      ...(downloads ? [() => downloads.dispose()] : []),
      ...(taskStore ? [() => taskStore.dispose()] : []),
    ].filter((dispose) => typeof dispose === "function");
    throw new FeatureRegistrationError(cause, partialDisposers);
  }
}

let disposeRuntime = null;

export async function activate(ctx) {
  const stored = await ctx.storage.get(SETTINGS_KEY).catch(() => null);
  const settings = ctx.vue.ref(normalizeSettings(stored));
  const runtime = createHelperRuntime(ctx, { settings });
  await runtime.start().catch(() => undefined);
  let disposers;
  try {
    disposers = registerEchoMusicKeeperFeatures(ctx, runtime, settings);
  } catch (error) {
    await disposeAll(error instanceof FeatureRegistrationError ? error.disposers : []);
    await runtime.stop();
    throw error instanceof FeatureRegistrationError ? error.cause : error;
  }
  let disposed = false;
  disposeRuntime = async () => {
    if (disposed) return;
    disposed = true;
    const disposeError = await disposeAll(disposers);
    let stopError;
    try {
      await runtime.stop();
    } catch (error) {
      stopError = error;
    }
    if (disposeError) throw disposeError;
    if (stopError) throw stopError;
  };
  ctx.dispose(() => { void disposeRuntime?.().catch(() => undefined); });
}

export async function deactivate() {
  const dispose = disposeRuntime;
  disposeRuntime = null;
  await dispose?.();
}
