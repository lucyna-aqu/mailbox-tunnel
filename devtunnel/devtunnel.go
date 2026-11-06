// Package devtunnel speaks to the tunnel service behind GitHub codespace ports.
// It is a package of its own so that only the desktop app carries the tunnel library.
package devtunnel

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
	"github.com/microsoft/dev-tunnels/go/tunnels"
)

// apiVersion is the only one the service accepts from GitHub's command line, which this follows.
const apiVersion = "2023-09-27-preview"

// Announce connects to the codespace's tunnel as a client for a moment and asks its host to pick up new ports.
// A port created through the service stays unreachable until the host hears this, as GitHub's own port forwarding does it.
func Announce(ctx context.Context, details tunnel.TunnelDetails) error {
	service, err := url.Parse(details.ServiceURI)
	if err != nil {
		return err
	}
	manager, err := tunnels.NewManager([]tunnels.UserAgent{{Name: "mailbox-tunnel"}}, nil, service, http.DefaultClient, apiVersion)
	if err != nil {
		return err
	}
	tokens := map[tunnels.TunnelAccessScope]string{
		tunnels.TunnelAccessScopeConnect:     details.ConnectAccessToken,
		tunnels.TunnelAccessScopeManagePorts: details.ManagePortsAccessToken,
	}
	described, err := manager.GetTunnel(ctx, &tunnels.Tunnel{
		AccessTokens: tokens,
		TunnelID:     details.TunnelID,
		ClusterID:    details.ClusterID,
		Domain:       details.Domain,
	}, &tunnels.TunnelRequestOptions{IncludePorts: true})
	if err != nil {
		return err
	}
	described.AccessTokens = tokens
	client, err := tunnels.NewClient(log.New(io.Discard, "", 0), described, false)
	if err != nil {
		return err
	}
	if err := client.Connect(ctx, ""); err != nil {
		return err
	}
	defer client.Close()
	return client.RefreshPorts(ctx)
}
