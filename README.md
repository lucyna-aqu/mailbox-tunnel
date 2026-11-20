# mailbox-tunnel

A censorship-resistant TCP tunnel that uses a Google Apps Script web app's `CacheService` as an asynchronous mailbox.

```text
┌─────────────────────────┐          ┌──────────────────────────────┐          ┌──────────────┐
│  Client (SOCKS5 / HTTP  │ ──POST─► │  User's Apps Script Cache    │ ◄─POST── │  Shared Node │ ──► Internet
│   or Android VpnService)│ ◄─GET─── │  (script.google.com fronted) │ ───GET─► │  (Exit)      │
└─────────────────────────┘          └──────────────────────────────┘          └──────────────┘
```

The client and the exit node **never connect directly to each other**. Both sides only store and fetch encrypted batches through Google Apps Script's cache, so the client only needs reachability to a Google front-end IP.

Because `apps_script/Code.gs` never calls `UrlFetchApp` for tunnel traffic, Google's 20,000 `UrlFetchApp` calls/day quota does not apply.

---

## Features

- **Shared multi-user nodes:** One node can serve many users simultaneously. Each user deploys their own Apps Script mailbox and registers with the node through the node's registration mailbox (sealed with NaCl anonymous box encryption to the node's public key).
- **End-to-end encryption:** X25519 key exchange, HKDF-SHA256 directional key derivation (`up` / `down`), and ChaCha20-Poly1305 AEAD. Google only sees base64-encoded ciphertext.
- **Domain fronting + Chrome TLS fingerprint:** Connects to Google edge IPs using a fronting SNI (default `www.google.com`) with `uTLS` (`HelloChrome_Auto`); `script.google.com` only appears inside the encrypted HTTP/2 stream.
- **Built-in route scanner:** Automatically probes Google edge IPs and fronting domains to find working low-latency routes and bypass poisoned DNS answers.
- **Reliable ordered transport over cache:** Handles batching, 95 KB chunking (to stay under Apps Script's 100 KB cache entry limit), gzip (`HuffmanOnly`) uplink compression, ACKs, retransmissions, round-robin per-stream fairness, and instant stream cancellation.
- **Cross-platform:** Desktop GUI (Windows via Wails + System Tray), Android VPN app (`gomobile` + `tun2socks` + FakeDNS), interactive console app, CLI, and Docker node.
- **Backup relay support:** Supports a self-hosted mailbox relay (`-mode relay`) or a personal GitHub Codespaces backup route when Google routes are unavailable.

---

## Quick Start for Users (Client)

To connect, you need two things:
1. **Your own Apps Script mailbox URL** (takes ~2 minutes, free with any Google account).
2. **A node string** (`mtn://...`) shared by a node operator (or from a node you host yourself).

### Step 1: Deploy Your Personal Apps Script Mailbox

1. Open [script.google.com](https://script.google.com) and click **New project**.
2. Delete the default code and paste the full contents of [`apps_script/Code.gs`](apps_script/Code.gs).
3. Leave `const AUTH_KEY = "";` empty if you are using the desktop/Android/interactive app (the app will automatically claim the mailbox with a random key on first setup).
   - *CLI users who want a manual key can set a strong random string (at least 32 characters) in `AUTH_KEY`.*
4. Click **Deploy → New deployment**.
5. Select type **Web app**:
   - **Execute as:** `Me`
   - **Who has access:** `Anyone`
6. Click **Deploy**, authorize the script when prompted, and copy the **Web app URL** (it must end with `/exec`, not `/dev`).

> If you ever lose your claimed key, open the Apps Script editor and run the `forgetKey` function from `Code.gs`, then set up the app again.

### Step 2: Connect

#### Option A: Desktop App / Android App / Interactive Console
1. Launch `mailbox-tunnel-desktop` (Windows), install the Android APK, or double-click `mailbox-tunnel` (interactive console).
2. Paste your **Web app URL** (ending in `/exec`) and the **node string** (`mtn://...`).
3. Click **Connect**.
   - On Windows, the app configures the system proxy automatically (and restores it on exit or crash recovery).
   - On Android, it starts a local `VpnService` with `tun2socks` and FakeDNS so apps route through the tunnel automatically.
4. To set up another device quickly, copy the **`mtc://...` setup code** from Settings and paste it in one step.

> **Multiple devices:** A node tracks one active session per `DeploymentID`. If two devices connect at the same time using the exact same Apps Script deployment, the newer connection replaces the older one. To stay connected on two devices simultaneously, deploy a second Apps Script project for the second device.

#### Option B: Command-Line Client (SOCKS5 & HTTP Proxy)
```bash
mailbox-tunnel \
  -mode client \
  -deployment YOUR_DEPLOYMENT_ID \
  -key YOUR_AUTH_KEY \
  -node "mtn://..." \
  -listen 127.0.0.1:1080
```
Point your browser or applications to `127.0.0.1:1080` (supports both **SOCKS5** and **HTTP** proxy on the same port).

#### Finding a Working Google Route (Optional)
If the default Google IP/SNI is blocked on your network, scan for working routes first:
```bash
mailbox-tunnel -mode scan -deployment YOUR_DEPLOYMENT_ID -key YOUR_AUTH_KEY
```
Then pass the fastest IP and SNI using `-google-ip` and `-front-domain`.

---

## Running a Shared Multi-User Node

A single node serves **many users concurrently**. Every user creates their own Apps Script mailbox (so Google's per-account limit of 30 concurrent Apps Script executions stays isolated to each user) and shares only your `mtn://...` node string.

### Step 1: Create the Node's Registration Mailbox

1. Follow the Apps Script steps above to deploy [`apps_script/Code.gs`](apps_script/Code.gs) as a Web app (**Execute as:** `Me`, **Who has access:** `Anyone`).
2. **Important:** Before deploying, set a strong random `AUTH_KEY` (32+ characters) directly inside `Code.gs` for the node's registration mailbox.
3. Save the **Deployment ID** (the segment between `/macros/s/` and `/exec` in the Web app URL) and your `AUTH_KEY`.

### Step 2: Deploy the Node

#### Option A: Docker Compose (Recommended for VPS / 24/7 Shared Nodes)

1. Clone the repository on any Linux VPS with open internet access and Docker installed:
   ```bash
   git clone https://github.com/lucyna-aqu/mailbox-tunnel.git
   cd mailbox-tunnel
   ```

2. Generate a permanent node key pair (no Go toolchain required):
   ```bash
   docker compose --profile tools run --rm keygen
   ```
   Copy the printed `node key (keep secret)` value.

3. Create your `.env` file from the example template:
   ```bash
   cp .env.example .env
   ```
   Edit `.env` and fill in:
   ```env
   MAILBOX_DEPLOYMENT_ID=YOUR_NODE_REGISTRATION_DEPLOYMENT_ID
   MAILBOX_AUTH_KEY=YOUR_NODE_REGISTRATION_AUTH_KEY
   MAILBOX_NODE_KEY=YOUR_GENERATED_PRIVATE_KEY
   ```

4. Start the node in the background:
   ```bash
   docker compose up -d
   ```

5. Retrieve your shareable `mtn://...` node string from the logs or local status page:
   ```bash
   docker compose logs -f node
   # or locally on the server:
   curl http://127.0.0.1:7860
   ```
   Share the `mtn://...` string with your users. Keep `MAILBOX_NODE_KEY` strictly secret.

> By default, `docker-compose.yml` binds the status page to `127.0.0.1:7860` so your `mtn://` string is not exposed publicly. To disable the status page completely, set `STATUS_LISTEN=` (empty) in `.env`.

#### Option B: Hugging Face Spaces (Free Docker Hosting)

See [`huggingface/README.md`](huggingface/README.md) for running a node on a free Docker Space:
1. Create or duplicate a Docker Space using this repository's [`Dockerfile`](Dockerfile) (`app_port: 7860`).
2. Add three Space **Secrets**: `MAILBOX_DEPLOYMENT_ID`, `MAILBOX_AUTH_KEY`, and `MAILBOX_NODE_KEY`.
3. To prevent free Spaces from sleeping after 48 hours of inactivity, set `KEEPALIVE_URL` in `Code.gs` to your Space URL and add an hourly time-driven trigger for `keepAlive()`.

#### Option C: Direct Binary (CLI)

```bash
# 1. Generate a node key once and keep it secret
mailbox-tunnel -mode keygen

# 2. Run the node
mailbox-tunnel \
  -mode node \
  -deployment YOUR_NODE_DEPLOYMENT_ID \
  -key YOUR_NODE_AUTH_KEY \
  -node-key YOUR_NODE_PRIVATE_KEY \
  -status-listen 127.0.0.1:7860
```

#### Self-Hosted Relay Mode (`-mode relay`)

If you want to host a mailbox server directly (without Google Apps Script), run a relay behind HTTPS (for example, behind Caddy/Nginx or inside a personal GitHub Codespace):
```bash
mailbox-tunnel \
  -mode relay \
  -listen :8080 \
  -deployment relay.example.com \
  -key YOUR_RELAY_AUTH_KEY \
  -node-key YOUR_NODE_PRIVATE_KEY
```
Clients can pass the resulting relay `mtn://...` string via `-backup` (or configure it in the desktop app) as an automatic fallback when Google is unreachable.

> **Warning about GitHub Actions workflows (`.github/workflows/node.yml` & `watchdog.yml`):**
> Do **not** run 24/7 chained tunnel/proxy nodes on GitHub Actions, especially on your main GitHub account. Running persistent proxy or tunneling workloads violates GitHub's Acceptable Use Policies and can result in workflows being disabled or your account being suspended. Use **Docker Compose on a VPS** or **Hugging Face Spaces** for persistent nodes.

---

## Configuration Reference

All main CLI flags can also be supplied via environment variables:

| CLI Flag | Environment Variable | Applies To | Description |
| :--- | :--- | :--- | :--- |
| `-mode` | — | All | `client`, `node`, `relay`, `scan`, or `keygen` |
| `-deployment` | `MAILBOX_DEPLOYMENT_ID` | client, node, relay, scan | Apps Script deployment ID (or public hostname for `relay`) |
| `-key` | `MAILBOX_AUTH_KEY` | client, node, relay, scan | `AUTH_KEY` from `Code.gs` (or relay key) |
| `-node` | `MAILBOX_NODE` | client | Node string (`mtn://<deployment>/<key>#<pubkey>`) |
| `-backup` | `MAILBOX_BACKUP` | client | Optional relay `mtn://` fallback route |
| `-node-key` | `MAILBOX_NODE_KEY` | node, relay | Base64url X25519 private key from `-mode keygen` |
| `-listen` | — | client, relay | Listen address (default `127.0.0.1:1080`) |
| `-status-listen` | `STATUS_LISTEN` | node | Optional plain-text status page address (e.g. `:7860`) |
| `-google-ip` | — | client, node, scan | Google edge IP to dial for domain fronting |
| `-front-domain` | — | client, node, scan | TLS SNI shown on the wire (default `www.google.com`) |
| `-duration` | — | client, node | Stop automatically after duration (e.g. `350m`, `0` = forever) |
| `-handover` | — | node | Stop accepting new registrations this long before `-duration` ends |

---

## Building from Source

Requires Go `1.27+` (see [`go.mod`](go.mod)).

### CLI Binary & Tests
```bash
go test ./...
go build -trimpath -ldflags="-s -w" -o mailbox-tunnel .
```

### Desktop App (Windows / Wails)
Install [Wails v2](https://wails.io/) and build inside `desktop/`:
```bash
cd desktop
wails build
```

### Android APK
See [`.github/workflows/android.yml`](.github/workflows/android.yml) for the exact `gomobile bind` + Gradle build steps (requires Go, Android NDK, Java 17, and Gradle 8.11.1), or trigger the **android** workflow manually in GitHub Actions to download the built APK artifact.

---

## Security & Trust Model

- **What Google sees:** Encrypted binary blobs (base64-encoded ChaCha20-Poly1305 ciphertext), request timings, and payload sizes. Google cannot read tunnel traffic or see target hostnames/IPs.
- **What the node sees:** The exit node decrypts your tunnel streams and opens outbound TCP connections on your behalf, so it sees destination hostnames/IPs (just like any standard proxy or VPN exit). End-to-end HTTPS/TLS inside your browser remains encrypted between you and the destination website.
- **Keep secrets safe:** Never commit `.env` or share `MAILBOX_NODE_KEY` or your personal mailbox `AUTH_KEY`. Sharing `mtn://...` only shares the node's registration mailbox and public key so users can register securely.
