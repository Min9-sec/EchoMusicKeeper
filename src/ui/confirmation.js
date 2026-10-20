/** Plugin-owned confirmation: EchoMusic beta.16 has no ctx.dialog.confirm API. */
export function createConfirmation(ctx) {
  let active = null;
  let disposed = false;
  const vue = ctx?.vue ?? {};
  const h = vue.h ?? ((type, props, children) => ({ type, props, children }));

  function confirm({ title, content, confirmText = "确定", cancelText = "取消" }) {
    if (disposed) return Promise.resolve(false);
    active?.();
    return new Promise((resolve) => {
      let settled = false;
      let unmount = null;
      const previousFocus = typeof document !== "undefined" ? document.activeElement : null;
      const onKey = (event) => {
        if (event.key === "Escape") { event.preventDefault(); finish(false); }
      };
      const finish = (accepted) => {
        if (settled) return;
        settled = true;
        if (active === cancel) active = null;
        if (typeof window !== "undefined") window.removeEventListener("keydown", onKey);
        try { unmount?.(); } finally {
          previousFocus?.focus?.();
          resolve(accepted === true);
        }
      };
      const cancel = () => finish(false);
      active = cancel;
      const component = {
        name: "EchoMusicKeeperConfirmation",
        setup() {
          vue.onBeforeUnmount?.(cancel);
          return () => h("div", { class: "echo-music-keeper-dialog-layer", onClick: (event) => { if (event.target === event.currentTarget) cancel(); } }, [
            h("section", { class: "echo-music-keeper-dialog", role: "alertdialog", "aria-modal": "true", "aria-label": title, tabindex: -1,
              onVnodeMounted: (node) => node.el?.querySelector?.("button")?.focus?.(),
              onKeydown: (event) => {
                if (event.key !== "Tab") return;
                const buttons = event.currentTarget.querySelectorAll("button");
                if (!buttons.length) return;
                const first = buttons[0], last = buttons[buttons.length - 1];
                if (event.shiftKey && event.target === first) { event.preventDefault(); last.focus(); }
                else if (!event.shiftKey && event.target === last) { event.preventDefault(); first.focus(); }
              } }, [
              h("h3", title), h("p", content),
              h("footer", { class: "echo-music-keeper-dialog__actions" }, [
                h("button", { type: "button", class: "echo-music-keeper-confirm-button", onClick: cancel }, cancelText),
                h("button", { type: "button", class: "echo-music-keeper-confirm-button", onClick: () => finish(true) }, confirmText),
              ]),
            ]),
          ]);
        },
      };
      try {
        if (typeof window !== "undefined") window.addEventListener("keydown", onKey);
        const mounted = ctx?.ui?.teleport?.(component, { className: "echo-music-keeper-dialog-host" });
        if (typeof mounted !== "function") throw new Error("teleport unavailable");
        unmount = mounted;
        if (settled) unmount();
      } catch {
        ctx?.toast?.warning?.("无法打开确认窗口，请重新启用插件后重试。");
        finish(false);
      }
    });
  }

  return { confirm, dispose() { disposed = true; active?.(); } };
}
