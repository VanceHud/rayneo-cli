import { bindings, defineConfig, exports } from "cf/config";

export default defineConfig({
  worker: {
    name: "rayneo-token-broker",
    entrypoint: "./src/index.ts",
    compatibilityDate: "2026-11-20",
    compatibilityFlags: ["nodejs_compat"],
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
