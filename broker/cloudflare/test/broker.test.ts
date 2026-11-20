import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { DatabaseSync } from "node:sqlite";

import {
  ALLOWED_CLIENT_HEADERS,
  MAX_BODY_BYTES,
  applyTokenUpdateWithFp,
  clearTokenStateFromSql,
  computeTokenFingerprint,
  extractValidAccessToken,
  formatEtag,
  handleBrokerRequest,
  initTokenStateSchema,
  readTokenStateFromSql,
  verifyBearerSecret,
  writeTokenStateToSql,
  type BrokerStore,
  type SqlRow,
  type SqlStorageLike,
  type StoredTokenState,
} from "../src/core.ts";

const TEST_SECRET = "test-broker-secret-0123456789abcdef";
const TEST_TOKEN_1 = "synthetic-access-token-alpha-001";
const TEST_TOKEN_2 = "synthetic-access-token-beta-002";

function expectedFp(token: string): string {
  return createHash("sha256").update(token, "utf8").digest("hex").slice(0, 12);
}

function createMemoryStore(): BrokerStore {
  let state: StoredTokenState | null = null;
  return {
    getState: () => (state ? { ...state, client_headers: { ...state.client_headers } } : null),
    saveState: (next) => {
      state = { ...next, client_headers: { ...next.client_headers } };
    },
    clearState: () => {
      state = null;
    },
  };
}

function createNodeSqlStorage(db: DatabaseSync): SqlStorageLike {
  return {
    exec<T extends SqlRow = SqlRow>(query: string, ...bindings: unknown[]): { toArray(): T[] } {
      const trimmed = query.trim().toUpperCase();
      const stmt = db.prepare(query);
      const bound = bindings as Array<string | number | null | Uint8Array>;
      if (trimmed.startsWith("SELECT")) {
        const rows = stmt.all(...bound) as unknown as T[];
        return { toArray: () => rows };
      }
      stmt.run(...bound);
      return { toArray: () => [] };
    },
  };
}

/**
 * Mirrors TokenBrokerDO's exact SQLite read -> merge -> write execution path
 * against a real SQLite database via Node 22's built-in node:sqlite.
 */
function createSqliteDoStore(db: DatabaseSync): { store: BrokerStore; sql: SqlStorageLike } {
  const sql = createNodeSqlStorage(db);
  initTokenStateSchema(sql);

  const store: BrokerStore = {
    getState: () => readTokenStateFromSql(sql),
    saveState: (next) => writeTokenStateToSql(sql, next),
    clearState: () => clearTokenStateFromSql(sql),
    upsertJson: async (rawJson: string, nowMs: number) => {
      const rawPayload: unknown = JSON.parse(rawJson);
      const tokenCheck = extractValidAccessToken(rawPayload);
      if (!tokenCheck.ok) {
        return tokenCheck;
      }
      const tokenFp = await computeTokenFingerprint(tokenCheck.accessToken);
      const existing = readTokenStateFromSql(sql);
      const outcome = applyTokenUpdateWithFp(existing, rawPayload, tokenFp, nowMs);
      if (outcome.ok && outcome.response.updated) {
        writeTokenStateToSql(sql, outcome.state);
      }
      return outcome;
    },
  };
  return { store, sql };
}

function makeRequest(
  method: string,
  path: string,
  opts: { auth?: string; body?: string; headers?: Record<string, string> } = {}
): Request {
  const headers = new Headers(opts.headers);
  if (opts.auth !== undefined) {
    headers.set("Authorization", opts.auth);
  }
  if (opts.body !== undefined && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  return new Request(`https://broker.example.test${path}`, {
    method,
    headers,
    body: opts.body,
  });
}

test("GET /health succeeds without auth even when secret is unset; non-GET returns 405", async () => {
  const logs: Record<string, unknown>[] = [];
  const store = createMemoryStore();

  const res = await handleBrokerRequest(makeRequest("GET", "/health"), {
    secret: "",
    store,
    log: (e) => logs.push(e),
  });
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.deepEqual(await res.json(), {
    ok: true,
    service: "rayneo-token-broker",
    version: "1",
  });

  const postHealth = await handleBrokerRequest(makeRequest("POST", "/health"), {
    secret: "",
    store,
    log: (e) => logs.push(e),
  });
  assert.equal(postHealth.status, 405);
  assert.equal(postHealth.headers.get("Allow"), "GET");
});

test("fail-closed 503 when BROKER_SECRET is unset or empty", async () => {
  const store = createMemoryStore();
  for (const method of ["GET", "POST", "DELETE"]) {
    const res = await handleBrokerRequest(
      makeRequest(method, "/v1/token", {
        auth: "Bearer anything",
        body: method === "POST" ? JSON.stringify({ access_token: TEST_TOKEN_1 }) : undefined,
      }),
      { secret: "   ", store, log: () => {} }
    );
    assert.equal(res.status, 503);
    assert.equal(res.headers.get("Cache-Control"), "no-store");
    const json = (await res.json()) as { ok: boolean; error: string };
    assert.equal(json.ok, false);
  }
});

test("auth rejects invalid headers, supports case-insensitive Bearer, and invokes Workers timingSafeEqual when present", async () => {
  const logs: Record<string, unknown>[] = [];
  const store = createMemoryStore();

  const badAuths = [
    undefined,
    "",
    "Basic dXNlcjpwYXNz",
    "Bearer ",
    "Bearer short",
    `Bearer ${"x".repeat(TEST_SECRET.length)}`,
    `Bearer ${TEST_SECRET}-extra`,
    TEST_SECRET,
  ];
  for (const auth of badAuths) {
    const res = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      log: (e) => logs.push(e),
    });
    assert.equal(res.status, 401);
    assert.deepEqual(await res.json(), { ok: false, error: "unauthorized" });
  }

  // Case-insensitive Bearer succeeds auth and returns 404 on empty store.
  const okRes = await handleBrokerRequest(
    makeRequest("GET", "/v1/token", { auth: `bearer ${TEST_SECRET}` }),
    { secret: TEST_SECRET, store, log: (e) => logs.push(e) }
  );
  assert.equal(okRes.status, 404);
  assert.deepEqual(await okRes.json(), { ok: false, error: "no token synced yet" });

  // Verify Workers crypto.subtle.timingSafeEqual hook is called with equal-length 32-byte SHA-256 digests.
  let timingSafeCalls = 0;
  const mockSubtle = {
    digest: (alg: AlgorithmIdentifier, data: BufferSource) => crypto.subtle.digest(alg, data),
    timingSafeEqual: (a: ArrayBuffer | ArrayBufferView, b: ArrayBuffer | ArrayBufferView) => {
      timingSafeCalls++;
      const ua = a instanceof ArrayBuffer ? new Uint8Array(a) : new Uint8Array(a.buffer, a.byteOffset, a.byteLength);
      const ub = b instanceof ArrayBuffer ? new Uint8Array(b) : new Uint8Array(b.buffer, b.byteOffset, b.byteLength);
      assert.equal(ua.byteLength, 32);
      assert.equal(ub.byteLength, 32);
      return Buffer.compare(ua, ub) === 0;
    },
  } as unknown as SubtleCrypto & { timingSafeEqual: (a: ArrayBuffer, b: ArrayBuffer) => boolean };

  assert.equal(await verifyBearerSecret(`Bearer ${TEST_SECRET}`, TEST_SECRET, mockSubtle), "ok");
  assert.equal(await verifyBearerSecret("Bearer wrong", TEST_SECRET, mockSubtle), "unauthorized");
  assert.equal(timingSafeCalls, 2);

  // Secret must never appear in logs.
  assert.equal(JSON.stringify(logs).includes(TEST_SECRET), false);
});

test("POST /v1/token validation, 64KB body cap (413), unknown route (404), unsupported method (405)", async () => {
  const store = createMemoryStore();
  const auth = `Bearer ${TEST_SECRET}`;

  const badBodies = [
    "",
    "{broken",
    "[]",
    JSON.stringify({ source: "manual" }),
    JSON.stringify({ access_token: "   " }),
    JSON.stringify({ access_token: "a".repeat(513) }),
    JSON.stringify({ access_token: "line1\nline2" }),
    JSON.stringify({ access_token: "abc", captured_at: "not-a-date" }),
    JSON.stringify({ access_token: "abc", captured_at: "2026-02-30T12:00:00Z" }),
    JSON.stringify({ access_token: "abc", expires_in: "7200" }),
  ];

  for (const body of badBodies) {
    const res = await handleBrokerRequest(makeRequest("POST", "/v1/token", { auth, body }), {
      secret: TEST_SECRET,
      store,
      log: () => {},
    });
    assert.equal(res.status, 400, `expected 400 for body=${body.slice(0, 60)}`);
  }

  // Streamed body exceeding 64 KiB (even without Content-Length) returns 413.
  const bigChunk = new Uint8Array(MAX_BODY_BYTES + 128).fill(0x61);
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(bigChunk.subarray(0, 40000));
      controller.enqueue(bigChunk.subarray(40000));
      controller.close();
    },
  });
  const bigReq = new Request("https://broker.example.test/v1/token", {
    method: "POST",
    headers: { Authorization: auth, "Content-Type": "application/json" },
    body: stream,
    duplex: "half",
  } as RequestInit);
  const bigRes = await handleBrokerRequest(bigReq, { secret: TEST_SECRET, store, log: () => {} });
  assert.equal(bigRes.status, 413);

  const notFound = await handleBrokerRequest(makeRequest("GET", "/v1/other", { auth }), {
    secret: TEST_SECRET,
    store,
    log: () => {},
  });
  assert.equal(notFound.status, 404);

  const methodNotAllowed = await handleBrokerRequest(makeRequest("PUT", "/v1/token", { auth }), {
    secret: TEST_SECRET,
    store,
    log: () => {},
  });
  assert.equal(methodNotAllowed.status, 405);
  assert.equal(methodNotAllowed.headers.get("Allow"), "GET, POST, DELETE");
});

test("SQLite DO store: strips refresh_token/refreshToken, enforces exact 15-header allowlist, and keeps logs clean", async () => {
  const db = new DatabaseSync(":memory:");
  try {
    const { store } = createSqliteDoStore(db);
    const logs: Record<string, unknown>[] = [];
    const baseMs = Date.parse("2026-11-20T12:00:00Z");
    const auth = `Bearer ${TEST_SECRET}`;

    assert.equal(ALLOWED_CLIENT_HEADERS.size, 15);

    const payload = {
      access_token: `  ${TEST_TOKEN_1}  `,
      refresh_token: "SUPER-SECRET-REFRESH-1",
      refreshToken: "SUPER-SECRET-REFRESH-2",
      expires_in: 999999,
      captured_at: "2026-11-20T19:59:30+08:00",
      source: "loon:refresh",
      base_url: "https://xr-api-p.rayneo.cn/",
      region: "cn",
      account: "synthetic-user",
      account_id: 424242,
      device_id: "synthetic-device-01",
      client_headers: {
        "User-Agent": "RayNeoApp/3.0",
        platform: "ios",
        env: "prod",
        locale: "zh_CN",
        physicalRegion: "CN",
        "time-zone": "Asia/Shanghai",
        "app-version": "3.0.1",
        version: "301",
        "app-build-number": "1001",
        "phone-os-version": "18.1",
        "phone-model": "iPhone16,2",
        "device-id": "dev-1",
        deviceid: "dev-2",
        device_id: "dev-3",
        "device-code": "dev-code-1",
        "xtcl-access-token": "MUST-DROP",
        "xtcl-authorization": "MUST-DROP",
        Authorization: "Bearer MUST-DROP",
        Host: "xr-api-p.rayneo.cn",
        "Content-Type": "application/json",
        "Content-Length": "123",
        "Accept-Encoding": "gzip",
        Cookie: "sid=MUST-DROP",
        "x-arbitrary": "MUST-DROP",
      },
    };

    const postRes = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", { auth, body: JSON.stringify(payload) }),
      { secret: TEST_SECRET, store, now: () => baseMs, log: (e) => logs.push(e) }
    );
    assert.equal(postRes.status, 200);
    assert.equal(postRes.headers.get("Cache-Control"), "no-store");
    const postJson = (await postRes.json()) as Record<string, unknown>;
    assert.deepEqual(postJson, {
      ok: true,
      updated: true,
      revision: 1,
      token_fp: expectedFp(TEST_TOKEN_1),
      updated_at: "2026-11-20T12:00:00Z",
      expires_at: "2026-11-21T12:00:00Z", // capped to 86400s
      expires_estimated: false,
    });

    // Verify GET response
    const getRes = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => baseMs + 50_000,
      log: (e) => logs.push(e),
    });
    assert.equal(getRes.status, 200);
    assert.equal(getRes.headers.get("Cache-Control"), "no-store");
    assert.equal(getRes.headers.get("ETag"), formatEtag(1, expectedFp(TEST_TOKEN_1)));

    const rawGetText = await getRes.text();
    assert.equal(rawGetText.includes("refresh_token"), false);
    assert.equal(rawGetText.includes("refreshToken"), false);
    assert.equal(rawGetText.includes("SUPER-SECRET-REFRESH"), false);
    assert.equal(rawGetText.includes("MUST-DROP"), false);

    const getJson = JSON.parse(rawGetText) as Record<string, unknown>;
    assert.equal(getJson["access_token"], TEST_TOKEN_1);
    assert.equal(getJson["captured_at"], "2026-11-20T11:59:30Z");
    assert.equal(getJson["base_url"], "https://xr-api-p.rayneo.cn");
    assert.equal(getJson["expired"], false);
    assert.equal(getJson["remaining_seconds"], 86350);

    const headers = getJson["client_headers"] as Record<string, string>;
    assert.equal(Object.keys(headers).length, 15);
    assert.equal(headers["user-agent"], "RayNeoApp/3.0");
    assert.equal(headers["physicalregion"], "CN");

    // Inspect SQLite directly to verify zero refresh_token columns or leaked strings.
    const cols = db.prepare("PRAGMA table_info(token_state)").all() as Array<{ name: string }>;
    assert.equal(cols.some((c) => c.name.toLowerCase().includes("refresh")), false);
    const rawRows = JSON.stringify(db.prepare("SELECT * FROM token_state").all());
    assert.equal(rawRows.includes("SUPER-SECRET-REFRESH"), false);
    assert.equal(rawRows.includes("MUST-DROP"), false);

    // Inspect logs to ensure zero credential leaks.
    const logDump = JSON.stringify(logs);
    assert.equal(logDump.includes(TEST_TOKEN_1), false);
    assert.equal(logDump.includes(TEST_SECRET), false);
    assert.equal(logDump.includes("SUPER-SECRET-REFRESH"), false);
  } finally {
    db.close();
  }
});

test("metadata merge, duplicate-token TTL non-extension, expiry 200 behavior, ETag 304, and DELETE", async () => {
  const db = new DatabaseSync(":memory:");
  try {
    const { store } = createSqliteDoStore(db);
    const auth = `Bearer ${TEST_SECRET}`;
    let nowMs = Date.parse("2026-11-20T10:00:00Z");

    // 1) Initial loon:refresh POST with exact expires_in=7200
    const r1 = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({
          access_token: TEST_TOKEN_1,
          expires_in: 7200,
          source: "loon:refresh",
          base_url: "https://xr-api-p.rayneo.cn",
          region: "cn",
          account_id: 1001,
          client_headers: { "user-agent": "RayNeo/1.0" },
        }),
      }),
      { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
    );
    const p1 = (await r1.json()) as Record<string, unknown>;
    assert.equal(p1["updated"], true);
    assert.equal(p1["revision"], 1);
    assert.equal(p1["expires_at"], "2026-11-20T12:00:00Z");
    assert.equal(p1["expires_estimated"], false);

    // Advance 30 minutes
    nowMs += 30 * 60 * 1000;

    // 2) Duplicate token without expires_in or new metadata -> updated=false, rev=1, TTL NOT extended!
    const r2 = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({ access_token: TEST_TOKEN_1, source: "loon:header" }),
      }),
      { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
    );
    const p2 = (await r2.json()) as Record<string, unknown>;
    assert.equal(p2["updated"], false);
    assert.equal(p2["revision"], 1);
    assert.equal(p2["expires_at"], "2026-11-20T12:00:00Z");
    assert.equal(p2["expires_estimated"], false);

    // 3) Duplicate token enriching headers & device_id -> rev=2, headers merged, TTL NOT extended, source stays loon:refresh!
    const r3 = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({
          access_token: TEST_TOKEN_1,
          source: "loon:header",
          device_id: "dev-enriched",
          client_headers: { "phone-model": "iPhone16,2" },
        }),
      }),
      { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
    );
    const p3 = (await r3.json()) as Record<string, unknown>;
    assert.equal(p3["updated"], true);
    assert.equal(p3["revision"], 2);
    assert.equal(p3["expires_at"], "2026-11-20T12:00:00Z");
    assert.equal(p3["expires_estimated"], false);

    const g3 = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => nowMs,
      log: () => {},
    });
    const etag2 = g3.headers.get("ETag")!;
    assert.equal(etag2, formatEtag(2, expectedFp(TEST_TOKEN_1)));
    const g3Json = (await g3.json()) as Record<string, unknown>;
    assert.equal(g3Json["source"], "loon:refresh");
    assert.equal(g3Json["base_url"], "https://xr-api-p.rayneo.cn");
    assert.equal(g3Json["region"], "cn");
    assert.equal(g3Json["account_id"], 1001);
    assert.equal(g3Json["device_id"], "dev-enriched");
    assert.equal(g3Json["expired"], false);
    assert.equal(g3Json["remaining_seconds"], 5400);
    assert.deepEqual(g3Json["client_headers"], {
      "user-agent": "RayNeo/1.0",
      "phone-model": "iPhone16,2",
    });

    // 4) ETag If-None-Match variants -> 304 Not Modified
    for (const inm of [etag2, `W/${etag2}`, `"rev-1-000000000000", W/${etag2}`, "*"]) {
      const r304 = await handleBrokerRequest(
        makeRequest("GET", "/v1/token", { auth, headers: { "If-None-Match": inm } }),
        { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
      );
      assert.equal(r304.status, 304, `expected 304 for If-None-Match=${inm}`);
      assert.equal(r304.headers.get("ETag"), etag2);
      assert.equal(r304.headers.get("Cache-Control"), "no-store");
      assert.equal(await r304.text(), "");
    }

    // 5) Advance past expiry -> still returns 200 with expired:true, remaining_seconds:0
    nowMs = Date.parse("2026-11-20T12:05:00Z");
    const gExp = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => nowMs,
      log: () => {},
    });
    assert.equal(gExp.status, 200);
    const gExpJson = (await gExp.json()) as Record<string, unknown>;
    assert.equal(gExpJson["expired"], true);
    assert.equal(gExpJson["remaining_seconds"], 0);

    // 6) New token without expires_in -> estimated +7200s, preserves existing metadata
    const r4 = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({ access_token: TEST_TOKEN_2, source: "loon:header" }),
      }),
      { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
    );
    const p4 = (await r4.json()) as Record<string, unknown>;
    assert.equal(p4["updated"], true);
    assert.equal(p4["revision"], 3);
    assert.equal(p4["expires_estimated"], true);
    assert.equal(p4["expires_at"], "2026-11-20T14:05:00Z");

    const g4 = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => nowMs,
      log: () => {},
    });
    const g4Json = (await g4.json()) as Record<string, unknown>;
    assert.equal(g4Json["base_url"], "https://xr-api-p.rayneo.cn");
    assert.deepEqual(g4Json["client_headers"], {
      "user-agent": "RayNeo/1.0",
      "phone-model": "iPhone16,2",
    });

    // 7) DELETE wipes state -> GET returns 404
    const delRes = await handleBrokerRequest(makeRequest("DELETE", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => nowMs,
      log: () => {},
    });
    assert.equal(delRes.status, 200);
    assert.equal(delRes.headers.get("Cache-Control"), "no-store");
    assert.deepEqual(await delRes.json(), { ok: true, cleared: true });

    const gAfterDel = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => nowMs,
      log: () => {},
    });
    assert.equal(gAfterDel.status, 404);
  } finally {
    db.close();
  }
});
