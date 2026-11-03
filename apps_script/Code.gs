/**
 * mailbox-tunnel: Apps Script mailbox.
 * Deploy as a Web app (Execute as: Me, Who has access: Anyone).
 */
const AUTH_KEY = "CHANGE_ME_TO_A_LONG_RANDOM_SECRET";
const MAX_WAIT_MS = 300000;
const POLL_INTERVAL_MS = 100;
const MESSAGE_TTL_SECONDS = 600;

function doPost(e) {
  try {
    const request = JSON.parse(e.postData.contents);
    if (request.k !== AUTH_KEY) {
      return reply({ error: "unauthorized" });
    }
    const cache = CacheService.getScriptCache();

    switch (request.op) {
      case "ping":
        return reply({ ok: true });

      case "put":
        cache.put(request.key, request.value, MESSAGE_TTL_SECONDS);
        return reply({ ok: true, progress: readProgress(cache, request.progressKey) });

      case "status":
        return reply({ ok: true, progress: readProgress(cache, request.progressKey) });

      // Read without removing; the receiver acks keys on its next call.
      case "wait":
        if (request.ack && request.ack.length) {
          cache.removeAll(request.ack);
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

function waitFor(cache, key, timeoutMs, remove) {
  const deadline = Date.now() + Math.min(timeoutMs || 20000, MAX_WAIT_MS);
  while (true) {
    const value = cache.get(key);
    if (value !== null) {
      if (remove) {
        cache.remove(key);
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
