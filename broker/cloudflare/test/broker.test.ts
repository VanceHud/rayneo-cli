import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { DatabaseSync } from "node:sqlite";

import {
  ALLOWED_CLIENT_HEADERS,
  EXPIRED_PURGE_GRACE_MS,
  MAX_BODY_BYTES,
  applySanitizedTokenUpdate,
  authorizeBrokerRequest,
  clearTokenStateFromSql,
  computePurgeAlarmMs,
  deriveStorageAesKey,
  formatEtag,
  handleBrokerRequest,
  initTokenStateSchema,
  isPastPurgeGrace,
  isStrongSecret,
  readEncryptedTokenStateFromSql,
  readTokenExpiryFromSql,
  verifyBearerSecret,
  writeEncryptedTokenStateToSql,
  writeTokenStateToSql,
  type BrokerStore,
  type RateLimiterLike,
  type SanitizedTokenUpsert,
  type SqlRow,
  type SqlStorageLike,
  type StoredTokenState,
} from "../src/core.ts";

const TEST_SECRET = "test-broker-secret-0123456789abcdef";
const TEST_READ_SECRET = "test-read-only-secret-0123456789abcd";
const TEST_WRITE_SECRET = "test-write-only-secret-0123456789abc";
const TEST_ENC_KEY = "test-dedicated-enc-key-0123456789abc";
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
 * Mirrors TokenBrokerDO's encrypted SQLite read -> merge -> encrypt -> write + alarm
 * lifecycle against a real SQLite database via Node 22's built-in node:sqlite.
 */
function createEncryptedSqliteDoStore(
  db: DatabaseSync,
  keyMaterial: string
): {
  store: BrokerStore;
  sql: SqlStorageLike;
  getAlarmAt: () => number | null;
  runAlarm: (nowMs: number) => Promise<void>;
  getLastSanitizedInput: () => SanitizedTokenUpsert | null;
  rotateKey: (newKeyMaterial: string) => void;
} {
  const sql = createNodeSqlStorage(db);
  initTokenStateSchema(sql);

  let currentKeyMaterial = keyMaterial;
  let alarmAt: number | null = null;
  let lastSanitized: SanitizedTokenUpsert | null = null;

  const getKey = () => deriveStorageAesKey(currentKeyMaterial);

  const store: BrokerStore = {
    getState: async (nowMs?: number) => {
      const key = await getKey();
      const state = await readEncryptedTokenStateFromSql(sql, key);
      if (!state) return null;
      if (nowMs !== undefined && isPastPurgeGrace(state.expires_at, nowMs)) {
        clearTokenStateFromSql(sql);
        alarmAt = null;
        return null;
      }
      return state;
    },
    saveState: async (next) => {
      const key = await getKey();
      await writeEncryptedTokenStateToSql(sql, next, key);
    },
    clearState: () => {
      clearTokenStateFromSql(sql);
      alarmAt = null;
    },
    upsertSanitized: async (input: SanitizedTokenUpsert, nowMs: number) => {
      lastSanitized = input;
      const key = await getKey();
      const existing = await readEncryptedTokenStateFromSql(sql, key);
      const activeExisting = existing && !isPastPurgeGrace(existing.expires_at, nowMs) ? existing : null;
      const outcome = applySanitizedTokenUpdate(activeExisting, input, nowMs);
      if (outcome.ok && outcome.response.updated) {
        await writeEncryptedTokenStateToSql(sql, outcome.state, key);
        alarmAt = computePurgeAlarmMs(outcome.state.expires_at, nowMs);
      }
      return outcome;
    },
  };

  return {
    store,
    sql,
    getAlarmAt: () => alarmAt,
    runAlarm: async (nowMs: number) => {
      const expiresAt = readTokenExpiryFromSql(sql);
      if (expiresAt === null) {
        alarmAt = null;
        return;
      }
      if (isPastPurgeGrace(expiresAt, nowMs)) {
        clearTokenStateFromSql(sql);
        alarmAt = null;
      } else {
        alarmAt = computePurgeAlarmMs(expiresAt, nowMs);
      }
    },
    getLastSanitizedInput: () => lastSanitized,
    rotateKey: (newKeyMaterial: string) => {
      currentKeyMaterial = newKeyMaterial;
    },
  };
}

function makeRequest(
  method: string,
  path: string,
  opts: {
    origin?: string;
    auth?: string;
    body?: string | Uint8Array;
    headers?: Record<string, string>;
    omitContentType?: boolean;
  } = {}
): Request {
  const headers = new Headers(opts.headers);
  if (opts.auth !== undefined) {
    headers.set("Authorization", opts.auth);
  }
  if (opts.body !== undefined && !opts.omitContentType && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const origin = opts.origin ?? "https://broker.example.test";
  return new Request(`${origin}${path}`, {
    method,
    headers,
    body: opts.body,
  });
}

test("GET /health succeeds without auth (minimal fingerprint) and includes security headers; non-GET returns 405", async () => {
  const store = createMemoryStore();

  const res = await handleBrokerRequest(makeRequest("GET", "/health"), {
    secret: "",
    store,
    log: () => {},
  });
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.equal(res.headers.get("X-Content-Type-Options"), "nosniff");
  assert.equal(res.headers.get("X-Frame-Options"), "DENY");
  assert.equal(res.headers.get("Referrer-Policy"), "no-referrer");
  assert.equal(res.headers.get("Strict-Transport-Security"), "max-age=31536000; includeSubDomains");
  assert.deepEqual(await res.json(), { ok: true });

  const postHealth = await handleBrokerRequest(makeRequest("POST", "/health"), {
    secret: "",
    store,
    log: () => {},
  });
  assert.equal(postHealth.status, 405);
  assert.equal(postHealth.headers.get("Allow"), "GET");
});

test("HTTPS enforcement: rejects non-loopback HTTP with 426; allows loopback HTTP for local dev", async () => {
  const store = createMemoryStore();

  const httpRes = await handleBrokerRequest(
    makeRequest("GET", "/v1/token", {
      origin: "http://broker.example.test",
      auth: `Bearer ${TEST_SECRET}`,
    }),
    { secret: TEST_SECRET, store, log: () => {} }
  );
  assert.equal(httpRes.status, 426);
  assert.equal(httpRes.headers.get("Strict-Transport-Security"), null);

  const localhostRes = await handleBrokerRequest(
    makeRequest("GET", "/v1/token", {
      origin: "http://localhost:8787",
      auth: `Bearer ${TEST_SECRET}`,
    }),
    { secret: TEST_SECRET, store, log: () => {} }
  );
  assert.equal(localhostRes.status, 404);
});

test("fail-closed 503 when secrets are unset, short, low-diversity, or known placeholders", async () => {
  const store = createMemoryStore();
  const weakSecrets = [
    undefined,
    "",
    "   ",
    "replace-with-your-random-secret",
    "REPLACE-WITH-YOUR-RANDOM-SECRET-12345",
    "short-secret-123",
    "a".repeat(64),
    "abababababababababababababababab",
    "secret-with\nnewline-0123456789abcdef",
  ];

  for (const s of weakSecrets) {
    assert.equal(isStrongSecret(s), false, `expected weak secret rejected: ${String(s)}`);
    const res = await handleBrokerRequest(
      makeRequest("GET", "/v1/token", { auth: "Bearer anything" }),
      { secret: s, store, log: () => {} }
    );
    assert.equal(res.status, 503);
    assert.deepEqual(await res.json(), { ok: false, error: "service unavailable" });
  }

  // Weak encryption key must also fail closed even if BROKER_SECRET is strong.
  const weakEncRes = await handleBrokerRequest(
    makeRequest("GET", "/v1/token", { auth: `Bearer ${TEST_SECRET}` }),
    { secrets: { shared: TEST_SECRET, encryptionKey: "too-short" }, store, log: () => {} }
  );
  assert.equal(weakEncRes.status, 503);
});

test("read/write secret separation enforces least privilege while remaining backward compatible with BROKER_SECRET", async () => {
  const store = createMemoryStore();
  const nowMs = Date.parse("2026-11-20T12:00:00Z");

  // Mode A: Pure split secrets (BROKER_READ_SECRET + BROKER_WRITE_SECRET, no BROKER_SECRET)
  const splitOpts = {
    secrets: { read: TEST_READ_SECRET, write: TEST_WRITE_SECRET },
    store,
    now: () => nowMs,
    log: () => {},
  };

  // Read secret cannot POST or DELETE -> 403 Forbidden
  const readPost = await handleBrokerRequest(
    makeRequest("POST", "/v1/token", {
      auth: `Bearer ${TEST_READ_SECRET}`,
      body: JSON.stringify({ access_token: TEST_TOKEN_1 }),
    }),
    splitOpts
  );
  assert.equal(readPost.status, 403);
  assert.deepEqual(await readPost.json(), { ok: false, error: "forbidden" });

  const readDel = await handleBrokerRequest(
    makeRequest("DELETE", "/v1/token", { auth: `Bearer ${TEST_READ_SECRET}` }),
    splitOpts
  );
  assert.equal(readDel.status, 403);

  // Write secret can POST, cannot GET -> 403 Forbidden
  const writePost = await handleBrokerRequest(
    makeRequest("POST", "/v1/token", {
      auth: `Bearer ${TEST_WRITE_SECRET}`,
      body: JSON.stringify({ access_token: TEST_TOKEN_1 }),
    }),
    splitOpts
  );
  assert.equal(writePost.status, 200);

  const writeGet = await handleBrokerRequest(
    makeRequest("GET", "/v1/token", { auth: `Bearer ${TEST_WRITE_SECRET}` }),
    splitOpts
  );
  assert.equal(writeGet.status, 403);

  // Read secret can GET -> 200 OK
  const readGet = await handleBrokerRequest(
    makeRequest("GET", "/v1/token", { auth: `Bearer ${TEST_READ_SECRET}` }),
    splitOpts
  );
  assert.equal(readGet.status, 200);
  const readJson = (await readGet.json()) as { access_token: string };
  assert.equal(readJson.access_token, TEST_TOKEN_1);

  // Write secret can DELETE -> 200 OK
  const writeDel = await handleBrokerRequest(
    makeRequest("DELETE", "/v1/token", { auth: `Bearer ${TEST_WRITE_SECRET}` }),
    splitOpts
  );
  assert.equal(writeDel.status, 200);

  // Mode B: Hybrid migration (legacy BROKER_SECRET + optional BROKER_READ_SECRET)
  const hybridOpts = {
    secrets: { shared: TEST_SECRET, read: TEST_READ_SECRET },
    store,
    now: () => nowMs,
    log: () => {},
  };
  assert.equal(
    (
      await handleBrokerRequest(
        makeRequest("POST", "/v1/token", {
          auth: `Bearer ${TEST_SECRET}`,
          body: JSON.stringify({ access_token: TEST_TOKEN_2 }),
        }),
        hybridOpts
      )
    ).status,
    200
  );
  assert.equal(
    (
      await handleBrokerRequest(
        makeRequest("GET", "/v1/token", { auth: `Bearer ${TEST_READ_SECRET}` }),
        hybridOpts
      )
    ).status,
    200
  );
  assert.equal(
    (
      await handleBrokerRequest(
        makeRequest("DELETE", "/v1/token", { auth: `Bearer ${TEST_READ_SECRET}` }),
        hybridOpts
      )
    ).status,
    403
  );
  assert.equal(
    (
      await handleBrokerRequest(
        makeRequest("DELETE", "/v1/token", { auth: `Bearer ${TEST_SECRET}` }),
        hybridOpts
      )
    ).status,
    200
  );
});

test("auth rejects invalid headers, enforces rate limiting before auth, and invokes timingSafeEqual", async () => {
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

  // Rate limiter returns 429 before touching store/auth.
  const blockedLimiter: RateLimiterLike = { limit: async () => ({ success: false }) };
  const rlRes = await handleBrokerRequest(
    makeRequest("GET", "/v1/token", {
      auth: `Bearer ${TEST_SECRET}`,
      headers: { "CF-Connecting-IP": "203.0.113.5" },
    }),
    { secret: TEST_SECRET, store, rateLimiter: blockedLimiter, log: () => {} }
  );
  assert.equal(rlRes.status, 429);
  assert.equal(rlRes.headers.get("Retry-After"), "60");

  // Verify Workers crypto.subtle.timingSafeEqual hook is called with 32-byte SHA-256 digests.
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
  const authRes = await authorizeBrokerRequest(
    `Bearer ${TEST_READ_SECRET}`,
    { shared: TEST_SECRET, read: TEST_READ_SECRET, write: TEST_WRITE_SECRET },
    mockSubtle
  );
  assert.equal(authRes.status, "ok");
  assert.equal(timingSafeCalls, 5); // 1 + 1 + 3 (evaluates all configured slots without short-circuit)

  // Secret must never appear in logs.
  assert.equal(JSON.stringify(logs).includes(TEST_SECRET), false);
});

test("POST /v1/token strict input validation: base_url allowlist, query rejection, Content-Type, UTF-8, C1 controls, body cap", async () => {
  const store = createMemoryStore();
  const auth = `Bearer ${TEST_SECRET}`;
  const nowMs = Date.parse("2026-11-20T12:00:00Z");

  // Unexpected query parameters on /v1/token -> 400
  const queryRes = await handleBrokerRequest(makeRequest("GET", "/v1/token?secret=oops", { auth }), {
    secret: TEST_SECRET,
    store,
    now: () => nowMs,
    log: () => {},
  });
  assert.equal(queryRes.status, 400);

  // Wrong Content-Type -> 415
  const ctRes = await handleBrokerRequest(
    makeRequest("POST", "/v1/token", {
      auth,
      body: JSON.stringify({ access_token: TEST_TOKEN_1 }),
      headers: { "Content-Type": "text/plain" },
    }),
    { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
  );
  assert.equal(ctRes.status, 415);

  // Missing Content-Type with JSON body -> 415
  const noCtRes = await handleBrokerRequest(
    makeRequest("POST", "/v1/token", {
      auth,
      body: JSON.stringify({ access_token: TEST_TOKEN_1 }),
      omitContentType: true,
    }),
    { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
  );
  assert.equal(noCtRes.status, 415);

  // Invalid UTF-8 bytes -> 400
  const invalidUtf8 = new Uint8Array([0x7b, 0xff, 0xfe, 0x7d]);
  const utf8Res = await handleBrokerRequest(
    makeRequest("POST", "/v1/token", { auth, body: invalidUtf8 }),
    { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
  );
  assert.equal(utf8Res.status, 400);

  const badBodies = [
    "",
    "{broken",
    "[]",
    JSON.stringify({ source: "manual" }),
    JSON.stringify({ access_token: "   " }),
    JSON.stringify({ access_token: "a".repeat(513) }),
    JSON.stringify({ access_token: "line1\nline2" }),
    JSON.stringify({ access_token: "has space inside" }),
    JSON.stringify({ access_token: "has\u009bc1" }),
    JSON.stringify({ access_token: "abc", captured_at: "not-a-date" }),
    JSON.stringify({ access_token: "abc", captured_at: "2026-02-30T12:00:00Z" }),
    JSON.stringify({ access_token: "abc", captured_at: "9999-12-31T23:59:59-23:59" }),
    JSON.stringify({ access_token: "abc", captured_at: "1999-12-31T23:59:59Z" }),
    JSON.stringify({ access_token: "abc", captured_at: "2027-01-01T00:00:00Z" }),
    JSON.stringify({ access_token: "abc", expires_in: "7200" }),
    // Untrusted / poisoned base_url values MUST be rejected with 400
    JSON.stringify({ access_token: "abc", base_url: "http://xr-api-p.rayneo.cn" }),
    JSON.stringify({ access_token: "abc", base_url: "https://evil.example.com" }),
    JSON.stringify({ access_token: "abc", base_url: "http://127.0.0.1:8080" }),
    JSON.stringify({ access_token: "abc", base_url: "https://xr-api-p.rayneo.cn:8443" }),
    JSON.stringify({ access_token: "abc", base_url: "https://xr-api-p.rayneo.cn/extra" }),
    JSON.stringify({ access_token: "abc", base_url: "https://xr-api-p.rayneo.cn?x=1" }),
    JSON.stringify({ access_token: "abc", base_url: "https://user:pass@xr-api-p.rayneo.cn" }),
    JSON.stringify({ access_token: "abc", base_url: "https://xr-api-p.rayneo.cn.evil.com" }),
  ];

  for (const body of badBodies) {
    const res = await handleBrokerRequest(makeRequest("POST", "/v1/token", { auth, body }), {
      secret: TEST_SECRET,
      store,
      now: () => nowMs,
      log: () => {},
    });
    assert.equal(res.status, 400, `expected 400 for body=${body.slice(0, 80)}`);
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
  const bigRes = await handleBrokerRequest(bigReq, { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} });
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

test("Encrypted SQLite DO store: encrypts secrets at rest (AES-256-GCM), strips refresh_token before DO RPC, supports legacy plaintext migration and graceful key rotation", async () => {
  const db = new DatabaseSync(":memory:");
  try {
    const harness = createEncryptedSqliteDoStore(db, TEST_ENC_KEY);
    const { store, sql } = harness;
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
      base_url: "https://XR-API-P.RAYNEO.CN/",
      region: "cn",
      account: "synthetic-phone-13800001111",
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
        "x-c1-header": "bad\u009bval",
      },
    };

    const postRes = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", { auth, body: JSON.stringify(payload) }),
      { secret: TEST_SECRET, store, now: () => baseMs, log: (e) => logs.push(e) }
    );
    assert.equal(postRes.status, 200);
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

    // Verify alarm was scheduled for expires_at + 24h
    assert.equal(harness.getAlarmAt(), Date.parse("2026-11-21T12:00:00Z") + EXPIRED_PURGE_GRACE_MS);

    // Verify sanitized DTO crossing DO RPC contains ZERO refresh_token or unallowlisted fields.
    const rpcInputStr = JSON.stringify(harness.getLastSanitizedInput());
    assert.equal(rpcInputStr.includes("SUPER-SECRET-REFRESH"), false);
    assert.equal(rpcInputStr.includes("refresh_token"), false);
    assert.equal(rpcInputStr.includes("MUST-DROP"), false);

    // Inspect raw SQLite storage: sensitive columns MUST be encrypted (enc:v1:) with ZERO plaintext!
    const rawRow = db.prepare("SELECT * FROM token_state WHERE id = 1").get() as Record<string, unknown>;
    assert.equal(String(rawRow["access_token"]).startsWith("enc:v1:"), true);
    assert.equal(String(rawRow["account"]).startsWith("enc:v1:"), true);
    assert.equal(String(rawRow["device_id"]).startsWith("enc:v1:"), true);
    assert.equal(String(rawRow["client_headers_json"]).startsWith("enc:v1:"), true);

    const rawDump = JSON.stringify(rawRow);
    assert.equal(rawDump.includes(TEST_TOKEN_1), false);
    assert.equal(rawDump.includes("synthetic-phone-13800001111"), false);
    assert.equal(rawDump.includes("synthetic-device-01"), false);
    assert.equal(rawDump.includes("RayNeoApp/3.0"), false);
    assert.equal(rawDump.includes("SUPER-SECRET-REFRESH"), false);
    assert.equal(rawDump.includes("MUST-DROP"), false);

    // Verify GET decrypts and returns normalized values
    const getRes = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => baseMs + 50_000,
      log: (e) => logs.push(e),
    });
    assert.equal(getRes.status, 200);
    assert.equal(getRes.headers.get("ETag"), formatEtag(1, expectedFp(TEST_TOKEN_1)));
    const getJson = (await getRes.json()) as Record<string, unknown>;
    assert.equal(getJson["access_token"], TEST_TOKEN_1);
    assert.equal(getJson["base_url"], "https://xr-api-p.rayneo.cn");
    assert.equal(getJson["account"], "synthetic-phone-13800001111");
    assert.equal(getJson["device_id"], "synthetic-device-01");
    assert.equal(Object.keys(getJson["client_headers"] as Record<string, string>).length, 15);

    // Verify column-swap tampering fails GCM AAD verification gracefully (-> 404, never 500)
    db.prepare("UPDATE token_state SET access_token = account WHERE id = 1").run();
    const tamperedGet = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => baseMs + 60_000,
      log: () => {},
    });
    assert.equal(tamperedGet.status, 404);

    // Verify secret rotation degrades gracefully to 404 and allows fresh POST overwrite
    await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({ access_token: TEST_TOKEN_1, expires_in: 3600 }),
      }),
      { secret: TEST_SECRET, store, now: () => baseMs, log: () => {} }
    );
    harness.rotateKey("rotated-new-encryption-key-9876543210");
    const afterRotateGet = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => baseMs + 1000,
      log: () => {},
    });
    assert.equal(afterRotateGet.status, 404);

    const afterRotatePost = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({ access_token: TEST_TOKEN_2, expires_in: 3600 }),
      }),
      { secret: TEST_SECRET, store, now: () => baseMs + 2000, log: () => {} }
    );
    assert.equal(afterRotatePost.status, 200);

    // Verify legacy plaintext SQLite row is readable seamlessly (zero-downtime upgrade)
    writeTokenStateToSql(sql, {
      revision: 9,
      access_token: TEST_TOKEN_1,
      token_fp: expectedFp(TEST_TOKEN_1),
      source: "loon:refresh",
      captured_at: "2026-11-20T12:00:00Z",
      updated_at: "2026-11-20T12:00:00Z",
      expires_at: "2026-11-20T14:00:00Z",
      expires_estimated: false,
      base_url: "https://xr-api-p.rayneo.cn",
      region: "cn",
      account: "legacy-acct",
      account_id: 99,
      device_id: "legacy-dev",
      client_headers: { platform: "ios" },
    });
    const legacyGet = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => baseMs + 10_000,
      log: () => {},
    });
    assert.equal(legacyGet.status, 200);
    assert.equal(((await legacyGet.json()) as { access_token: string }).access_token, TEST_TOKEN_1);

    // Inspect logs to ensure zero credential leaks.
    const logDump = JSON.stringify(logs);
    assert.equal(logDump.includes(TEST_TOKEN_1), false);
    assert.equal(logDump.includes(TEST_SECRET), false);
    assert.equal(logDump.includes("SUPER-SECRET-REFRESH"), false);
  } finally {
    db.close();
  }
});

test("metadata merge, duplicate-token TTL non-extension, anti-downgrade, account switch reset, ETag 304, 24h auto-purge alarm, and DELETE", async () => {
  const db = new DatabaseSync(":memory:");
  try {
    const harness = createEncryptedSqliteDoStore(db, TEST_SECRET);
    const { store } = harness;
    const auth = `Bearer ${TEST_SECRET}`;
    let nowMs = Date.parse("2026-11-20T10:00:00Z");

    // 1) Initial loon:refresh POST with exact expires_in=7200
    const r1 = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({
          access_token: TEST_TOKEN_1,
          expires_in: 7200,
          captured_at: "2026-11-20T10:00:00Z",
          source: "loon:refresh",
          base_url: "https://xr-api-p.rayneo.cn",
          region: "cn",
          account: "acct-one",
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

    // 1b) Anti-downgrade: stale in-flight loon:header with older captured_at MUST NOT overwrite newer token!
    const staleRes = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({
          access_token: "stale-old-token-from-before-refresh",
          captured_at: "2026-11-20T09:59:55Z",
          source: "loon:header",
        }),
      }),
      { secret: TEST_SECRET, store, now: () => nowMs + 1000, log: () => {} }
    );
    assert.equal(staleRes.status, 200);
    const staleJson = (await staleRes.json()) as Record<string, unknown>;
    assert.equal(staleJson["updated"], false);
    assert.equal(staleJson["revision"], 1);
    assert.equal(staleJson["token_fp"], expectedFp(TEST_TOKEN_1));

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

    // 3) Duplicate token enriching headers & device_id -> rev=2, headers merged, TTL NOT extended!
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

    const g3 = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => nowMs,
      log: () => {},
    });
    const etag2 = g3.headers.get("ETag")!;
    assert.equal(etag2, formatEtag(2, expectedFp(TEST_TOKEN_1)));
    const g3Json = (await g3.json()) as Record<string, unknown>;
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
      assert.equal(r304.status, 304);
      assert.equal(r304.headers.get("ETag"), etag2);
      assert.equal(r304.headers.get("Strict-Transport-Security"), "max-age=31536000; includeSubDomains");
    }

    // 5) Cross-account switch (account_id 1001 -> 2002) clears old account, device_id & client_headers!
    const switchRes = await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({
          access_token: TEST_TOKEN_2,
          source: "loon:login",
          expires_in: 3600,
          account_id: 2002,
          client_headers: { platform: "android" },
        }),
      }),
      { secret: TEST_SECRET, store, now: () => nowMs, log: () => {} }
    );
    assert.equal(switchRes.status, 200);
    const gSwitch = (await (
      await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
        secret: TEST_SECRET,
        store,
        now: () => nowMs,
        log: () => {},
      })
    ).json()) as Record<string, unknown>;
    assert.equal(gSwitch["account_id"], 2002);
    assert.equal(gSwitch["account"], "");
    assert.equal(gSwitch["device_id"], "");
    assert.deepEqual(gSwitch["client_headers"], { platform: "android" });

    // 6) Advance 5 min past expiry -> still returns 200 with expired:true (within 24h grace window)
    nowMs = Date.parse("2026-11-20T11:35:00Z");
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

    // 7) Advance past 24h grace window and fire DO alarm -> automatically purged!
    const afterGraceMs = Date.parse("2026-11-20T11:30:00Z") + EXPIRED_PURGE_GRACE_MS + 1000;
    await harness.runAlarm(afterGraceMs);
    assert.equal(harness.getAlarmAt(), null);
    assert.equal(db.prepare("SELECT COUNT(*) AS c FROM token_state").get()?.["c"], 0);

    const gAfterAlarm = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => afterGraceMs,
      log: () => {},
    });
    assert.equal(gAfterAlarm.status, 404);

    // 8) Re-seed and verify DELETE wipes state & cancels alarm
    await handleBrokerRequest(
      makeRequest("POST", "/v1/token", {
        auth,
        body: JSON.stringify({ access_token: TEST_TOKEN_1, source: "manual" }),
      }),
      { secret: TEST_SECRET, store, now: () => afterGraceMs, log: () => {} }
    );
    assert.notEqual(harness.getAlarmAt(), null);

    const delRes = await handleBrokerRequest(makeRequest("DELETE", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => afterGraceMs,
      log: () => {},
    });
    assert.equal(delRes.status, 200);
    assert.equal(harness.getAlarmAt(), null);

    const gAfterDel = await handleBrokerRequest(makeRequest("GET", "/v1/token", { auth }), {
      secret: TEST_SECRET,
      store,
      now: () => afterGraceMs,
      log: () => {},
    });
    assert.equal(gAfterDel.status, 404);
  } finally {
    db.close();
  }
});
