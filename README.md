# mailbox-tunnel

A TCP tunnel that uses a Google Apps Script web app as a mailbox.

```
 client (SOCKS5) ──► your Apps Script cache ◄── node ──► internet
```

The client and the node never connect to each other. Both only store and fetch messages in the user's own script cache, so the client needs nothing but access to Google.

The script never calls `UrlFetchApp`, so the 20,000 fetches/day quota does not apply.

- **Shared nodes:** one node serves many users. A user registers their mailbox with a node through the node's own registration mailbox, sealed with the node's public key.
- **End-to-end encryption** between client and node (X25519, ChaCha20-Poly1305). Google only sees ciphertext.
- **Domain fronting:** the client dials a Google IP with `www.google.com` as SNI. `script.google.com` only appears inside the encrypted HTTP/2 stream.
- **Chrome TLS fingerprint** via uTLS.
- **Reliable ordering:** batches are acked and retransmitted when lost. Clients re-register automatically after a node restart.

The node sees the destinations of the traffic it carries, like any proxy.

## Using a node

1. Deploy `apps_script/Code.gs` as a Web app (Execute as: Me, Who has access: Anyone) with your own `AUTH_KEY`.
2. Run the client with a node string and point applications at `127.0.0.1:1080` (SOCKS5):
   ```
   mailbox-tunnel -mode client -deployment DEPLOYMENT_ID -key AUTH_KEY -node mtn://...
   ```

## Running a node

1. Deploy `apps_script/Code.gs` as above. This is the node's registration mailbox.
2. Create a node key once and keep it secret:
   ```
   mailbox-tunnel -mode keygen
   ```
3. Run the node somewhere with open internet. It prints the `mtn://` string to share with users:
   ```
   mailbox-tunnel -mode node -deployment DEPLOYMENT_ID -key AUTH_KEY -node-key NODE_KEY
   ```

All values can also be passed as the environment variables `MAILBOX_DEPLOYMENT_ID`, `MAILBOX_AUTH_KEY`, `MAILBOX_NODE` and `MAILBOX_NODE_KEY`.
