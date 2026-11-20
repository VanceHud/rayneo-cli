import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

const require = createRequire(import.meta.url);
const scriptPath = path.join(__dirname, "rayneo-token-sync.js");
const pluginPath = path.join(__dirname, "rayneo-token-sync.plugin");
const syncScript = require(scriptPath);
const rawScriptSource = fs.readFileSync(scriptPath, "utf8");
const rawPluginSource = fs.readFileSync(pluginPath, "utf8");

function createMockStore(initial = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    read(key) {
      return data.has(key) ? data.get(key) : null;
    },
    write(value, key) {
      data.set(key, String(value));
      return true;
    },
  };
}

function createHarness(opts = {}) {
  const store = opts.store || createMockStore();
  const posts = [];
  const notifications = [];
  const logs = [];
  const doneCalls = [];
  let nowMs = opts.nowMs ?? Date.parse("2026-11-20T11:30:00.000Z");

  const defaultPostImpl = (options, cb) => {
    posts.push({
      ...options,
      parsedBody: JSON.parse(options.body),
    });
    cb(
      null,
      { status: 200 },
      JSON.stringify({
        ok: true,
        updated: true,
        revision: posts.length,
        token_fp: "a1b2c3d4e5f6",
        updated_at: new Date(nowMs).toISOString(),
        expires_at: new Date(nowMs + 7200 * 1000).toISOString(),
        expires_estimated: false,
      })
    );
  };

  let postImpl = opts.postImpl || defaultPostImpl;

  function runCall(callOpts = {}) {
    const startDoneCount = doneCalls.length;
    const env = {
      $request: callOpts.$request,
      $response: callOpts.$response,
      $argument:
        callOpts.$argument !== undefined
          ? callOpts.$argument
          : {
              broker_url: "https://broker.example.test",
              broker_secret: "test-broker-secret-xyz",
              notify: opts.notify ?? false,
              debug: opts.debug ?? false,
            },
      $persistentStore: callOpts.$persistentStore || store,
      $httpClient: callOpts.$httpClient || {
        post(options, cb) {
          return postImpl(options, cb);
        },
      },
      $notification: {
        post(title, subtitle, body) {
          notifications.push({ title, subtitle, body });
        },
      },
      $done(result) {
        doneCalls.push(result);
      },
      now: () => nowMs,
      log(msg) {
        logs.push(msg);
      },
    };
    syncScript.run(env);
    assert.equal(
      doneCalls.length,
      startDoneCount + 1,
      "$done must be called exactly once per invocation"
    );
    assert.deepEqual(
      doneCalls[doneCalls.length - 1],
      {},
      "$done must always receive empty object {} to pass traffic through unmodified"
    );
  }

  return {
    store,
    posts,
    notifications,
    logs,
    doneCalls,
    setNow(ms) {
      nowMs = ms;
    },
    advanceMs(delta) {
      nowMs += delta;
    },
    setPostImpl(fn) {
      postImpl = fn;
    },
    runCall,
  };
}

// ---------------------------------------------------------------------------
// 1. Loon .plugin file structure & regex tests
// ---------------------------------------------------------------------------

test(".plugin file contains required metadata, arguments, script rules, and MITM hostnames", () => {
  for (const meta of ["#!name", "#!desc", "#!author", "#!homepage", "#!tag"]) {
    assert.ok(rawPluginSource.includes(meta), `missing metadata header ${meta}`);
  }
  assert.match(rawPluginSource, /\[Argument\]/);
  assert.match(rawPluginSource, /^broker_url\s*=\s*input/m);
  assert.match(rawPluginSource, /^broker_secret\s*=\s*input/m);
  assert.match(rawPluginSource, /^notify\s*=\s*switch,\s*false/m);
  assert.match(rawPluginSource, /^debug\s*=\s*switch,\s*false/m);

  assert.match(rawPluginSource, /\[Script\]/);
  assert.match(
    rawPluginSource,
    /https:\/\/raw\.githubusercontent\.com\/VanceHud\/rayneo-cli\/main\/loon\/rayneo-token-sync\.js/
  );
  assert.match(rawPluginSource, /requires-body\s*=\s*true/);
  assert.match(rawPluginSource, /timeout\s*=\s*10/);
  assert.match(
    rawPluginSource,
    /argument=\[\{broker_url\},\{broker_secret\},\{notify\},\{debug\}\]/
  );

  assert.match(rawPluginSource, /\[MITM\]/);
  assert.match(
    rawPluginSource,
    /hostname\s*=\s*xr-api-p\.rayneo\.cn,\s*xr-api-\*\.rayneo\.cn,\s*xr-api-\*\.rayneo\.com/
  );
});

test(".plugin URL regexes accurately match/exclude real RayNeo API paths", () => {
  const respMatch = rawPluginSource.match(/^http-response\s+(\S+)\s+/m);
  const reqMatch = rawPluginSource.match(/^http-request\s+(\S+)\s+/m);
  assert.ok(respMatch, "http-response rule not found in .plugin");
  assert.ok(reqMatch, "http-request rule not found in .plugin");

  const respRe = new RegExp(respMatch[1]);
  const reqRe = new RegExp(reqMatch[1]);

  const cases = [
    {
      url: "https://xr-api-p.rayneo.cn/authapi/token/refresh",
      expectResp: true,
      expectReq: false,
    },
    {
      url: "https://xr-api-p.rayneo.cn/authapi/token/refresh?ts=1732100000",
      expectResp: true,
      expectReq: false,
    },
    {
      url: "https://xr-api-p.rayneo.cn/identify/quickLogin",
      expectResp: true,
      expectReq: false,
    },
    {
      url: "https://xr-api-p.rayneo.cn/identify/quickLogin?lang=zh",
      expectResp: true,
      expectReq: false,
    },
    {
      url: "https://xr-api-p.rayneo.cn/identify/common/sendVerifyCode",
      expectResp: false,
      expectReq: false,
    },
    {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events?date=2026-11-20",
      expectResp: false,
      expectReq: true,
    },
    {
      url: "https://xr-api-p.rayneo.cn/clsapi/aiTask/v2/selectPage",
      expectResp: false,
      expectReq: true,
    },
    {
      url: "https://xr-api-us.rayneo.com/lifelog/api/v1/events?date=2026-11-20",
      expectResp: false,
      expectReq: true,
    },
    {
      url: "https://xr-api-eu-1.rayneo.com/authapi/token/refresh",
      expectResp: true,
      expectReq: false,
    },
    {
      url: "https://example.com/lifelog/api/v1/events",
      expectResp: false,
      expectReq: false,
    },
  ];

  for (const c of cases) {
    assert.equal(
      respRe.test(c.url),
      c.expectResp,
      `http-response regex mismatch for ${c.url}`
    );
    assert.equal(
      reqRe.test(c.url),
      c.expectReq,
      `http-request regex mismatch for ${c.url}`
    );
  }
});

// ---------------------------------------------------------------------------
// 2. Security invariants: zero refreshToken leakage & strict header allowlist
// ---------------------------------------------------------------------------

test("SECURITY: http-response on /authapi/token/refresh NEVER reads or leaks refreshToken from request header or response body", () => {
  const SECRET_REQ_REFRESH = "REFRESH_TOKEN_IN_XTCL_HEADER_DO_NOT_LEAK";
  const SECRET_BODY_REFRESH = "REFRESH_TOKEN_IN_BODY_DO_NOT_LEAK";
  const NEW_ACCESS_TOKEN = "ACCESS_TOKEN_FRESH_0001_SAFE";
  const BROKER_SECRET = "SUPER_SECRET_BROKER_KEY_999";

  const h = createHarness({ notify: true, debug: true });
  h.runCall({
    $argument: {
      broker_url: "https://broker.example.test/",
      broker_secret: BROKER_SECRET,
      notify: true,
      debug: true,
    },
    $request: {
      url: "https://xr-api-p.rayneo.cn/authapi/token/refresh",
      headers: {
        // On /authapi/token/refresh, RayNeo puts refreshToken in xtcl-access-token!
        "xtcl-access-token": SECRET_REQ_REFRESH,
        "User-Agent": "RayNeo/3.5.0 (iPhone; iOS 18.1)",
        "Device-Code": "DEV-CODE-MOCK-01",
        "Xtcl-Authorization": "signed-do-not-forward",
        Cookie: "session=secret",
      },
    },
    $response: {
      status: 200,
      body: JSON.stringify({
        code: 0,
        msg: "操作成功",
        data: {
          token: NEW_ACCESS_TOKEN,
          refreshToken: SECRET_BODY_REFRESH,
          refresh_token: SECRET_BODY_REFRESH,
          expiresIn: 7200,
        },
      }),
    },
  });

  assert.equal(h.posts.length, 1);
  const post = h.posts[0];
  assert.equal(post.url, "https://broker.example.test/v1/token");
  assert.equal(post.headers.Authorization, `Bearer ${BROKER_SECRET}`);
  assert.equal(post.parsedBody.access_token, NEW_ACCESS_TOKEN);
  assert.equal(post.parsedBody.expires_in, 7200);
  assert.equal(post.parsedBody.source, "loon:refresh");
  assert.equal(post.parsedBody.base_url, "https://xr-api-p.rayneo.cn");
  assert.equal(post.parsedBody.device_id, "DEV-CODE-MOCK-01");
  assert.deepEqual(post.parsedBody.client_headers, {
    "user-agent": "RayNeo/3.5.0 (iPhone; iOS 18.1)",
    "device-code": "DEV-CODE-MOCK-01",
  });
  assert.equal("refreshToken" in post.parsedBody, false);
  assert.equal("refresh_token" in post.parsedBody, false);

  // Verify zero leakage across body, persistent store, logs, and notifications
  const allSerialized = JSON.stringify({
    body: post.body,
    store: Object.fromEntries(h.store.data),
    logs: h.logs,
    notifications: h.notifications,
  });
  assert.equal(allSerialized.includes(SECRET_REQ_REFRESH), false);
  assert.equal(allSerialized.includes(SECRET_BODY_REFRESH), false);

  const logsAndNotify = JSON.stringify({
    logs: h.logs,
    notifications: h.notifications,
  });
  assert.equal(logsAndNotify.includes(BROKER_SECRET), false);
  assert.equal(logsAndNotify.includes(NEW_ACCESS_TOKEN), false);
});

test("SECURITY: http-request JS defense-in-depth blocks /authapi/token/refresh and login paths even if invoked directly", () => {
  const blockedUrls = [
    "https://xr-api-p.rayneo.cn/authapi/token/refresh",
    "https://xr-api-p.rayneo.cn/authapi/token/refresh?foo=bar",
    "https://xr-api-p.rayneo.cn/AUTHAPI/TOKEN/REFRESH",
    "https://xr-api-p.rayneo.cn//authapi//token//refresh",
    "https://xr-api-p.rayneo.cn/authapi/%74oken/refresh",
    "https://xr-api-p.rayneo.cn/identify/quickLogin",
    "https://xr-api-p.rayneo.cn/identify/common/sendVerifyCode",
    "https://xr-api-p.rayneo.cn/identify/login",
    "https://evil.example.com/lifelog/api/v1/events",
  ];

  const h = createHarness();
  for (const url of blockedUrls) {
    h.runCall({
      $request: {
        url,
        headers: {
          "xtcl-access-token": "SHOULD_NEVER_BE_SNIFFED",
          "user-agent": "RayNeo/3.5.0",
        },
      },
    });
  }

  assert.equal(h.posts.length, 0);
  assert.equal(h.store.data.size, 0);
});

test("SECURITY: client_headers enforces exact 15-header allowlist from internal/har/har.go", () => {
  const expectedAllowed = [
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
  ];
  assert.deepEqual(
    Object.keys(syncScript.ALLOWED_CLIENT_HEADERS).sort(),
    [...expectedAllowed].sort()
  );

  const h = createHarness();
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events?date=2026-11-20",
      headers: {
        "XTCL-Access-Token": "ACCESS_TOK_ALLOWLIST_TEST",
        "Xtcl-App": "rayneo-ios",
        "Xtcl-Authorization": "hmac-sig",
        "Xtcl-Timestamp": "1732100000",
        Authorization: "Bearer should-not-forward",
        Host: "xr-api-p.rayneo.cn",
        "Content-Type": "application/json",
        "Content-Length": "42",
        "Accept-Encoding": "gzip, deflate, br",
        Cookie: "sid=secret",
        "Refresh-Token": "nope",
        "X-Custom-Header": "dropped",
        constructor: "prototype-guard",
        // All 15 allowed headers (mixed case):
        "User-Agent": "RayNeo/3.5.0",
        Platform: "iOS",
        Env: "prod",
        Locale: "zh_CN",
        PhysicalRegion: "CN",
        "Time-Zone": "Asia/Shanghai",
        "App-Version": "3.5.0",
        Version: "350",
        "App-Build-Number": "35012",
        "Phone-OS-Version": "18.1",
        "Phone-Model": "iPhone16,2",
        "Device-Id": "dev-1",
        DeviceID: "dev-2",
        Device_Id: "dev-3",
        "Device-Code": "dev-code-primary",
        // Metadata headers (not in client_headers allowlist, mapped to top-level fields)
        Region: "cn",
        "Account-Id": "90012345",
      },
    },
  });

  assert.equal(h.posts.length, 1);
  const body = h.posts[0].parsedBody;
  assert.equal(body.region, "cn");
  assert.equal(body.account_id, 90012345);
  assert.equal(body.device_id, "dev-code-primary");
  assert.deepEqual(
    Object.keys(body.client_headers).sort(),
    [...expectedAllowed].sort()
  );
  assert.equal(body.client_headers["user-agent"], "RayNeo/3.5.0");
  assert.equal(body.client_headers["device-code"], "dev-code-primary");
  assert.equal("xtcl-access-token" in body.client_headers, false);
  assert.equal("authorization" in body.client_headers, false);
  assert.equal("accept-encoding" in body.client_headers, false);
  assert.equal("host" in body.client_headers, false);
  assert.equal("cookie" in body.client_headers, false);
  assert.equal("content-type" in body.client_headers, false);
  assert.equal("region" in body.client_headers, false);
});

// ---------------------------------------------------------------------------
// 3. Deduplication, fast-path, and metadata enrichment lifecycle
// ---------------------------------------------------------------------------

test("LIFECYCLE: http-response refresh -> single http-request enrichment (no expires_in) -> instant fast-path -> rotation", () => {
  const h = createHarness({ notify: true });

  // Step 1: /authapi/token/refresh http-response (no request headers exposed)
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/authapi/token/refresh",
    },
    $response: {
      status: 200,
      body: JSON.stringify({
        code: 0,
        msg: "操作成功",
        data: { token: "TOK_V1_ABCDEF123456", expiresIn: 7200 },
      }),
    },
  });
  assert.equal(h.posts.length, 1);
  assert.equal(h.posts[0].parsedBody.source, "loon:refresh");
  assert.equal(h.posts[0].parsedBody.expires_in, 7200);
  assert.equal(h.posts[0].parsedBody.access_token, "TOK_V1_ABCDEF123456");
  assert.equal("client_headers" in h.posts[0].parsedBody, false);
  assert.equal("region" in h.posts[0].parsedBody, false);
  assert.equal("account" in h.posts[0].parsedBody, false);
  assert.equal("account_id" in h.posts[0].parsedBody, false);
  assert.equal("device_id" in h.posts[0].parsedBody, false);
  assert.equal(h.notifications.length, 1);

  // Step 1b: duplicate http-response for same token is skipped
  h.runCall({
    $request: { url: "https://xr-api-p.rayneo.cn/authapi/token/refresh" },
    $response: {
      status: 200,
      body: JSON.stringify({
        code: 0,
        data: { token: "TOK_V1_ABCDEF123456", expiresIn: 7200 },
      }),
    },
  });
  assert.equal(h.posts.length, 1);

  // Step 2: first business http-request with TOK_V1 enriches full headers ONCE without expires_in
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events?date=2026-11-20",
      headers: {
        "xtcl-access-token": "TOK_V1_ABCDEF123456",
        "user-agent": "RayNeo/3.5.0",
        "app-version": "3.5.0",
        "phone-model": "iPhone16,2",
        "device-code": "DEV-XYZ",
        region: "cn",
        "account-id": "12345678",
      },
    },
  });
  assert.equal(h.posts.length, 2);
  const enrichBody = h.posts[1].parsedBody;
  assert.equal(enrichBody.source, "loon:header");
  assert.equal(enrichBody.access_token, "TOK_V1_ABCDEF123456");
  assert.equal("expires_in" in enrichBody, false, "enrichment MUST omit expires_in");
  assert.equal(enrichBody.region, "cn");
  assert.equal(enrichBody.account_id, 12345678);
  assert.equal(enrichBody.device_id, "DEV-XYZ");
  assert.deepEqual(enrichBody.client_headers, {
    "user-agent": "RayNeo/3.5.0",
    "app-version": "3.5.0",
    "phone-model": "iPhone16,2",
    "device-code": "DEV-XYZ",
  });
  // Enrichment must NOT spam a duplicate notification!
  assert.equal(h.notifications.length, 1);

  // Step 3: next 5 business http-requests hit instant fast-path with ZERO HTTP calls
  for (let i = 0; i < 5; i++) {
    h.runCall({
      $request: {
        url: "https://xr-api-p.rayneo.cn/clsapi/aiTask/v2/selectPage",
        headers: {
          "xtcl-access-token": "TOK_V1_ABCDEF123456",
          "user-agent": "RayNeo/3.5.0",
        },
      },
    });
  }
  assert.equal(h.posts.length, 2, "fast-path must make zero $httpClient calls");

  // Step 4: token rotates to TOK_V2 on /authapi/token/refresh
  h.advanceMs(7000 * 1000);
  h.runCall({
    $request: { url: "https://xr-api-p.rayneo.cn/authapi/token/refresh" },
    $response: {
      status: 200,
      body: JSON.stringify({
        code: 0,
        data: { token: "TOK_V2_NEWROTATED999", expiresIn: 7200 },
      }),
    },
  });
  assert.equal(h.posts.length, 3);
  assert.equal(h.posts[2].parsedBody.access_token, "TOK_V2_NEWROTATED999");
  assert.equal(h.posts[2].parsedBody.source, "loon:refresh");
  assert.equal(h.posts[2].parsedBody.expires_in, 7200);
  assert.equal(h.notifications.length, 2);

  // Step 5: first http-request with TOK_V2 enriches metadata once, then fast-path resumes
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events?date=2026-11-20",
      headers: {
        "xtcl-access-token": "TOK_V2_NEWROTATED999",
        "app-version": "3.5.0",
      },
    },
  });
  assert.equal(h.posts.length, 4);
  assert.equal(h.posts[3].parsedBody.source, "loon:header");
  assert.equal("expires_in" in h.posts[3].parsedBody, false);
  assert.equal(h.notifications.length, 2);

  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events?date=2026-11-20",
      headers: {
        "xtcl-access-token": "TOK_V2_NEWROTATED999",
        "app-version": "3.5.0",
      },
    },
  });
  assert.equal(h.posts.length, 4);
});

test("LIFECYCLE: quickLogin http-response extracts account (stripping +86-), account_id, and ignores refreshToken", () => {
  const h = createHarness({ notify: true });
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/identify/quickLogin",
      headers: {
        "user-agent": "RayNeo/3.5.0",
        region: "cn",
      },
    },
    $response: {
      status: 200,
      body: JSON.stringify({
        code: 0,
        data: {
          token: "LOGIN_ACCESS_TOKEN_111",
          refreshToken: "LOGIN_REFRESH_TOKEN_SECRET",
          expiresIn: 7200,
          userInfo: {
            accountId: 88776655,
            phone: "+86-13800138000",
          },
        },
      }),
    },
  });

  assert.equal(h.posts.length, 1);
  const body = h.posts[0].parsedBody;
  assert.equal(body.source, "loon:login");
  assert.equal(body.access_token, "LOGIN_ACCESS_TOKEN_111");
  assert.equal(body.expires_in, 7200);
  assert.equal(body.account, "13800138000");
  assert.equal(body.account_id, 88776655);
  assert.equal(body.region, "cn");
  assert.equal(h.posts[0].body.includes("LOGIN_REFRESH_TOKEN_SECRET"), false);
  assert.equal(h.notifications.length, 1);
});

test("LIFECYCLE: brand new token first seen in http-request syncs immediately with loon:header and then fast-paths", () => {
  const h = createHarness({ notify: true });

  h.runCall({
    $request: {
      url: "https://xr-api-us.rayneo.com/lifelog/api/v1/events?accountId=424242",
      headers: {
        "xtcl-access-token": "BRAND_NEW_HEADER_TOK",
        "device-id": "fallback-dev-id",
        platform: "iOS",
      },
    },
  });

  assert.equal(h.posts.length, 1);
  const body = h.posts[0].parsedBody;
  assert.equal(body.source, "loon:header");
  assert.equal(body.access_token, "BRAND_NEW_HEADER_TOK");
  assert.equal("expires_in" in body, false);
  assert.equal(body.base_url, "https://xr-api-us.rayneo.com");
  assert.equal(body.device_id, "fallback-dev-id");
  assert.equal(body.account_id, 424242);
  assert.deepEqual(body.client_headers, {
    "device-id": "fallback-dev-id",
    platform: "iOS",
  });
  assert.equal(h.notifications.length, 1);

  // Second request hits instant fast-path
  h.runCall({
    $request: {
      url: "https://xr-api-us.rayneo.com/lifelog/api/v1/events",
      headers: {
        "xtcl-access-token": "BRAND_NEW_HEADER_TOK",
        platform: "iOS",
      },
    },
  });
  assert.equal(h.posts.length, 1);
});

test("LIFECYCLE: changing broker_url invalidates cached dedup state and pushes immediately to new broker", () => {
  const h = createHarness();

  h.runCall({
    $argument: {
      broker_url: "https://broker-a.example.test",
      broker_secret: "sec-a",
    },
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: {
        "xtcl-access-token": "TOK_MIGRATE_01",
        "user-agent": "RayNeo/3.5.0",
      },
    },
  });
  assert.equal(h.posts.length, 1);
  assert.equal(h.posts[0].url, "https://broker-a.example.test/v1/token");

  // Switch to Broker B with the same token -> must sync immediately!
  h.runCall({
    $argument: {
      broker_url: "https://broker-b.example.test/",
      broker_secret: "sec-b",
    },
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: {
        "xtcl-access-token": "TOK_MIGRATE_01",
        "user-agent": "RayNeo/3.5.0",
      },
    },
  });
  assert.equal(h.posts.length, 2);
  assert.equal(h.posts[1].url, "https://broker-b.example.test/v1/token");
  assert.equal(h.posts[1].headers.Authorization, "Bearer sec-b");
});

// ---------------------------------------------------------------------------
// 4. Argument parsing & fallback config
// ---------------------------------------------------------------------------

test("ARGUMENT PARSING: handles object, query-string (preserving base64 +/=), JSON string, placeholders, and $persistentStore fallback", () => {
  // Query string with trailing slash, base64 secret containing + and ==, and boolean strings
  const qStore = createMockStore();
  const cfgQuery = syncScript.resolveConfig(
    "broker_url=https://broker.example.test/v1/token/&broker_secret=ab+cd/ef==&notify=true&debug=1",
    { read: (k) => qStore.read(k) }
  );
  assert.equal(cfgQuery.brokerUrl, "https://broker.example.test");
  assert.equal(cfgQuery.brokerSecret, "ab+cd/ef==");
  assert.equal(cfgQuery.notify, true);
  assert.equal(cfgQuery.debug, true);

  // JSON string
  const cfgJson = syncScript.resolveConfig(
    JSON.stringify({
      broker_url: "http://100.64.0.2:8787/",
      broker_secret: "json-secret",
      notify: false,
      debug: "true",
    }),
    { read: () => "" }
  );
  assert.equal(cfgJson.brokerUrl, "http://100.64.0.2:8787");
  assert.equal(cfgJson.brokerSecret, "json-secret");
  assert.equal(cfgJson.notify, false);
  assert.equal(cfgJson.debug, true);

  // Unexpanded placeholders fall back cleanly to $persistentStore
  const fallbackStore = createMockStore({
    rayneo_sync_broker_url: "https://fallback.example.test///",
    rayneo_sync_broker_secret: "fallback-secret",
    rayneo_sync_notify: "true",
    rayneo_sync_debug: "false",
  });
  const cfgFallback = syncScript.resolveConfig(
    {
      broker_url: "{broker_url}",
      broker_secret: "${broker_secret}",
    },
    { read: (k) => fallbackStore.read(k) || "" }
  );
  assert.equal(cfgFallback.brokerUrl, "https://fallback.example.test");
  assert.equal(cfgFallback.brokerSecret, "fallback-secret");
  assert.equal(cfgFallback.notify, true);
  assert.equal(cfgFallback.debug, false);

  // Missing config makes zero HTTP calls and still completes cleanly
  const h = createHarness();
  h.runCall({
    $argument: { broker_url: "", broker_secret: "" },
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "SOME_TOKEN" },
    },
  });
  assert.equal(h.posts.length, 0);
});

// ---------------------------------------------------------------------------
// 5. Error resilience, failure backoff, and invalid input handling
// ---------------------------------------------------------------------------

test("RESILIENCE: broker failure triggers short cooldown on same token, retries after cooldown, and never blocks app", () => {
  const h = createHarness();
  let attempts = 0;

  // Broker unreachable
  h.setPostImpl((options, cb) => {
    attempts++;
    cb("The request timed out.", null, null);
  });

  // First attempt fails -> LAST_TOKEN must NOT be written
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "TOK_RETRY_TEST", platform: "iOS" },
    },
  });
  assert.equal(attempts, 1);
  assert.equal(h.store.read(syncScript.STORAGE_KEYS.LAST_TOKEN), null);

  // Immediate second request (within 15s cooldown) -> skipped to protect app latency
  h.advanceMs(2000);
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "TOK_RETRY_TEST", platform: "iOS" },
    },
  });
  assert.equal(attempts, 1);

  // Different token immediately bypasses cooldown!
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "TOK_DIFFERENT", platform: "iOS" },
    },
  });
  assert.equal(attempts, 2);

  // After cooldown expires and broker recovers, TOK_RETRY_TEST succeeds
  h.advanceMs(syncScript.FAIL_COOLDOWN_MS + 1000);
  h.setPostImpl((options, cb) => {
    attempts++;
    h.posts.push({ ...options, parsedBody: JSON.parse(options.body) });
    cb(null, { status: 200 }, JSON.stringify({ ok: true, token_fp: "deadbeef1234" }));
  });

  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "TOK_RETRY_TEST", platform: "iOS" },
    },
  });
  assert.equal(attempts, 3);
  assert.equal(
    h.store.read(syncScript.STORAGE_KEYS.LAST_TOKEN),
    "TOK_RETRY_TEST"
  );
});

test("RESILIENCE: broker 401/500, sync throw in $httpClient, throwing $persistentStore, and bad RayNeo responses all call $done({})", () => {
  const h = createHarness();

  // Non-200 RayNeo auth response
  h.runCall({
    $request: { url: "https://xr-api-p.rayneo.cn/authapi/token/refresh" },
    $response: {
      status: 401,
      body: JSON.stringify({ code: 0, data: { token: "IGNORED" } }),
    },
  });
  assert.equal(h.posts.length, 0);

  // Non-zero RayNeo API code
  h.runCall({
    $request: { url: "https://xr-api-p.rayneo.cn/authapi/token/refresh" },
    $response: {
      status: 200,
      body: JSON.stringify({ code: 10401, data: { token: "IGNORED" } }),
    },
  });
  assert.equal(h.posts.length, 0);

  // Malformed JSON response body
  h.runCall({
    $request: { url: "https://xr-api-p.rayneo.cn/authapi/token/refresh" },
    $response: { status: 200, body: "<html>502 Bad Gateway</html>" },
  });
  assert.equal(h.posts.length, 0);

  // Broker returns HTTP 401 -> does not mark LAST_TOKEN
  h.setPostImpl((options, cb) => {
    cb(null, { status: 401 }, JSON.stringify({ ok: false, error: "unauthorized" }));
  });
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "TOK_UNAUTH_TEST" },
    },
  });
  assert.equal(h.store.read(syncScript.STORAGE_KEYS.LAST_TOKEN), null);

  // $httpClient.post throws synchronously
  h.runCall({
    $httpClient: {
      post() {
        throw new Error("boom in httpClient");
      },
    },
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "TOK_THROW_TEST" },
    },
  });

  // $persistentStore throws on read/write -> degrades gracefully
  let sawPost = false;
  h.runCall({
    $persistentStore: {
      read() {
        throw new Error("storage read failed");
      },
      write() {
        throw new Error("storage write failed");
      },
    },
    $httpClient: {
      post(options, cb) {
        sawPost = true;
        cb(null, { status: 200 }, JSON.stringify({ ok: true }));
      },
    },
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "TOK_BROKEN_STORE" },
    },
  });
  assert.equal(sawPost, true);
});

// ---------------------------------------------------------------------------
// 6. Pure Loon JavaScriptCore sandbox execution (no Node globals, no URL)
// ---------------------------------------------------------------------------

test("JSC SANDBOX: raw script runs top-level in minimal Loon sandbox (no module, no URL, no Node APIs)", () => {
  const store = createMockStore();
  const sent = [];
  const doneArgs = [];

  // 1. Run as http-response in minimal sandbox
  const respSandbox = {
    $request: {
      url: "https://xr-api-p.rayneo.cn/authapi/token/refresh",
      headers: {
        "xtcl-access-token": "REFRESH_SHOULD_NOT_LEAK",
        "user-agent": "RayNeo/3.5.0",
      },
    },
    $response: {
      status: 200,
      body: JSON.stringify({
        code: 0,
        data: { token: "JSC_SANDBOX_TOK_1", expiresIn: 7200 },
      }),
    },
    $argument: {
      broker_url: "https://broker.example.test",
      broker_secret: "jsc-secret",
      notify: false,
      debug: false,
    },
    $persistentStore: store,
    $httpClient: {
      post(options, cb) {
        sent.push(JSON.parse(options.body));
        cb(null, { status: 200 }, '{"ok":true,"token_fp":"112233445566"}');
      },
    },
    $notification: { post() {} },
    $done(arg) {
      doneArgs.push(arg);
    },
    console: { log() {} },
  };
  vm.runInNewContext(rawScriptSource, respSandbox);

  assert.equal(doneArgs.length, 1);
  assert.deepEqual({ ...doneArgs[0] }, {});
  assert.equal(sent.length, 1);
  assert.equal(sent[0].access_token, "JSC_SANDBOX_TOK_1");
  assert.equal(sent[0].expires_in, 7200);
  assert.equal(sent[0].source, "loon:refresh");

  // 2. Run as http-request (metadata enrichment) in minimal sandbox
  const reqSandbox = {
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events?date=2026-11-20",
      headers: {
        "xtcl-access-token": "JSC_SANDBOX_TOK_1",
        "device-code": "JSC-DEV-1",
        region: "cn",
      },
    },
    $argument: "broker_url=https://broker.example.test&broker_secret=jsc-secret",
    $persistentStore: store,
    $httpClient: {
      post(options, cb) {
        sent.push(JSON.parse(options.body));
        cb(null, { status: 200 }, '{"ok":true,"token_fp":"112233445566"}');
      },
    },
    $notification: { post() {} },
    $done(arg) {
      doneArgs.push(arg);
    },
    console: { log() {} },
  };
  vm.runInNewContext(rawScriptSource, reqSandbox);

  assert.equal(doneArgs.length, 2);
  assert.equal(sent.length, 2);
  assert.equal(sent[1].source, "loon:header");
  assert.equal("expires_in" in sent[1], false);
  assert.equal(sent[1].device_id, "JSC-DEV-1");

  // 3. Subsequent http-request hits fast-path with 0 HTTP calls
  vm.runInNewContext(rawScriptSource, reqSandbox);
  assert.equal(doneArgs.length, 3);
  assert.equal(sent.length, 2);
});

test("EDGE CASES: top-level quickLogin fields, delayed enrichment, header/query accountId variants, URL normalization, and refresh-failure rescue", () => {
  // Top-level data.accountId & data.email, string HTTP status
  const parsed = syncScript.parseAuthResponse(
    JSON.stringify({
      code: 0,
      data: {
        token: "TOP_LEVEL_TOK_1",
        refreshToken: "NEVER_READ_ME",
        expiresIn: "7200",
        accountId: 556677,
        email: "demo@example.test",
      },
    })
  );
  assert.deepEqual(parsed, {
    accessToken: "TOP_LEVEL_TOK_1",
    expiresIn: 7200,
    accountId: 556677,
    account: "demo@example.test",
  });
  assert.equal(syncScript.parseHttpStatus({ status: "HTTP/1.1 200 OK" }), 200);
  assert.equal(syncScript.parseHttpStatus({ statusCode: 204 }), 204);

  // All account-id header variants + query fallback
  assert.equal(syncScript.extractAccountId({ "account-id": "101" }, null), 101);
  assert.equal(syncScript.extractAccountId({ accountid: "102" }, null), 102);
  assert.equal(syncScript.extractAccountId({ account_id: "103" }, null), 103);
  assert.equal(syncScript.extractAccountId({ "xr-account-id": "104" }, null), 104);
  assert.equal(syncScript.extractAccountId({}, { accountId: "105" }), 105);

  // URL normalization edge cases
  assert.equal(
    syncScript.normalizeBrokerUrl("https://example.test/rayneo-broker/"),
    "https://example.test/rayneo-broker"
  );
  assert.equal(
    syncScript.normalizeBrokerUrl("http://100.64.0.10:8787/"),
    "http://100.64.0.10:8787"
  );
  assert.equal(syncScript.normalizeBrokerUrl("javascript:alert(1)"), "");

  // Refresh http-response fails -> next http-request rescues immediately!
  const h = createHarness();
  let failOnce = true;
  h.setPostImpl((options, cb) => {
    if (failOnce) {
      failOnce = false;
      cb("network offline", null, null);
      return;
    }
    h.posts.push({ ...options, parsedBody: JSON.parse(options.body) });
    cb(null, { status: 200 }, JSON.stringify({ ok: true, token_fp: "rescued12345" }));
  });

  h.runCall({
    $request: { url: "https://xr-api-p.rayneo.cn/authapi/token/refresh" },
    $response: {
      status: 200,
      body: JSON.stringify({
        code: 0,
        data: { token: "TOK_RESCUE_01", expiresIn: 7200 },
      }),
    },
  });
  assert.equal(h.posts.length, 0);

  // Next http-request rescues immediately without cooldown blocking it
  h.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: {
        "xtcl-access-token": "TOK_RESCUE_01",
        "user-agent": "RayNeo/3.5.0",
      },
    },
  });
  assert.equal(h.posts.length, 1);
  assert.equal(h.posts[0].parsedBody.access_token, "TOK_RESCUE_01");
  assert.equal(h.posts[0].parsedBody.source, "loon:header");

  // Delayed enrichment: http-response succeeds -> 1st http-request has no metadata (skipped) -> 2nd has metadata (enriches once) -> 3rd fast-paths
  const h2 = createHarness();
  h2.runCall({
    $request: { url: "https://xr-api-p.rayneo.cn/authapi/token/refresh" },
    $response: {
      status: 200,
      body: JSON.stringify({
        code: 0,
        data: { token: "TOK_DELAYED_META", expiresIn: 7200 },
      }),
    },
  });
  assert.equal(h2.posts.length, 1);

  // 1st http-request with only xtcl-access-token (no metadata) -> skipped
  h2.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/ping",
      headers: { "xtcl-access-token": "TOK_DELAYED_META" },
    },
  });
  assert.equal(h2.posts.length, 1);

  // 2nd http-request with metadata -> enriches once
  h2.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: {
        "xtcl-access-token": "TOK_DELAYED_META",
        "phone-model": "iPhone16,2",
      },
    },
  });
  assert.equal(h2.posts.length, 2);
  assert.equal("expires_in" in h2.posts[1].parsedBody, false);

  // 3rd http-request -> fast-path
  h2.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: {
        "xtcl-access-token": "TOK_DELAYED_META",
        "phone-model": "iPhone16,2",
      },
    },
  });
  assert.equal(h2.posts.length, 2);

  // Overlong token (>512 chars) rejected safely
  h2.runCall({
    $request: {
      url: "https://xr-api-p.rayneo.cn/lifelog/api/v1/events",
      headers: { "xtcl-access-token": "a".repeat(513) },
    },
  });
  assert.equal(h2.posts.length, 2);
});

