export const SERVICE_NAME = "rayneo-token-broker";
export const SERVICE_VERSION = "1";

export const MAX_BODY_BYTES = 64 * 1024;
export const MAX_TOKEN_LEN = 512;
export const MAX_SOURCE_LEN = 64;
export const MAX_HEADER_VAL_LEN = 1024;
export const MIN_SECRET_LEN = 32;
export const DEFAULT_TTL_SECONDS = 7200;
export const MAX_TTL_SECONDS = 86400;
export const EXPIRED_PURGE_GRACE_MS = 24 * 60 * 60 * 1000; // 24h after expires_at
export const MAX_CAPTURED_AT_FUTURE_SKEW_MS = 24 * 60 * 60 * 1000;

const MIN_RFC3339_YEAR = 2000;
const MAX_RFC3339_YEAR = 2100;

const RAYNEO_API_HOST_RE = /^xr-api-[a-z0-9-]+\.rayneo\.(cn|com)$/i;

const FORBIDDEN_SECRETS: ReadonlySet<string> = new Set([
  "replace-with-your-random-secret",
  "your-strong-random-secret",
  "your-broker-secret",
  "your-secret",
  "changeme",
  "change-me",
  "placeholder",
  "default",
  "example",
  "password",
  "secret",
]);

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

/**
 * Sanitized, allowlisted upsert payload computed in the Worker isolate before
 * crossing the Durable Object RPC boundary so refresh_token or arbitrary raw
 * JSON fields never enter DO memory.
 */
export type SanitizedTokenUpsert = {
  access_token: string;
  token_fp: string;
  expires_in: number;
  captured_at: string;
  source: string;
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
  getState(nowMs?: number): StoredTokenState | null | Promise<StoredTokenState | null>;
  saveState(state: StoredTokenState): void | Promise<void>;
  clearState(): void | Promise<void>;
  /**
   * Atomic read-modify-write hook accepting a pre-validated, allowlisted DTO
   * (zero refresh_token / unknown fields crossing the DO RPC boundary).
   */
  upsertSanitized?(input: SanitizedTokenUpsert, nowMs: number): Promise<UpsertTokenOutcome>;
}

export interface RateLimiterLike {
  limit(options: { key: string }): Promise<{ success: boolean }>;
}

export type BrokerSecrets = {
  /** Legacy/shared full-access secret (GET, POST, DELETE) — backward compatible. */
  shared?: string;
  /** Optional read-only secret (GET only). */
  read?: string;
  /** Optional write secret (POST, DELETE only). */
  write?: string;
  /** Optional dedicated at-rest encryption key (validated for strength if set). */
  encryptionKey?: string;
};

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

const textEncoder = new TextEncoder();
const textDecoderStrict = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false });

const STORAGE_ENC_PREFIX = "enc:v1:";
const STORAGE_HKDF_SALT = textEncoder.encode("rayneo-token-broker:do-sqlite:v1:salt");
const STORAGE_HKDF_INFO = textEncoder.encode("rayneo-token-broker:aes-256-gcm:v1");
const STORAGE_IV_BYTES = 12;

function toArrayBuffer(bytes: Uint8Array): ArrayBuffer {
  const copy = new Uint8Array(bytes.byteLength);
  copy.set(bytes);
  return copy.buffer;
}

function bytesToBase64Url(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.byteLength; i++) {
    binary += String.fromCharCode(bytes[i]!);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function base64UrlToBytes(input: string): Uint8Array | null {
  if (!/^[A-Za-z0-9_-]+$/.test(input)) {
    return null;
  }
  const padLen = (4 - (input.length % 4)) % 4;
  const b64 = input.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat(padLen);
  try {
    const binary = atob(b64);
    const out = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) {
      out[i] = binary.charCodeAt(i);
    }
    return out;
  } catch {
    return null;
  }
}

export function resolveEncryptionKeyMaterial(env: {
  BROKER_ENCRYPTION_KEY?: string;
  BROKER_SECRET?: string;
  BROKER_WRITE_SECRET?: string;
  BROKER_READ_SECRET?: string;
}): string | null {
  for (const candidate of [
    env.BROKER_ENCRYPTION_KEY,
    env.BROKER_SECRET,
    env.BROKER_WRITE_SECRET,
    env.BROKER_READ_SECRET,
  ]) {
    const trimmed = typeof candidate === "string" ? candidate.trim() : "";
    if (trimmed.length > 0) {
      return trimmed;
    }
  }
  return null;
}

export async function deriveStorageAesKey(secretMaterial: string): Promise<CryptoKey> {
  const baseKey = await crypto.subtle.importKey(
    "raw",
    toArrayBuffer(textEncoder.encode(secretMaterial)),
    "HKDF",
    false,
    ["deriveKey"]
  );
  return crypto.subtle.deriveKey(
    {
      name: "HKDF",
      hash: "SHA-256",
      salt: toArrayBuffer(STORAGE_HKDF_SALT),
      info: toArrayBuffer(STORAGE_HKDF_INFO),
    },
    baseKey,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"]
  );
}

function fieldAad(fieldName: string): ArrayBuffer {
  return toArrayBuffer(textEncoder.encode(`rayneo-token-broker:v1:token_state:${fieldName}`));
}

export async function encryptStorageField(
  key: CryptoKey,
  fieldName: string,
  plaintext: string
): Promise<string> {
  if (plaintext === "") {
    return "";
  }
  const iv = new Uint8Array(STORAGE_IV_BYTES);
  crypto.getRandomValues(iv);
  const ctBuf = await crypto.subtle.encrypt(
    {
      name: "AES-GCM",
      iv: toArrayBuffer(iv),
      additionalData: fieldAad(fieldName),
    },
    key,
    toArrayBuffer(textEncoder.encode(plaintext))
  );
  const ct = new Uint8Array(ctBuf);
  const packed = new Uint8Array(STORAGE_IV_BYTES + ct.byteLength);
  packed.set(iv, 0);
  packed.set(ct, STORAGE_IV_BYTES);
  return STORAGE_ENC_PREFIX + bytesToBase64Url(packed);
}

export async function decryptStorageField(
  key: CryptoKey | null,
  fieldName: string,
  stored: string
): Promise<{ ok: true; value: string } | { ok: false }> {
  if (stored === "") {
    return { ok: true, value: "" };
  }
  // Backward-compatible migration: legacy plaintext rows written before encryption
  // are readable seamlessly and upgraded to enc:v1: on the next write.
  if (!stored.startsWith(STORAGE_ENC_PREFIX)) {
    return { ok: true, value: stored };
  }
  if (!key) {
    return { ok: false };
  }
  const packed = base64UrlToBytes(stored.slice(STORAGE_ENC_PREFIX.length));
  if (!packed || packed.byteLength <= STORAGE_IV_BYTES + 16) {
    return { ok: false };
  }
  const iv = packed.subarray(0, STORAGE_IV_BYTES);
  const ct = packed.subarray(STORAGE_IV_BYTES);
  try {
    const ptBuf = await crypto.subtle.decrypt(
      {
        name: "AES-GCM",
        iv: toArrayBuffer(iv),
        additionalData: fieldAad(fieldName),
      },
      key,
      toArrayBuffer(ct)
    );
    return { ok: true, value: textDecoderStrict.decode(new Uint8Array(ptBuf)) };
  } catch {
    return { ok: false };
  }
}

export function isPastPurgeGrace(expiresAtStr: string, nowMs: number): boolean {
  const expMs = parseRfc3339Ms(expiresAtStr);
  if (expMs === null) {
    return true;
  }
  return nowMs >= expMs + EXPIRED_PURGE_GRACE_MS;
}

export function computePurgeAlarmMs(expiresAtStr: string, nowMs: number): number {
  const expMs = parseRfc3339Ms(expiresAtStr);
  if (expMs === null) {
    return nowMs + EXPIRED_PURGE_GRACE_MS;
  }
  return expMs + EXPIRED_PURGE_GRACE_MS;
}

export function readTokenExpiryFromSql(sql: SqlStorageLike): string | null {
  const rows = sql
    .exec<{ expires_at: string }>("SELECT expires_at FROM token_state WHERE id = 1")
    .toArray();
  const row = rows[0];
  return row ? String(row.expires_at) : null;
}

function hydrateTokenStateRow(
  row: TokenStateRow,
  accessToken: string,
  account: string,
  deviceId: string,
  headersJson: string
): StoredTokenState {
  let parsedHeaders: unknown = {};
  try {
    parsedHeaders = JSON.parse(headersJson);
  } catch {
    parsedHeaders = {};
  }
  const baseCheck = validateRayNeoBaseUrl(String(row.base_url));
  return {
    revision: Number(row.revision),
    access_token: accessToken,
    token_fp: String(row.token_fp),
    source: String(row.source),
    captured_at: String(row.captured_at),
    updated_at: String(row.updated_at),
    expires_at: String(row.expires_at),
    expires_estimated: Number(row.expires_estimated) !== 0,
    base_url: baseCheck.ok ? baseCheck.value : "",
    region: String(row.region),
    account,
    account_id: Number(row.account_id),
    device_id: deviceId,
    client_headers: filterClientHeaders(parsedHeaders),
  };
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
  return hydrateTokenStateRow(
    row,
    String(row.access_token),
    String(row.account),
    String(row.device_id),
    String(row.client_headers_json)
  );
}

export async function readEncryptedTokenStateFromSql(
  sql: SqlStorageLike,
  key: CryptoKey | null,
  onDecryptFailure?: () => void
): Promise<StoredTokenState | null> {
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
  const [tokRes, acctRes, devRes, hdrRes] = await Promise.all([
    decryptStorageField(key, "access_token", String(row.access_token)),
    decryptStorageField(key, "account", String(row.account)),
    decryptStorageField(key, "device_id", String(row.device_id)),
    decryptStorageField(key, "client_headers_json", String(row.client_headers_json)),
  ]);
  if (!tokRes.ok || !acctRes.ok || !devRes.ok || !hdrRes.ok || tokRes.value === "") {
    if (onDecryptFailure) {
      onDecryptFailure();
    }
    return null;
  }
  return hydrateTokenStateRow(row, tokRes.value, acctRes.value, devRes.value, hdrRes.value);
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

export async function writeEncryptedTokenStateToSql(
  sql: SqlStorageLike,
  state: StoredTokenState,
  key: CryptoKey
): Promise<void> {
  const headersJson = JSON.stringify(filterClientHeaders(state.client_headers));
  const [encToken, encAccount, encDeviceId, encHeaders] = await Promise.all([
    encryptStorageField(key, "access_token", state.access_token),
    encryptStorageField(key, "account", state.account),
    encryptStorageField(key, "device_id", state.device_id),
    encryptStorageField(key, "client_headers_json", headersJson),
  ]);
  sql.exec(
    `INSERT OR REPLACE INTO token_state (
       id, revision, access_token, token_fp, source, captured_at, updated_at,
       expires_at, expires_estimated, base_url, region, account,
       account_id, device_id, client_headers_json
     ) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    state.revision,
    encToken,
    state.token_fp,
    state.source,
    state.captured_at,
    state.updated_at,
    state.expires_at,
    state.expires_estimated ? 1 : 0,
    state.base_url,
    state.region,
    encAccount,
    state.account_id,
    encDeviceId,
    encHeaders
  );
}

export function clearTokenStateFromSql(sql: SqlStorageLike): void {
  sql.exec("DELETE FROM token_state WHERE id = 1");
}

export async function sha256Digest(input: string): Promise<ArrayBuffer> {
  return crypto.subtle.digest("SHA-256", toArrayBuffer(textEncoder.encode(input)));
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

/**
 * Enforces strong secret hygiene: rejects empty, short (<32 chars), low-diversity,
 * and known placeholder values (such as `.dev.vars.example` defaults) fail-closed.
 */
export function isStrongSecret(raw: string | undefined): boolean {
  if (typeof raw !== "string") {
    return false;
  }
  const s = raw.trim();
  if (s.length < MIN_SECRET_LEN || hasControlChars(s)) {
    return false;
  }
  const lower = s.toLowerCase();
  if (FORBIDDEN_SECRETS.has(lower) || lower.includes("replace-with")) {
    return false;
  }
  const distinct = new Set<string>();
  for (const ch of s) {
    distinct.add(ch);
    if (distinct.size >= 4) {
      return true;
    }
  }
  return false;
}

export type AuthCheckResult = "ok" | "unconfigured" | "unauthorized";

export type AuthScopeResult =
  | { status: "unconfigured"; reason: "unconfigured_secret" | "weak_or_placeholder_secret" }
  | { status: "unauthorized" }
  | { status: "ok"; scopes: { read: boolean; write: boolean } };

function extractBearerToken(authHeader: string | null): string | null {
  if (!authHeader) {
    return null;
  }
  const trimmed = authHeader.trim();
  const spaceIdx = trimmed.indexOf(" ");
  if (spaceIdx <= 0) {
    return null;
  }
  const scheme = trimmed.slice(0, spaceIdx);
  if (scheme.toLowerCase() !== "bearer") {
    return null;
  }
  const provided = trimmed.slice(spaceIdx + 1).trim();
  return provided.length > 0 ? provided : null;
}

export async function verifyBearerSecret(
  authHeader: string | null,
  expectedSecret: string | undefined,
  subtle: SubtleWithTimingSafe = crypto.subtle as SubtleWithTimingSafe
): Promise<AuthCheckResult> {
  const res = await authorizeBrokerRequest(authHeader, { shared: expectedSecret }, subtle);
  if (res.status === "unconfigured") {
    return "unconfigured";
  }
  return res.status === "ok" ? "ok" : "unauthorized";
}

/**
 * Constant-time Bearer authorization supporting both legacy single-secret mode
 * (`BROKER_SECRET`) and least-privilege read/write separation (`BROKER_READ_SECRET`
 * + `BROKER_WRITE_SECRET`). Evaluates all configured secrets without early exit.
 */
export async function authorizeBrokerRequest(
  authHeader: string | null,
  secrets: BrokerSecrets,
  subtle: SubtleWithTimingSafe = crypto.subtle as SubtleWithTimingSafe
): Promise<AuthScopeResult> {
  const shared = typeof secrets.shared === "string" ? secrets.shared.trim() : "";
  const read = typeof secrets.read === "string" ? secrets.read.trim() : "";
  const write = typeof secrets.write === "string" ? secrets.write.trim() : "";
  const encKey = typeof secrets.encryptionKey === "string" ? secrets.encryptionKey.trim() : "";

  if (shared.length === 0 && read.length === 0 && write.length === 0) {
    return { status: "unconfigured", reason: "unconfigured_secret" };
  }
  // Fail-closed if ANY configured secret or encryption key is weak or a placeholder.
  if (
    (shared.length > 0 && !isStrongSecret(shared)) ||
    (read.length > 0 && !isStrongSecret(read)) ||
    (write.length > 0 && !isStrongSecret(write)) ||
    (encKey.length > 0 && !isStrongSecret(encKey))
  ) {
    return { status: "unconfigured", reason: "weak_or_placeholder_secret" };
  }

  const provided = extractBearerToken(authHeader);
  if (!provided) {
    return { status: "unauthorized" };
  }

  // Hash provided token and every configured secret to fixed 32-byte SHA-256 digests
  // so differing lengths never short-circuit or leak via timing.
  const providedHash = await subtle.digest("SHA-256", toArrayBuffer(textEncoder.encode(provided)));

  const checkSlot = async (expected: string): Promise<boolean> => {
    if (expected.length === 0) {
      return false;
    }
    const expectedHash = await subtle.digest("SHA-256", toArrayBuffer(textEncoder.encode(expected)));
    return constantTimeEqualBytes(providedHash, expectedHash, subtle);
  };

  const [matchesShared, matchesRead, matchesWrite] = await Promise.all([
    checkSlot(shared),
    checkSlot(read),
    checkSlot(write),
  ]);

  if (!matchesShared && !matchesRead && !matchesWrite) {
    return { status: "unauthorized" };
  }
  return {
    status: "ok",
    scopes: {
      read: matchesShared || matchesRead,
      write: matchesShared || matchesWrite,
    },
  };
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

/**
 * Rejects ASCII C0 controls (<0x20), DEL (0x7F), and C1 controls (0x80..0x9F,
 * e.g. 8-bit ANSI CSI \u009B) to prevent header/log/terminal escape injection.
 */
export function hasControlChars(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const code = s.charCodeAt(i);
    if (code < 0x20 || (code >= 0x7f && code <= 0x9f)) {
      return true;
    }
  }
  return false;
}

function isPrintableAsciiNoSpace(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const code = s.charCodeAt(i);
    if (code < 0x21 || code > 0x7e) {
      return false;
    }
  }
  return true;
}

export function validateRayNeoBaseUrl(
  raw: string
): { ok: true; value: string } | { ok: false } {
  const trimmed = raw.trim();
  if (trimmed === "") {
    return { ok: true, value: "" };
  }
  if (trimmed.length > 256 || hasControlChars(trimmed)) {
    return { ok: false };
  }
  const normalizedInput = trimmed.replace(/\/+$/, "");
  let u: URL;
  try {
    u = new URL(normalizedInput);
  } catch {
    return { ok: false };
  }
  if (
    u.protocol !== "https:" ||
    u.username !== "" ||
    u.password !== "" ||
    u.port !== "" ||
    (u.pathname !== "" && u.pathname !== "/") ||
    u.search !== "" ||
    u.hash !== "" ||
    !RAYNEO_API_HOST_RE.test(u.hostname)
  ) {
    return { ok: false };
  }
  return { ok: true, value: `https://${u.hostname.toLowerCase()}` };
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
    year < MIN_RFC3339_YEAR ||
    year > MAX_RFC3339_YEAR ||
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
  if (!Number.isFinite(ms)) {
    return null;
  }
  const utcYear = new Date(ms).getUTCFullYear();
  if (utcYear < MIN_RFC3339_YEAR || utcYear > MAX_RFC3339_YEAR) {
    return null;
  }
  return ms;
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
  if (
    accessToken.length < 1 ||
    accessToken.length > MAX_TOKEN_LEN ||
    !isPrintableAsciiNoSpace(accessToken)
  ) {
    return { ok: false, status: 400, error: "access_token is required (1..512 chars)" };
  }
  return { ok: true, accessToken };
}

/**
 * Validates and projects an untrusted JSON payload into the strict allowlisted
 * upsert schema, stripping refresh_token / refreshToken and unknown keys immediately.
 */
export function validateAndSanitizeUpsertPayload(
  rawPayload: unknown,
  tokenFp: string,
  nowMs: number
): { ok: true; input: SanitizedTokenUpsert } | { ok: false; status: number; error: string } {
  const tokenCheck = extractValidAccessToken(rawPayload);
  if (!tokenCheck.ok) {
    return tokenCheck;
  }
  const body = rawPayload as Record<string, unknown>;
  const accessToken = tokenCheck.accessToken;

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
    if (capMs === null || capMs > nowMs + MAX_CAPTURED_AT_FUTURE_SKEW_MS) {
      return { ok: false, status: 400, error: "invalid captured_at (expected RFC3339)" };
    }
    normalizedCapturedAt = formatUtcSeconds(capMs);
  }

  const baseUrlCheck = validateRayNeoBaseUrl(baseUrlRaw.value);
  if (!baseUrlCheck.ok) {
    return { ok: false, status: 400, error: "invalid base_url" };
  }

  return {
    ok: true,
    input: {
      access_token: accessToken,
      token_fp: tokenFp,
      expires_in: expiresIn,
      captured_at: normalizedCapturedAt,
      source: sanitizeShortString(srcRaw.value, MAX_SOURCE_LEN),
      base_url: baseUrlCheck.value,
      region: sanitizeShortString(regionRaw.value, 64),
      account: sanitizeShortString(accountRaw.value, 256),
      account_id: accountId,
      device_id: sanitizeShortString(deviceIdRaw.value, 256),
      client_headers: filterClientHeaders(body["client_headers"]),
    },
  };
}

export async function prepareSanitizedUpsert(
  rawPayload: unknown,
  nowMs: number
): Promise<{ ok: true; input: SanitizedTokenUpsert } | { ok: false; status: number; error: string }> {
  const tokenCheck = extractValidAccessToken(rawPayload);
  if (!tokenCheck.ok) {
    return tokenCheck;
  }
  const tokenFp = await computeTokenFingerprint(tokenCheck.accessToken);
  return validateAndSanitizeUpsertPayload(rawPayload, tokenFp, nowMs);
}

/**
 * Pure synchronous state merge operating on a pre-validated SanitizedTokenUpsert.
 */
export function applySanitizedTokenUpdate(
  existing: StoredTokenState | null,
  input: SanitizedTokenUpsert,
  nowMs: number
): UpsertTokenOutcome {
  // Defense-in-depth re-validation in case DO RPC is invoked directly.
  const baseCheck = validateRayNeoBaseUrl(input.base_url);
  if (!baseCheck.ok) {
    return { ok: false, status: 400, error: "invalid base_url" };
  }
  const accessToken = input.access_token.trim();
  if (
    accessToken.length < 1 ||
    accessToken.length > MAX_TOKEN_LEN ||
    !isPrintableAsciiNoSpace(accessToken)
  ) {
    return { ok: false, status: 400, error: "access_token is required (1..512 chars)" };
  }

  const normalizedCapturedAt = input.captured_at;
  const source = sanitizeShortString(input.source, MAX_SOURCE_LEN);
  const baseUrl = baseCheck.value;
  const region = sanitizeShortString(input.region, 64);
  const account = sanitizeShortString(input.account, 256);
  const accountId = Number.isSafeInteger(input.account_id) && input.account_id > 0 ? input.account_id : 0;
  const deviceId = sanitizeShortString(input.device_id, 256);
  const incomingHeaders = filterClientHeaders(input.client_headers);
  let expiresIn = Number.isFinite(input.expires_in) ? Math.floor(input.expires_in) : 0;
  if (expiresIn < 0) expiresIn = 0;
  if (expiresIn > MAX_TTL_SECONDS) expiresIn = MAX_TTL_SECONDS;

  const nowSecMs = Math.floor(nowMs / 1000) * 1000;
  const nowStr = formatUtcSeconds(nowSecMs);

  const isNewToken = existing === null || existing.access_token !== accessToken;

  // Anti-downgrade guard: reject out-of-order stale non-authoritative captures
  // (e.g., an in-flight loon:header with an older token arriving after loon:refresh).
  if (isNewToken && existing !== null && !isAuthoritativeSource(source) && normalizedCapturedAt !== "") {
    const incomingCapMs = parseRfc3339Ms(normalizedCapturedAt);
    const existingCapMs = parseRfc3339Ms(existing.captured_at);
    if (incomingCapMs !== null && existingCapMs !== null && incomingCapMs < existingCapMs) {
      return {
        ok: true,
        state: existing,
        response: {
          ok: true,
          updated: false,
          revision: existing.revision,
          token_fp: existing.token_fp,
          updated_at: existing.updated_at,
          expires_at: existing.expires_at,
          expires_estimated: existing.expires_estimated,
        },
      };
    }
  }

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

  // Cross-account switch: reset stale PII/device metadata before applying incoming fields.
  if (accountId > 0 && existing !== null && existing.account_id > 0 && existing.account_id !== accountId) {
    if (next.account !== "" || next.device_id !== "" || Object.keys(next.client_headers).length > 0) {
      next.account = "";
      next.device_id = "";
      next.client_headers = {};
      changed = true;
    }
  }

  if (isNewToken) {
    changed = true;
    next.access_token = accessToken;
    next.token_fp = input.token_fp;
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

/**
 * Pure synchronous state merge & validation using a precomputed SHA-256 fingerprint.
 */
export function applyTokenUpdateWithFp(
  existing: StoredTokenState | null,
  rawPayload: unknown,
  tokenFp: string,
  nowMs: number
): UpsertTokenOutcome {
  const sanitized = validateAndSanitizeUpsertPayload(rawPayload, tokenFp, nowMs);
  if (!sanitized.ok) {
    return sanitized;
  }
  return applySanitizedTokenUpdate(existing, sanitized.input, nowMs);
}

export async function applyTokenUpdate(
  existing: StoredTokenState | null,
  rawPayload: unknown,
  nowMs: number
): Promise<UpsertTokenOutcome> {
  const sanitized = await prepareSanitizedUpsert(rawPayload, nowMs);
  if (!sanitized.ok) {
    return sanitized;
  }
  return applySanitizedTokenUpdate(existing, sanitized.input, nowMs);
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
 * Reads a request body incrementally while enforcing a hard byte cap and strict
 * UTF-8 decoding so oversized or malformed payloads cannot exhaust Worker memory.
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
  } finally {
    try {
      reader.releaseLock();
    } catch {
      // Ignore if already released
    }
  }
  const merged = new Uint8Array(total);
  let offset = 0;
  for (const c of chunks) {
    merged.set(c, offset);
    offset += c.byteLength;
  }
  try {
    return { ok: true, text: textDecoderStrict.decode(merged) };
  } catch {
    return { ok: false, status: 400, error: "invalid utf-8 body" };
  }
}

function isLoopbackHost(hostname: string): boolean {
  const h = hostname.toLowerCase();
  return h === "localhost" || h === "127.0.0.1" || h === "[::1]" || h === "::1";
}

function isJsonContentType(rawHeader: string): boolean {
  const mediaType = rawHeader.split(";")[0]?.trim().toLowerCase() ?? "";
  return mediaType === "application/json" || mediaType.endsWith("+json");
}

export function buildSecurityHeaders(
  isHttps: boolean,
  extraHeaders?: Record<string, string>
): Headers {
  const headers = new Headers({
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "no-referrer",
    "X-Frame-Options": "DENY",
    "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
    Vary: "Authorization",
    ...extraHeaders,
  });
  if (isHttps) {
    headers.set("Strict-Transport-Security", "max-age=31536000; includeSubDomains");
  }
  return headers;
}

function jsonResponse(
  payload: unknown,
  status: number,
  isHttps: boolean,
  extraHeaders?: Record<string, string>
): Response {
  const headers = buildSecurityHeaders(isHttps, {
    "Content-Type": "application/json; charset=utf-8",
    ...extraHeaders,
  });
  return new Response(JSON.stringify(payload), { status, headers });
}

export interface BrokerRequestOptions {
  /** Legacy single full-access secret (backward compatible). */
  secret?: string | undefined;
  /** Optional split/shared secret configuration (takes precedence alongside `secret`). */
  secrets?: BrokerSecrets;
  store: BrokerStore;
  rateLimiter?: RateLimiterLike;
  allowInsecureHttp?: boolean;
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
  const isHttps = url.protocol === "https:";

  // Enforce HTTPS on all non-loopback hosts (never 301-redirect requests carrying Bearer auth).
  if (!isHttps && !opts.allowInsecureHttp && !isLoopbackHost(url.hostname)) {
    emitLog({ event: "insecure_http_reject", method, path, status: 426 });
    return jsonResponse({ ok: false, error: "https required" }, 426, false);
  }

  if (path === "/health") {
    if (method !== "GET") {
      return jsonResponse({ ok: false, error: "method not allowed" }, 405, isHttps, { Allow: "GET" });
    }
    return jsonResponse({ ok: true }, 200, isHttps);
  }

  // Optional edge rate limiting by client IP before authentication & DO access.
  if (opts.rateLimiter) {
    const clientIp =
      request.headers.get("CF-Connecting-IP")?.trim() ||
      request.headers.get("X-Real-IP")?.trim() ||
      "unknown";
    const { success } = await opts.rateLimiter.limit({ key: clientIp });
    if (!success) {
      emitLog({ event: "rate_limited", method, path, status: 429 });
      return jsonResponse({ ok: false, error: "too many requests" }, 429, isHttps, {
        "Retry-After": "60",
      });
    }
  }

  const effectiveSecrets: BrokerSecrets = {
    shared: opts.secrets?.shared ?? opts.secret,
    read: opts.secrets?.read,
    write: opts.secrets?.write,
    encryptionKey: opts.secrets?.encryptionKey,
  };

  // All non-/health endpoints require valid configured secret(s) and constant-time Bearer auth.
  const authResult = await authorizeBrokerRequest(request.headers.get("Authorization"), effectiveSecrets);
  if (authResult.status === "unconfigured") {
    emitLog({ event: "auth_reject", method, path, status: 503, reason: authResult.reason });
    return jsonResponse({ ok: false, error: "service unavailable" }, 503, isHttps);
  }
  if (authResult.status !== "ok") {
    emitLog({ event: "auth_reject", method, path, status: 401, reason: "unauthorized" });
    return jsonResponse({ ok: false, error: "unauthorized" }, 401, isHttps, {
      "WWW-Authenticate": 'Bearer realm="rayneo-token-broker"',
    });
  }

  if (path !== "/v1/token") {
    return jsonResponse({ ok: false, error: "not found" }, 404, isHttps);
  }

  if (url.search !== "") {
    emitLog({ event: "query_reject", method, path, status: 400 });
    return jsonResponse({ ok: false, error: "unexpected query parameters" }, 400, isHttps);
  }

  if (method === "GET") {
    if (!authResult.scopes.read) {
      emitLog({ event: "auth_forbidden", method, path, status: 403, required_scope: "read" });
      return jsonResponse({ ok: false, error: "forbidden" }, 403, isHttps);
    }
    const nowMs = now();
    const state = await opts.store.getState(nowMs);
    if (!state || isPastPurgeGrace(state.expires_at, nowMs)) {
      if (state) {
        await opts.store.clearState();
      }
      emitLog({ event: "token_get", status: 404 });
      return jsonResponse({ ok: false, error: "no token synced yet" }, 404, isHttps);
    }
    const etag = formatEtag(state.revision, state.token_fp);
    if (etagMatches(request.headers.get("If-None-Match"), etag)) {
      emitLog({ event: "token_get", status: 304, revision: state.revision, token_fp: state.token_fp });
      return new Response(null, {
        status: 304,
        headers: buildSecurityHeaders(isHttps, { ETag: etag }),
      });
    }
    const body = buildGetTokenResponse(state, nowMs);
    emitLog({
      event: "token_get",
      status: 200,
      revision: state.revision,
      token_fp: state.token_fp,
      expired: body.expired,
    });
    return jsonResponse(body, 200, isHttps, { ETag: etag });
  }

  if (method === "POST") {
    if (!authResult.scopes.write) {
      emitLog({ event: "auth_forbidden", method, path, status: 403, required_scope: "write" });
      return jsonResponse({ ok: false, error: "forbidden" }, 403, isHttps);
    }
    const contentType = request.headers.get("Content-Type");
    if (contentType !== null && contentType.trim() !== "" && !isJsonContentType(contentType)) {
      emitLog({ event: "token_post", status: 415, reason: "unsupported_media_type" });
      return jsonResponse({ ok: false, error: "content-type must be application/json" }, 415, isHttps);
    }
    const bodyRead = await readLimitedBody(request, MAX_BODY_BYTES);
    if (!bodyRead.ok) {
      emitLog({ event: "token_post", status: bodyRead.status, reason: bodyRead.error });
      return jsonResponse({ ok: false, error: bodyRead.error }, bodyRead.status, isHttps);
    }
    const trimmed = bodyRead.text.trim();
    if (trimmed.length === 0 || !trimmed.startsWith("{")) {
      return jsonResponse({ ok: false, error: "invalid json body" }, 400, isHttps);
    }
    if (contentType === null || contentType.trim() === "") {
      emitLog({ event: "token_post", status: 415, reason: "missing_content_type" });
      return jsonResponse({ ok: false, error: "content-type must be application/json" }, 415, isHttps);
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(trimmed);
    } catch {
      return jsonResponse({ ok: false, error: "invalid json body" }, 400, isHttps);
    }
    if (!isPlainObject(parsed)) {
      return jsonResponse({ ok: false, error: "invalid json body" }, 400, isHttps);
    }

    const nowMs = now();
    // Validate, strip refresh_token & non-allowlisted headers, and hash token_fp ONCE
    // in the Worker isolate before touching DO RPC or storage.
    const prepared = await prepareSanitizedUpsert(parsed, nowMs);
    if (!prepared.ok) {
      emitLog({ event: "token_post", status: prepared.status, reason: prepared.error });
      return jsonResponse({ ok: false, error: prepared.error }, prepared.status, isHttps);
    }

    let outcome: UpsertTokenOutcome;
    if (opts.store.upsertSanitized) {
      outcome = await opts.store.upsertSanitized(prepared.input, nowMs);
    } else {
      const existing = await opts.store.getState(nowMs);
      const effectiveExisting =
        existing && !isPastPurgeGrace(existing.expires_at, nowMs) ? existing : null;
      outcome = applySanitizedTokenUpdate(effectiveExisting, prepared.input, nowMs);
      if (outcome.ok && outcome.response.updated) {
        await opts.store.saveState(outcome.state);
      }
    }

    if (!outcome.ok) {
      emitLog({ event: "token_post", status: outcome.status, reason: outcome.error });
      return jsonResponse({ ok: false, error: outcome.error }, outcome.status, isHttps);
    }

    emitLog({
      event: "token_post",
      status: 200,
      updated: outcome.response.updated,
      revision: outcome.response.revision,
      token_fp: outcome.response.token_fp,
    });
    return jsonResponse(outcome.response, 200, isHttps);
  }

  if (method === "DELETE") {
    if (!authResult.scopes.write) {
      emitLog({ event: "auth_forbidden", method, path, status: 403, required_scope: "write" });
      return jsonResponse({ ok: false, error: "forbidden" }, 403, isHttps);
    }
    await opts.store.clearState();
    emitLog({ event: "token_delete", status: 200, cleared: true });
    return jsonResponse({ ok: true, cleared: true }, 200, isHttps);
  }

  return jsonResponse({ ok: false, error: "method not allowed" }, 405, isHttps, {
    Allow: "GET, POST, DELETE",
  });
}
