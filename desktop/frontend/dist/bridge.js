// On Android there is no Wails runtime. This emulates the part of it that app.js uses on top of the
// AndroidBridge interface, and does nothing on the desktop.
(function () {
  const bridge = window.AndroidBridge;
  if (!bridge) return;
  document.documentElement.dataset.platform = "android";

  let nextId = 1;
  const pending = new Map();
  window.__bridgeResolve = (id, result) => {
    const resolve = pending.get(id);
    pending.delete(id);
    if (resolve) resolve(result);
  };
  const call = (method, ...args) => new Promise((resolve) => {
    const id = nextId++;
    pending.set(id, resolve);
    bridge.call(id, method, JSON.stringify(args));
  });

  const listeners = {};
  const emit = (name, data) => (listeners[name] || []).forEach((callback) => callback(data));
  window.runtime = {
    EventsOn(name, callback) {
      (listeners[name] = listeners[name] || []).push(callback);
    },
  };

  window.go = { main: { App: {
    State: async () => JSON.parse(await call("State")),
    Traffic: async () => JSON.parse(await call("Traffic")),
    Logs: async () => {
      const text = await call("Logs");
      return text ? text.split("\n") : [];
    },
    SaveSettings: (mailbox, node) => call("SaveSettings", mailbox, node),
    SetupCode: () => call("SetupCode"),
    FindRoute: () => call("FindRoute"),
    ResetSettings: () => call("ResetSettings"),
    SetAutoConnect: (enabled) => call("SetAutoConnect", enabled),
    SetEnvProxy: async () => {},
    Connect: () => call("Connect"),
    Disconnect: () => call("Disconnect"),
  } } };

  // The desktop pushes state and log events; here they are polled.
  let shownLogLines = 0;
  setInterval(async () => {
    emit("state", await window.go.main.App.State());
    const lines = await window.go.main.App.Logs();
    if (lines.length < shownLogLines) shownLogLines = 0;
    lines.slice(shownLogLines).forEach((line) => emit("log", line));
    shownLogLines = lines.length;
  }, 700);
})();
