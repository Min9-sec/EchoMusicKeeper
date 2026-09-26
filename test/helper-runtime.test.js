import assert from "node:assert/strict";
import test from "node:test";
import { createHelperRuntime } from "../src/helper-runtime.js";

const settings = { cacheRoot: "", downloadRoot: "", cacheLimitBytes: 1024 * 1024 * 1024 };

// Existing cases model the Windows host; the macOS case below pins its own platform.
const createRuntime = (ctx, options = {}) => createHelperRuntime(ctx, { platform: "windows", ...options });

function fakeContext({ launch, terminate } = {}) {
  return {
    descriptor: { directory: "C:\\Plugins\\echo-music-keeper" },
    process: {
      launch: launch ?? (async () => ({ ok: true, pid: 101 })),
      terminate: terminate ?? (async () => {}),
    },
  };
}

test("starts helper with separate arguments, config fields, and a private token", async () => {
  const launches = [];
  const configs = [];
  const runtime = createRuntime(fakeContext({
    launch: async (request) => {
      launches.push(request);
      return { ok: true, pid: 42 };
    },
  }), {
    settings,
    managedDownloadRoots: ["D:\\Archive"],
    completedDownloadPaths: ["D:\\Archive\\saved.mp3"],
    randomPort: () => 50123,
    clientFactory: () => ({
      health: async () => ({ defaultMusicPath: "C:\\Users\\me\\Music" }),
      updateConfig: async (config) => configs.push(config),
      shutdown: async () => {},
    }),
  });

  assert.equal(await runtime.start(), true);
  assert.equal(runtime.pluginRoot, "C:\\Plugins\\echo-music-keeper");
  assert.equal(runtime.defaultMusicRoot, "C:\\Users\\me\\Music");
  assert.equal(runtime.status().state, "online");
  assert.equal(runtime.client.token, undefined);
  assert.equal(launches.length, 1);
  assert.equal(launches[0].executable, "bin/echo-music-keeper-helper.exe");
  assert.deepEqual(launches[0].args.slice(0, 2), ["--port", "50123"]);
  const token = launches[0].args[launches[0].args.indexOf("--token") + 1];
  assert.match(token, /^[0-9a-f]{64}$/);
  assert.deepEqual(configs, [{
    cacheRoot: "C:\\Plugins\\echo-music-keeper\\cache",
    downloadRoot: "C:\\Users\\me\\Music",
    managedDownloadRoots: ["D:\\Archive"],
    completedDownloadPaths: ["D:\\Archive\\saved.mp3"],
    cacheLimitBytes: 1024 * 1024 * 1024,
  }]);
});

test("retries failed health checks, cleans each PID, and falls back without throwing", async () => {
  const terminated = [];
  let nextPID = 0;
  const runtime = createRuntime(fakeContext({
    launch: async () => ({ ok: true, pid: ++nextPID }),
    terminate: async (pid) => terminated.push(pid),
  }), {
    settings,
    randomPort: () => 51000,
    sleep: async () => {},
    clientFactory: () => ({ health: async () => { throw new Error("not ready"); } }),
  });
  assert.equal(await runtime.start(), false);
  assert.deepEqual(terminated, [1, 2, 3]);
  assert.deepEqual(runtime.status(), { state: "fallback", reason: "health-check-failed" });
});

test("a queued stop cleans a starting PID without resurrection", async () => {
  const terminated = [];
  let releaseHealth;
  const healthWait = new Promise((resolve) => { releaseHealth = resolve; });
  const runtime = createRuntime(fakeContext({
    launch: async () => ({ ok: true, pid: 99 }),
    terminate: async (pid) => terminated.push(pid),
  }), {
    settings,
    clientFactory: () => ({
      health: async () => healthWait.then(() => ({ defaultMusicPath: "C:\\Users\\me\\Music" })),
      updateConfig: async () => {},
      shutdown: async () => {},
    }),
  });
  const starting = runtime.start();
  await Promise.resolve();
  const stopping = runtime.stop();
  releaseHealth();
  assert.equal(await starting, true);
  await stopping;
  assert.deepEqual(terminated, [99]);
  assert.deepEqual(runtime.status(), { state: "stopped", reason: null });
});

test("a new start after stop does not inherit a stale process", async () => {
  const terminated = [];
  let releaseFirstHealth;
  const firstHealth = new Promise((resolve) => { releaseFirstHealth = resolve; });
  let launches = 0;
  let clients = 0;
  const runtime = createRuntime(fakeContext({
    launch: async () => ({ ok: true, pid: ++launches }),
    terminate: async (pid) => terminated.push(pid),
  }), {
    settings,
    clientFactory: () => {
      const clientNumber = ++clients;
      return {
        health: async () => clientNumber === 1
          ? firstHealth.then(() => ({ defaultMusicPath: "C:\\Users\\me\\Music" }))
          : { defaultMusicPath: "C:\\Users\\me\\Music" },
        updateConfig: async () => {},
        shutdown: async () => {},
      };
    },
  });
  const first = runtime.start();
  await Promise.resolve();
  const stopping = runtime.stop();
  releaseFirstHealth();
  assert.equal(await first, true);
  await stopping;
  const second = runtime.start();
  assert.equal(await second, true);
  assert.deepEqual(terminated, [1]);
  assert.deepEqual(runtime.status(), { state: "online", reason: null });
});

test("stop gives shutdown a bounded timeout then terminates only the tracked PID", async () => {
  const terminated = [];
  let shutdownSignal;
  const runtime = createRuntime(fakeContext({
    terminate: async (pid) => terminated.push(pid),
  }), {
    settings,
    clientFactory: () => ({
      health: async () => ({ defaultMusicPath: "C:\\Users\\me\\Music" }),
      updateConfig: async () => {},
      shutdown: async ({ signal }) => { shutdownSignal = signal; return new Promise(() => {}); },
    }),
  });
  await runtime.start();
  const original = AbortSignal.timeout;
  AbortSignal.timeout = () => AbortSignal.abort();
  try {
    await runtime.stop();
  } finally {
    AbortSignal.timeout = original;
  }
  assert.ok(shutdownSignal);
  assert.deepEqual(terminated, [101]);
});

test("uses the documented cancellation result without retrying", async () => {
  const launches = [];
  const runtime = createRuntime(fakeContext({
    launch: async (request) => {
      launches.push(request);
      return { ok: false, canceled: true, error: "user declined" };
    },
  }), { settings });
  assert.equal(await runtime.start(), false);
  assert.equal(launches.length, 1);
  assert.deepEqual(runtime.status(), { state: "fallback", reason: "user-canceled" });
});

test("retries ordinary launch failures without terminating a PID", async () => {
  const terminated = [];
  let launches = 0;
  const runtime = createRuntime(fakeContext({
    launch: async () => {
      launches += 1;
      if (launches === 1) throw new Error("host launch threw");
      return { ok: false, canceled: false, error: "host launch failed", pid: 88 };
    },
    terminate: async (pid) => terminated.push(pid),
  }), { settings });
  assert.equal(await runtime.start(), false);
  assert.equal(launches, 3);
  assert.deepEqual(terminated, []);
  assert.deepEqual(runtime.status(), { state: "fallback", reason: "launch-failed" });
});

test("holds a queued start until blocked shutdown has terminated its owner", async () => {
  const terminated = [];
  let launches = 0;
  let releaseShutdown;
  const shutdownBlocked = new Promise((resolve) => { releaseShutdown = resolve; });
  const runtime = createRuntime(fakeContext({
    launch: async () => ({ ok: true, pid: ++launches }),
    terminate: async (pid) => terminated.push(pid),
  }), {
    settings,
    clientFactory: () => ({
      health: async () => ({ defaultMusicPath: "C:\\Users\\me\\Music" }),
      updateConfig: async () => {},
      shutdown: async () => shutdownBlocked,
    }),
  });
  await runtime.start();
  const stopping = runtime.stop();
  await Promise.resolve();
  const starting = runtime.start();
  await Promise.resolve();
  assert.equal(launches, 1);
  assert.deepEqual(terminated, []);
  releaseShutdown();
  await stopping;
  assert.equal(await starting, true);
  assert.equal(launches, 2);
  assert.deepEqual(terminated, [1]);
});

test("deduplicates start calls and serializes restart followed by stop", async () => {
  const terminated = [];
  let launches = 0;
  const runtime = createRuntime(fakeContext({
    launch: async () => ({ ok: true, pid: ++launches }),
    terminate: async (pid) => terminated.push(pid),
  }), {
    settings,
    clientFactory: () => ({
      health: async () => ({ defaultMusicPath: "C:\\Users\\me\\Music" }),
      updateConfig: async () => {},
      shutdown: async () => {},
    }),
  });
  const first = runtime.start();
  assert.equal(runtime.start(), first);
  assert.equal(await first, true);
  const restarting = runtime.restart();
  const stopping = runtime.stop();
  assert.equal(await restarting, true);
  await stopping;
  assert.equal(launches, 2);
  assert.deepEqual(terminated, [1, 2]);
  assert.deepEqual(runtime.status(), { state: "stopped", reason: null });
});

test("serializes delayed launch, health, and config before a queued replacement", async () => {
  const terminated = [];
  let launches = 0;
  let clients = 0;
  let releaseLaunch;
  let releaseHealth;
  let releaseConfig;
  let launchSeen;
  let healthSeen;
  let configSeen;
  const launchGate = new Promise((resolve) => { releaseLaunch = resolve; });
  const healthGate = new Promise((resolve) => { releaseHealth = resolve; });
  const configGate = new Promise((resolve) => { releaseConfig = resolve; });
  const runtime = createRuntime(fakeContext({
    launch: async () => {
      launches += 1;
      if (launches === 1) {
        launchSeen();
        await launchGate;
      }
      return { ok: true, pid: launches };
    },
    terminate: async (pid) => terminated.push(pid),
  }), {
    settings,
    clientFactory: () => {
      const clientNumber = ++clients;
      return {
        health: async () => {
          if (clientNumber === 1) {
            healthSeen();
            await healthGate;
          }
          return { defaultMusicPath: "C:\\Users\\me\\Music" };
        },
        updateConfig: async () => {
          if (clientNumber === 1) {
            configSeen();
            await configGate;
          }
        },
        shutdown: async () => {},
      };
    },
  });
  const launchWait = new Promise((resolve) => { launchSeen = resolve; });
  const healthWait = new Promise((resolve) => { healthSeen = resolve; });
  const configWait = new Promise((resolve) => { configSeen = resolve; });
  const first = runtime.start();
  await launchWait;
  releaseLaunch();
  await healthWait;
  releaseHealth();
  await configWait;
  const stopping = runtime.stop();
  const replacement = runtime.start();
  assert.equal(launches, 1);
  releaseConfig();
  assert.equal(await first, true);
  await stopping;
  assert.equal(await replacement, true);
  assert.equal(launches, 2);
  assert.deepEqual(terminated, [1]);
});

test("releases ownership after termination errors", async () => {
  const terminated = [];
  let nextPID = 0;
  const runtime = createRuntime(fakeContext({
    launch: async () => ({ ok: true, pid: ++nextPID }),
    terminate: async (pid) => {
      terminated.push(pid);
      throw new Error("host termination failed");
    },
  }), {
    settings,
    sleep: async () => {},
    clientFactory: () => ({ health: async () => { throw new Error("not ready"); } }),
  });
  assert.equal(await runtime.start(), false);
  await runtime.stop();
  assert.deepEqual(terminated, [1, 2, 3]);
});

test("launches the macOS helper and derives macOS default paths", async () => {
  const launches = [];
  const configs = [];
  const pluginRoot = "/Applications/EchoMusic/plugins/echo-music-keeper";
  const runtime = createRuntime({
    descriptor: { directory: pluginRoot },
    process: {
      launch: async (request) => {
        launches.push(request);
        return { ok: true, pid: 77 };
      },
      terminate: async () => {},
    },
  }, {
    settings,
    platform: "macos",
    randomPort: () => 50124,
    managedDownloadRoots: ["/Users/me/Music/Archive"],
    completedDownloadPaths: ["/Users/me/Music/Archive/saved.mp3"],
    clientFactory: () => ({
      health: async () => ({ defaultMusicPath: "/Users/me/Music" }),
      updateConfig: async (config) => configs.push(config),
      shutdown: async () => {},
    }),
  });

  assert.equal(await runtime.start(), true);
  assert.equal(runtime.platform, "macos");
  assert.equal(runtime.pathSeparator, "/");
  assert.equal(runtime.executable, "bin/echo-music-keeper-helper-macos");
  assert.equal(runtime.defaultMusicRoot, "/Users/me/Music");
  assert.equal(launches.length, 1);
  assert.equal(launches[0].executable, "bin/echo-music-keeper-helper-macos");
  assert.deepEqual(launches[0].args.slice(0, 2), ["--port", "50124"]);
  assert.deepEqual(configs, [{
    cacheRoot: `${pluginRoot}/cache`,
    downloadRoot: "/Users/me/Music",
    managedDownloadRoots: ["/Users/me/Music/Archive"],
    completedDownloadPaths: ["/Users/me/Music/Archive/saved.mp3"],
    cacheLimitBytes: 1024 * 1024 * 1024,
  }]);
});

test("detects the host platform from the host API when no override is provided", () => {
  const runtime = createHelperRuntime({
    descriptor: { directory: "/Applications/EchoMusic/plugins/echo-music-keeper" },
    electron: { platform: "darwin" },
    process: { launch: async () => ({ ok: true, pid: 1 }), terminate: async () => {} },
  }, { settings });
  assert.equal(runtime.platform, "macos");
  assert.equal(runtime.pathSeparator, "/");
  assert.equal(runtime.executable, "bin/echo-music-keeper-helper-macos");
});

test("prefers an injected environment over the host platform API", () => {
  const runtime = createHelperRuntime({
    descriptor: { directory: "C:\\Plugins\\echo-music-keeper" },
    electron: { platform: "darwin" },
    process: { launch: async () => ({ ok: true, pid: 1 }), terminate: async () => {} },
  }, {
    settings,
    environment: { process: { platform: "win32" } },
  });
  assert.equal(runtime.platform, "windows");
  assert.equal(runtime.pathSeparator, "\\");
  assert.equal(runtime.executable, "bin/echo-music-keeper-helper.exe");
});
