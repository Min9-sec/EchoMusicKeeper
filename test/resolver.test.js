import assert from "node:assert/strict";
import test from "node:test";
import { createAudioResolver, resolveCachedOrProxy } from "../src/resolver.js";

const catalogHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const actualHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const track = { hash: catalogHash, title: "Song", artist: "Artist", album: "Album" };

function onlineRuntime(client) {
  return { status: () => ({ state: "online" }), client };
}

function context(overrides = {}) {
  return {
    fs: { getFileUrl: async (path) => ({ ok: true, url: `file://${path}` }) },
    toast: { warning: () => {} },
    ...overrides,
  };
}

test("uses an offline-restart alias complete hit before any Kugou request", async () => {
  const calls = [];
  const cachedKey = `${actualHash}.flac.none`;
  const client = { cacheLookup: async (request) => {
    calls.push(request);
    return { status: "complete", key: cachedKey, path: "D:/Custom Cache/song.flac", track: { key: cachedKey, catalogHash, hash: actualHash, requestedQuality: "flac", quality: "flac", effect: "none" } };
  } };
  const ctx = context({ kugou: { music: {
    getSongPrivilegeLite: async () => assert.fail("should not resolve privilege"),
    getSongUrl: async () => assert.fail("should not resolve URL"),
  } } });
  const result = await resolveCachedOrProxy({
    ctx, runtime: onlineRuntime(client), settings: { autoCache: false }, track, quality: "lossless",
  });
  assert.deepEqual(result, { url: "file://D:/Custom Cache/song.flac", quality: "flac", effect: "none" });
  assert.deepEqual(calls, [{ catalogHash, requestedQuality: "flac", effect: "none" }]);
});

test("creates one proxy session after alias and actual misses", async () => {
  const calls = [];
  const client = {
    cacheLookup: async (request) => { calls.push(["lookup", request]); return { status: "miss" }; },
    createSession: async (request) => { calls.push(["create", request]); return { playUrl: "http://127.0.0.1/stream" }; },
  };
  const ctx = context({ kugou: { music: {
    getSongPrivilegeLite: async () => { calls.push(["privilege"]); return { data: [{ relate_goods: [{ quality: "320", hash: actualHash }] }] }; },
    getSongUrl: async () => { calls.push(["url"]); return { data: { url: "https://cdn.example/song.mp3" } }; },
  } } });
  const result = await resolveCachedOrProxy({
    ctx, runtime: onlineRuntime(client), settings: { autoCache: true }, track, quality: "320",
  });
  assert.deepEqual(result, { url: "http://127.0.0.1/stream", quality: "320", effect: "none" });
  assert.deepEqual(calls.map(([kind]) => kind), ["lookup", "privilege", "url", "lookup", "create"]);
  assert.equal(calls[3][1].key, `${actualHash}.320.none`);
  assert.equal(calls[4][1].track.catalogHash, catalogHash);
  assert.equal(calls[4][1].track.hash, actualHash);
});

test("does not proxy a cache miss when automatic caching is disabled", async () => {
  let privilegeCalls = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ kugou: { music: { getSongPrivilegeLite: async () => { privilegeCalls += 1; } } } }),
    runtime: onlineRuntime({ cacheLookup: async () => ({ status: "miss" }) }),
    settings: { autoCache: false }, track, quality: "128",
  });
  assert.equal(result, null);
  assert.equal(privilegeCalls, 0);
});

test("rejects non-Kugou sources, effects, and unhealthy helpers", async () => {
  const client = { cacheLookup: async () => assert.fail("cache should not be called") };
  for (const candidate of [
    { ...track, source: "cloud" }, { ...track, hash: "" },
  ]) {
    assert.equal(await resolveCachedOrProxy({ ctx: context(), runtime: onlineRuntime(client), settings: {}, track: candidate, quality: "128" }), null);
  }
  assert.equal(await resolveCachedOrProxy({ ctx: context(), runtime: onlineRuntime(client), settings: {}, track, quality: "128", effect: "spatial" }), null);
  assert.equal(await resolveCachedOrProxy({ ctx: context(), runtime: { status: () => ({ state: "fallback" }), client }, settings: {}, track, quality: "128" }), null);
});

test("force reload refreshes a single needs-url task without another create", async () => {
  const calls = [];
  const client = {
    cacheLookup: async (request) => {
      calls.push(["lookup", request]);
      const key = `${actualHash}.128.none`;
      return request.key ? { status: "partial", key, track: { key, catalogHash, hash: actualHash, requestedQuality: "128", quality: "128", effect: "none" } } : { status: "miss" };
    },
    listTasks: async () => assert.fail("resolver must not own task polling"),
    createSession: async () => assert.fail("must reuse existing needs-url task"),
  };
  const taskStore = {
    refreshNeedsURL: async (key, url) => {
      calls.push(["refresh", key, url]);
      return key === `${actualHash}.128.none`;
    },
  };
  const ctx = context({ kugou: { music: {
    getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
    getSongUrl: async () => ({ url: "https://cdn.example/new.mp3" }),
  } } });
  const result = await resolveCachedOrProxy({
    ctx, runtime: onlineRuntime(client), taskStore, settings: { autoCache: true }, track, quality: "128", forceReload: true,
  });
  assert.equal(result, null);
  assert.deepEqual(calls.map(([kind]) => kind), ["lookup", "lookup", "refresh"]);
});

test("uses the actual downgraded quality in the actual-key lookup and session", async () => {
  const calls = [];
  const client = {
    cacheLookup: async (request) => { calls.push(["lookup", request]); return { status: "miss" }; },
    createSession: async (request) => { calls.push(["create", request]); return { playUrl: "http://127.0.0.1/stream" }; },
  };
  const ctx = context({ kugou: { music: {
    getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "high", hash: actualHash }] }] }),
    getSongUrl: async (hash, quality) => { calls.push(["url", hash, quality]); return { url: "https://cdn.example/song.mp3" }; },
  } } });
  const result = await resolveCachedOrProxy({ ctx, runtime: onlineRuntime(client), settings: { autoCache: true }, track, quality: "super" });
  assert.deepEqual(result, { url: "http://127.0.0.1/stream", quality: "high", effect: "none" });
  assert.equal(calls[1][2], "high");
  assert.equal(calls[2][1].key, `${actualHash}.high.none`);
  assert.equal(calls[3][1].track.requestedQuality, "super");
  assert.equal(calls[3][1].track.quality, "high");
});

test("rejects malformed helper lookup responses before Kugou or session work", async () => {
  const warnings = [];
  for (const lookup of [
    {}, { status: "unknown" }, { status: "complete", path: "relative.mp3" }, { status: "partial", key: "bad" },
  ]) {
    const resolver = createAudioResolver(
      context({ toast: { warning: (message) => warnings.push(message) }, kugou: { music: {
        getSongPrivilegeLite: async () => assert.fail("must not resolve privilege"),
        getSongUrl: async () => assert.fail("must not resolve URL"),
      } } }),
      onlineRuntime({ cacheLookup: async () => lookup, createSession: async () => assert.fail("must not create") }),
      { value: { autoCache: true } },
    );
    assert.equal(await resolver.resolve({ track, quality: "128", effect: "none" }), null);
  }
  assert.equal(warnings.length, 4);
});

test("rejects a valid-looking alias lookup for another catalog without opening its file", async () => {
  const otherCatalogHash = "cccccccccccccccccccccccccccccccc";
  const key = `${actualHash}.128.none`;
  let fileCalls = 0;
  let privilegeCalls = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({
      fs: { getFileUrl: async () => { fileCalls += 1; return { ok: true, url: "file:///cache/other.mp3" }; } },
      kugou: { music: { getSongPrivilegeLite: async () => { privilegeCalls += 1; } } },
    }),
    runtime: onlineRuntime({ cacheLookup: async () => ({
      status: "complete", key, path: "/cache/other.mp3",
      track: { key, catalogHash: otherCatalogHash, hash: actualHash, requestedQuality: "128", quality: "128", effect: "none" },
    }) }),
    settings: { autoCache: true }, track, quality: "128",
  });
  assert.equal(result, null);
  assert.equal(fileCalls, 0);
  assert.equal(privilegeCalls, 0);
});

test("rejects uppercase helper cache keys and hashes before opening a local file", async () => {
  const key = `${actualHash.toUpperCase()}.128.none`;
  let fileCalls = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ fs: { getFileUrl: async () => { fileCalls += 1; return { ok: true, url: "file:///cache/song.mp3" }; } } }),
    runtime: onlineRuntime({ cacheLookup: async () => ({
      status: "complete", key, path: "/cache/song.mp3",
      track: { key, catalogHash, hash: actualHash.toUpperCase(), requestedQuality: "128", quality: "128", effect: "none" },
    }) }),
    settings: { autoCache: true }, track, quality: "128",
  });
  assert.equal(result, null);
  assert.equal(fileCalls, 0);
});

test("rejects normalized alias lookup metadata instead of raw canonical metadata", async () => {
  const key = `${actualHash}.128.none`;
  for (const trackMetadata of [
    { catalogHash: catalogHash.toUpperCase(), requestedQuality: "128", quality: "128" },
    { catalogHash, requestedQuality: "standard", quality: "128" },
  ]) {
    let fileCalls = 0;
    const result = await resolveCachedOrProxy({
      ctx: context({ fs: { getFileUrl: async () => { fileCalls += 1; return { ok: true, url: "file:///cache/song.mp3" }; } } }),
      runtime: onlineRuntime({ cacheLookup: async () => ({
        status: "complete", key, path: "/cache/song.mp3",
        track: { key, hash: actualHash, effect: "none", ...trackMetadata },
      }) }),
      settings: { autoCache: true }, track, quality: "128",
    });
    assert.equal(result, null);
    assert.equal(fileCalls, 0);
  }
});

test("rejects normalized exact lookup metadata before creating a session", async () => {
  const key = `${actualHash}.flac.none`;
  let lookupCalls = 0;
  let sessions = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ kugou: { music: {
      getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "flac", hash: actualHash }] }] }),
      getSongUrl: async () => ({ url: "https://cdn.example/song.flac" }),
    } } }),
    runtime: onlineRuntime({
      cacheLookup: async () => {
        lookupCalls += 1;
        return lookupCalls === 1 ? { status: "miss" } : {
          status: "partial", key,
          track: { key, catalogHash, hash: actualHash, requestedQuality: "lossless", quality: "lossless", effect: "none" },
        };
      },
      createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
    }),
    settings: { autoCache: true }, track, quality: "flac",
  });
  assert.equal(result, null);
  assert.equal(sessions, 0);
});

test("reuses a shared actual complete entry stored for a different requested quality", async () => {
  const key = `${actualHash}.high.none`;
  let lookups = 0;
  let sessions = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ kugou: { music: {
      getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "high", hash: actualHash }] }] }),
      getSongUrl: async () => ({ url: "https://cdn.example/song.flac" }),
    } } }),
    runtime: onlineRuntime({
      cacheLookup: async () => {
        lookups += 1;
        return lookups === 1 ? { status: "miss" } : {
          status: "complete", key, path: "/cache/song.flac",
          track: { key, catalogHash, hash: actualHash, requestedQuality: "super", quality: "high", effect: "none" },
        };
      },
      createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
    }),
    settings: { autoCache: true }, track, quality: "high",
  });
  assert.deepEqual(result, { url: "file:///cache/song.flac", quality: "high", effect: "none" });
  assert.equal(sessions, 0);
});

test("reuses a shared actual partial entry stored for another catalog alias", async () => {
  const otherCatalogHash = "cccccccccccccccccccccccccccccccc";
  const key = `${actualHash}.128.none`;
  let lookups = 0;
  let sessions = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ kugou: { music: {
      getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
      getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
    } } }),
    runtime: onlineRuntime({
      cacheLookup: async () => {
        lookups += 1;
        return lookups === 1 ? { status: "miss" } : {
          status: "partial", key,
          track: { key, catalogHash: otherCatalogHash, hash: actualHash, requestedQuality: "super", quality: "128", effect: "none" },
        };
      },
      createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
    }),
    settings: { autoCache: true }, track, quality: "128",
  });
  assert.equal(result?.url, "http://127.0.0.1/stream");
  assert.equal(sessions, 1);
});

test("rejects an exact entry with noncanonical stored alias metadata", async () => {
  const key = `${actualHash}.128.none`;
  let lookups = 0;
  let sessions = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ kugou: { music: {
      getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
      getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
    } } }),
    runtime: onlineRuntime({
      cacheLookup: async () => {
        lookups += 1;
        return lookups === 1 ? { status: "miss" } : {
          status: "partial", key,
          track: { key, catalogHash: catalogHash.toUpperCase(), hash: actualHash, requestedQuality: "standard", quality: "128", effect: "none" },
        };
      },
      createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
    }),
    settings: { autoCache: true }, track, quality: "128",
  });
  assert.equal(result, null);
  assert.equal(sessions, 0);
});

test("uses recovered exact entries with an empty alias pair", async () => {
  const key = `${actualHash}.128.none`;
  for (const status of ["complete", "partial"]) {
    let lookups = 0;
    let sessions = 0;
    const result = await resolveCachedOrProxy({
      ctx: context({ kugou: { music: {
        getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
        getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
      } } }),
      runtime: onlineRuntime({
        cacheLookup: async () => {
          lookups += 1;
          return lookups === 1 ? { status: "miss" } : {
            status, key, path: status === "complete" ? "/cache/song.mp3" : undefined,
            track: { key, catalogHash: "", hash: actualHash, requestedQuality: "", quality: "128", effect: "none" },
          };
        },
        createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
      }),
      settings: { autoCache: true }, track, quality: "128",
    });
    assert.equal(result?.url, status === "complete" ? "file:///cache/song.mp3" : "http://127.0.0.1/stream", status);
    assert.equal(sessions, status === "complete" ? 0 : 1, status);
  }
});

test("rejects a recovered exact entry with only one empty alias field", async () => {
  const key = `${actualHash}.128.none`;
  for (const aliases of [
    { catalogHash: "", requestedQuality: "128" },
    { catalogHash, requestedQuality: "" },
  ]) {
    let lookups = 0;
    let sessions = 0;
    const result = await resolveCachedOrProxy({
      ctx: context({ kugou: { music: {
        getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
        getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
      } } }),
      runtime: onlineRuntime({
        cacheLookup: async () => {
          lookups += 1;
          return lookups === 1 ? { status: "miss" } : { status: "partial", key, track: { key, hash: actualHash, quality: "128", effect: "none", ...aliases } };
        },
        createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
      }),
      settings: { autoCache: true }, track, quality: "128",
    });
    assert.equal(result, null);
    assert.equal(sessions, 0);
  }
});

test("rejects non-string identity fields for alias and exact cache lookups", async () => {
  const numericHash = 1111111111111111;
  const numericKey = "1111111111111111.128.none";
  const keyObject = { toString: () => `${actualHash}.128.none` };
  for (const response of [
    {
      status: "complete", key: numericKey, path: "/cache/song.mp3",
      track: { key: numericKey, catalogHash, hash: numericHash, requestedQuality: "128", quality: "128", effect: "none" },
    },
    {
      status: "complete", key: keyObject, path: "/cache/song.mp3",
      track: { key: keyObject, catalogHash, hash: actualHash, requestedQuality: "128", quality: "128", effect: "none" },
    },
  ]) {
    let fileCalls = 0;
    const result = await resolveCachedOrProxy({
      ctx: context({ fs: { getFileUrl: async () => { fileCalls += 1; return { ok: true, url: "file:///cache/song.mp3" }; } } }),
      runtime: onlineRuntime({ cacheLookup: async () => response }), settings: { autoCache: true }, track, quality: "128",
    });
    assert.equal(result, null);
    assert.equal(fileCalls, 0);
  }

  for (const hash of [numericHash, { toString: () => numericKey.slice(0, 16) }]) {
    let lookups = 0;
    let sessions = 0;
    const result = await resolveCachedOrProxy({
      ctx: context({ kugou: { music: {
        getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: numericKey.slice(0, 16) }] }] }),
        getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
      } } }),
      runtime: onlineRuntime({
        cacheLookup: async () => {
          lookups += 1;
          return lookups === 1 ? { status: "miss" } : {
            status: "partial", key: numericKey,
            track: { key: numericKey, catalogHash, hash, requestedQuality: "128", quality: "128", effect: "none" },
          };
        },
        createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
      }), settings: { autoCache: true }, track, quality: "128",
    });
    assert.equal(result, null);
    assert.equal(sessions, 0);
  }
});

test("rejects a valid-looking actual lookup for another key before session creation", async () => {
  const otherHash = "cccccccccccccccccccccccccccccccc";
  const otherKey = `${otherHash}.128.none`;
  let lookups = 0;
  let sessions = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ kugou: { music: {
      getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
      getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
    } } }),
    runtime: onlineRuntime({
      cacheLookup: async () => {
        lookups += 1;
        return lookups === 1 ? { status: "miss" } : {
          status: "partial", key: otherKey,
          track: { key: otherKey, catalogHash, hash: otherHash, requestedQuality: "128", quality: "128", effect: "none" },
        };
      },
      createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
    }),
    settings: { autoCache: true }, track, quality: "128",
  });
  assert.equal(result, null);
  assert.equal(sessions, 0);
});

test("does not trust an injected lookup task when no matching authenticated task exists", async () => {
  const key = `${actualHash}.128.none`;
  let refreshes = 0;
  let sessions = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ kugou: { music: {
      getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
      getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
    } } }),
    runtime: onlineRuntime({
      cacheLookup: async (request) => request.key ? {
        status: "partial", key, task: { id: "injected", state: "needs-url", track: { key } },
        track: { key, catalogHash, hash: actualHash, requestedQuality: "128", quality: "128", effect: "none" },
      } : { status: "miss" },
      listTasks: async () => assert.fail("resolver must not own task polling"),
      refreshTaskUrl: async () => { refreshes += 1; },
      createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
    }),
    taskStore: { refreshNeedsURL: async () => false },
    settings: { autoCache: true }, track, quality: "128", forceReload: true,
  });
  assert.equal(result?.url, "http://127.0.0.1/stream");
  assert.equal(refreshes, 0);
  assert.equal(sessions, 1);
});

test("does not resolve remotely when a complete file URL conversion fails", async () => {
  const key = `${actualHash}.128.none`;
  let privilegeCalls = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ fs: { getFileUrl: async () => ({ ok: false }) }, kugou: { music: { getSongPrivilegeLite: async () => { privilegeCalls += 1; } } } }),
    runtime: onlineRuntime({ cacheLookup: async () => ({ status: "complete", key, path: "/cache/song.mp3", track: { key, catalogHash, hash: actualHash, requestedQuality: "128", quality: "128", effect: "none" } }) }),
    settings: { autoCache: true }, track, quality: "128",
  });
  assert.equal(result, null);
  assert.equal(privilegeCalls, 0);
});

test("force reload creates a session after an ordinary actual-key miss", async () => {
  let sessions = 0;
  const result = await resolveCachedOrProxy({
    ctx: context({ kugou: { music: {
      getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
      getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
    } } }),
    runtime: onlineRuntime({
      cacheLookup: async () => ({ status: "miss" }),
      listTasks: async () => assert.fail("resolver must not own task polling"),
      createSession: async () => { sessions += 1; return { playUrl: "http://127.0.0.1/stream" }; },
    }),
    taskStore: { refreshNeedsURL: async () => false },
    settings: { autoCache: true }, track, quality: "128", forceReload: true,
  });
  assert.equal(result?.url, "http://127.0.0.1/stream");
  assert.equal(sessions, 1);
});

test("returns null and emits one bounded warning on resolver failure", async () => {
  const warnings = [];
  const resolver = createAudioResolver(
    context({ toast: { warning: (message) => warnings.push(message) } }),
    onlineRuntime({ cacheLookup: async () => { throw new Error("https://signed.example/path?token=secret"); } }),
    { value: { autoCache: true } },
  );
  assert.equal(await resolver.resolve({ track, quality: "128", effect: "none" }), null);
  assert.equal(await resolver.resolve({ track, quality: "128", effect: "none" }), null);
  assert.equal(warnings.length, 1);
  assert.doesNotMatch(warnings[0], /token|https?:/i);
});

test("matches both current and documented helper runtime status contracts", () => {
  const ctx = context();
  assert.equal(createAudioResolver(ctx, { status: () => ({ state: "online" }), client: {} }, { value: {} }).match({ track, effect: "none" }), true);
  assert.equal(createAudioResolver(ctx, { status: { value: "ready" }, client: {} }, { value: {} }).match({ track, effect: "none" }), true);
});
