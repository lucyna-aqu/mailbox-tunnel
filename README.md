# mailbox-tunnel

A TCP tunnel that uses a Google Apps Script web app as a mailbox.

```
 client (SOCKS5) ──► Apps Script cache ◄── node ──► internet
```

The client and the node never connect to each other. Both only store and fetch messages in the script's cache, so the client needs nothing but access to Google.

The script never calls `UrlFetchApp`, so the 20,000 fetches/day quota does not apply.

- **Domain fronting:** the client dials a Google IP with `www.google.com` as SNI. `script.google.com` only appears inside the encrypted HTTP/2 stream.
- **Chrome TLS fingerprint** via uTLS.
- **Reliable ordering:** batches are acked and retransmitted when lost. Client restarts are detected by the node.

## Usage

1. Deploy `apps_script/Code.gs` as a Web app with your own `AUTH_KEY`.
2. Run the node somewhere with open internet:
   ```
   mailbox-tunnel -mode node -google-ip= -session NAME
   ```
3. Run the client and point applications at `127.0.0.1:1080` (SOCKS5):
   ```
   mailbox-tunnel -mode client -session NAME
   ```

The deployment ID and key are passed with `-deployment` and `-key`, or the `MAILBOX_DEPLOYMENT_ID` and `MAILBOX_AUTH_KEY` environment variables.
