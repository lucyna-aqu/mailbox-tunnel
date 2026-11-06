package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// A relay can run in the user's own GitHub codespace, made from a public repository whose dev container starts it.
// The relay reads its keys from the user's codespace secrets, and the port it listens on is made public through the tunnel service.
const (
	relayPort             = 8080
	relayMachine          = "basicLinux32gb"
	relayIdleMinutes      = 240
	relayAuthSecret       = "MT_RELAY_AUTH_KEY"
	relayNodeKeySecret    = "MT_RELAY_NODE_KEY"
	tunnelsAPIVersion     = "2023-09-27-preview"
	codespaceReadyTimeout = 10 * time.Minute
)

// githubAPI is GitHub's API root; tests point it at a local server.
var githubAPI = "https://api.github.com"

// CodespaceRelay manages the relay codespace of one GitHub user.
type CodespaceRelay struct {
	Token string // the user's token, with the codespace scope
	Repo  string // the public repository with the relay's dev container, as "owner/name"
	Name  string // the codespace's name once it exists
	// Announce tells the codespace's tunnel host about the new port, which it does not notice on its own.
	// GitHub's command line does it by connecting to the tunnel; the desktop app sets it, so other builds stay free of that library.
	Announce func(context.Context, TunnelDetails) error
}

type codespaceInfo struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Connection struct {
		TunnelProperties TunnelDetails `json:"tunnelProperties"`
	} `json:"connection"`
}

// TunnelDetails are what GitHub tells about a codespace's port tunnel.
type TunnelDetails struct {
	ConnectAccessToken     string `json:"connectAccessToken"`
	ManagePortsAccessToken string `json:"managePortsAccessToken"`
	ServiceURI             string `json:"serviceUri"`
	TunnelID               string `json:"tunnelId"`
	ClusterID              string `json:"clusterId"`
	Domain                 string `json:"domain"`
}

// SetUp creates the relay codespace with fresh keys, makes its port public and returns the relay's node string.
// progress, when set, hears which step is running.
func (r *CodespaceRelay) SetUp(ctx context.Context, progress func(step string)) (string, error) {
	if progress == nil {
		progress = func(string) {}
	}
	var repository struct {
		ID int64 `json:"id"`
	}
	if err := r.call(ctx, http.MethodGet, "/repos/"+r.Repo, nil, &repository); err != nil {
		return "", fmt.Errorf("finding the relay repository: %w", err)
	}
	privateKey, publicKey, err := GenerateNodeKey()
	if err != nil {
		return "", err
	}
	authKey := make([]byte, 32)
	if _, err := rand.Read(authKey); err != nil {
		return "", err
	}
	authKeyText := base64URL.EncodeToString(authKey)

	progress("secrets")
	for name, value := range map[string]string{relayAuthSecret: authKeyText, relayNodeKeySecret: privateKey} {
		if err := r.setSecret(ctx, name, value, repository.ID); err != nil {
			return "", fmt.Errorf("saving the relay keys: %w", err)
		}
	}

	progress("create")
	var created codespaceInfo
	request := map[string]any{
		"repository_id":        repository.ID,
		"ref":                  "main",
		"machine":              relayMachine,
		"idle_timeout_minutes": relayIdleMinutes,
		"display_name":         "Mailbox Tunnel relay",
	}
	if err := r.call(ctx, http.MethodPost, "/user/codespaces", request, &created); err != nil {
		return "", fmt.Errorf("creating the codespace: %w", err)
	}
	r.Name = created.Name

	if err := r.Start(ctx, progress); err != nil {
		return "", err
	}
	host := r.host()
	return nodeScheme + host + "/" + authKeyText + "#" + publicKey, nil
}

// Start wakes the codespace if it sleeps, makes sure the relay's port is public and waits until the relay answers.
func (r *CodespaceRelay) Start(ctx context.Context, progress func(step string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	progress("start")
	info, err := r.waitAvailable(ctx)
	if err != nil {
		return err
	}
	progress("port")
	if err := publishPort(ctx, info.Connection.TunnelProperties, relayPort); err != nil {
		return fmt.Errorf("making the relay's port public: %w", err)
	}
	if r.Announce != nil {
		if err := r.Announce(ctx, info.Connection.TunnelProperties); err != nil {
			return fmt.Errorf("telling the codespace about the port: %w", err)
		}
	}
	progress("relay")
	return r.waitForRelay(ctx)
}

// Delete removes the codespace from the user's account.
func (r *CodespaceRelay) Delete(ctx context.Context) error {
	return r.call(ctx, http.MethodDelete, "/user/codespaces/"+url.PathEscape(r.Name), nil, nil)
}

// host is the relay's public host name.
func (r *CodespaceRelay) host() string {
	return fmt.Sprintf("%s-%d.app.github.dev", r.Name, relayPort)
}

// waitAvailable starts the codespace when it is shut down and polls until it runs, then returns it with its tunnel details.
func (r *CodespaceRelay) waitAvailable(ctx context.Context) (codespaceInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, codespaceReadyTimeout)
	defer cancel()
	started := false
	for {
		var info codespaceInfo
		if err := r.call(ctx, http.MethodGet, "/user/codespaces/"+url.PathEscape(r.Name), nil, &info); err != nil {
			return info, fmt.Errorf("checking the codespace: %w", err)
		}
		switch info.State {
		case "Available":
			// The tunnel details are asked for only once the codespace runs, as GitHub's own command line does.
			if err := r.call(ctx, http.MethodGet, "/user/codespaces/"+url.PathEscape(r.Name)+"?internal=true&refresh=true", nil, &info); err != nil {
				return info, fmt.Errorf("reading the codespace's tunnel: %w", err)
			}
			if info.Connection.TunnelProperties.TunnelID == "" {
				return info, errors.New("the codespace did not say how to reach its ports")
			}
			return info, nil
		case "Shutdown":
			if !started {
				if err := r.call(ctx, http.MethodPost, "/user/codespaces/"+url.PathEscape(r.Name)+"/start", nil, nil); err != nil {
					return info, fmt.Errorf("starting the codespace: %w", err)
				}
				started = true
			}
		case "Deleted", "Failed", "Unavailable":
			return info, fmt.Errorf("the codespace is %s", strings.ToLower(info.State))
		}
		select {
		case <-ctx.Done():
			return info, fmt.Errorf("the codespace did not start in time (last state %s)", info.State)
		case <-time.After(5 * time.Second):
		}
	}
}

// waitForRelay pings the relay's mailbox until it answers; the relay starts a little after the codespace.
func (r *CodespaceRelay) waitForRelay(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	mailbox := NewMailbox(MailboxConfig{DeploymentID: r.host()})
	for {
		// Any answer from the mailbox server, even a refusal of the empty key, means the relay is up and its port is public.
		_, err := mailbox.call(ctx, map[string]any{"op": "ping"})
		if err == nil || strings.HasPrefix(err.Error(), "mailbox:") {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the relay did not answer: %w", err)
		case <-time.After(5 * time.Second):
		}
	}
}

// setSecret saves a codespace secret for the user, readable only by codespaces of the given repository.
// GitHub takes secrets sealed to its public key.
func (r *CodespaceRelay) setSecret(ctx context.Context, name, value string, repositoryID int64) error {
	var key struct {
		ID  string `json:"key_id"`
		Key string `json:"key"`
	}
	if err := r.call(ctx, http.MethodGet, "/user/codespaces/secrets/public-key", nil, &key); err != nil {
		return err
	}
	sealed, err := sealSecret(key.Key, value)
	if err != nil {
		return err
	}
	request := map[string]any{"encrypted_value": sealed, "key_id": key.ID, "selected_repository_ids": []int64{repositoryID}}
	return r.call(ctx, http.MethodPut, "/user/codespaces/secrets/"+name, request, nil)
}

func sealSecret(publicKeyBase64, value string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(publicKeyBase64)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("GitHub's secrets key is not a 32-byte key")
	}
	var publicKey [32]byte
	copy(publicKey[:], decoded)
	sealed, err := box.SealAnonymous(nil, []byte(value), &publicKey, rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// call sends a request to GitHub's API and decodes the answer into result when it is not nil.
func (r *CodespaceRelay) call(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		content, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(content)
	}
	request, err := http.NewRequestWithContext(ctx, method, githubAPI+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+r.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return doJSON(request, result)
}

// publishPort creates the port in the codespace's tunnel with anonymous access, which is what a public port is.
// A port that exists already is replaced, the way GitHub's own command line changes a port's visibility.
func publishPort(ctx context.Context, properties TunnelDetails, port int) error {
	portURL, err := tunnelPortURL(properties, port)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"portNumber": port,
		"protocol":   "http",
		"accessControl": map[string]any{"entries": []map[string]any{
			{"type": "Anonymous", "subjects": []string{}, "scopes": []string{"connect"}},
		}},
	})
	send := func(method string, content []byte, onlyIfNew bool) (int, error) {
		var reader io.Reader
		if content != nil {
			reader = bytes.NewReader(content)
		}
		request, err := http.NewRequestWithContext(ctx, method, portURL, reader)
		if err != nil {
			return 0, err
		}
		request.Header.Set("Authorization", "Tunnel "+properties.ManagePortsAccessToken)
		request.Header.Set("Content-Type", "application/json;charset=UTF-8")
		if onlyIfNew {
			request.Header.Set("If-None-Match", "*")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode, nil
	}
	status, err := send(http.MethodPut, body, true)
	if err == nil && status == http.StatusConflict {
		if status, err = send(http.MethodDelete, nil, false); err == nil && status < 300 {
			status, err = send(http.MethodPut, body, true)
		}
	}
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("tunnel service: HTTP %d", status)
	}
	return nil
}

// tunnelPortURL is the tunnel service's address for one port: the cluster is a subdomain of the service.
func tunnelPortURL(properties TunnelDetails, port int) (string, error) {
	service, err := url.Parse(properties.ServiceURI)
	if err != nil || service.Host == "" || properties.TunnelID == "" || properties.ClusterID == "" {
		return "", errors.New("incomplete tunnel details")
	}
	host := strings.TrimPrefix(service.Host, "global.")
	if !strings.HasPrefix(host, properties.ClusterID+".") {
		host = properties.ClusterID + "." + host
	}
	return fmt.Sprintf("https://%s/tunnels/%s/ports/%d?api-version=%s", host, url.PathEscape(properties.TunnelID), port, tunnelsAPIVersion), nil
}

// doJSON sends a request and decodes a successful answer into result when it is not nil.
func doJSON(request *http.Request, result any) error {
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if response.StatusCode >= 300 {
		var problem struct {
			Message string `json:"message"`
		}
		json.Unmarshal(content, &problem)
		return fmt.Errorf("HTTP %d %s", response.StatusCode, problem.Message)
	}
	if result == nil || len(content) == 0 {
		return nil
	}
	return json.Unmarshal(content, result)
}
