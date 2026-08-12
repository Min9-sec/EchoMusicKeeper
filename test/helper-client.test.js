import assert from "node:assert/strict";
import test from "node:test";
import { createHelperClient } from "../src/helper-client.js";

function jsonResponse(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

test("sends bearer token and parses JSON", async () => {
  const calls = [];
  const client = createHelperClient({
    baseUrl: "http://127.0.0.1:45678",
    token: "secret",
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      return jsonResponse({ ok: true });
    },
  });
  assert.deepEqual(await client.health(), { ok: true });
  assert.equal(calls[0].init.headers.Authorization, "Bearer secret");
  assert.equal("token" in client, false);
});

test("maps structured and non-JSON helper errors", async () => {
  const structured = createHelperClient({
    baseUrl: "http://127.0.0.1:45678",
    token: "secret",
    fetchImpl: async () => jsonResponse({ code: "cache-full", message: "cache full" }, 507),
  });
  await assert.rejects(structured.health(), (error) => error.code === "cache-full" && error.status === 507);

  const plain = createHelperClient({
    baseUrl: "http://127.0.0.1:45678",
    token: "secret",
    fetchImpl: async () => new Response("upstream unavailable", { status: 502 }),
  });
  await assert.rejects(plain.health(), /upstream unavailable/);
});

test("uses exact protocol methods, paths, encoded IDs, and JSON bodies", async () => {
  const calls = [];
  const client = createHelperClient({
    baseUrl: "http://127.0.0.1:45678/",
    token: "secret",
    fetchImpl: async (url, init) => {
      calls.push({ url, init });
      return init.method === "POST" && String(url).endsWith("/shutdown")
        ? new Response(null, { status: 204 })
        : jsonResponse({ ok: true });
    },
  });

  await client.updateConfig({ cacheRoot: "C:\\cache" });
  await client.cacheList();
  await client.cacheLookup({ key: "lookup" });
  await client.deleteCache("key / unicode");
  await client.clearCache();
  await client.migrateCache({ cacheRoot: "D:\\cache", mode: "fresh", deleteOld: false });
  await client.createSession({ track: { key: "song" }, remoteUrl: "https://example.test/audio" });
  await client.createDownload({ key: "song" });
  await client.deleteDownload("D:\\music\\song.mp3");
  await client.listTasks();
  await client.pauseTask("task / 1");
  await client.resumeTask("task / 1");
  await client.cancelTask("task / 1");
  await client.refreshTaskUrl("task / 1", "https://example.test/new");
  await client.revealFile({ kind: "select", path: "D:\\music\\song.mp3" });
  assert.equal(await client.shutdown(), undefined);

  assert.deepEqual(
    calls.map(({ url, init }) => [new URL(url).pathname, init.method]),
    [
      ["/v1/config", "PUT"], ["/v1/cache", "GET"], ["/v1/cache/lookup", "POST"],
      ["/v1/cache/key%20%2F%20unicode", "DELETE"], ["/v1/cache/clear", "POST"],
      ["/v1/cache/migrate", "POST"], ["/v1/proxy/sessions", "POST"], ["/v1/downloads", "POST"],
      ["/v1/downloads", "DELETE"], ["/v1/tasks", "GET"], ["/v1/tasks/task%20%2F%201/pause", "POST"],
      ["/v1/tasks/task%20%2F%201/resume", "POST"], ["/v1/tasks/task%20%2F%201/cancel", "POST"],
      ["/v1/tasks/task%20%2F%201/url", "PUT"], ["/v1/files/reveal", "POST"], ["/v1/shutdown", "POST"],
    ],
  );
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
  assert.deepEqual(JSON.parse(calls[0].init.body), { cacheRoot: "C:\\cache" });
  assert.deepEqual(JSON.parse(calls[13].init.body), { remoteUrl: "https://example.test/new" });
});

test("uses a 10 second default timeout and health overrides it", async () => {
  const original = AbortSignal.timeout;
  const values = [];
  AbortSignal.timeout = (milliseconds) => {
    values.push(milliseconds);
    return new AbortController().signal;
  };
  try {
    const client = createHelperClient({
      baseUrl: "http://127.0.0.1:45678",
      token: "secret",
      fetchImpl: async () => jsonResponse({ ok: true }),
    });
    await client.cacheList();
    await client.health({ timeoutMs: 500 });
  } finally {
    AbortSignal.timeout = original;
  }
  assert.deepEqual(values, [10_000, 500]);
});
