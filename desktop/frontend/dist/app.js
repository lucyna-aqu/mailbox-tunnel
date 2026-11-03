const statusText = {
  disconnected: "قطع",
  connecting: "در حال اتصال…",
  connected: "متصل",
};

const $ = (id) => document.getElementById(id);
const backend = () => window.go.main.App;
let current = { status: "disconnected", configured: false };

function render(state) {
  current = state;
  $("dot").className = "dot " + state.status;
  $("status-text").textContent = statusText[state.status] || state.status;
  $("address").textContent = state.address
    ? (state.systemProxy ? "پروکسی ویندوز: " : "پروکسی: ") + state.address
    : "";

  const toggle = $("toggle");
  const running = state.status !== "disconnected";
  toggle.textContent = running ? "قطع اتصال" : "اتصال";
  toggle.classList.toggle("stop", running);
  toggle.disabled = !state.configured;

  $("reset").hidden = !state.configured;
  if (!state.configured) {
    $("settings-panel").open = true;
  }
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

$("toggle").addEventListener("click", async () => {
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
  message.textContent = "در حال بررسی صندوق پستی…";
  $("save").disabled = true;
  const error = await backend().SaveSettings($("deployment").value, $("key").value, $("node").value);
  $("save").disabled = false;
  message.className = error ? "message bad" : "message";
  message.textContent = error || "ذخیره شد.";
  if (!error) {
    $("deployment").value = $("key").value = $("node").value = "";
    $("settings-panel").open = false;
  }
});

$("reset").addEventListener("click", () => backend().ResetSettings());

window.addEventListener("DOMContentLoaded", async () => {
  window.runtime.EventsOn("state", render);
  window.runtime.EventsOn("log", appendLog);
  render(await backend().State());
  (await backend().Logs()).forEach(appendLog);
});
