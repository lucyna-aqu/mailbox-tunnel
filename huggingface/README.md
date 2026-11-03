---
title: mailbox-tunnel node
emoji: 📮
colorFrom: gray
colorTo: blue
sdk: docker
app_port: 7860
pinned: false
---

# mailbox-tunnel node

This Space runs a [mailbox-tunnel](https://github.com/lucyna-aqu/mailbox-tunnel) node, which gives people behind internet shutdowns a way out through Google.

## Run your own

1. Click **Duplicate this Space**.
2. Deploy `apps_script/Code.gs` from the main repository as a Google Apps Script Web app (Execute as: Me, Who has access: Anyone). This is your node's registration mailbox.
3. Create a node key with `mailbox-tunnel -mode keygen`.
4. In the Space settings, add three **secrets**: `MAILBOX_DEPLOYMENT_ID`, `MAILBOX_AUTH_KEY` and `MAILBOX_NODE_KEY`.

The Space page then shows the `mtn://` string to share with users.

Free Spaces sleep after 48 hours without visits. To keep yours awake, set `KEEPALIVE_URL` in `Code.gs` to this Space's URL and add an hourly time-driven trigger for `keepAlive`.
