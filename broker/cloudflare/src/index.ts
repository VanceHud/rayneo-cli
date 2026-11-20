import { DurableObject } from "cloudflare:workers";
import {
  applySanitizedTokenUpdate,
  buildSecurityHeaders,
  clearTokenStateFromSql,
  computePurgeAlarmMs,
  deriveStorageAesKey,
  handleBrokerRequest,
  initTokenStateSchema,
  isPastPurgeGrace,
  readEncryptedTokenStateFromSql,
  readTokenExpiryFromSql,
  resolveEncryptionKeyMaterial,
  writeEncryptedTokenStateToSql,
  type BrokerStore,
  type RateLimiterLike,
  type SanitizedTokenUpsert,
  type StoredTokenState,
  type UpsertTokenOutcome,
} from "./core.ts";

export interface Env {
  TOKEN_BROKER: DurableObjectNamespace<TokenBrokerDO>;
  BROKER_RATE_LIMITER?: RateLimiterLike;
  /** Legacy/shared full-access secret (GET, POST, DELETE). */
  BROKER_SECRET?: string;
  /** Optional read-only secret (GET /v1/token only). */
  BROKER_READ_SECRET?: string;
  /** Optional write secret (POST & DELETE /v1/token only). */
  BROKER_WRITE_SECRET?: string;
  /** Optional dedicated at-rest encryption key (defaults to HKDF from configured broker secret). */
  BROKER_ENCRYPTION_KEY?: string;
}

/**
 * SQLite-backed Durable Object holding the single-slot access-token mirror
 * with strong consistency, AES-256-GCM at-rest encryption for sensitive fields,
 * and automatic 24h post-expiry purge via DO Alarms.
 */
export class TokenBrokerDO extends DurableObject<Env> {
  private storageKeyPromise: Promise<CryptoKey | null> | null = null;
  private mutationQueue: Promise<unknown> = Promise.resolve();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.ctx.blockConcurrencyWhile(async () => {
      initTokenStateSchema(this.ctx.storage.sql);
    });
  }

  private getStorageKey(): Promise<CryptoKey | null> {
    if (!this.storageKeyPromise) {
      const material = resolveEncryptionKeyMaterial(this.env);
      this.storageKeyPromise = material ? deriveStorageAesKey(material) : Promise.resolve(null);
    }
    return this.storageKeyPromise;
  }

  /**
   * Serializes read-modify-write mutations, clears, and alarm purges inside the
   * DO instance so async Web Crypto encrypt/decrypt never interleaves across calls.
   */
  private runExclusive<T>(fn: () => Promise<T>): Promise<T> {
    const next = this.mutationQueue.then(fn, fn);
    this.mutationQueue = next.then(
      () => undefined,
      () => undefined
    );
    return next;
  }

  async getTokenState(nowMs: number = Date.now()): Promise<StoredTokenState | null> {
    const key = await this.getStorageKey();
    const state = await readEncryptedTokenStateFromSql(this.ctx.storage.sql, key, () => {
      console.warn(
        JSON.stringify({
          event: "token_decrypt_failed",
          reason: "key_rotated_or_corrupt",
        })
      );
    });
    if (!state) {
      return null;
    }
    if (isPastPurgeGrace(state.expires_at, nowMs)) {
      await this.clearToken();
      return null;
    }
    return state;
  }

  async upsertToken(
    input: SanitizedTokenUpsert,
    nowMs: number = Date.now()
  ): Promise<UpsertTokenOutcome> {
    return this.runExclusive(async () => {
      const key = await this.getStorageKey();
      if (!key) {
        return { ok: false, status: 503, error: "service unavailable" };
      }

      const effectiveNowMs = Math.max(nowMs, Date.now());
      const existing = await readEncryptedTokenStateFromSql(this.ctx.storage.sql, key, () => {
        console.warn(
          JSON.stringify({
            event: "token_decrypt_failed",
            reason: "key_rotated_or_corrupt",
          })
        );
      });
      const activeExisting =
        existing && !isPastPurgeGrace(existing.expires_at, effectiveNowMs) ? existing : null;

      const outcome = applySanitizedTokenUpdate(activeExisting, input, effectiveNowMs);
      if (outcome.ok && outcome.response.updated) {
        await writeEncryptedTokenStateToSql(this.ctx.storage.sql, outcome.state, key);
        const alarmAt = computePurgeAlarmMs(outcome.state.expires_at, effectiveNowMs);
        await this.ctx.storage.setAlarm(alarmAt);
      }
      return outcome;
    });
  }

  async clearToken(): Promise<void> {
    return this.runExclusive(async () => {
      clearTokenStateFromSql(this.ctx.storage.sql);
      await this.ctx.storage.deleteAlarm();
    });
  }

  override async alarm(): Promise<void> {
    return this.runExclusive(async () => {
      const expiresAt = readTokenExpiryFromSql(this.ctx.storage.sql);
      if (expiresAt === null) {
        return;
      }
      const nowMs = Date.now();
      if (isPastPurgeGrace(expiresAt, nowMs)) {
        clearTokenStateFromSql(this.ctx.storage.sql);
        console.log(JSON.stringify({ event: "token_alarm_purge", cleared: true }));
      } else {
        await this.ctx.storage.setAlarm(computePurgeAlarmMs(expiresAt, nowMs));
      }
    });
  }
}

function createLazyDoStore(getStub: () => DurableObjectStub<TokenBrokerDO>): BrokerStore {
  return {
    getState: async (nowMs?: number) => await getStub().getTokenState(nowMs),
    saveState: async () => {
      throw new Error("saveState unused when atomic upsertSanitized RPC is provided");
    },
    clearState: async () => {
      await getStub().clearToken();
    },
    upsertSanitized: async (input: SanitizedTokenUpsert, nowMs: number) =>
      await getStub().upsertToken(input, nowMs),
  };
}

export default {
  async fetch(request: Request, env: Env, _ctx: ExecutionContext): Promise<Response> {
    const url = new URL(request.url);
    const isHttps = url.protocol === "https:";
    try {
      const store = createLazyDoStore(() => env.TOKEN_BROKER.getByName("default"));
      return await handleBrokerRequest(request, {
        secrets: {
          shared: env.BROKER_SECRET,
          read: env.BROKER_READ_SECRET,
          write: env.BROKER_WRITE_SECRET,
          encryptionKey: env.BROKER_ENCRYPTION_KEY,
        },
        rateLimiter: env.BROKER_RATE_LIMITER,
        store,
      });
    } catch (error) {
      const errorName = error instanceof Error ? error.name : "UnknownError";
      console.error(
        JSON.stringify({
          event: "unhandled_error",
          path: url.pathname,
          error_name: errorName,
        })
      );
      return new Response(JSON.stringify({ ok: false, error: "internal server error" }), {
        status: 500,
        headers: buildSecurityHeaders(isHttps, {
          "Content-Type": "application/json; charset=utf-8",
        }),
      });
    }
  },
} satisfies ExportedHandler<Env>;
