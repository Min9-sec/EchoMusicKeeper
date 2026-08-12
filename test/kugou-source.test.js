import assert from "node:assert/strict";
import test from "node:test";
import {
  extractSongURLs,
  normalizeQuality,
  resolveKugouSource,
  selectQualityHash,
} from "../src/kugou-source.js";

const catalogHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const actualHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

test("normalizes the supported Kugou quality aliases", () => {
  const cases = [
    ["128", "128"], ["128k", "128"], ["standard", "128"],
    ["320", "320"], ["320kbps", "320"], ["hq", "320"],
    ["flac", "flac"], ["lossless", "flac"],
    ["high", "high"], ["hi-res", "high"], ["hires", "high"],
    ["super", "super"], ["master", "super"],
    ["unknown", null], [null, null],
  ];
  for (const [input, expected] of cases) assert.equal(normalizeQuality(input), expected, String(input));
});

test("selects matched quality hashes across host response shapes", () => {
  const cases = [
    {
      name: "128 track relateGoods",
      track: { hash: catalogHash, relateGoods: [{ quality: "128k", hash: actualHash }] },
      payload: {}, quality: "128",
    },
    {
      name: "track relateGoods",
      track: { hash: catalogHash, relateGoods: [{ quality: "320", hash: actualHash }] },
      payload: {}, quality: "320",
    },
    {
      name: "array data relate_goods",
      track: { hash: catalogHash, relateGoods: [] },
      payload: { data: [{ relate_goods: [{ quality: "flac", hash: actualHash }] }] }, quality: "lossless",
    },
    {
      name: "object data relate_goods",
      track: { hash: catalogHash },
      payload: { data: { relate_goods: [{ quality: "highres", hash: actualHash }] } }, quality: "high",
    },
    {
      name: "direct catalog fallback",
      track: { hash: catalogHash }, payload: {}, quality: "super", expected: catalogHash, actualQuality: "128",
    },
  ];
  for (const entry of cases) {
    assert.deepEqual(
      selectQualityHash(entry.track, entry.payload, entry.quality),
      { hash: entry.expected ?? actualHash, quality: entry.actualQuality ?? normalizeQuality(entry.quality) },
      entry.name,
    );
  }
});

test("uses the actual available quality when the request must downgrade", () => {
  const hashes = {
    128: "11111111111111111111111111111111",
    320: "22222222222222222222222222222222",
    flac: "33333333333333333333333333333333",
    high: "44444444444444444444444444444444",
    super: "55555555555555555555555555555555",
  };
  const cases = [
    ["128", [], { hash: catalogHash, quality: "128" }],
    ["320", [{ quality: "128", hash: hashes[128] }], { hash: hashes[128], quality: "128" }],
    ["flac", [{ quality: "320", hash: hashes[320] }], { hash: hashes[320], quality: "320" }],
    ["high", [{ quality: "flac", hash: hashes.flac }], { hash: hashes.flac, quality: "flac" }],
    ["super", [{ quality: "high", hash: hashes.high }], { hash: hashes.high, quality: "high" }],
  ];
  for (const [requested, relate_goods, expected] of cases) {
    assert.deepEqual(
      selectQualityHash({ hash: catalogHash }, { data: [{ relate_goods }] }, requested),
      expected,
      requested,
    );
  }
});

test("extracts only http song URLs from documented response fields", () => {
  const urls = extractSongURLs({
    url: "https://cdn.example/song.mp3?token=secret",
    backup_url: "http://backup.example/song.flac",
    data: {
      url: "https://data.example/song.m4a",
      backupUrl: "ftp://invalid.example/song.mp3",
    },
  });
  assert.deepEqual(urls, [
    "https://cdn.example/song.mp3?token=secret",
    "http://backup.example/song.flac",
    "https://data.example/song.m4a",
  ]);
  assert.deepEqual(extractSongURLs({ url: "", data: { backupUrl: "javascript:alert(1)" } }), []);
});

test("retries exactly once after a successful verification challenge", async () => {
  const calls = { privilege: 0, url: 0, verification: [] };
  const ctx = {
    kugou: { music: {
      getSongPrivilegeLite: async () => {
        calls.privilege += 1;
        return calls.privilege === 1
          ? { status: 0, error_code: 20028, eventId: "challenge-1" }
          : { data: [{ relate_goods: [{ quality: "320", hash: actualHash }] }] };
      },
      getSongUrl: async () => {
        calls.url += 1;
        return { data: { url: "https://cdn.example/song.ogg" } };
      },
    } },
    kugouVerification: { request: async (eventId) => { calls.verification.push(eventId); return { ok: true }; } },
  };
  const source = await resolveKugouSource(ctx, { hash: catalogHash, albumId: 9 }, "320");
  assert.deepEqual(calls, { privilege: 2, url: 1, verification: ["challenge-1"] });
  assert.deepEqual(source, {
    catalogHash, actualHash, requestedQuality: "320", quality: "320",
    urls: ["https://cdn.example/song.ogg"], extension: "ogg",
  });
});

test("stops after a canceled verification and discards malformed URLs", async () => {
  let urlCalls = 0;
  const canceled = await resolveKugouSource({
    kugou: { music: {
      getSongPrivilegeLite: async () => ({ status: 0, ssaCode: "challenge-2" }),
      getSongUrl: async () => { urlCalls += 1; return {}; },
    } },
    kugouVerification: { request: async () => ({ ok: false, canceled: true }) },
  }, { hash: catalogHash }, "128");
  assert.equal(canceled, null);
  assert.equal(urlCalls, 0);
});

test("does not continue after repeat security challenges or failed verification", async () => {
  const cases = [
    {
      name: "privilege repeat", privilege: [
        { status: 0, error_code: 20028, eventId: "first" },
        { status: 0, error_code: 20028, eventId: "second" },
      ], url: [], expected: { privilege: 2, url: 0, verification: 1 },
    },
    {
      name: "URL repeat", privilege: [{ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }], url: [
        { status: 0, error_code: 20028, eventId: "first" },
        { status: 0, error_code: 20028, eventId: "second" },
      ], expected: { privilege: 1, url: 2, verification: 1 },
    },
    {
      name: "verification failure", privilege: [{ status: 0, error_code: 20028, eventId: "first" }], url: [],
      verification: { ok: false, error: "declined" }, expected: { privilege: 1, url: 0, verification: 1 },
    },
  ];
  for (const entry of cases) {
    const calls = { privilege: 0, url: 0, verification: 0 };
    const source = await resolveKugouSource({
      kugou: { music: {
        getSongPrivilegeLite: async () => entry.privilege[calls.privilege++],
        getSongUrl: async () => entry.url[calls.url++],
      } },
      kugouVerification: { request: async () => { calls.verification += 1; return entry.verification ?? { ok: true }; } },
    }, { hash: catalogHash }, "128");
    assert.equal(source, null, entry.name);
    assert.deepEqual(calls, entry.expected, entry.name);
  }
});

test("rejects explicit business failures without consuming stale hashes or URLs", async () => {
  const failures = [
    { status: 0 }, { error_code: 401 }, { success: false }, { error: "login required" },
  ];
  for (const failure of failures) {
    let urlCalls = 0;
    const source = await resolveKugouSource({ kugou: { music: {
      getSongPrivilegeLite: async () => ({ ...failure, data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
      getSongUrl: async () => { urlCalls += 1; return { url: "https://cdn.example/stale.mp3" }; },
    } } }, { hash: catalogHash }, "128");
    assert.equal(source, null, JSON.stringify(failure));
    assert.equal(urlCalls, 0, JSON.stringify(failure));
  }

  const source = await resolveKugouSource({ kugou: { music: {
    getSongPrivilegeLite: async () => ({ data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
    getSongUrl: async () => ({ status: 0, url: "https://cdn.example/stale.mp3" }),
  } } }, { hash: catalogHash }, "128");
  assert.equal(source, null);
});

test("does not verify an ordinary business error merely because it has an event ID", async () => {
  let verificationCalls = 0;
  const source = await resolveKugouSource({
    kugou: { music: {
      getSongPrivilegeLite: async () => ({ eventId: "ordinary-error", success: false, error: "login required" }),
      getSongUrl: async () => assert.fail("must not call URL endpoint"),
    } },
    kugouVerification: { request: async () => { verificationCalls += 1; return { ok: true }; } },
  }, { hash: catalogHash }, "128");
  assert.equal(source, null);
  assert.equal(verificationCalls, 0);
});

test("recognizes only documented missing-status ssaCode challenges", async () => {
  const cases = [
    { name: "ssa missing status", payload: { ssaCode: "ssa-missing" }, expected: 1 },
    { name: "event missing status", payload: { eventId: "event-missing" }, expected: 0 },
    { name: "ssa null status", payload: { ssaCode: "ssa-null", status: null }, expected: 0 },
    { name: "ssa empty status", payload: { ssaCode: "ssa-empty", status: "" }, expected: 0 },
    { name: "ssa false status", payload: { ssaCode: "ssa-false", status: false }, expected: 0 },
    { name: "ssa zero status", payload: { ssaCode: "ssa-zero", status: "0" }, expected: 1 },
    { name: "event zero status", payload: { eventId: "event-zero", status: 0 }, expected: 1 },
    { name: "ssa one status", payload: { ssaCode: "ssa-one", status: 1 }, expected: 0 },
  ];
  for (const entry of cases) {
    let verificationCalls = 0;
    let privilegeCalls = 0;
    const source = await resolveKugouSource({
      kugou: { music: {
        getSongPrivilegeLite: async () => {
          privilegeCalls += 1;
          return privilegeCalls === 1
            ? entry.payload
            : { data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] };
        },
        getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
      } },
      kugouVerification: { request: async () => { verificationCalls += 1; return { ok: true }; } },
    }, { hash: catalogHash }, "128");
    assert.equal(verificationCalls, entry.expected, entry.name);
    if (entry.expected) assert.equal(source?.actualHash, actualHash, entry.name);
  }
});

test("uses fresh direct privilege hashes before stale same-quality track metadata", () => {
  const hashes = {
    128: "11111111111111111111111111111111",
    320: "22222222222222222222222222222222",
    flac: "33333333333333333333333333333333",
    high: "44444444444444444444444444444444",
    super: "55555555555555555555555555555555",
  };
  for (const quality of ["128", "320", "flac", "high", "super"]) {
    const field = quality === "128" ? "hash" : `${quality}Hash`;
    const stale = "99999999999999999999999999999999";
    assert.deepEqual(
      selectQualityHash(
        { hash: catalogHash, relateGoods: [{ quality, hash: stale }] },
        { [field]: hashes[quality] },
        quality,
      ),
      { hash: hashes[quality], quality },
      quality,
    );
  }
  assert.deepEqual(
    selectQualityHash(
      { hash: catalogHash, relateGoods: [{ quality: "flac", hash: hashes.flac }] },
      { highHash: hashes.high },
      "super",
    ),
    { hash: hashes.high, quality: "high" },
  );
});

test("treats present malformed statuses as terminal failures without verification", async () => {
  const malformed = [null, "", false, {}, [], 2, "unexpected"];
  for (const status of malformed) {
    let urlCalls = 0;
    let verificationCalls = 0;
    const source = await resolveKugouSource({
      kugou: { music: {
        getSongPrivilegeLite: async () => ({ status, ssaCode: "not-a-challenge", data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
        getSongUrl: async () => { urlCalls += 1; return { url: "https://cdn.example/stale.mp3" }; },
      } },
      kugouVerification: { request: async () => { verificationCalls += 1; return { ok: true }; } },
    }, { hash: catalogHash }, "128");
    assert.equal(source, null, String(status));
    assert.equal(urlCalls, 0, String(status));
    assert.equal(verificationCalls, 0, String(status));
  }
});

test("uses only documented success or challenge status forms", async () => {
  const cases = [
    { name: "ssa missing status and zero error", privilege: { ssaCode: "challenge", error_code: 0 }, verify: 1, source: true },
    { name: "numeric one", privilege: { status: 1, data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }, verify: 0, source: true },
    { name: "string one", privilege: { status: "1", data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }, verify: 0, source: true },
    { name: "absent status", privilege: { data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }, verify: 0, source: true },
  ];
  for (const entry of cases) {
    let privilegeCalls = 0;
    let verificationCalls = 0;
    const source = await resolveKugouSource({
      kugou: { music: {
        getSongPrivilegeLite: async () => {
          privilegeCalls += 1;
          return privilegeCalls === 1 ? entry.privilege : { data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] };
        },
        getSongUrl: async () => ({ url: "https://cdn.example/song.mp3" }),
      } },
      kugouVerification: { request: async () => { verificationCalls += 1; return { ok: true }; } },
    }, { hash: catalogHash }, "128");
    assert.equal(verificationCalls, entry.verify, entry.name);
    assert.equal(Boolean(source), entry.source, entry.name);
  }
});

test("discovers direct privilege hashes within data objects and arrays", () => {
  const hashes = {
    128: "11111111111111111111111111111111",
    320: "22222222222222222222222222222222",
    flac: "33333333333333333333333333333333",
    high: "44444444444444444444444444444444",
    super: "55555555555555555555555555555555",
  };
  for (const quality of ["128", "320", "flac", "high", "super"]) {
    const field = quality === "128" ? "hash" : `${quality}Hash`;
    const stale = "99999999999999999999999999999999";
    for (const payload of [{ data: { [field]: hashes[quality] } }, { data: [{ [field]: hashes[quality] }] }]) {
      assert.deepEqual(
        selectQualityHash({ hash: catalogHash, relateGoods: [{ quality, hash: stale }] }, payload, quality),
        { hash: hashes[quality], quality },
        `${quality}:${Array.isArray(payload.data) ? "array" : "object"}`,
      );
    }
  }
});

test("bounds candidate traversal for cyclic and wide privilege payloads", () => {
  const cyclicArray = [];
  const cyclicPayload = { data: cyclicArray };
  cyclicArray.push(cyclicPayload);
  assert.doesNotThrow(() => selectQualityHash({ hash: catalogHash }, cyclicPayload, "128"));

  let reads = 0;
  const wide = new Proxy(new Array(10_000), {
    get(target, property, receiver) {
      if (/^\d+$/.test(String(property))) reads += 1;
      return Reflect.get(target, property, receiver);
    },
  });
  assert.deepEqual(
    selectQualityHash({ hash: catalogHash }, { data: wide }, "128"),
    { hash: catalogHash, quality: "128" },
  );
  assert.ok(reads <= 64, `wide payload reads = ${reads}`);
});

test("rejects non-string challenge identifiers without verification", async () => {
  for (const [field, value] of [
    ["ssaCode", {}], ["ssaCode", []], ["ssaCode", false], ["ssaCode", 1],
    ["eventId", {}], ["eventId", []], ["eventId", false], ["eventId", 1],
  ]) {
    let verificationCalls = 0;
    let urlCalls = 0;
    const source = await resolveKugouSource({
      kugou: { music: {
        getSongPrivilegeLite: async () => ({ [field]: value, status: 0, data: [{ relate_goods: [{ quality: "128", hash: actualHash }] }] }),
        getSongUrl: async () => { urlCalls += 1; return { url: "https://cdn.example/song.mp3" }; },
      } },
      kugouVerification: { request: async () => { verificationCalls += 1; return { ok: true }; } },
    }, { hash: catalogHash }, "128");
    assert.equal(source, null, `${field}:${String(value)}`);
    assert.equal(verificationCalls, 0, `${field}:${String(value)}`);
    assert.equal(urlCalls, 0, `${field}:${String(value)}`);
  }
});
