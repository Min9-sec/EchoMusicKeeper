const bytes = (value) => {
  const amount = Number(value) || 0;
  if (amount < 1024) return `${amount} B`;
  const units = ["KiB", "MiB", "GiB"];
  let unit = -1;
  let current = amount;
  while (current >= 1024 && unit < units.length - 1) { current /= 1024; unit += 1; }
  return `${current.toFixed(current >= 10 ? 0 : 1)} ${units[unit]}`;
};
const seconds = (value) => !Number.isFinite(value) || value < 0 ? "--" : `${Math.ceil(value / 60)} 分`;

function hostComponent(ctx, name, fallback) {
  const loader = ctx?.ui?.components?.[name];
  return typeof ctx?.vue?.defineAsyncComponent === "function" && typeof loader === "function"
    ? ctx.vue.defineAsyncComponent(loader)
    : fallback;
}

/** The helper-backed cache and download management page. */
export function createManagementPage(ctx, taskStore, downloads) {
  const vue = ctx?.vue ?? {};
  const h = vue.h ?? ((type, props, children) => ({ type, props, children }));
  return {
    name: "EchoMusicKeeperManagementPage",
    setup() {
      const Button = hostComponent(ctx, "Button", "button");
      const tab = vue.ref?.("tasks") ?? { value: "tasks" };
      const loading = vue.ref?.(true) ?? { value: true };
      const Icon = vue.resolveComponent?.("Icon");
      const icon = (keys, fallback) => keys.map((key) => ctx?.icons?.[key]).find(Boolean) ?? fallback;
      const icons = {
        play: icon(["iconPlayerPlay", "iconPlay"], "tabler:player-play"),
        pause: icon(["iconPlayerPause", "iconPause"], "tabler:player-pause"),
        cancel: icon(["iconX", "iconCancel"], "tabler:x"),
        retry: icon(["iconRefresh", "iconReload"], "tabler:refresh"),
        reveal: icon(["iconFolderOpen", "iconFolder"], "tabler:folder-open"),
        remove: icon(["iconTrash", "iconX"], "tabler:trash"),
      };
      const confirm = async (message) => ctx?.dialog?.confirm?.({ title: "缓存管理", content: message });
      const runAction = (action) => async () => {
        try {
          await action();
        } catch (error) {
          ctx?.toast?.warning?.(error instanceof Error && error.message ? error.message : "操作失败，请稍后重试。");
        }
      };
      const refresh = async () => {
        loading.value = true;
        await taskStore.refresh();
        await downloads.reconcile();
        loading.value = false;
      };
      vue.onMounted?.(() => { taskStore.setVisible(true); void refresh().catch(() => { loading.value = false; }); });
      vue.onUnmounted?.(() => taskStore.setVisible(false));
      const command = (title, action, iconValue) => h(Button, { class: "echo-music-keeper-icon-button", title, "aria-label": title, onClick: runAction(action) }, () => Icon ? h(Icon, { icon: iconValue, width: 16, height: 16 }) : title);
      const tabButton = (value, title) => h(Button, { class: { active: tab.value === value }, onClick: () => { tab.value = value; } }, () => title);
      const taskRow = (task) => {
        const progress = task.totalBytes > 0 ? Math.min(100, Math.round((task.downloadedBytes || 0) / task.totalBytes * 100)) : 0;
        const remaining = task.bytesPerSecond > 0 && task.totalBytes > 0 ? (task.totalBytes - task.downloadedBytes) / task.bytesPerSecond : NaN;
        const completedDownload = task.outputPath ? downloads.records.value.find((entry) => entry.path === task.outputPath && !entry.missing) : null;
        return h("li", { class: "echo-music-keeper-task", key: task.id }, [
          h("div", { class: "echo-music-keeper-task__song" }, [h("strong", task.track?.title || "未知歌曲"), h("span", `${task.track?.artist || "未知艺人"} · ${task.track?.quality || "--"}`)]),
          h("div", { class: "echo-music-keeper-task__progress" }, [h("div", { class: "echo-music-keeper-progress", style: { "--progress": `${progress}%` } }), h("span", `${progress}%`)]),
          h("span", `${bytes(task.bytesPerSecond)}/s`), h("span", seconds(remaining)), h("span", { class: `echo-music-keeper-state is-${task.state}` }, task.state),
          h("div", { class: "echo-music-keeper-task__actions" }, [
            task.pausable ? command(task.state === "paused" ? "继续" : "暂停", () => task.state === "paused" ? taskStore.resume(task) : taskStore.pause(task), task.state === "paused" ? icons.play : icons.pause) : null,
            task.cancelable ? command("取消", () => taskStore.cancel(task), icons.cancel) : null,
            task.retryable || task.state === "failed" ? command("重试", () => taskStore.retry(task), icons.retry) : null,
            completedDownload ? command("打开位置", () => downloads.reveal(completedDownload), icons.reveal) : null,
          ]),
        ]);
      };
      const downloadRow = (entry) => h("li", { class: "echo-music-keeper-entry", key: entry.path }, [
        h("div", [h("strong", entry.title || "未知歌曲"), h("span", `${entry.artist || "未知艺人"} · ${entry.quality || "--"} · ${bytes(entry.size)}`)]),
        h("div", { class: "echo-music-keeper-task__actions" }, [
          command("打开位置", () => downloads.reveal(entry), icons.reveal),
          command("删除下载", () => downloads.remove(entry), icons.remove),
        ]),
      ]);
      return () => h("main", { class: "echo-music-keeper-page" }, [
        h("nav", { class: "echo-music-keeper-tabs", "aria-label": "缓存管理" }, [tabButton("tasks", "下载任务"), tabButton("cache", "缓存管理")]),
        loading.value ? h("p", { class: "echo-music-keeper-empty" }, "正在读取缓存状态…") : null,
        taskStore.error.value === "helper-offline" ? h("p", { class: "echo-music-keeper-empty" }, "缓存服务未连接") : null,
        taskStore.error.value === "directory-error" ? h("p", { class: "echo-music-keeper-empty" }, "缓存目录不可用") : null,
        tab.value === "tasks" ? h("section", { class: "echo-music-keeper-section" }, [
          h("ul", { class: "echo-music-keeper-task-list" }, taskStore.tasks.value.map(taskRow)),
          !taskStore.tasks.value.length && !loading.value ? h("p", { class: "echo-music-keeper-empty" }, "暂无下载任务") : null,
          h("h3", { class: "echo-music-keeper-subheading" }, "已完成下载"),
          h("ul", { class: "echo-music-keeper-entry-list" }, downloads.records.value.filter((entry) => !entry.missing).map(downloadRow)),
          !downloads.records.value.some((entry) => !entry.missing) && !loading.value ? h("p", { class: "echo-music-keeper-empty" }, "暂无已完成下载") : null,
        ]) : h("section", { class: "echo-music-keeper-section" }, [
          h("div", { class: "echo-music-keeper-cache-summary" }, [
            h("div", { class: "echo-music-keeper-progress", style: { "--progress": `${Math.min(100, taskStore.cacheStats.value.cacheBytes / Math.max(1, taskStore.cacheStats.value.cacheLimitBytes) * 100)}%` } }),
            h("span", `${bytes(taskStore.cacheStats.value.cacheBytes)} / ${bytes(taskStore.cacheStats.value.cacheLimitBytes)}`),
            h(Button, { onClick: runAction(async () => { if (await confirm("确定清空所有未使用缓存吗？")) await taskStore.clearCache(); }) }, () => "清空缓存"),
          ]),
          h("ul", { class: "echo-music-keeper-entry-list" }, taskStore.cacheEntries.value.map((entry) => h("li", { class: "echo-music-keeper-entry", key: entry.key }, [
            h("div", [h("strong", entry.track?.title || entry.key), h("span", `${entry.track?.artist || ""} · ${bytes(entry.size)}`)]),
            h("div", { class: "echo-music-keeper-task__actions" }, [command("删除缓存", async () => { if (await confirm("确定删除此缓存吗？")) await taskStore.deleteCache(entry); }, icons.remove)]),
          ]))),
          !taskStore.cacheEntries.value.length && !loading.value ? h("p", { class: "echo-music-keeper-empty" }, "暂无完整缓存") : null,
        ]),
      ]);
    },
  };
}
