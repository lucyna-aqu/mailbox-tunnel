package tunnel

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

const (
	waitTimeout      = 20 * time.Second
	callTimeout      = waitTimeout + 30*time.Second
	connectTimeout   = 10 * time.Second
	compressAbove    = 1024
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
	config  MailboxConfig
	client  *http.Client
	failing atomic.Bool

	// Recovered gets a signal when a call succeeds after network failures, for example after sleep or a Wi-Fi change.
	Recovered chan struct{}
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
			ctx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()
			rawConn, err := (&net.Dialer{}).DialContext(ctx, network, dialAddress)
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
		// A connection that went silent, as after sleep or a network change, is found by a ping and dropped within seconds.
		ReadIdleTimeout:  10 * time.Second,
		PingTimeout:      5 * time.Second,
		WriteByteTimeout: 10 * time.Second,
	}
	return &Mailbox{
		Recovered: make(chan struct{}, 1),
		config:    config,
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

// Version returns the mailbox script's version; scripts from before versioning report 1.
func (m *Mailbox) Version(ctx context.Context) (int, error) {
	reply, err := m.call(ctx, map[string]any{"op": "ping"})
	if err != nil {
		return 0, err
	}
	if version, ok := reply["version"].(float64); ok {
		return int(version), nil
	}
	return 1, nil
}

func (m *Mailbox) call(parent context.Context, request map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(parent, callTimeout)
	defer cancel()
	started := time.Now()

	request["k"] = m.config.AuthKey
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	execURL := "https://script.google.com/macros/s/" + m.config.DeploymentID + "/exec"
	status, location, content, err := m.do(ctx, http.MethodPost, execURL, body)
	if err == nil && status >= 300 && status < 400 && location != "" {
		status, _, content, err = m.do(ctx, http.MethodGet, location, nil)
	}
	if err != nil {
		if parent.Err() == nil {
			m.failing.Store(true)
		}
		return nil, err
	}
	if m.failing.Swap(false) {
		select {
		case m.Recovered <- struct{}{}:
		default:
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
	if op := request["op"]; op != "wait" && op != "take" {
		recordLatency(time.Since(started))
	}
	return reply, nil
}

var latency struct {
	sync.Mutex
	average time.Duration
}

// recordLatency keeps a moving average of mailbox round trips, leaving out long polls.
func recordLatency(sample time.Duration) {
	latency.Lock()
	defer latency.Unlock()
	if latency.average == 0 {
		latency.average = sample
		return
	}
	latency.average = (latency.average*4 + sample) / 5
}

// Latency is the moving average time of a mailbox round trip, or 0 before the first one.
func Latency() time.Duration {
	latency.Lock()
	defer latency.Unlock()
	return latency.average
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
	// Google unpacks gzip request bodies before the script runs.
	// Base64 shrinks by about a quarter, which wins back most of its overhead on slow uplinks.
	compressed := len(body) > compressAbove
	if compressed {
		var packed bytes.Buffer
		// Base64 of encrypted data has no repeats to find, and the other levels then store it uncompressed.
		writer, _ := gzip.NewWriterLevel(&packed, gzip.HuffmanOnly)
		writer.Write(body)
		writer.Close()
		body = packed.Bytes()
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
	if compressed {
		request.Header.Set("Content-Encoding", "gzip")
	}
	response, err := m.client.Do(request)
	if err != nil {
		// Drop the URL from the error: it contains the deployment ID, which must not end up in logs.
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return 0, "", nil, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return 0, "", nil, err
	}
	return response.StatusCode, response.Header.Get("Location"), content, nil
}
