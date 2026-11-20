import { bindings, defineConfig, exports } from "cf/config";

const customDomains = ((globalThis as { process?: { env?: Record<string, string | undefined> } })
  .process?.env?.BROKER_CUSTOM_DOMAIN ?? "")
  .split(",")
  .map((s) => s.trim())
  .filter(Boolean)
  .map((hostname) => ({ hostname, production: true, previews: false }));

export default defineConfig({
  worker: {
    name: "rayneo-token-broker",
    entrypoint: "./src/index.ts",
    compatibilityDate: "2026-11-20",
    compatibilityFlags: ["nodejs_compat"],
    ...(customDomains.length > 0 ? { domains: customDomains } : {}),
    exports: {
      TokenBrokerDO: exports.durableObject({ storage: "sqlite" }),
    },
    env: {
      TOKEN_BROKER: bindings.durableObject({
        worker: "rayneo-token-broker",
        exportName: "TokenBrokerDO",
      }),
      BROKER_SECRET: bindings.secret(),
    },
    observability: {
      enabled: true,
      logs: {
        enabled: true,
        headSamplingRate: 1,
      },
      traces: {
        enabled: true,
        headSamplingRate: 0.1,
      },
    },
  },
});
