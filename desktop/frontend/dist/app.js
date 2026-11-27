const statusText = {
  disconnected: "Not connected",
  scanning: "Finding a route…",
  connecting: "Connecting…",
  connected: "Connected",
};

// The backend speaks English; explanations are shown in Persian, and anything unknown is shown as it is.
const persian = {
  "Enter your settings first.": "اول تنظیمات را وارد کنید.",
  "That is not a relay's node string.": "این رشتهٔ یک رله نیست.",
  "The relay did not answer. Check that it is running and its port is public.": "رله جواب نداد. مطمئن شوید روشن است و پورتش عمومی است.",
  "That is not a valid node string.": "این رشتهٔ نود درست نیست.",
  "No working route to Google was found on this network.": "روی این شبکه راهی به گوگل پیدا نشد.",
  "That setup code is incomplete. Copy all of it again.": "کد راه‌اندازی ناقص است. دوباره کامل کپی‌اش کنید.",
  "That is not a Web app address. Copy the URL Google shows after deploying, the one ending in /exec.": "این آدرس Web app نیست. آدرسی را کپی کنید که گوگل بعد از دیپلوی نشان می‌دهد و آخرش /exec است.",
  "Your mailbox did not answer. Check the address and try again.": "صندوق جواب نداد. آدرس را بررسی کنید و دوباره امتحان کنید.",
  "This mailbox already belongs to another device. Paste the setup code from that device instead.": "این صندوق مال دستگاه دیگری است. به‌جای آدرس، کد راه‌اندازی آن دستگاه را بچسبانید.",
  "This mailbox runs an older Code.gs. Deploy the latest Code.gs and try again.": "این صندوق Code.gs قدیمی دارد. آخرین Code.gs را دیپلوی کنید و دوباره امتحان کنید.",
  "Could not create a key on this device.": "ساختن کلید روی این دستگاه ممکن نشد.",
  "This build cannot set up a GitHub relay.": "این نسخهٔ برنامه نمی‌تواند رلهٔ گیتهاب بسازد.",
  "Could not reach GitHub to sign in.": "برای ورود به GitHub دسترسی نشد. اتصال اینترنت را بررسی کنید.",
  "Sign-in was refused on GitHub.": "ورود در GitHub رد شد.",
  "The code was not entered in time. Try again.": "کد به‌موقع وارد نشد. دوباره امتحان کنید.",
  "The relay could not be set up. See Logs for the reason.": "ساختن رله ممکن نشد. دلیلش در گزارش‌ها هست.",
  "GitHub does not let this account create codespaces yet. Sign in with an older account, or try again in a few days.": "GitHub هنوز به این حساب اجازهٔ ساختن کداسپیس نمی‌دهد؛ این محدودیت برای حساب‌های تازه است. با یک حساب قدیمی‌تر وارد شوید، یا چند روز دیگر دوباره امتحان کنید.",
};

function translate(message) {
  if (!message) return "";
  if (persian[message]) return persian[message];
  const found = /^Using (.+) via (.+) \((\d+) ms\)\. Reconnect to use it\.$/.exec(message);
  if (found) return `راه تازه: ${found[1]} از ${found[2]} (${found[3]} ms). برای استفاده دوباره وصل شوید.`;
  return message;
}

let scanProgress = "";
let editingSetup = false;

const $ = (id) => document.getElementById(id);
const backend = () => window.go.main.App;
let current = { status: "disconnected", configured: false, connectedAt: 0 };
let lastTraffic = null;

function showPage(name) {
  document.querySelectorAll(".page").forEach((page) => page.classList.toggle("active", page.id === "page-" + name));
  document.querySelectorAll(".nav").forEach((button) => button.classList.toggle("active", button.dataset.page === name));
}

function formatDuration(milliseconds) {
  const seconds = Math.floor(milliseconds / 1000);
  return [Math.floor(seconds / 3600), Math.floor(seconds / 60) % 60, seconds % 60]
    .map((part) => String(part).padStart(2, "0"))
    .join(":");
}

function formatRate(bytesPerSecond) {
  if (bytesPerSecond >= 1024 * 1024) return (bytesPerSecond / 1024 / 1024).toFixed(1) + " MB/s";
  if (bytesPerSecond >= 1024) return Math.round(bytesPerSecond / 1024) + " KB/s";
  return Math.round(bytesPerSecond) + " B/s";
}

function formatSize(bytes) {
  if (bytes >= 1024 * 1024 * 1024) return (bytes / 1024 / 1024 / 1024).toFixed(2) + " GB";
  if (bytes >= 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + " MB";
  return Math.round(bytes / 1024) + " KB";
}

// The route arrives as "<front domain> via <Google IP>", or "Backup relay" while on the relay.
function routeName(route) {
  return route === "Backup relay" ? "GitHub relay" : "Google";
}

function statusDetail(state) {
  if (state.status === "scanning") return scanProgress ? "Testing routes " + scanProgress : "Testing routes";
  if (state.status === "disconnected") return state.configured ? "Press to connect" : "Set up in Settings first";
  if (state.address === "VPN") return "Android VPN on";
  if (!state.address) return "";
  return state.systemProxy ? "Windows proxy on · " + state.address : state.address + " · set your apps to use it";
}

function render(state) {
  current = state;
  document.body.className = state.status;
  $("status-text").textContent = statusText[state.status] || state.status;
  $("proxy").textContent = statusDetail(state);
  $("power").setAttribute("aria-label", state.status === "disconnected" ? "Connect" : "Disconnect");
  $("node").textContent = state.node || "Not set";
  $("route").textContent = routeName(state.route);
  $("timer").textContent = state.connectedAt ? formatDuration(Date.now() - state.connectedAt) : "—";

  $("setup-view").hidden = state.configured && !editingSetup;
  $("settings-view").hidden = !state.configured || editingSetup;
  $("cancel-setup").hidden = !state.configured;
  if (!state.configured) {
    $("setup-code").hidden = true;
    $("setup-code").value = "";
  }
  $("route-current").textContent = state.route && state.route !== "Backup relay" ? state.route : "";
  $("backup-current").textContent = state.backup ? "Set" : "Not set up";
  $("backup-current").classList.toggle("on", !!state.backup);
  const preferRelay = !!(state.backup && state.preferRelay);
  $("prefer-google").setAttribute("aria-pressed", String(!preferRelay));
  $("prefer-relay").setAttribute("aria-pressed", String(preferRelay));
  $("prefer-relay").disabled = !state.backup;
  $("prefer-note").textContent = !state.backup
    ? "برای انتخاب رله، اول رلهٔ گیتهاب را در پایین راه‌اندازی کنید."
    : state.status === "disconnected"
      ? "اگر مسیر اول کار نکند، برنامه خودش به مسیر دوم می‌رود."
      : "از اتصال بعدی اعمال می‌شود. اگر مسیر اول کار نکند، برنامه خودش به مسیر دوم می‌رود.";
  renderRelay(state);
  $("auto-connect").checked = state.autoConnect;
  $("env-proxy").checked = state.envProxy;
  $("update-banner").hidden = !state.update;
  $("page-home").classList.toggle("with-update", !!state.update);
  $("update-version").textContent = state.update ? "v" + state.update.version : "";
  // Each version number is isolated so it reads left to right inside the Persian line.
  const isolate = (text) => "⁦" + text + "⁩";
  let version = state.version ? isolate("v" + state.version) : "";
  if (state.update) version += " · نسخهٔ تازه: " + isolate("v" + state.update.version);
  else if (state.updateKnown) version += " · آخرین نسخه را دارید";
  $("app-version").textContent = version;
  showError(state.error);
}

const relaySteps = {
  connecting: "در حال اتصال به GitHub…",
  code: "منتظر ورود شما در GitHub…",
  secrets: "ذخیرهٔ کلیدهای رله در حساب شما…",
  create: "ساختن کداسپیس…",
  start: "روشن شدن کداسپیس…",
  port: "باز کردن درگاه رله…",
  relay: "منتظر جواب رله…",
};

function renderRelay(state) {
  const relay = state.relay || {};
  const busy = !!relaySteps[relay.step];
  $("relay-setup").hidden = !state.relayAvailable || busy || !!state.relayManaged;
  $("relay-remove").hidden = busy || !state.relayManaged;
  $("relay-progress").hidden = !busy;
  $("relay-code-row").hidden = relay.step !== "code";
  $("relay-code").textContent = relay.code || "";
  $("relay-url").textContent = relay.url || "";
  $("relay-step").textContent = relaySteps[relay.step] || "";
  if (relay.error) showMessage("backup-message", relay.error, true);
  else if (relay.step === "done") showMessage("backup-message", "رله آماده است و به‌عنوان مسیر پشتیبان ذخیره شد.");
}

function showError(message) {
  $("error").hidden = !message;
  $("error").textContent = translate(message);
}

function showMessage(id, text, bad) {
  const message = $(id);
  message.hidden = !text;
  message.className = bad ? "message bad" : "message";
  message.textContent = translate(text);
}

function appendLog(line) {
  const log = $("log");
  log.textContent += line + "\n";
  log.scrollTop = log.scrollHeight;
}

async function tick() {
  render(current);
  const traffic = await backend().Traffic();
  const now = Date.now();
  const live = current.status !== "disconnected";
  if (lastTraffic && live) {
    const seconds = (now - lastTraffic.time) / 1000;
    const deltaDown = traffic.received - lastTraffic.received;
    const deltaUp = traffic.sent - lastTraffic.sent;
    if (seconds > 0 && deltaDown >= 0 && deltaUp >= 0) {
      $("rate-down").textContent = formatRate(deltaDown / seconds);
      $("rate-up").textContent = formatRate(deltaUp / seconds);
    } else {
      $("rate-down").textContent = $("rate-up").textContent = "0 B/s";
    }
  } else {
    $("rate-down").textContent = $("rate-up").textContent = "0 B/s";
  }
  $("total").textContent = formatSize(traffic.received + traffic.sent);
  $("ping").textContent = live && traffic.latencyMs > 0 ? traffic.latencyMs + " ms" : "—";
  lastTraffic = live ? { ...traffic, time: now } : null;
}

document.querySelectorAll(".nav").forEach((button) => button.addEventListener("click", () => showPage(button.dataset.page)));
document.querySelectorAll("[data-goto]").forEach((button) => button.addEventListener("click", () => showPage(button.dataset.goto)));

$("power").addEventListener("click", async () => {
  if (!current.configured) {
    showPage("settings");
    return;
  }
  if (current.status === "disconnected") {
    showError(await backend().Connect());
  } else {
    await backend().Disconnect();
  }
});

$("settings").addEventListener("submit", async (event) => {
  event.preventDefault();
  showMessage("settings-message", "در حال بررسی صندوق…");
  $("save").disabled = true;
  const error = await backend().SaveSettings($("mailbox").value, $("node-input").value);
  $("save").disabled = false;
  if (error) {
    showMessage("settings-message", error, true);
    return;
  }
  showMessage("settings-message", "");
  $("mailbox").value = $("node-input").value = "";
  $("node-row").hidden = false;
  editingSetup = false;
  render(current);
  showPage("home");
});

// A setup code already holds the node.
$("mailbox").addEventListener("input", (event) => {
  $("node-row").hidden = event.target.value.trim().startsWith("mtc://");
});

$("edit-setup").addEventListener("click", () => {
  editingSetup = true;
  render(current);
});

$("cancel-setup").addEventListener("click", () => {
  editingSetup = false;
  showMessage("settings-message", "");
  render(current);
});

$("show-setup-code").addEventListener("click", async () => {
  const code = $("setup-code");
  code.value = await backend().SetupCode();
  code.hidden = false;
  code.focus();
  code.select();
});

$("reset").addEventListener("click", () => backend().ResetSettings());
$("relay-setup").addEventListener("click", async () => {
  // Hide the button at once; the backend's own state takes over when it answers.
  $("relay-setup").hidden = true;
  $("relay-progress").hidden = false;
  $("relay-step").textContent = relaySteps.connecting;
  showMessage("backup-message", "");
  const error = await backend().SetUpRelay();
  if (error) showMessage("backup-message", error, true);
});
$("relay-cancel").addEventListener("click", () => backend().CancelRelaySetup());
$("relay-remove").addEventListener("click", async () => {
  showMessage("backup-message", "در حال حذف رله…");
  const error = await backend().RemoveRelay();
  showMessage("backup-message", error || "رله حذف شد.", !!error);
});
$("relay-copy").addEventListener("click", () => {
  navigator.clipboard.writeText($("relay-code").textContent).catch(() => {});
});
$("open-update").addEventListener("click", () => backend().OpenUpdate());
$("prefer-google").addEventListener("click", () => backend().SetPreferRelay(false));
$("prefer-relay").addEventListener("click", () => backend().SetPreferRelay(true));
$("auto-connect").addEventListener("change", (event) => backend().SetAutoConnect(event.target.checked));
$("env-proxy").addEventListener("change", (event) => backend().SetEnvProxy(event.target.checked));

$("scan").addEventListener("click", async () => {
  showMessage("scan-message", "در حال آزمودن راه‌ها روی این شبکه…");
  $("scan").disabled = true;
  const result = await backend().FindRoute();
  $("scan").disabled = false;
  showMessage("scan-message", result, result === "No working route to Google was found on this network.");
  scanProgress = "";
});

$("save-backup").addEventListener("click", async () => {
  const value = $("backup-input").value.trim();
  showMessage("backup-message", value ? "در حال بررسی رله…" : "در حال حذف رله…");
  $("save-backup").disabled = true;
  const error = await backend().SaveBackup(value);
  $("save-backup").disabled = false;
  if (error) {
    showMessage("backup-message", error, true);
    return;
  }
  showMessage("backup-message", value ? "ذخیره شد. برای استفاده دوباره وصل شوید." : "رله حذف شد.");
  $("backup-input").value = "";
});

function onScanProgress(progress) {
  scanProgress = progress.done + "/" + progress.total;
  if (!$("scan").disabled) return;
  $("scan-message").textContent = "در حال آزمودن راه‌ها روی این شبکه… " + scanProgress;
}

window.addEventListener("DOMContentLoaded", async () => {
  window.runtime.EventsOn("state", render);
  window.runtime.EventsOn("log", appendLog);
  window.runtime.EventsOn("scan", onScanProgress);
  setInterval(tick, 1000);
  render(await backend().State());
  if (!current.configured) showPage("settings");
  ((await backend().Logs()) || []).forEach(appendLog);
});
