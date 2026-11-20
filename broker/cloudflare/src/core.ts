export const SERVICE_NAME = "rayneo-token-broker";
export const SERVICE_VERSION = "1";

export const MAX_BODY_BYTES = 64 * 1024;
export const MAX_TOKEN_LEN = 512;
export const MAX_SOURCE_LEN = 64;
export const MAX_HEADER_VAL_LEN = 1024;
export const DEFAULT_TTL_SECONDS = 7200;
export const MAX_TTL_SECONDS = 86400;

/**
 * Exact 15-header allowlist mirroring internal/har/har.go.
 * Never store xtcl-*, authorization, host, content-*, accept-encoding, or cookie.
 */
export const ALLOWED_CLIENT_HEADERS: ReadonlySet<string> = new Set([
  "user-agent",
  "platform",
  "env",
  "locale",
  "physicalregion",
  "time-zone",
  "app-version",
  "version",
  "app-build-number",
  "phone-os-version",
  "phone-model",
  "device-id",
  "deviceid",
  "device_id",
  "device-code",
]);

/**
 * Single-slot access-token state. Refresh tokens are intentionally absent
 * and must never be accepted, stored, logged, or returned.
 */
export type StoredTokenState = {
  revision: number;
  access_token: string;
  token_fp: string;
  source: string;
  captured_at: string;
  updated_at: string;
  expires_at: string;
  expires_estimated: boolean;
  base_url: string;
  region: string;
  account: string;
  account_id: number;
  device_id: string;
  client_headers: Record<string, string>;
};

export type PostTokenResult = {
  ok: true;
  updated: boolean;
  revision: number;
  token_fp: string;
  updated_at: string;
  expires_at: string;
  expires_estimated: boolean;
};

export type GetTokenResponse = {
  ok: true;
  revision: number;
  access_token: string;
  token_fp: string;
  source: string;
  captured_at: string;
  updated_at: string;
  expires_at: string;
  expires_estimated: boolean;
  expired: boolean;
  remaining_seconds: number;
  base_url: string;
  region: string;
  account: string;
  account_id: number;
  device_id: string;
  client_headers: Record<string, string>;
};

export type UpsertTokenOutcome =
  | { ok: true; state: StoredTokenState; response: PostTokenResult }
  | { ok: false; status: number; error: string };

export interface BrokerStore {
  getState(): StoredTokenState | null | Promise<StoredTokenState | null>;
  saveState(state: StoredTokenState): void | Promise<void>;
  clearState(): void | Promise<void>;
  /**
   * Optional atomic read-modify-write hook accepting the bounded raw JSON string
   * (RPC-safe primitive string for TokenBrokerDO so read + merge + SQLite write
   * executes in a single DO turn without races).
   */
  upsertJson?(rawJson: string, nowMs: number): Promise<UpsertTokenOutcome>;
}

export type SqlValue = string | number | null | ArrayBuffer;
export type SqlRow = Record<string, SqlValue>;

export interface SqlStorageLike {
  exec<T extends SqlRow = SqlRow>(query: string, ...bindings: unknown[]): { toArray(): T[] };
}

type TokenStateRow = {
  revision: number;
  access_token: string;
  token_fp: string;
  source: string;
  captured_at: string;
  updated_at: string;
  expires_at: string;
  expires_estimated: number;
  base_url: string;
  region: string;
  account: string;
  account_id: number;
  device_id: string;
  client_headers_json: string;
};

export const TOKEN_STATE_SCHEMA_SQL = `
CREATE TABLE IF NOT EXISTS token_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  revision INTEGER NOT NULL,
  access_token TEXT NOT NULL,
  token_fp TEXT NOT NULL,
  source TEXT NOT NULL,
  captured_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  expires_estimated INTEGER NOT NULL,
  base_url TEXT NOT NULL,
  region TEXT NOT NULL,
  account TEXT NOT NULL,
  account_id INTEGER NOT NULL,
  device_id TEXT NOT NULL,
  client_headers_json TEXT NOT NULL
)`;

export function initTokenStateSchema(sql: SqlStorageLike): void {
  sql.exec(TOKEN_STATE_SCHEMA_SQL);
}

export function readTokenStateFromSql(sql: SqlStorageLike): StoredTokenState | null {
  const rows = sql
    .exec<TokenStateRow>(
      `SELECT revision, access_token, token_fp, source, captured_at, updated_at,
              expires_at, expires_estimated, base_url, region, account,
              account_id, device_id, client_headers_json
         FROM token_state
        WHERE id = 1`
    )
    .toArray();
  const row = rows[0];
  if (!row || !row.access_token) {
    return null;
  }
  let parsedHeaders: unknown = {};
  try {
    parsedHeaders = JSON.parse(row.client_headers_json);
  } catch {
    parsedHeaders = {};
  }
  return {
    revision: Number(row.revision),
    access_token: String(row.access_token),
    token_fp: String(row.token_fp),
    source: String(row.source),
    captured_at: String(row.captured_at),
    updated_at: String(row.updated_at),
    expires_at: String(row.expires_at),
    expires_estimated: Number(row.expires_estimated) !== 0,
    base_url: String(row.base_url),
    region: String(row.region),
    account: String(row.account),
    account_id: Number(row.account_id),
    device_id: String(row.device_id),
    client_headers: filterClientHeaders(parsedHeaders),
  };
}

export function writeTokenStateToSql(sql: SqlStorageLike, state: StoredTokenState): void {
  sql.exec(
    `INSERT OR REPLACE INTO token_state (
       id, revision, access_token, token_fp, source, captured_at, updated_at,
       expires_at, expires_estimated, base_url, region, account,
       account_id, device_id, client_headers_json
     ) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    state.revision,
    state.access_token,
    state.token_fp,
    state.source,
    state.captured_at,
    state.updated_at,
    state.expires_at,
    state.expires_estimated ? 1 : 0,
    state.base_url,
    state.region,
    state.account,
    state.account_id,
    state.device_id,
    JSON.stringify(filterClientHeaders(state.client_headers))
  );
}

export function clearTokenStateFromSql(sql: SqlStorageLike): void {
  sql.exec("DELETE FROM token_state WHERE id = 1");
}

const textEncoder = new TextEncoder();

export async function sha256Digest(input: string): Promise<ArrayBuffer> {
  return crypto.subtle.digest("SHA-256", textEncoder.encode(input));
}

export async function computeTokenFingerprint(accessToken: string): Promise<string> {
  const buf = await sha256Digest(accessToken);
  const bytes = new Uint8Array(buf);
  let hex = "";
  for (let i = 0; i < 6; i++) {
    hex += bytes[i]!.toString(16).padStart(2, "0");
  }
  return hex;
}

type SubtleWithTimingSafe = SubtleCrypto & {
  timingSafeEqual?: (a: ArrayBuffer | ArrayBufferView, b: ArrayBuffer | ArrayBufferView) => boolean;
};

export function constantTimeEqualBytes(
  aBuf: ArrayBuffer,
  bBuf: ArrayBuffer,
  subtle: SubtleWithTimingSafe = crypto.subtle as SubtleWithTimingSafe
): boolean {
  if (typeof subtle.timingSafeEqual === "function") {
    return subtle.timingSafeEqual(aBuf, bBuf);
  }
  const a = new Uint8Array(aBuf);
  const b = new Uint8Array(bBuf);
  if (a.byteLength !== b.byteLength) {
    return false;
  }
  let diff = 0;
  for (let i = 0; i < a.byteLength; i++) {
    diff |= a[i]! ^ b[i]!;
  }
  return diff === 0;
}

export type AuthCheckResult = "ok" | "unconfigured" | "unauthorized";

export async function verifyBearerSecret(
  authHeader: string | null,
  expectedSecret: string | undefined,
  subtle: SubtleWithTimingSafe = crypto.subtle as SubtleWithTimingSafe
): Promise<AuthCheckResult> {
  const expected = typeof expectedSecret === "string" ? expectedSecret.trim() : "";
  if (expected.length === 0) {
    return "unconfigured";
  }
  if (!authHeader) {
    return "unauthorized";
  }
  const trimmed = authHeader.trim();
  const spaceIdx = trimmed.indexOf(" ");
  if (spaceIdx <= 0) {
    return "unauthorized";
  }
  const scheme = trimmed.slice(0, spaceIdx);
  if (scheme.toLowerCase() !== "bearer") {
    return "unauthorized";
  }
  const provided = trimmed.slice(spaceIdx + 1).trim();
  if (provided.length === 0) {
    return "unauthorized";
  }
  // Hash both values with SHA-256 so differing lengths never short-circuit or leak via timing.
  const [providedHash, expectedHash] = await Promise.all([
    subtle.digest("SHA-256", textEncoder.encode(provided)),
    subtle.digest("SHA-256", textEncoder.encode(expected)),
  ]);
  return constantTimeEqualBytes(providedHash, expectedHash, subtle) ? "ok" : "unauthorized";
}

export function formatEtag(revision: number, tokenFp: string): string {
  return `"rev-${revision}-${tokenFp}"`;
}

export function etagMatches(ifNoneMatch: string | null, currentEtag: string): boolean {
  if (!ifNoneMatch) {
    return false;
  }
  const hdr = ifNoneMatch.trim();
  if (hdr === "") {
    return false;
  }
  if (hdr === "*") {
    return true;
  }
  for (const rawPart of hdr.split(",")) {
    let tag = rawPart.trim();
    if (tag === "") {
      continue;
    }
    if (tag === "*") {
      return true;
    }
    if (tag.startsWith("W/") || tag.startsWith("w/")) {
      tag = tag.slice(2).trim();
    }
    if (tag === currentEtag) {
      return true;
    }
  }
  return false;
}

export function hasControlChars(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const code = s.charCodeAt(i);
    if (code < 0x20 || code === 0x7f) {
      return true;
    }
  }
  return false;
}

export function filterClientHeaders(raw: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    return out;
  }
  for (const [rawKey, rawVal] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof rawVal !== "string") {
      continue;
    }
    const k = rawKey.trim().toLowerCase();
    if (!ALLOWED_CLIENT_HEADERS.has(k)) {
      continue;
    }
    const v = rawVal.trim();
    if (v.length === 0 || v.length > MAX_HEADER_VAL_LEN || hasControlChars(v)) {
      continue;
    }
    out[k] = v;
  }
  return out;
}

const RFC3339_RE =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|z|([+-])(\d{2}):(\d{2}))$/;

function daysInMonth(year: number, month: number): number {
  return new Date(Date.UTC(year, month, 0)).getUTCDate();
}

export function parseRfc3339Ms(raw: string): number | null {
  const m = RFC3339_RE.exec(raw);
  if (!m) {
    return null;
  }
  const year = Number(m[1]);
  const month = Number(m[2]);
  const day = Number(m[3]);
  const hour = Number(m[4]);
  const minute = Number(m[5]);
  const second = Number(m[6]);
  if (
    month < 1 ||
    month > 12 ||
    day < 1 ||
    day > daysInMonth(year, month) ||
    hour > 23 ||
    minute > 59 ||
    second > 59
  ) {
    return null;
  }
  if (m[8] && m[9] && m[10]) {
    const offH = Number(m[9]);
    const offM = Number(m[10]);
    if (offH > 23 || offM > 59) {
      return null;
    }
  }
  const ms = Date.parse(raw);
  return Number.isFinite(ms) ? ms : null;
}

export function formatUtcSeconds(ms: number): string {
  const secMs = Math.floor(ms / 1000) * 1000;
  return new Date(secMs).toISOString().replace(/\.\d{3}Z$/, "Z");
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function sanitizeShortString(v: string, maxLen: number): string {
  const trimmed = v.trim();
  if (trimmed.length === 0 || hasControlChars(trimmed)) {
    return "";
  }
  return trimmed.length > maxLen ? trimmed.slice(0, maxLen) : trimmed;
}

function isAuthoritativeSource(src: string): boolean {
  return src === "loon:refresh" || src === "loon:login";
}

export function extractValidAccessToken(
  rawPayload: unknown
): { ok: true; accessToken: string } | { ok: false; status: number; error: string } {
  if (!isPlainObject(rawPayload)) {
    return { ok: false, status: 400, error: "invalid json body" };
  }
  const rawToken = rawPayload["access_token"];
  if (typeof rawToken !== "string") {
    return { ok: false, status: 400, error: "access_token is required (1..512 chars)" };
  }
  const accessToken = rawToken.trim();
  if (accessToken.length < 1 || accessToken.length > MAX_TOKEN_LEN || hasControlChars(accessToken)) {
    return { ok: false, status: 400, error: "access_token is required (1..512 chars)" };
  }
  return { ok: true, accessToken };
}

/**
 * Pure synchronous state merge & validation using a precomputed SHA-256 fingerprint.
 * Keeping this synchronous lets TokenBrokerDO run read -> merge -> SQLite write
 * in a single uninterrupted turn without any await between storage calls.
 */
export function applyTokenUpdateWithFp(
  existing: StoredTokenState | null,
  rawPayload: unknown,
  tokenFp: string,
  nowMs: number
): UpsertTokenOutcome {
  const tokenCheck = extractValidAccessToken(rawPayload);
  if (!tokenCheck.ok) {
    return tokenCheck;
  }
  const body = rawPayload as Record<string, unknown>;
  const accessToken = tokenCheck.accessToken;

  // Validate optional scalar types strictly (while ignoring unknown keys like refresh_token / refreshToken).
  const readOptionalString = (key: string): { ok: true; value: string } | { ok: false } => {
    const val = body[key];
    if (val === undefined || val === null) {
      return { ok: true, value: "" };
    }
    if (typeof val !== "string") {
      return { ok: false };
    }
    return { ok: true, value: val };
  };

  const capRaw = readOptionalString("captured_at");
  const srcRaw = readOptionalString("source");
  const baseUrlRaw = readOptionalString("base_url");
  const regionRaw = readOptionalString("region");
  const accountRaw = readOptionalString("account");
  const deviceIdRaw = readOptionalString("device_id");
  if (!capRaw.ok || !srcRaw.ok || !baseUrlRaw.ok || !regionRaw.ok || !accountRaw.ok || !deviceIdRaw.ok) {
    return { ok: false, status: 400, error: "invalid json body" };
  }

  let expiresIn = 0;
  if (body["expires_in"] !== undefined && body["expires_in"] !== null) {
    const v = body["expires_in"];
    if (typeof v !== "number" || !Number.isFinite(v)) {
      return { ok: false, status: 400, error: "invalid json body" };
    }
    expiresIn = Math.floor(v);
    if (expiresIn < 0) expiresIn = 0;
    if (expiresIn > MAX_TTL_SECONDS) expiresIn = MAX_TTL_SECONDS;
  }

  let accountId = 0;
  if (body["account_id"] !== undefined && body["account_id"] !== null) {
    const v = body["account_id"];
    if (typeof v !== "number" || !Number.isSafeInteger(v)) {
      return { ok: false, status: 400, error: "invalid json body" };
    }
    accountId = v > 0 ? v : 0;
  }

  if (
    body["client_headers"] !== undefined &&
    body["client_headers"] !== null &&
    !isPlainObject(body["client_headers"])
  ) {
    return { ok: false, status: 400, error: "invalid json body" };
  }

  let normalizedCapturedAt = "";
  const capTrimmed = capRaw.value.trim();
  if (capTrimmed !== "") {
    const capMs = parseRfc3339Ms(capTrimmed);
    if (capMs === null) {
      return { ok: false, status: 400, error: "invalid captured_at (expected RFC3339)" };
    }
    normalizedCapturedAt = formatUtcSeconds(capMs);
  }

  const source = sanitizeShortString(srcRaw.value, MAX_SOURCE_LEN);
  const baseUrl = sanitizeShortString(baseUrlRaw.value, 256).replace(/\/+$/, "");
  const region = sanitizeShortString(regionRaw.value, 64);
  const account = sanitizeShortString(accountRaw.value, 256);
  const deviceId = sanitizeShortString(deviceIdRaw.value, 256);
  const incomingHeaders = filterClientHeaders(body["client_headers"]);

  const nowSecMs = Math.floor(nowMs / 1000) * 1000;
  const nowStr = formatUtcSeconds(nowSecMs);

  const isNewToken = existing === null || existing.access_token !== accessToken;

  const next: StoredTokenState = existing
    ? {
        ...existing,
        client_headers: { ...filterClientHeaders(existing.client_headers) },
      }
    : {
        revision: 0,
        access_token: "",
        token_fp: "",
        source: "",
        captured_at: "",
        updated_at: "",
        expires_at: "",
        expires_estimated: false,
        base_url: "",
        region: "",
        account: "",
        account_id: 0,
        device_id: "",
        client_headers: {},
      };

  let changed = false;

  if (isNewToken) {
    changed = true;
    next.access_token = accessToken;
    next.token_fp = tokenFp;
    next.source = source || "manual";
    next.captured_at = normalizedCapturedAt || nowStr;
    if (expiresIn > 0) {
      next.expires_at = formatUtcSeconds(nowSecMs + expiresIn * 1000);
      next.expires_estimated = false;
    } else {
      next.expires_at = formatUtcSeconds(nowSecMs + DEFAULT_TTL_SECONDS * 1000);
      next.expires_estimated = true;
    }
  } else {
    // Same token: NEVER extend TTL when expires_in is omitted/0.
    if (expiresIn > 0) {
      const newExp = formatUtcSeconds(nowSecMs + expiresIn * 1000);
      if (next.expires_at !== newExp || next.expires_estimated) {
        next.expires_at = newExp;
        next.expires_estimated = false;
        changed = true;
      }
    }
    if (source !== "") {
      if (next.source === "" || (isAuthoritativeSource(source) && !isAuthoritativeSource(next.source))) {
        if (next.source !== source) {
          next.source = source;
          changed = true;
        }
      }
    }
    if (next.captured_at === "") {
      next.captured_at = normalizedCapturedAt || nowStr;
      changed = true;
    }
  }

  // Metadata merge: preserve existing non-empty fields; merge client_headers keys.
  if (baseUrl !== "" && next.base_url !== baseUrl) {
    next.base_url = baseUrl;
    changed = true;
  }
  if (region !== "" && next.region !== region) {
    next.region = region;
    changed = true;
  }
  if (account !== "" && next.account !== account) {
    next.account = account;
    changed = true;
  }
  if (accountId > 0 && next.account_id !== accountId) {
    next.account_id = accountId;
    changed = true;
  }
  if (deviceId !== "" && next.device_id !== deviceId) {
    next.device_id = deviceId;
    changed = true;
  }
  for (const [k, v] of Object.entries(incomingHeaders)) {
    if (next.client_headers[k] !== v) {
      next.client_headers[k] = v;
      changed = true;
    }
  }

  if (changed) {
    next.revision = existing ? existing.revision + 1 : 1;
    next.updated_at = nowStr;
  }

  const finalState = changed ? next : (existing as StoredTokenState);

  return {
    ok: true,
    state: finalState,
    response: {
      ok: true,
      updated: changed,
      revision: finalState.revision,
      token_fp: finalState.token_fp,
      updated_at: finalState.updated_at,
      expires_at: finalState.expires_at,
      expires_estimated: finalState.expires_estimated,
    },
  };
}

export async function applyTokenUpdate(
  existing: StoredTokenState | null,
  rawPayload: unknown,
  nowMs: number
): Promise<UpsertTokenOutcome> {
  const tokenCheck = extractValidAccessToken(rawPayload);
  if (!tokenCheck.ok) {
    return tokenCheck;
  }
  const tokenFp = await computeTokenFingerprint(tokenCheck.accessToken);
  return applyTokenUpdateWithFp(existing, rawPayload, tokenFp, nowMs);
}

export function computeExpiry(
  expiresAtStr: string,
  nowMs: number
): { expired: boolean; remaining_seconds: number } {
  const expMs = parseRfc3339Ms(expiresAtStr);
  if (expMs === null) {
    return { expired: true, remaining_seconds: 0 };
  }
  const diffMs = expMs - nowMs;
  if (diffMs <= 0) {
    return { expired: true, remaining_seconds: 0 };
  }
  return { expired: false, remaining_seconds: Math.ceil(diffMs / 1000) };
}

export function buildGetTokenResponse(state: StoredTokenState, nowMs: number): GetTokenResponse {
  const { expired, remaining_seconds } = computeExpiry(state.expires_at, nowMs);
  return {
    ok: true,
    revision: state.revision,
    access_token: state.access_token,
    token_fp: state.token_fp,
    source: state.source,
    captured_at: state.captured_at,
    updated_at: state.updated_at,
    expires_at: state.expires_at,
    expires_estimated: state.expires_estimated,
    expired,
    remaining_seconds,
    base_url: state.base_url,
    region: state.region,
    account: state.account,
    account_id: state.account_id,
    device_id: state.device_id,
    client_headers: filterClientHeaders(state.client_headers),
  };
}

/**
 * Reads a request body incrementally while enforcing a hard byte cap so oversized
 * payloads cannot exhaust Worker memory.
 */
export async function readLimitedBody(
  request: Request,
  maxBytes: number = MAX_BODY_BYTES
): Promise<{ ok: true; text: string } | { ok: false; status: number; error: string }> {
  const contentLength = request.headers.get("Content-Length");
  if (contentLength !== null) {
    const declared = Number(contentLength);
    if (Number.isFinite(declared) && declared > maxBytes) {
      return { ok: false, status: 413, error: "request body too large" };
    }
  }
  if (!request.body) {
    return { ok: true, text: "" };
  }
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (value) {
        total += value.byteLength;
        if (total > maxBytes) {
          await reader.cancel();
          return { ok: false, status: 413, error: "request body too large" };
        }
        chunks.push(value);
      }
    }
  } catch {
    return { ok: false, status: 400, error: "failed to read request body" };
  }
  const merged = new Uint8Array(total);
  let offset = 0;
  for (const c of chunks) {
    merged.set(c, offset);
    offset += c.byteLength;
  }
  return { ok: true, text: new TextDecoder("utf-8").decode(merged) };
}

function jsonResponse(payload: unknown, status: number, extraHeaders?: Record<string, string>): Response {
  const headers = new Headers({
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    ...extraHeaders,
  });
  return new Response(JSON.stringify(payload), { status, headers });
}

export interface BrokerRequestOptions {
  secret: string | undefined;
  store: BrokerStore;
  now?: () => number;
  log?: (entry: Record<string, unknown>) => void;
}

export async function handleBrokerRequest(
  request: Request,
  opts: BrokerRequestOptions
): Promise<Response> {
  const now = opts.now ?? Date.now;
  const emitLog = opts.log ?? ((entry: Record<string, unknown>) => console.log(JSON.stringify(entry)));
  const url = new URL(request.url);
  const path = url.pathname;
  const method = request.method.toUpperCase();

  if (path === "/health") {
    if (method !== "GET") {
      return jsonResponse({ ok: false, error: "method not allowed" }, 405, { Allow: "GET" });
    }
    return jsonResponse({ ok: true, service: SERVICE_NAME, version: SERVICE_VERSION }, 200);
  }

  // All non-/health endpoints require configured BROKER_SECRET and constant-time Bearer auth.
  const authResult = await verifyBearerSecret(request.headers.get("Authorization"), opts.secret);
  if (authResult === "unconfigured") {
    emitLog({ event: "auth_reject", method, path, status: 503, reason: "unconfigured_secret" });
    return jsonResponse({ ok: false, error: "broker secret not configured" }, 503);
  }
  if (authResult !== "ok") {
    emitLog({ event: "auth_reject", method, path, status: 401, reason: "unauthorized" });
    return jsonResponse({ ok: false, error: "unauthorized" }, 401, {
      "WWW-Authenticate": 'Bearer realm="rayneo-token-broker"',
    });
  }

  if (path !== "/v1/token") {
    return jsonResponse({ ok: false, error: "not found" }, 404);
  }

  if (method === "GET") {
    const state = await opts.store.getState();
    if (!state) {
      emitLog({ event: "token_get", status: 404 });
      return jsonResponse({ ok: false, error: "no token synced yet" }, 404);
    }
    const etag = formatEtag(state.revision, state.token_fp);
    if (etagMatches(request.headers.get("If-None-Match"), etag)) {
      emitLog({ event: "token_get", status: 304, revision: state.revision, token_fp: state.token_fp });
      return new Response(null, {
        status: 304,
        headers: {
          ETag: etag,
          "Cache-Control": "no-store",
          "X-Content-Type-Options": "nosniff",
        },
      });
    }
    const body = buildGetTokenResponse(state, now());
    emitLog({
      event: "token_get",
      status: 200,
      revision: state.revision,
      token_fp: state.token_fp,
      expired: body.expired,
    });
    return jsonResponse(body, 200, { ETag: etag });
  }

  if (method === "POST") {
    const bodyRead = await readLimitedBody(request, MAX_BODY_BYTES);
    if (!bodyRead.ok) {
      emitLog({ event: "token_post", status: bodyRead.status, reason: bodyRead.error });
      return jsonResponse({ ok: false, error: bodyRead.error }, bodyRead.status);
    }
    const trimmed = bodyRead.text.trim();
    if (trimmed.length === 0 || !trimmed.startsWith("{")) {
      return jsonResponse({ ok: false, error: "invalid json body" }, 400);
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(trimmed);
    } catch {
      return jsonResponse({ ok: false, error: "invalid json body" }, 400);
    }
    if (!isPlainObject(parsed)) {
      return jsonResponse({ ok: false, error: "invalid json body" }, 400);
    }

    const nowMs = now();
    let outcome: UpsertTokenOutcome;
    if (opts.store.upsertJson) {
      outcome = await opts.store.upsertJson(trimmed, nowMs);
    } else {
      const existing = await opts.store.getState();
      outcome = await applyTokenUpdate(existing, parsed, nowMs);
      if (outcome.ok && outcome.response.updated) {
        await opts.store.saveState(outcome.state);
      }
    }

    if (!outcome.ok) {
      emitLog({ event: "token_post", status: outcome.status, reason: outcome.error });
      return jsonResponse({ ok: false, error: outcome.error }, outcome.status);
    }

    emitLog({
      event: "token_post",
      status: 200,
      updated: outcome.response.updated,
      revision: outcome.response.revision,
      token_fp: outcome.response.token_fp,
    });
    return jsonResponse(outcome.response, 200);
  }

  if (method === "DELETE") {
    await opts.store.clearState();
    emitLog({ event: "token_delete", status: 200, cleared: true });
    return jsonResponse({ ok: true, cleared: true }, 200);
  }

  return jsonResponse({ ok: false, error: "method not allowed" }, 405, {
    Allow: "GET, POST, DELETE",
  });
}
