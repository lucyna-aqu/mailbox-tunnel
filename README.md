# Mailbox Tunnel

[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.27+-00ADD8.svg)](go.mod)
[![Platforms](https://img.shields.io/badge/Platforms-Windows%20%7C%20Android%20%7C%20Linux%20%7C%20Docker-brightgreen.svg)](#building-from-source)

**A censorship-resistant TCP tunnel that uses Google Apps Script's `CacheService` as an encrypted, asynchronous mailbox.**

The client and exit node **never connect directly to each other**. Both ends only store and fetch encrypted batches through Google Apps Script's cache, so the client only needs reachability to any Google edge IP — and the exit node needs **zero open ports, no domain, and no public IPv4**.

```text
┌───────────────────────────┐          ┌─────────────────────────────────┐          ┌────────────────┐
│  Client (Windows GUI /    │ ──POST─► │  User's Apps Script Cache       │ ◄─POST── │  Exit Node     │ ──► Internet
│  Android VPN / SOCKS5)    │ ◄─GET─── │  (Domain-fronted Google Edge)   │ ───GET─► │  (VPS / NAT)   │
└───────────────────────────┘          └─────────────────────────────────┘          └────────────────┘
```

---

## Why Mailbox Tunnel?

| Problem with Traditional Tunnels | How Mailbox Tunnel Solves It |
| :--- | :--- |
| Server IPs get blocked or throttled by DPI | Client only talks to Google edge IPs using Chrome TLS fingerprints (`uTLS`) + domain fronting |
| Apps Script proxies hit the 20,000 `UrlFetchApp`/day quota | Traffic never uses `UrlFetchApp` — it uses `CacheService` only |
| One shared proxy hits per-account execution limits | Each user deploys their own free Apps Script mailbox, isolating Google's 30-concurrent-execution limit per account |
| Hosting a node requires a dedicated public IPv4 and open ports | The exit node is **outbound-only** — it runs cleanly on cheap NAT/IPv6 VPSes or even behind home NAT |
| Unusable on slow/lossy uplinks | 95 KB cache chunking, `HuffmanOnly` gzip compression, round-robin stream fairness, and instant stream cancellation |

---

## Quick Start (For Users)

You need two things to connect:
1. **Your personal Apps Script Web App URL** (free, takes ~2 minutes with any Google account).
2. **A node string** (`mtn://...`) from a trusted node operator (or a node you host yourself).

### 1. Deploy Your Personal Mailbox

1. Open [script.google.com](https://script.google.com) and click **New project**.
2. Replace the default code with the full contents of [`apps_script/Code.gs`](apps_script/Code.gs).
   - *Leave `const AUTH_KEY = "";` empty* — the app automatically claims the mailbox with a random key on first setup.
3. Click **Deploy → New deployment** → select **Web app**:
   - **Execute as:** `Me`
   - **Who has access:** `Anyone`
4. Click **Deploy**, authorize the script, and copy the **Web app URL** (it must end with `/exec`, not `/dev`).

### 2. Connect

- **Windows Desktop App** (`mailbox-tunnel-desktop`): Paste your Web app URL and `mtn://...` node string, then click **Connect**. The app configures the Windows system proxy automatically and restores it on disconnect or crash recovery.
- **Android App** (`mailbox-tunnel.apk`): Paste your Web app URL and `mtn://...` node string (or a single `mtc://...` setup code from Desktop), then tap **Connect**. Routes device traffic via Android `VpnService` + `tun2socks` + local FakeDNS.
- **Interactive Console**: Double-click `mailbox-tunnel` with no arguments for a guided setup.

> **Using multiple devices?** Copy your **`mtc://...` setup code** from Settings to configure another device in one paste. Note that a node keeps one active session per `DeploymentID`; to stay connected on two devices simultaneously, deploy a second Apps Script project.
>
> **Lost your claimed key?** Open your Apps Script editor, run the `forgetKey` function, and set up the app again.

<details>
<summary><strong>CLI Client & Route Scanner (Advanced)</strong></summary>

For CLI usage, set a strong random `AUTH_KEY` (32+ chars) inside `Code.gs` before deploying, then run:

```bash
# Start local SOCKS5 + HTTP proxy on 127.0.0.1:1080
mailbox-tunnel \
  -mode client \
  -deployment YOUR_DEPLOYMENT_ID \
  -key YOUR_AUTH_KEY \
  -node "mtn://..." \
  -listen 127.0.0.1:1080
```

If the default Google edge IP/SNI is blocked on your network, scan for fast working routes first:

```bash
mailbox-tunnel -mode scan -deployment YOUR_DEPLOYMENT_ID -key YOUR_AUTH_KEY
```

Then pass the best result via `-google-ip` and `-front-domain`.

</details>

---

## Hosting a Shared Exit Node

A single lightweight node (even **512 MB RAM**, **NAT IPv4**, or **IPv6-only**) can serve dozens of users concurrently without opening a single inbound port.

### 1. Create the Node's Registration Mailbox

1. Deploy [`apps_script/Code.gs`](apps_script/Code.gs) as a Web app (**Execute as:** `Me`, **Who has access:** `Anyone`).
2. **Important:** Set a strong random `AUTH_KEY` (32+ characters) directly inside `Code.gs` before deploying.
3. Copy the **Deployment ID** (the segment between `/macros/s/` and `/exec`) and your `AUTH_KEY`.

### 2. Run the Node

#### Option A: Docker Compose (Recommended for 1 GB+ VPS)

```bash
git clone https://github.com/lucyna-aqu/mailbox-tunnel.git
cd mailbox-tunnel

# 1. Generate a permanent X25519 node key pair (keep the private key secret)
docker compose --profile tools run --rm keygen

# 2. Configure environment
cp .env.example .env
# Edit .env with MAILBOX_DEPLOYMENT_ID, MAILBOX_AUTH_KEY, and MAILBOX_NODE_KEY

# 3. Start the node
docker compose up -d

# 4. Print your shareable mtn://... node string
docker compose logs -f node
# or locally on the server:
curl http://127.0.0.1:7860
```

By default, `docker-compose.yml` binds the status page to `127.0.0.1:7860` so your `mtn://` string is never exposed publicly. Set `STATUS_LISTEN=` in `.env` to disable it completely.

#### Option B: Standalone Binary (Best for 512 MB / NAT VPS)

The Go binary is statically linked (~15 MB) and uses under 50 MB RAM with zero Docker overhead:

```bash
# Generate your private node key once
./mailbox-tunnel -mode keygen

# Run the node
./mailbox-tunnel \
  -mode node \
  -deployment YOUR_NODE_DEPLOYMENT_ID \
  -key YOUR_NODE_AUTH_KEY \
  -node-key YOUR_NODE_PRIVATE_KEY \
  -status-listen 127.0.0.1:7860
```

Share the printed `mtn://...` string with your users. Keep `MAILBOX_NODE_KEY` strictly secret.

<details>
<summary><strong>Self-Hosted Backup Relay, Hugging Face & GitHub Actions Notes</strong></summary>

#### Self-Hosted Relay Mode (`-mode relay`)
Run a direct in-memory mailbox + exit node behind HTTPS (e.g., behind Caddy/Nginx or inside a personal GitHub Codespace) as a backup route when Google is unreachable:

```bash
mailbox-tunnel \
  -mode relay \
  -listen :8080 \
  -deployment relay.example.com \
  -key YOUR_RELAY_AUTH_KEY \
  -node-key YOUR_NODE_PRIVATE_KEY
```

Pass the resulting `mtn://...` string to clients via `-backup` (or in Desktop Settings).

#### Hugging Face Spaces
See [`huggingface/README.md`](huggingface/README.md) if deploying via Docker Spaces (`app_port: 7860` with `MAILBOX_DEPLOYMENT_ID`, `MAILBOX_AUTH_KEY`, and `MAILBOX_NODE_KEY` secrets). Note that Hugging Face free-tier policies may change over time.

#### GitHub Actions Warning
Do **not** run 24/7 chained tunnel nodes on GitHub Actions (`.github/workflows/node.yml` / `watchdog.yml`). Persistent proxy workloads violate GitHub's Acceptable Use Policies and can lead to account suspension. Use a VPS instead.

</details>

---

## How It Works (Architecture)

- **Zero-direct-contact registration:** Clients seal their personal `DeploymentID`, `AuthKey`, ephemeral X25519 public key, and `RunID` using NaCl anonymous box encryption (`box.SealAnonymous`) to the node's public key, then drop it into the node's registration mailbox.
- **End-to-end encryption:** Ephemeral X25519 key exchange + HKDF-SHA256 directional key derivation (`up` / `down`) + ChaCha20-Poly1305 AEAD per batch. Google only sees opaque base64 ciphertext.
- **Domain fronting & TLS camouflage:** Uses `uTLS` (`HelloChrome_Auto`) over HTTP/2 to dial Google edge IPs with a fronting SNI (default `www.google.com`). `script.google.com` only appears inside the encrypted HTTP/2 `:authority` header, and redirects are followed manually through the fronted connection.
- **Reliable ordered transport over cache:** Splits batches into 95 KB cache parts (under Apps Script's 100 KB entry limit), compresses uploads with `gzip.HuffmanOnly` to reclaim base64 overhead, uses asymmetric parallelism (9 download / 3 upload slots), round-robin per-stream fairness, and instant queued-frame dropping (`Sender.Drop`) when a stream closes.
- **Android VPN without DNS round trips:** Combines `gomobile`, `tun2socks`, and an in-process `FakeDNS` (`198.18.0.0/15`) so DNS lookups resolve instantly locally while actual domain names are sent to the exit node in `OPEN` frames.

---

## Security & Trust Model

- **What Google sees:** Base64-encoded ciphertext blobs, payload sizes, and request timings. Google cannot decrypt traffic or see destination hostnames/IPs.
- **What the exit node sees:** Destination hostnames/IPs and decrypted TCP streams (just like any standard VPN or proxy exit). End-to-end HTTPS/TLS between your browser and websites remains encrypted.
- **Built-in node hardening:**
  - Exit dials block loopback, RFC1918 private LANs, link-local / cloud metadata (`169.254.169.254`), CGNAT, and FakeDNS ranges via socket-level `Dialer.Control` (immune to DNS rebinding).
  - Registration validates `DeploymentID` format to prevent mailbox SSRF.
  - Bounded per-stream buffers (8 MB), concurrent stream caps (256/session), session caps (200/node), and in-memory relay caps (128 MB).
- **Keep secrets safe:** Never commit `.env` or share `MAILBOX_NODE_KEY` or your personal `AUTH_KEY` / `mtc://` setup code.

---

<details>
<summary><strong>Configuration Reference (CLI Flags & Environment Variables)</strong></summary>

| CLI Flag | Environment Variable | Applies To | Description |
| :--- | :--- | :--- | :--- |
| `-mode` | — | All | `client`, `node`, `relay`, `scan`, or `keygen` |
| `-deployment` | `MAILBOX_DEPLOYMENT_ID` | client, node, relay, scan | Apps Script deployment ID (or public hostname for `relay`) |
| `-key` | `MAILBOX_AUTH_KEY` | client, node, relay, scan | `AUTH_KEY` from `Code.gs` (or relay key) |
| `-node` | `MAILBOX_NODE` | client | Node string (`mtn://<deployment>/<key>#<pubkey>`) |
| `-backup` | `MAILBOX_BACKUP` | client | Optional relay `mtn://` fallback route |
| `-node-key` | `MAILBOX_NODE_KEY` | node, relay | Base64url X25519 private key from `-mode keygen` |
| `-listen` | — | client, relay | Listen address (default `127.0.0.1:1080`) |
| `-status-listen` | `STATUS_LISTEN` | node | Optional plain-text status page address (e.g. `127.0.0.1:7860`) |
| `-google-ip` | — | client, node, scan | Google edge IP to dial for domain fronting |
| `-front-domain` | — | client, node, scan | TLS SNI shown on the wire (default `www.google.com`) |
| `-duration` | — | client, node | Stop automatically after duration (e.g. `350m`, `0` = forever) |
| `-handover` | — | node | Stop accepting new registrations this long before `-duration` ends |

</details>

---

## Building from Source

Requires **Go 1.27+** (see [`go.mod`](go.mod)).

```bash
# Run tests & build CLI binary
go test ./...
go build -trimpath -ldflags="-s -w" -o mailbox-tunnel .

# Build Windows Desktop GUI (requires Wails v2)
cd desktop && wails build
```

For the **Android APK**, trigger the [`android`](.github/workflows/android.yml) GitHub Actions workflow or run `gomobile bind` + Gradle 8.11.1 (Java 17 + Android NDK) locally.

---

## License

Licensed under the [GNU General Public License v3.0](LICENSE).
