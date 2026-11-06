package tunnel

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestSealSecretOpensWithGitHubsKey(t *testing.T) {
	public, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealSecret(base64.StdEncoding.EncodeToString(public[:]), "relay key")
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(sealed)
	opened, ok := box.OpenAnonymous(nil, decoded, public, private)
	if !ok || string(opened) != "relay key" {
		t.Fatalf("opened %q, %v", opened, ok)
	}
	if _, err := sealSecret("not a key", "x"); err == nil {
		t.Fatal("a bad key was accepted")
	}
}

func TestTunnelPortURL(t *testing.T) {
	for service, want := range map[string]string{
		"https://global.rel.tunnels.api.visualstudio.com/": "https://usw2.rel.tunnels.api.visualstudio.com/tunnels/abc/ports/8080?api-version=" + tunnelsAPIVersion,
		"https://rel.tunnels.api.visualstudio.com":         "https://usw2.rel.tunnels.api.visualstudio.com/tunnels/abc/ports/8080?api-version=" + tunnelsAPIVersion,
		"https://usw2.rel.tunnels.api.visualstudio.com/":   "https://usw2.rel.tunnels.api.visualstudio.com/tunnels/abc/ports/8080?api-version=" + tunnelsAPIVersion,
	} {
		got, err := tunnelPortURL(TunnelDetails{ServiceURI: service, TunnelID: "abc", ClusterID: "usw2"}, 8080)
		if err != nil || got != want {
			t.Errorf("%s: got %s, %v", service, got, err)
		}
	}
	if _, err := tunnelPortURL(TunnelDetails{ServiceURI: "https://rel.tunnels.api.visualstudio.com"}, 8080); err == nil {
		t.Error("missing tunnel details were accepted")
	}
}
