/**
 * mailbox-tunnel: Apps Script mailbox.
 * Deploy as a Web app (Execute as: Me, Who has access: Anyone).
 */
const AUTH_KEY = "CHANGE_ME_TO_A_LONG_RANDOM_SECRET";
const MAX_WAIT_MS = 300000;
const POLL_INTERVAL_MS = 100;
const MESSAGE_TTL_SECONDS = 600;
const VERSION = 2;

// A cache entry holds at most 100KB, so larger values are split into parts under <key>#<n>.
// The entry at <key> then holds "~" and the number of parts; base64 values never start with "~".
const PART_CHARS = 95000;
const MAX_PARTS = 10;

// Node operators only: the node's status URL, visited hourly by keepAlive so a free host does not put it to sleep.
const KEEPALIVE_URL = "";

function keepAlive() {
  if (KEEPALIVE_URL) {
    UrlFetchApp.fetch(KEEPALIVE_URL, { muteHttpExceptions: true });
  }
}

function doPost(e) {
  try {
    const request = JSON.parse(e.postData.contents);
    if (request.k !== AUTH_KEY) {
      return reply({ error: "unauthorized" });
    }
    const cache = CacheService.getScriptCache();

    switch (request.op) {
      case "ping":
        return reply({ ok: true, version: VERSION });

      case "put":
        store(cache, request.key, request.value);
        return reply({ ok: true, progress: readProgress(cache, request.progressKey) });

      case "status":
        return reply({ ok: true, progress: readProgress(cache, request.progressKey) });

      // Read without removing; the receiver acks keys on its next call.
      case "wait":
        if (request.ack && request.ack.length) {
          cache.removeAll(withParts(request.ack));
        }
        if (request.progressKey) {
          cache.put(request.progressKey, String(request.progress), MESSAGE_TTL_SECONDS);
        }
        return reply({ ok: true, value: waitFor(cache, request.key, request.timeoutMs, false) });

      case "take":
        return reply({ ok: true, value: waitFor(cache, request.key, request.timeoutMs, true) });
    }
    return reply({ error: "unknown op" });
  } catch (error) {
    return reply({ error: String(error) });
  }
}

function doGet() {
  return reply({ ok: true });
}

function store(cache, key, value) {
  if (value.length <= PART_CHARS) {
    cache.put(key, value, MESSAGE_TTL_SECONDS);
    return;
  }
  const entries = {};
  let count = 0;
  for (let start = 0; start < value.length; start += PART_CHARS) {
    entries[key + "#" + count] = value.substring(start, start + PART_CHARS);
    count++;
  }
  if (count > MAX_PARTS) {
    throw new Error("value too large");
  }
  entries[key] = "~" + count;
  cache.putAll(entries, MESSAGE_TTL_SECONDS);
}

// load returns the value at key, or null while it or any of its parts is missing.
function load(cache, key) {
  const value = cache.get(key);
  if (value === null || value.charAt(0) !== "~") {
    return value;
  }
  const names = [];
  for (let i = 0; i < Number(value.substring(1)); i++) {
    names.push(key + "#" + i);
  }
  const parts = cache.getAll(names);
  let joined = "";
  for (const name of names) {
    if (parts[name] === undefined) {
      return null;
    }
    joined += parts[name];
  }
  return joined;
}

function withParts(keys) {
  const all = [];
  for (const key of keys) {
    all.push(key);
    for (let i = 0; i < MAX_PARTS; i++) {
      all.push(key + "#" + i);
    }
  }
  return all;
}

function waitFor(cache, key, timeoutMs, remove) {
  const deadline = Date.now() + Math.min(timeoutMs || 20000, MAX_WAIT_MS);
  while (true) {
    const value = load(cache, key);
    if (value !== null) {
      if (remove) {
        cache.removeAll(withParts([key]));
      }
      return value;
    }
    if (Date.now() >= deadline) {
      return null;
    }
    Utilities.sleep(POLL_INTERVAL_MS);
  }
}

function readProgress(cache, key) {
  if (!key) {
    return null;
  }
  const value = cache.get(key);
  return value === null ? null : Number(value);
}

function reply(payload) {
  return ContentService.createTextOutput(JSON.stringify(payload)).setMimeType(ContentService.MimeType.JSON);
}
