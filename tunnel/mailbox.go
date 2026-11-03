package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

const (
	waitTimeout      = 20 * time.Second
	callTimeout      = waitTimeout + 30*time.Second
	maxResponseBytes = 64 << 20
)

type MailboxConfig struct {
	DeploymentID string
	AuthKey      string
	GoogleIP     string // if set, dial this IP and send FrontDomain as SNI
	FrontDomain  string
}

// Mailbox is an HTTP/2 client for the Apps Script mailbox with a Chrome TLS fingerprint.
// Redirects are followed manually so they are fronted too.
type Mailbox struct {
	config MailboxConfig
	client *http.Client
}

func NewMailbox(config MailboxConfig) *Mailbox {
	transport := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			dialAddress := address
			if config.GoogleIP != "" {
				dialAddress = net.JoinHostPort(config.GoogleIP, port)
			}
			rawConn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, dialAddress)
			if err != nil {
				return nil, err
			}
			tlsConn := utls.UClient(rawConn, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				rawConn.Close()
				return nil, err
			}
			if protocol := tlsConn.ConnectionState().NegotiatedProtocol; protocol != "h2" {
				tlsConn.Close()
				return nil, fmt.Errorf("expected HTTP/2, server chose %q", protocol)
			}
			return tlsConn, nil
		},
		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
	return &Mailbox{
		config: config,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Put stores payload under key and returns the progress at progressKey, or -1.
func (m *Mailbox) Put(ctx context.Context, key string, payload []byte, progressKey string) (int, error) {
	reply, err := m.call(ctx, map[string]any{
		"op":          "put",
		"key":         key,
		"value":       base64.StdEncoding.EncodeToString(payload),
		"progressKey": progressKey,
	})
	if err != nil {
		return -1, err
	}
	return progressFrom(reply), nil
}

// Status returns the progress at progressKey, or -1.
func (m *Mailbox) Status(ctx context.Context, progressKey string) (int, error) {
	reply, err := m.call(ctx, map[string]any{"op": "status", "progressKey": progressKey})
	if err != nil {
		return -1, err
	}
	return progressFrom(reply), nil
}

// Wait deletes the acked keys, records progress, then long-polls key without removing it.
// Returns nil on timeout.
func (m *Mailbox) Wait(ctx context.Context, key string, acked []string, progressKey string, progress int) ([]byte, error) {
	return m.valueFrom(m.call(ctx, map[string]any{
		"op":          "wait",
		"key":         key,
		"timeoutMs":   waitTimeout.Milliseconds(),
		"ack":         acked,
		"progressKey": progressKey,
		"progress":    progress,
	}))
}

// Take long-polls key and removes it. Returns nil on timeout.
func (m *Mailbox) Take(ctx context.Context, key string) ([]byte, error) {
	return m.valueFrom(m.call(ctx, map[string]any{"op": "take", "key": key, "timeoutMs": waitTimeout.Milliseconds()}))
}

func (m *Mailbox) valueFrom(reply map[string]any, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	encoded, ok := reply["value"].(string)
	if !ok {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(encoded)
}

func progressFrom(reply map[string]any) int {
	if progress, ok := reply["progress"].(float64); ok {
		return int(progress)
	}
	return -1
}

// Ping checks the deployment and the key.
func (m *Mailbox) Ping(ctx context.Context) error {
	_, err := m.call(ctx, map[string]any{"op": "ping"})
	return err
}

func (m *Mailbox) call(ctx context.Context, request map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	request["k"] = m.config.AuthKey
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	execURL := "https://script.google.com/macros/s/" + m.config.DeploymentID + "/exec"
	status, location, content, err := m.do(ctx, http.MethodPost, execURL, body)
	if err != nil {
		return nil, err
	}
	if status >= 300 && status < 400 && location != "" {
		status, _, content, err = m.do(ctx, http.MethodGet, location, nil)
		if err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	var reply map[string]any
	if err := json.Unmarshal(content, &reply); err != nil {
		return nil, fmt.Errorf("not JSON (wrong deployment or key?): %.80s", content)
	}
	if message, failed := reply["error"]; failed {
		return nil, fmt.Errorf("mailbox: %v", message)
	}
	return reply, nil
}

func (m *Mailbox) do(ctx context.Context, method, rawURL string, body []byte) (int, string, []byte, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return 0, "", nil, err
	}
	requestURL := *target
	if m.config.GoogleIP != "" {
		requestURL.Host = m.config.FrontDomain
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bodyReader)
	if err != nil {
		return 0, "", nil, err
	}
	request.Host = target.Host
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := m.client.Do(request)
	if err != nil {
		return 0, "", nil, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return 0, "", nil, err
	}
	return response.StatusCode, response.Header.Get("Location"), content, nil
}
