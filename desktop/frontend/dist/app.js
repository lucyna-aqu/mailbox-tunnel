const statusText = {
  disconnected: "Not connected",
  scanning: "Finding a route…",
  connecting: "Connecting…",
  connected: "Protected",
};
let scanProgress = "";

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

function render(state) {
  current = state;
  document.body.className = state.status;
  const connectedFor = state.connectedAt ? " • " + formatDuration(Date.now() - state.connectedAt) : "";
  const scanning = state.status === "scanning" && scanProgress ? " " + scanProgress : "";
  $("status-text").textContent = (statusText[state.status] || state.status) + connectedFor + scanning;
  $("route").textContent = $("route-current").textContent = state.route || "Default";
  $("route-section").hidden = !state.configured;
  $("backup-section").hidden = !state.configured;
  $("setup-code-section").hidden = !state.configured;
  if (!state.configured) {
    $("setup-code").hidden = true;
    $("setup-code").value = "";
  }
  $("backup-current").textContent = state.backup ? "Set: used while Google does not work" : "Not set";
  $("auto-connect-row").hidden = !state.configured;
  $("auto-connect").checked = state.autoConnect;
  $("env-proxy-row").hidden = $("env-proxy-note").hidden = !state.configured;
  $("env-proxy").checked = state.envProxy;
  $("power").setAttribute("aria-label", state.status === "disconnected" ? "Connect" : "Disconnect");
  $("node").textContent = state.node || "Not set";
  if (state.address === "VPN") {
    $("proxy").textContent = "Android VPN on";
  } else {
    $("proxy").textContent = state.address
      ? state.address + (state.systemProxy ? " · Windows proxy on" : " · set your apps to use it")
      : "Off";
  }
  $("timer").textContent = state.connectedAt ? formatDuration(Date.now() - state.connectedAt) : "--:--:--";
  $("reset").hidden = !state.configured;
  showError(state.error);
}

function showError(message) {
  $("error").hidden = !message;
  $("error").textContent = message || "";
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
    $("rate-down").textContent = formatRate((traffic.received - lastTraffic.received) / seconds);
    $("rate-up").textContent = formatRate((traffic.sent - lastTraffic.sent) / seconds);
  } else {
    $("rate-down").textContent = $("rate-up").textContent = "0 B/s";
  }
  $("total").textContent = traffic.received + traffic.sent > 0 ? formatSize(traffic.received + traffic.sent) + " total" : "";
  $("ping").textContent = live && traffic.latencyMs > 0 ? traffic.latencyMs + " ms" : "-- ms";
  lastTraffic = { ...traffic, time: now };
}

function formatSize(bytes) {
  if (bytes >= 1024 * 1024 * 1024) return (bytes / 1024 / 1024 / 1024).toFixed(2) + " GB";
  if (bytes >= 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + " MB";
  return Math.round(bytes / 1024) + " KB";
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
  const message = $("settings-message");
  message.hidden = false;
  message.className = "message";
  message.textContent = "Checking your mailbox…";
  $("save").disabled = true;
  const error = await backend().SaveSettings($("mailbox").value, $("node-input").value);
  $("save").disabled = false;
  message.className = error ? "message bad" : "message";
  message.textContent = error || "Saved.";
  if (!error) {
    $("mailbox").value = $("node-input").value = "";
    $("node-row").hidden = false;
    showPage("home");
  }
});

// A setup code already holds the node.
$("mailbox").addEventListener("input", (event) => {
  $("node-row").hidden = event.target.value.trim().startsWith("mtc://");
});

$("show-setup-code").addEventListener("click", async () => {
  const code = $("setup-code");
  code.value = await backend().SetupCode();
  code.hidden = false;
  code.focus();
  code.select();
});

$("reset").addEventListener("click", () => backend().ResetSettings());
$("auto-connect").addEventListener("change", (event) => backend().SetAutoConnect(event.target.checked));
$("env-proxy").addEventListener("change", (event) => backend().SetEnvProxy(event.target.checked));

$("scan").addEventListener("click", async () => {
  const message = $("scan-message");
  message.hidden = false;
  message.className = "message";
  message.textContent = "Testing routes on this network…";
  $("scan").disabled = true;
  const result = await backend().FindRoute();
  $("scan").disabled = false;
  message.textContent = result;
  scanProgress = "";
});

$("save-backup").addEventListener("click", async () => {
  const message = $("backup-message");
  const value = $("backup-input").value.trim();
  message.hidden = false;
  message.className = "message";
  message.textContent = value ? "Checking the relay…" : "Removing the backup…";
  $("save-backup").disabled = true;
  const error = await backend().SaveBackup(value);
  $("save-backup").disabled = false;
  message.className = error ? "message bad" : "message";
  message.textContent = error || (value ? "Saved. Reconnect to use it." : "Backup removed.");
  if (!error) $("backup-input").value = "";
});

function onScanProgress(progress) {
  scanProgress = progress.done + "/" + progress.total;
  if (!$("scan").disabled) return;
  $("scan-message").textContent = "Testing routes on this network… " + scanProgress;
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
