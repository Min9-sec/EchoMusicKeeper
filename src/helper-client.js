class HelperClientError extends Error {
  constructor(message, { code = "helper_error", status = 0 } = {}) {
    super(message);
    this.name = "HelperClientError";
    this.code = code;
    this.status = status;
  }
}

const jsonHeaders = Object.freeze({ "Content-Type": "application/json" });

function endpoint(baseUrl, path) {
  return `${String(baseUrl).replace(/\/+$/, "")}${path}`;
}

async function responseBody(response) {
  if (response.status === 204) return undefined;
  const text = await response.text();
  if (!text) return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** A small authenticated client for the local cache-helper control protocol. */
export function createHelperClient({ baseUrl, token, fetchImpl = globalThis.fetch }) {
  if (typeof fetchImpl !== "function") throw new TypeError("fetchImpl must be a function");

  async function request(path, options = {}) {
    const method = options.method ?? "GET";
    const signal = options.signal ?? AbortSignal.timeout(options.timeoutMs ?? 10_000);
    const headers = { Authorization: `Bearer ${token}` };
    let body;
    if (options.json !== undefined) {
      Object.assign(headers, jsonHeaders);
      body = JSON.stringify(options.json);
    }
    const response = await fetchImpl(endpoint(baseUrl, path), { method, headers, body, signal });
    const payload = await responseBody(response);
    if (!response.ok) {
      const structured = payload && typeof payload === "object" ? payload : {};
      throw new HelperClientError(
        structured.message || (typeof payload === "string" && payload) || `helper request failed (${response.status})`,
        { code: structured.code, status: response.status },
      );
    }
    return payload;
  }

  const taskPath = (id, action) => `/v1/tasks/${encodeURIComponent(id)}/${action}`;
  return Object.freeze({
    health: (options) => request("/v1/health", options),
    updateConfig: (config, options) => request("/v1/config", { ...options, method: "PUT", json: config }),
    cacheList: (options) => request("/v1/cache", options),
    cacheLookup: (lookup, options) => request("/v1/cache/lookup", { ...options, method: "POST", json: lookup }),
    deleteCache: (key, options) => request(`/v1/cache/${encodeURIComponent(key)}`, { ...options, method: "DELETE" }),
    clearCache: (options) => request("/v1/cache/clear", { ...options, method: "POST" }),
    migrateCache: (migration, options) => request("/v1/cache/migrate", { ...options, method: "POST", json: migration }),
    createSession: (session, options) => request("/v1/proxy/sessions", { ...options, method: "POST", json: session }),
    createDownload: (download, options) => request("/v1/downloads", { ...options, method: "POST", json: download }),
    deleteDownload: (path, options) => request("/v1/downloads", { ...options, method: "DELETE", json: { path } }),
    listTasks: (options) => request("/v1/tasks", options),
    pauseTask: (id, options) => request(taskPath(id, "pause"), { ...options, method: "POST" }),
    resumeTask: (id, options) => request(taskPath(id, "resume"), { ...options, method: "POST" }),
    cancelTask: (id, options) => request(taskPath(id, "cancel"), { ...options, method: "POST" }),
    refreshTaskUrl: (id, remoteUrl, options) => request(taskPath(id, "url"), { ...options, method: "PUT", json: { remoteUrl } }),
    revealFile: (target, options) => request("/v1/files/reveal", { ...options, method: "POST", json: target }),
    shutdown: (options) => request("/v1/shutdown", { ...options, method: "POST" }),
  });
}

export { HelperClientError };
