/**
 * RayNeo Token Sync — Loon Passive Read-Only Sync Script (Scheme A)
 *
 * Passively mirrors RayNeo App access tokens (`xtcl-access-token` / auth response
 * `data.token`) to a user-owned Token Broker (`POST <broker_url>/v1/token`).
 *
 * Security & Reliability Invariants:
 * 1. NEVER reads, stores, logs, or transmits `refreshToken` / `refresh_token`.
 * 2. STRICTLY excludes `/authapi/token/refresh` (and login/OTP paths) from
 *    `http-request` header sniffing because `/authapi/token/refresh` carries
 *    the `refreshToken` inside the `xtcl-access-token` request header.
 * 3. NEVER modifies or blocks RayNeo App traffic: always exits with `$done({})`.
 * 4. Instant <1ms fast-path on `http-request` when the current token and its
 *    metadata have already been synced to the configured broker.
 * 5. Pure JavaScriptCore-compatible (no `URL` / `URLSearchParams` / Node APIs).
 */
(function () {
  "use strict";

  var LOG_PREFIX = "[RayNeo Sync]";
  var HTTP_TIMEOUT_MS = 4000;
  var INFLIGHT_COOLDOWN_MS = 10000;
  var FAIL_COOLDOWN_MS = 15000;
  var MAX_TOKEN_LEN = 512;
  var MAX_BODY_LEN = 2 * 1024 * 1024;

  var RAYNEO_API_HOST_RE = /^xr-api-[a-z0-9-]+\.rayneo\.(cn|com)$/i;
  var PLACEHOLDER_RE = /^\$?\{[a-zA-Z0-9_]+\}$/;

  // Exact 15-header allowlist matching internal/har/har.go (`allowedClientHeaders`).
  // Never include xtcl-*, authorization, host, content-*, accept-encoding, or cookie.
  var ALLOWED_CLIENT_HEADERS = {
    "user-agent": true,
    "platform": true,
    "env": true,
    "locale": true,
    "physicalregion": true,
    "time-zone": true,
    "app-version": true,
    "version": true,
    "app-build-number": true,
    "phone-os-version": true,
    "phone-model": true,
    "device-id": true,
    "deviceid": true,
    "device_id": true,
    "device-code": true,
  };

  var ACCOUNT_ID_HEADERS = [
    "account-id",
    "accountid",
    "account_id",
    "xr-account-id",
  ];

  var STORAGE_KEYS = {
    LAST_TOKEN: "rayneo_sync_last_token",
    META_SYNCED_TOKEN: "rayneo_sync_meta_synced_token",
    AUTH_SYNCED_TOKEN: "rayneo_sync_auth_synced_token",
    LAST_BROKER_URL: "rayneo_sync_last_broker_url",
    INFLIGHT_STATE: "rayneo_sync_inflight_state",
    FAIL_STATE: "rayneo_sync_fail_state",
    // Fallback config keys when $argument is omitted
    CFG_BROKER_URL: "rayneo_sync_broker_url",
    CFG_BROKER_SECRET: "rayneo_sync_broker_secret",
    CFG_NOTIFY: "rayneo_sync_notify",
    CFG_DEBUG: "rayneo_sync_debug",
  };

  function hasOwn(obj, key) {
    return Object.prototype.hasOwnProperty.call(obj, key);
  }

  function safeDecodeURIComponent(str) {
    try {
      return decodeURIComponent(str);
    } catch (_) {
      return str;
    }
  }

  /**
   * Pure-JS URL parser (JavaScriptCore in Loon does not guarantee global URL).
   */
  function parseUrl(rawUrl) {
    if (typeof rawUrl !== "string") return null;
    var trimmed = rawUrl.trim();
    var m = trimmed.match(
      /^([a-zA-Z][a-zA-Z0-9+.-]*):\/\/([^\/?#]+)([^?#]*)(?:\?([^#]*))?(?:#.*)?$/
    );
    if (!m) return null;

    var scheme = m[1].toLowerCase();
    var authority = m[2];
    var atIdx = authority.lastIndexOf("@");
    var host = atIdx >= 0 ? authority.slice(atIdx + 1) : authority;
    if (!host) return null;

    var hostname = host.replace(/:\d+$/, "").toLowerCase();
    var origin = scheme + "://" + host;
    var pathname = m[3] || "/";
    var search = m[4] || "";
    var query = Object.create(null);

    if (search) {
      var parts = search.split("&");
      for (var i = 0; i < parts.length; i++) {
        var part = parts[i];
        if (!part) continue;
        var eq = part.indexOf("=");
        var k = eq >= 0 ? part.slice(0, eq) : part;
        var v = eq >= 0 ? part.slice(eq + 1) : "";
        k = safeDecodeURIComponent(k);
        v = safeDecodeURIComponent(v);
        if (k && !(k in query)) {
          query[k] = v;
        }
      }
    }

    return {
      scheme: scheme,
      host: host,
      hostname: hostname,
      origin: origin,
      pathname: pathname,
      search: search,
      query: query,
    };
  }

  function isRayNeoHost(hostname) {
    return typeof hostname === "string" && RAYNEO_API_HOST_RE.test(hostname);
  }

  function normalizePathname(pathname) {
    if (typeof pathname !== "string") return "";
    return safeDecodeURIComponent(pathname)
      .replace(/\/+/g, "/")
      .toLowerCase();
  }

  /**
   * Defense-in-depth exclusion for `http-request` header sniffing.
   * CRITICAL: `/authapi/token/refresh` carries `refreshToken` inside the
   * `xtcl-access-token` request header and MUST NEVER be sniffed.
   */
  function isExcludedSniffPath(pathname) {
    var norm = normalizePathname(pathname);
    if (!norm) return true;
    if (norm.indexOf("/token/refresh") !== -1) return true;
    if (norm.indexOf("/authapi/token/refresh") !== -1) return true;
    if (norm.indexOf("/identify/quicklogin") !== -1) return true;
    if (norm.indexOf("/identify/common/sendverifycode") !== -1) return true;
    if (norm.indexOf("/quicklogin") !== -1) return true;
    if (/\/login(\/|$)/.test(norm)) return true;
    return false;
  }

  function isAuthIssuePath(pathname) {
    var norm = normalizePathname(pathname);
    if (!norm) return false;
    return (
      norm.indexOf("/token/refresh") !== -1 ||
      norm.indexOf("/quicklogin") !== -1 ||
      /\/login(\/|$)/.test(norm)
    );
  }

  function isRefreshPath(pathname) {
    var norm = normalizePathname(pathname);
    return norm.indexOf("/token/refresh") !== -1;
  }

  function parseBool(val, defaultVal) {
    if (typeof val === "boolean") return val;
    if (typeof val === "number") return val !== 0;
    if (typeof val === "string") {
      var s = val.trim().toLowerCase();
      if (!s || PLACEHOLDER_RE.test(s)) return !!defaultVal;
      if (s === "true" || s === "1" || s === "yes" || s === "on") return true;
      if (s === "false" || s === "0" || s === "no" || s === "off") return false;
    }
    return !!defaultVal;
  }

  function cleanStringArg(val) {
    if (typeof val !== "string") return "";
    var s = val.trim();
    if (!s || PLACEHOLDER_RE.test(s)) return "";
    return s;
  }

  function normalizeBrokerUrl(rawUrl) {
    var s = cleanStringArg(rawUrl);
    if (!s) return "";
    s = s.replace(/\/+$/, "");
    // Gracefully normalize if user accidentally pasted the full `/v1/token` endpoint.
    s = s.replace(/\/v1\/token$/i, "").replace(/\/+$/, "");
    if (!/^https?:\/\/[^\/\s]+/i.test(s)) return "";
    return s;
  }

  /**
   * Parses Loon `$argument` in all formats Loon may pass:
   * - Plain JS object from `[Argument]` + `argument=[{broker_url},...]`
   * - Query-string `broker_url=...&broker_secret=...&notify=false&debug=false`
   * - JSON string `{"broker_url":"...","broker_secret":"..."}`
   * - Positional array `[broker_url, broker_secret, notify, debug]`
   */
  function parseArgument(rawArg) {
    var out = {};
    if (rawArg === null || rawArg === undefined) return out;

    if (Array.isArray(rawArg)) {
      if (rawArg.length > 0) out.broker_url = rawArg[0];
      if (rawArg.length > 1) out.broker_secret = rawArg[1];
      if (rawArg.length > 2) out.notify = rawArg[2];
      if (rawArg.length > 3) out.debug = rawArg[3];
      return out;
    }

    if (typeof rawArg === "object") {
      var keys = Object.keys(rawArg);
      for (var i = 0; i < keys.length; i++) {
        var k = keys[i];
        out[k] = rawArg[k];
      }
      return out;
    }

    if (typeof rawArg !== "string") return out;
    var trimmed = rawArg.trim();
    if (!trimmed) return out;

    if (trimmed[0] === "{" && trimmed[trimmed.length - 1] === "}") {
      try {
        var parsed = JSON.parse(trimmed);
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          return parsed;
        }
      } catch (_) {
        // Fall through to query-string parsing
      }
    }

    if (trimmed[0] === "?") {
      trimmed = trimmed.slice(1);
    }

    var pairs = trimmed.split("&");
    for (var j = 0; j < pairs.length; j++) {
      var pair = pairs[j];
      if (!pair) continue;
      var eqIdx = pair.indexOf("=");
      if (eqIdx <= 0) continue;
      var key = safeDecodeURIComponent(pair.slice(0, eqIdx).trim());
      // Split only on first `=` and do NOT turn `+` into space so base64 secrets stay intact.
      var value = safeDecodeURIComponent(pair.slice(eqIdx + 1).trim());
      if (key) {
        out[key] = value;
      }
    }
    return out;
  }

  function createStoreAdapter(persistentStore) {
    return {
      read: function (key) {
        if (!persistentStore || typeof persistentStore.read !== "function") {
          return "";
        }
        try {
          var v = persistentStore.read(key);
          return v === null || v === undefined ? "" : String(v);
        } catch (_) {
          return "";
        }
      },
      write: function (key, value) {
        if (!persistentStore || typeof persistentStore.write !== "function") {
          return false;
        }
        try {
          // Loon/Surge signature is $persistentStore.write(value, key)
          return !!persistentStore.write(
            value === null || value === undefined ? "" : String(value),
            key
          );
        } catch (_) {
          return false;
        }
      },
    };
  }

  function resolveConfig(rawArg, store) {
    var args = parseArgument(rawArg);

    var rawBrokerUrl =
      cleanStringArg(args.broker_url) ||
      cleanStringArg(args.brokerUrl) ||
      cleanStringArg(store.read(STORAGE_KEYS.CFG_BROKER_URL)) ||
      cleanStringArg(store.read("rayneo_broker_url"));

    var brokerSecret =
      cleanStringArg(args.broker_secret) ||
      cleanStringArg(args.brokerSecret) ||
      cleanStringArg(store.read(STORAGE_KEYS.CFG_BROKER_SECRET)) ||
      cleanStringArg(store.read("rayneo_broker_secret"));

    var notifyRaw = hasOwn(args, "notify")
      ? args.notify
      : store.read(STORAGE_KEYS.CFG_NOTIFY);
    var debugRaw = hasOwn(args, "debug")
      ? args.debug
      : store.read(STORAGE_KEYS.CFG_DEBUG);

    return {
      brokerUrl: normalizeBrokerUrl(rawBrokerUrl),
      brokerSecret: brokerSecret,
      notify: parseBool(notifyRaw, false),
      debug: parseBool(debugRaw, false),
    };
  }

  function normalizeHeaders(rawHeaders) {
    var map = Object.create(null);
    if (!rawHeaders || typeof rawHeaders !== "object") return map;
    var keys = Object.keys(rawHeaders);
    for (var i = 0; i < keys.length; i++) {
      var origKey = keys[i];
      var k = String(origKey).trim().toLowerCase();
      if (!k) continue;
      var rawVal = rawHeaders[origKey];
      if (rawVal === null || rawVal === undefined) continue;
      var v = String(rawVal).trim();
      if (v) {
        map[k] = v;
      }
    }
    return map;
  }

  function filterClientHeaders(headerMap) {
    var filtered = {};
    if (!headerMap) return filtered;
    var keys = Object.keys(headerMap);
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i];
      if (!hasOwn(ALLOWED_CLIENT_HEADERS, k)) continue;
      var v = headerMap[k];
      if (typeof v === "string" && v.trim()) {
        filtered[k] = v.trim();
      }
    }
    return filtered;
  }

  function parsePositiveInt(val) {
    if (typeof val === "number") {
      if (Number.isSafeInteger(val) && val > 0) return val;
      return 0;
    }
    if (typeof val === "string") {
      var s = val.trim();
      if (!/^\d+$/.test(s)) return 0;
      var n = parseInt(s, 10);
      if (Number.isSafeInteger(n) && n > 0) return n;
    }
    return 0;
  }

  function extractAccountId(headerMap, query) {
    if (headerMap) {
      for (var i = 0; i < ACCOUNT_ID_HEADERS.length; i++) {
        var id = parsePositiveInt(headerMap[ACCOUNT_ID_HEADERS[i]]);
        if (id > 0) return id;
      }
    }
    if (query && query.accountId) {
      var qId = parsePositiveInt(query.accountId);
      if (qId > 0) return qId;
    }
    return 0;
  }

  function extractRequestMetadata(headerMap, parsedUrl) {
    var clientHeaders = filterClientHeaders(headerMap);
    var region = (headerMap && headerMap["region"]) || "";
    var deviceId =
      (headerMap &&
        (headerMap["device-code"] ||
          headerMap["device-id"] ||
          headerMap["deviceid"] ||
          headerMap["device_id"])) ||
      "";
    var accountId = extractAccountId(headerMap, parsedUrl && parsedUrl.query);
    var baseUrl =
      parsedUrl && isRayNeoHost(parsedUrl.hostname) ? parsedUrl.origin : "";

    return {
      baseUrl: baseUrl,
      region: region,
      deviceId: deviceId,
      accountId: accountId,
      clientHeaders: clientHeaders,
      hasMetadata:
        Object.keys(clientHeaders).length > 0 ||
        !!region ||
        !!deviceId ||
        accountId > 0,
    };
  }

  function parseHttpStatus(resp) {
    if (!resp || typeof resp !== "object") return 0;
    var raw = resp.status !== undefined ? resp.status : resp.statusCode;
    if (typeof raw === "number") return raw;
    if (typeof raw === "string") {
      var m = raw.match(/\b(\d{3})\b/);
      if (m) return parseInt(m[1], 10);
    }
    return 0;
  }

  function firstNonEmptyStr(list) {
    for (var i = 0; i < list.length; i++) {
      if (typeof list[i] === "string" && list[i].trim()) {
        return list[i].trim();
      }
    }
    return "";
  }

  /**
   * Parses RayNeo `/identify/quickLogin` or `/authapi/token/refresh` JSON body.
   * CRITICAL: Intentionally NEVER reads `refreshToken` or `refresh_token`.
   */
  function parseAuthResponse(body) {
    if (typeof body !== "string" || !body || body.length > MAX_BODY_LEN) {
      return null;
    }
    var trimmed = body.trim();
    if (!trimmed || trimmed[0] !== "{") return null;

    var json;
    try {
      json = JSON.parse(trimmed);
    } catch (_) {
      return null;
    }
    if (!json || typeof json !== "object") return null;

    if (json.code !== undefined && json.code !== null && json.code !== 0) {
      return null;
    }

    var data = json.data;
    if (!data || typeof data !== "object") return null;

    var token = typeof data.token === "string" ? data.token.trim() : "";
    if (!token || token.length > MAX_TOKEN_LEN) return null;

    var expiresIn = parsePositiveInt(data.expiresIn);

    var userInfo =
      data.userInfo && typeof data.userInfo === "object" ? data.userInfo : null;

    var accountId = parsePositiveInt(data.accountId);
    if (!accountId && userInfo) {
      accountId = parsePositiveInt(userInfo.accountId);
    }

    var account = firstNonEmptyStr([data.phone, data.mobile, data.email]);
    if (!account && userInfo) {
      account = firstNonEmptyStr([
        userInfo.phone,
        userInfo.mobile,
        userInfo.email,
      ]);
    }
    if (account) {
      account = account.replace(/^\+86-/, "");
    }

    return {
      accessToken: token,
      expiresIn: expiresIn,
      accountId: accountId,
      account: account,
    };
  }

  function maskToken(token) {
    if (typeof token !== "string" || !token) return "<empty>";
    if (token.length < 12) return "*** (len=" + token.length + ")";
    return (
      token.slice(0, 4) +
      "..." +
      token.slice(-4) +
      " (len=" +
      token.length +
      ")"
    );
  }

  function readFailState(store) {
    var raw = store.read(STORAGE_KEYS.FAIL_STATE);
    if (!raw) return null;
    try {
      var parsed = JSON.parse(raw);
      if (
        parsed &&
        typeof parsed.token === "string" &&
        typeof parsed.brokerUrl === "string" &&
        typeof parsed.until === "number"
      ) {
        return parsed;
      }
    } catch (_) {
      // Ignore corrupt state
    }
    return null;
  }

  function writeFailState(store, token, brokerUrl, untilMs) {
    try {
      store.write(
        STORAGE_KEYS.FAIL_STATE,
        JSON.stringify({ token: token, brokerUrl: brokerUrl, until: untilMs })
      );
    } catch (_) {
      // Ignore
    }
  }

  function clearFailState(store) {
    store.write(STORAGE_KEYS.FAIL_STATE, "");
  }

  function readInflightState(store) {
    var raw = store.read(STORAGE_KEYS.INFLIGHT_STATE);
    if (!raw) return null;
    try {
      var parsed = JSON.parse(raw);
      if (
        parsed &&
        typeof parsed.token === "string" &&
        typeof parsed.brokerUrl === "string" &&
        typeof parsed.phase === "string" &&
        typeof parsed.until === "number"
      ) {
        return parsed;
      }
    } catch (_) {
      // Ignore corrupt state
    }
    return null;
  }

  function isInflightActive(store, token, brokerUrl, phase, nowMs) {
    var st = readInflightState(store);
    return !!(
      st &&
      st.token === token &&
      st.brokerUrl === brokerUrl &&
      st.phase === phase &&
      nowMs < st.until
    );
  }

  function writeInflightState(store, token, brokerUrl, phase, untilMs) {
    try {
      store.write(
        STORAGE_KEYS.INFLIGHT_STATE,
        JSON.stringify({
          token: token,
          brokerUrl: brokerUrl,
          phase: phase,
          until: untilMs,
        })
      );
    } catch (_) {
      // Ignore
    }
  }

  function clearInflightState(store) {
    store.write(STORAGE_KEYS.INFLIGHT_STATE, "");
  }

  function buildBrokerPayload(opts) {
    var payload = {
      access_token: opts.accessToken,
      captured_at: opts.capturedAt,
      source: opts.source,
    };

    if (typeof opts.expiresIn === "number" && opts.expiresIn > 0) {
      payload.expires_in = opts.expiresIn;
    }
    if (opts.baseUrl) {
      payload.base_url = opts.baseUrl;
    }
    if (opts.region) {
      payload.region = opts.region;
    }
    if (opts.account) {
      payload.account = opts.account;
    }
    if (typeof opts.accountId === "number" && opts.accountId > 0) {
      payload.account_id = opts.accountId;
    }
    if (opts.deviceId) {
      payload.device_id = opts.deviceId;
    }
    if (opts.clientHeaders && Object.keys(opts.clientHeaders).length > 0) {
      payload.client_headers = opts.clientHeaders;
    }
    return payload;
  }

  function resolveRuntimeEnv(overrides) {
    var env = overrides || {};
    return {
      $request: hasOwn(env, "$request")
        ? env.$request
        : typeof $request !== "undefined"
          ? $request
          : undefined,
      $response: hasOwn(env, "$response")
        ? env.$response
        : typeof $response !== "undefined"
          ? $response
          : undefined,
      $argument: hasOwn(env, "$argument")
        ? env.$argument
        : typeof $argument !== "undefined"
          ? $argument
          : undefined,
      $persistentStore: hasOwn(env, "$persistentStore")
        ? env.$persistentStore
        : typeof $persistentStore !== "undefined"
          ? $persistentStore
          : undefined,
      $httpClient: hasOwn(env, "$httpClient")
        ? env.$httpClient
        : typeof $httpClient !== "undefined"
          ? $httpClient
          : undefined,
      $notification: hasOwn(env, "$notification")
        ? env.$notification
        : typeof $notification !== "undefined"
          ? $notification
          : undefined,
      $done: hasOwn(env, "$done")
        ? env.$done
        : typeof $done !== "undefined"
          ? $done
          : function () {},
      now:
        typeof env.now === "function"
          ? env.now
          : function () {
              return Date.now();
            },
      log:
        typeof env.log === "function"
          ? env.log
          : function (msg) {
              if (typeof console !== "undefined" && console.log) {
                console.log(msg);
              }
            },
    };
  }

  function run(envOverrides) {
    var env = resolveRuntimeEnv(envOverrides);
    var finished = false;

    function finish() {
      if (finished) return;
      finished = true;
      try {
        env.$done({});
      } catch (_) {
        // Never allow $done errors to bubble
      }
    }

    try {
      var req = env.$request;
      var resp = env.$response;
      var isResponseMode = resp !== undefined && resp !== null;

      if (!req || typeof req.url !== "string") {
        return finish();
      }

      var parsedUrl = parseUrl(req.url);
      if (!parsedUrl || !isRayNeoHost(parsedUrl.hostname)) {
        return finish();
      }

      var store = createStoreAdapter(env.$persistentStore);
      var config = resolveConfig(env.$argument, store);

      function debugLog(msg) {
        if (config.debug) {
          try {
            env.log(LOG_PREFIX + " " + msg);
          } catch (_) {
            // Ignore log errors
          }
        }
      }

      function postNotification(subtitle, body) {
        if (
          !config.notify ||
          !env.$notification ||
          typeof env.$notification.post !== "function"
        ) {
          return;
        }
        try {
          env.$notification.post("RayNeo Token Sync", subtitle, body);
        } catch (_) {
          // Ignore notification errors
        }
      }

      var nowMs = env.now();
      var storedBrokerUrl = store.read(STORAGE_KEYS.LAST_BROKER_URL);
      var sameBroker = storedBrokerUrl === config.brokerUrl;
      var lastToken = sameBroker ? store.read(STORAGE_KEYS.LAST_TOKEN) : "";
      var metaSyncedToken = sameBroker
        ? store.read(STORAGE_KEYS.META_SYNCED_TOKEN)
        : "";
      var authSyncedToken = sameBroker
        ? store.read(STORAGE_KEYS.AUTH_SYNCED_TOKEN)
        : "";

      if (isResponseMode) {
        // ---- HTTP-RESPONSE MODE (quickLogin / token/refresh) ----
        if (!isAuthIssuePath(parsedUrl.pathname)) {
          return finish();
        }

        var hasExplicitStatus =
          resp.status !== undefined || resp.statusCode !== undefined;
        var respStatus = parseHttpStatus(resp);
        if (hasExplicitStatus && respStatus !== 200) {
          debugLog("skip non-200 auth response status=" + respStatus);
          return finish();
        }

        var auth = parseAuthResponse(resp.body);
        if (!auth) {
          debugLog("skip auth response without valid data.token");
          return finish();
        }

        if (!config.brokerUrl || !config.brokerSecret) {
          debugLog("broker_url or broker_secret not configured; skip sync");
          return finish();
        }

        if (authSyncedToken === auth.accessToken) {
          debugLog(
            "fast-path: auth response already synced " +
              maskToken(auth.accessToken)
          );
          return finish();
        }

        if (
          isInflightActive(store, auth.accessToken, config.brokerUrl, "auth", nowMs)
        ) {
          debugLog(
            "in-flight cooldown active (auth); skip duplicate " +
              maskToken(auth.accessToken)
          );
          return finish();
        }

        // Safely inspect request headers for allowlisted metadata only.
        // CRITICAL: NEVER read `xtcl-access-token` in http-response mode!
        var reqHeaders = normalizeHeaders(req.headers);
        var reqMeta = extractRequestMetadata(reqHeaders, parsedUrl);
        var source = isRefreshPath(parsedUrl.pathname)
          ? "loon:refresh"
          : "loon:login";
        var accountId = auth.accountId || reqMeta.accountId;

        var payload = buildBrokerPayload({
          accessToken: auth.accessToken,
          expiresIn: auth.expiresIn,
          capturedAt: new Date(nowMs).toISOString(),
          source: source,
          baseUrl: reqMeta.baseUrl,
          region: reqMeta.region,
          account: auth.account,
          accountId: accountId,
          deviceId: reqMeta.deviceId,
          clientHeaders: reqMeta.clientHeaders,
        });

        var isNewAuthToken = auth.accessToken !== lastToken;
        writeInflightState(
          store,
          auth.accessToken,
          config.brokerUrl,
          "auth",
          nowMs + INFLIGHT_COOLDOWN_MS
        );
        return uploadToBroker(
          env,
          config,
          payload,
          function onSuccess(brokerResp) {
            store.write(STORAGE_KEYS.LAST_BROKER_URL, config.brokerUrl);
            store.write(STORAGE_KEYS.LAST_TOKEN, auth.accessToken);
            store.write(STORAGE_KEYS.AUTH_SYNCED_TOKEN, auth.accessToken);
            // Leave META_SYNCED_TOKEN for the next business http-request so
            // full device/app/region headers are enriched once (without expires_in).
            clearInflightState(store);
            clearFailState(store);

            var fp =
              brokerResp && brokerResp.token_fp
                ? brokerResp.token_fp
                : maskToken(auth.accessToken);
            debugLog(
              "synced " +
                source +
                " token=" +
                maskToken(auth.accessToken) +
                " ttl=" +
                (auth.expiresIn || "unknown") +
                " fp=" +
                fp
            );
            if (isNewAuthToken) {
              postNotification(
                "同步成功 (" + source + ")",
                "fp: " +
                  fp +
                  (auth.expiresIn ? " · TTL " + auth.expiresIn + "s" : "")
              );
            }
          },
          function onFailure(reason) {
            clearInflightState(store);
            debugLog("sync failed (" + source + "): " + reason);
          },
          finish
        );
      }

      // ---- HTTP-REQUEST MODE (passive header sniffing) ----
      // Defense-in-depth: strictly exclude /authapi/token/refresh and login paths.
      if (isExcludedSniffPath(parsedUrl.pathname)) {
        return finish();
      }

      var headers = normalizeHeaders(req.headers);
      var token = headers["xtcl-access-token"] || "";
      if (!token || token.length > MAX_TOKEN_LEN) {
        return finish();
      }

      if (!config.brokerUrl || !config.brokerSecret) {
        debugLog("broker_url or broker_secret not configured; skip sync");
        return finish();
      }

      // INSTANT FAST-PATH: zero network calls when token + metadata already synced.
      if (token === lastToken && metaSyncedToken === token) {
        return finish();
      }

      // Prevent concurrent startup bursts from posting duplicate requests for the same token.
      if (isInflightActive(store, token, config.brokerUrl, "header", nowMs)) {
        debugLog("in-flight cooldown active (header); skip duplicate");
        return finish();
      }

      // Avoid stalling the app when broker is temporarily unreachable.
      var failState = readFailState(store);
      if (
        failState &&
        failState.brokerUrl === config.brokerUrl &&
        failState.token === token &&
        nowMs < failState.until
      ) {
        debugLog("cooldown active after prior failure; skip retry");
        return finish();
      }

      var meta = extractRequestMetadata(headers, parsedUrl);
      var isEnrichmentOnly = token === lastToken && metaSyncedToken !== token;
      if (isEnrichmentOnly && !meta.hasMetadata) {
        return finish();
      }

      // Intentionally omit `expires_in` for `loon:header` so the broker preserves
      // exact `expires_at` from `loon:refresh` / `loon:login`.
      var headerPayload = buildBrokerPayload({
        accessToken: token,
        capturedAt: new Date(nowMs).toISOString(),
        source: "loon:header",
        baseUrl: meta.baseUrl,
        region: meta.region,
        accountId: meta.accountId,
        deviceId: meta.deviceId,
        clientHeaders: meta.clientHeaders,
      });

      writeInflightState(
        store,
        token,
        config.brokerUrl,
        "header",
        nowMs + INFLIGHT_COOLDOWN_MS
      );

      return uploadToBroker(
        env,
        config,
        headerPayload,
        function onSuccess(brokerResp) {
          store.write(STORAGE_KEYS.LAST_BROKER_URL, config.brokerUrl);
          store.write(STORAGE_KEYS.LAST_TOKEN, token);
          if (meta.hasMetadata) {
            store.write(STORAGE_KEYS.META_SYNCED_TOKEN, token);
          }
          clearInflightState(store);
          clearFailState(store);

          var fp =
            brokerResp && brokerResp.token_fp
              ? brokerResp.token_fp
              : maskToken(token);
          debugLog(
            "synced loon:header" +
              (isEnrichmentOnly ? " (meta-enrichment)" : "") +
              " token=" +
              maskToken(token) +
              " headers=" +
              Object.keys(meta.clientHeaders).length +
              " fp=" +
              fp
          );
          if (!isEnrichmentOnly) {
            postNotification("同步成功 (loon:header)", "fp: " + fp);
          }
        },
        function onFailure(reason) {
          clearInflightState(store);
          writeFailState(store, token, config.brokerUrl, nowMs + FAIL_COOLDOWN_MS);
          debugLog("sync failed (loon:header): " + reason);
        },
        finish
      );
    } catch (err) {
      try {
        env.log(
          LOG_PREFIX +
            " unexpected error: " +
            (err && err.message ? err.message : String(err))
        );
      } catch (_) {
        // Ignore
      }
      return finish();
    }
  }

  function uploadToBroker(env, config, payload, onSuccess, onFailure, finish) {
    if (!env.$httpClient || typeof env.$httpClient.post !== "function") {
      try {
        onFailure("$httpClient.post unavailable");
      } catch (_) {}
      return finish();
    }

    var options = {
      url: config.brokerUrl + "/v1/token",
      timeout: HTTP_TIMEOUT_MS,
      headers: {
        "Content-Type": "application/json",
        Authorization: "Bearer " + config.brokerSecret,
      },
      body: JSON.stringify(payload),
    };

    try {
      env.$httpClient.post(options, function (error, response, data) {
        try {
          if (error) {
            onFailure(String(error));
            return;
          }
          var status = parseHttpStatus(response);
          if (status < 200 || status >= 300) {
            onFailure("HTTP " + status);
            return;
          }
          var parsed = null;
          if (typeof data === "string" && data.trim()) {
            try {
              parsed = JSON.parse(data);
            } catch (_) {
              parsed = null;
            }
          }
          if (parsed && parsed.ok === false) {
            onFailure(parsed.error || "broker returned ok=false");
            return;
          }
          onSuccess(parsed);
        } catch (cbErr) {
          try {
            onFailure(
              cbErr && cbErr.message ? cbErr.message : String(cbErr)
            );
          } catch (_) {}
        } finally {
          finish();
        }
      });
    } catch (syncErr) {
      try {
        onFailure(
          syncErr && syncErr.message ? syncErr.message : String(syncErr)
        );
      } catch (_) {}
      finish();
    }
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      ALLOWED_CLIENT_HEADERS: ALLOWED_CLIENT_HEADERS,
      STORAGE_KEYS: STORAGE_KEYS,
      INFLIGHT_COOLDOWN_MS: INFLIGHT_COOLDOWN_MS,
      FAIL_COOLDOWN_MS: FAIL_COOLDOWN_MS,
      HTTP_TIMEOUT_MS: HTTP_TIMEOUT_MS,
      RAYNEO_API_HOST_RE: RAYNEO_API_HOST_RE,
      parseUrl: parseUrl,
      parseBool: parseBool,
      parseArgument: parseArgument,
      normalizeBrokerUrl: normalizeBrokerUrl,
      resolveConfig: resolveConfig,
      isRayNeoHost: isRayNeoHost,
      isExcludedSniffPath: isExcludedSniffPath,
      isAuthIssuePath: isAuthIssuePath,
      isRefreshPath: isRefreshPath,
      normalizeHeaders: normalizeHeaders,
      filterClientHeaders: filterClientHeaders,
      extractAccountId: extractAccountId,
      extractRequestMetadata: extractRequestMetadata,
      parseHttpStatus: parseHttpStatus,
      parseAuthResponse: parseAuthResponse,
      maskToken: maskToken,
      buildBrokerPayload: buildBrokerPayload,
      run: run,
    };
  } else {
    run();
  }
})();
