import { toHelperConfig } from "./config.js";
import { createHelperClient } from "./helper-client.js";

const PORT_MIN = 49152;
const PORT_COUNT = 65535 - PORT_MIN + 1;
const START_ATTEMPTS = 3;
const START_TIMEOUT_MS = 5_000;
const HEALTH_INTERVAL_MS = 100;

const sleepNormally = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

function isAbsolutePath(path) {
  return /^(?:[A-Za-z]:[\\/]|\\\\[^\\/]+[\\/][^\\/]+|\/)/.test(path);
}

function randomToken() {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function waitForAbort(promise, signal) {
  if (!signal) return promise;
  if (signal.aborted) return Promise.resolve();
  return Promise.race([
    promise,
    new Promise((resolve) => signal.addEventListener("abort", resolve, { once: true })),
  ]);
}

/** Owns one helper process and exposes its authenticated local client only while online. */
export function createHelperRuntime(ctx, options = {}) {
  const pluginRoot = String(ctx?.descriptor?.directory ?? "");
  const settings = options.settings ?? {};
  const clientFactory = options.clientFactory ?? ((clientOptions) => createHelperClient({
    ...clientOptions,
    fetchImpl: options.fetchImpl,
  }));
  const sleep = options.sleep ?? sleepNormally;
  const choosePort = options.randomPort ?? (() => PORT_MIN + Math.floor(Math.random() * PORT_COUNT));
  const paths = {
    managedDownloadRoots: options.managedDownloadRoots ?? [],
    completedDownloadPaths: options.completedDownloadPaths ?? [],
  };

  let operation = Promise.resolve();
  let pendingStart = null;
  let stopQueued = false;
  let tracked = null;
  let onlineClient = null;
  let defaultMusicRoot = String(options.defaultMusicRoot ?? "");
  let currentStatus = { state: "stopped", reason: null };

  const setStatus = (state, reason = null) => { currentStatus = { state, reason }; };
  const fallback = (reason) => {
    onlineClient = null;
    setStatus("fallback", reason);
    return false;
  };

  function enqueue(work) {
    const next = operation.then(work, work);
    operation = next.catch(() => {});
    return next;
  }

  async function terminate(entry) {
    if (!entry || entry.terminated) return;
    entry.terminated = true;
    if (tracked === entry) tracked = null;
    try {
      await ctx.process.terminate(entry.pid);
    } catch {
      // Ownership is released even when the host reports an already-exited process.
    }
  }

  async function healthUntilReady(client, isCanceled) {
    const deadline = Date.now() + START_TIMEOUT_MS;
    const maximumProbes = Math.ceil(START_TIMEOUT_MS / HEALTH_INTERVAL_MS);
    for (let probe = 0; probe < maximumProbes && Date.now() < deadline && !isCanceled(); probe += 1) {
      try {
        const health = await client.health({ timeoutMs: 500 });
        if (!isCanceled()) return health;
      } catch {
        // The helper commonly needs a short interval to bind its loopback listener.
      }
      if (Date.now() >= deadline || isCanceled()) break;
      await sleep(HEALTH_INTERVAL_MS);
    }
    return null;
  }

  async function startInternal(signal) {
    if (onlineClient) return true;
    if (!isAbsolutePath(pluginRoot)) return fallback("invalid-plugin-root");

    let canceled = Boolean(signal?.aborted);
    const markCanceled = () => { canceled = true; };
    signal?.addEventListener("abort", markCanceled, { once: true });
    const isCanceled = () => canceled;
    const initialConfig = toHelperConfig(settings, {
      pluginRoot,
      defaultMusicRoot: "",
      ...paths,
    });
    const seenPorts = new Set();
    let launchedHelper = false;
    let launchFailed = false;
    setStatus("starting");
    try {
      for (let attempt = 0; attempt < START_ATTEMPTS && !isCanceled(); attempt += 1) {
        let port = Number(choosePort());
        if (!Number.isInteger(port) || port < PORT_MIN || port > 65535) {
          port = PORT_MIN + Math.floor(Math.random() * PORT_COUNT);
        }
        while (seenPorts.has(port)) port = PORT_MIN + ((port - PORT_MIN + 1) % PORT_COUNT);
        seenPorts.add(port);
        const token = randomToken();
        const client = clientFactory({ baseUrl: `http://127.0.0.1:${port}`, token, fetchImpl: options.fetchImpl });
        let launchResult;
        try {
          launchResult = await ctx.process.launch({
            executable: "bin/echo-music-keeper-helper.exe",
            args: [
              "--port", String(port), "--token", token,
              "--plugin-root", pluginRoot,
              "--cache-root", initialConfig.cacheRoot,
              "--download-root", initialConfig.downloadRoot,
              "--cache-limit-bytes", String(initialConfig.cacheLimitBytes),
            ],
          });
        } catch {
          launchFailed = true;
          continue;
        }
        if (launchResult?.canceled) return fallback("user-canceled");
        const pid = launchResult?.pid;
        if (launchResult?.ok !== true || !Number.isInteger(pid) || pid <= 0) {
          launchFailed = true;
          continue;
        }
        launchedHelper = true;
        const entry = { pid, client, terminated: false };
        tracked = entry;
        const health = await healthUntilReady(client, isCanceled);
        if (!health || isCanceled()) {
          await terminate(entry);
          continue;
        }
        defaultMusicRoot = String(health.defaultMusicPath ?? defaultMusicRoot);
        try {
          await client.updateConfig(toHelperConfig(settings, { pluginRoot, defaultMusicRoot, ...paths }));
        } catch {
          await terminate(entry);
          continue;
        }
        if (isCanceled() || tracked !== entry) {
          await terminate(entry);
          continue;
        }
        onlineClient = client;
        setStatus("online");
        return true;
      }
      if (isCanceled()) return fallback("user-canceled");
      return fallback(!launchedHelper && launchFailed ? "launch-failed" : "health-check-failed");
    } finally {
      signal?.removeEventListener("abort", markCanceled);
    }
  }

  async function stopInternal() {
    onlineClient = null;
    const entry = tracked;
    if (entry) {
      const signal = AbortSignal.timeout(2_000);
      try {
        await waitForAbort(Promise.resolve().then(() => entry.client.shutdown({ signal })), signal);
      } catch {
        // Process termination below is the bounded shutdown fallback.
      }
      if (tracked === entry) await terminate(entry);
    }
    setStatus("stopped");
  }

  function retainStart(promise) {
    pendingStart = promise;
    promise.finally(() => {
      if (pendingStart === promise) pendingStart = null;
    });
    return promise;
  }

  function start({ signal } = {}) {
    if (pendingStart) return pendingStart;
    if (onlineClient && !stopQueued) return Promise.resolve(true);
    return retainStart(enqueue(() => startInternal(signal)));
  }

  function stop() {
    stopQueued = true;
    pendingStart = null;
    return enqueue(async () => {
      await stopInternal();
      stopQueued = false;
    });
  }

  function restart() {
    stopQueued = true;
    pendingStart = null;
    return retainStart(enqueue(async () => {
      await stopInternal();
      stopQueued = false;
      return startInternal();
    }));
  }

  return Object.freeze({
    start,
    stop,
    restart,
    status: () => ({ ...currentStatus }),
    get client() { return onlineClient; },
    get pluginRoot() { return pluginRoot; },
    get defaultMusicRoot() { return defaultMusicRoot; },
  });
}
