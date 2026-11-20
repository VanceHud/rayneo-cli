import { DurableObject } from "cloudflare:workers";
import {
  applyTokenUpdateWithFp,
  clearTokenStateFromSql,
  computeTokenFingerprint,
  extractValidAccessToken,
  handleBrokerRequest,
  initTokenStateSchema,
  readTokenStateFromSql,
  writeTokenStateToSql,
  type BrokerStore,
  type StoredTokenState,
  type UpsertTokenOutcome,
} from "./core.ts";

export interface Env {
  TOKEN_BROKER: DurableObjectNamespace<TokenBrokerDO>;
  BROKER_SECRET?: string;
}

/**
 * SQLite-backed Durable Object holding the single-slot access-token mirror
 * with strong consistency across Cloudflare's edge.
 */
export class TokenBrokerDO extends DurableObject<Env> {
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.ctx.blockConcurrencyWhile(async () => {
      initTokenStateSchema(this.ctx.storage.sql);
    });
  }

  async getTokenState(): Promise<StoredTokenState | null> {
    return readTokenStateFromSql(this.ctx.storage.sql);
  }

  async upsertTokenJson(rawJson: string, nowMs: number = Date.now()): Promise<UpsertTokenOutcome> {
    let rawPayload: unknown;
    try {
      rawPayload = JSON.parse(rawJson);
    } catch {
      return { ok: false, status: 400, error: "invalid json body" };
    }

    // Validate access_token and compute SHA-256 fingerprint before touching SQLite so
    // the subsequent read -> merge -> write executes synchronously with zero awaits in between.
    const tokenCheck = extractValidAccessToken(rawPayload);
    if (!tokenCheck.ok) {
      return tokenCheck;
    }
    const tokenFp = await computeTokenFingerprint(tokenCheck.accessToken);

    const existing = readTokenStateFromSql(this.ctx.storage.sql);
    const outcome = applyTokenUpdateWithFp(existing, rawPayload, tokenFp, nowMs);
    if (outcome.ok && outcome.response.updated) {
      writeTokenStateToSql(this.ctx.storage.sql, outcome.state);
    }
    return outcome;
  }

  async clearToken(): Promise<void> {
    clearTokenStateFromSql(this.ctx.storage.sql);
  }
}

function createLazyDoStore(getStub: () => DurableObjectStub<TokenBrokerDO>): BrokerStore {
  return {
    getState: async () => await getStub().getTokenState(),
    saveState: async () => {
      throw new Error("saveState unused when atomic upsertJson RPC is provided");
    },
    clearState: async () => {
      await getStub().clearToken();
    },
    upsertJson: async (rawJson: string, nowMs: number) => await getStub().upsertTokenJson(rawJson, nowMs),
  };
}

export default {
  async fetch(request: Request, env: Env, _ctx: ExecutionContext): Promise<Response> {
    try {
      const store = createLazyDoStore(() => env.TOKEN_BROKER.getByName("default"));
      return await handleBrokerRequest(request, {
        secret: env.BROKER_SECRET,
        store,
      });
    } catch (error) {
      const message = error instanceof Error ? error.message : "unknown error";
      console.error(
        JSON.stringify({
          event: "unhandled_error",
          path: new URL(request.url).pathname,
          error: message,
        })
      );
      return new Response(JSON.stringify({ ok: false, error: "internal server error" }), {
        status: 500,
        headers: {
          "Content-Type": "application/json; charset=utf-8",
          "Cache-Control": "no-store",
          "X-Content-Type-Options": "nosniff",
        },
      });
    }
  },
} satisfies ExportedHandler<Env>;
