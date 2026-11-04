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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

const (
	waitTimeout = 20 * time.Second

	// A call is cut only when it goes silent, so data that keeps moving on a slow line is not cut.
	// Before its answer starts, a call may stay silent for answerLimit, and a long poll for its whole wait as well.
	silenceLimit = 15 * time.Second
	answerLimit  = 20 * time.Second
	silenceCheck = 250 * time.Millisecond

	// callTimeout only catches a call that keeps trickling without ever finishing.
	callTimeout      = 3 * time.Minute
	connectTimeout   = 10 * time.Second
	compressAbove    = 1024
	maxResponseBytes = 64 << 20
)

// errSilent reports a call that was given up because no data moved for too long.
var errSilent = errors.New("no data moved, gave up")

type MailboxConfig struct {
	DeploymentID string // an Apps Script deployment ID, or the host name of a self-hosted mailbox
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
	// Failing gets a signal when calls start failing, so the client can check its route at once.
	Failing chan struct{}
}

// selfHosted reports whether a deployment names a self-hosted mailbox; Apps Script IDs never contain a dot.
func selfHosted(deploymentID string) bool {
	return strings.Contains(deploymentID, ".")
}

func NewMailbox(config MailboxConfig) *Mailbox {
	// A self-hosted mailbox is reached directly; fronting only works for Google's own hosts.
	if selfHosted(config.DeploymentID) {
		config.GoogleIP = ""
	}
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
		Failing:   make(chan struct{}, 1),
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

var (
	// ErrClaimed means the mailbox already has another key, set by another device or written into its Code.gs.
	ErrClaimed = errors.New("mailbox already has another key")
	// ErrCannotClaim means the mailbox runs a Code.gs from before keys could be set by the app.
	ErrCannotClaim = errors.New("mailbox cannot take a key from the app")
)

// Claim gives a mailbox that has no key yet the configured one.
func (m *Mailbox) Claim(ctx context.Context) error {
	_, err := m.call(ctx, map[string]any{"op": "claim"})
	if err != nil {
		switch err.Error() {
		case "mailbox: claimed":
			return ErrClaimed
		case "mailbox: unauthorized":
			return ErrCannotClaim
		}
	}
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
	if selfHosted(m.config.DeploymentID) {
		execURL = "https://" + m.config.DeploymentID + "/"
	}
	answerWithin := answerLimit
	if op := request["op"]; op == "wait" || op == "take" {
		answerWithin = waitTimeout + 10*time.Second
	}
	status, location, content, err := m.do(ctx, cancel, http.MethodPost, execURL, body, answerWithin)
	if err == nil && status >= 300 && status < 400 && location != "" {
		status, _, content, err = m.do(ctx, cancel, http.MethodGet, location, nil, answerLimit)
	}
	if err != nil {
		if parent.Err() == nil && !m.failing.Swap(true) {
			select {
			case m.Failing <- struct{}{}:
			default:
			}
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

// do sends one request; if it goes silent, its watch gives up on the whole call through cancel.
func (m *Mailbox) do(ctx context.Context, cancel context.CancelFunc, method, rawURL string, body []byte, answerWithin time.Duration) (int, string, []byte, error) {
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
	// The call's context is cancelled only when the call ends or the watch fires, as cancelling a finished request early upsets the connection.
	watch := newSilenceWatch(silenceLimit)
	stop := make(chan struct{})
	defer close(stop)
	go watch.run(stop, cancel)
	newBody := func() io.Reader {
		return &progressReader{reader: bytes.NewReader(body), watch: watch, remaining: len(body), onDone: func() { watch.expect(answerWithin) }}
	}
	var bodyReader io.Reader
	if body == nil {
		watch.expect(answerWithin)
	} else {
		bodyReader = newBody()
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bodyReader)
	if err != nil {
		return 0, "", nil, err
	}
	request.Host = target.Host
	if body != nil {
		request.ContentLength = int64(len(body))
		// The transport resends the body from here when it has to retry on a fresh connection.
		request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(newBody()), nil }
		request.Header.Set("Content-Type", "application/json")
	}
	if compressed {
		request.Header.Set("Content-Encoding", "gzip")
	}
	response, err := m.client.Do(request)
	if err != nil {
		return 0, "", nil, callError(err, watch)
	}
	defer response.Body.Close()
	// The answer has started, so from here only silence in the middle of it counts.
	watch.expect(silenceLimit)
	content, err := io.ReadAll(&progressReader{reader: io.LimitReader(response.Body, maxResponseBytes), watch: watch})
	if err != nil {
		return 0, "", nil, callError(err, watch)
	}
	return response.StatusCode, response.Header.Get("Location"), content, nil
}

// callError names a call the silence watch gave up on, and drops the URL from other errors.
// The URL contains the deployment ID, which must not end up in logs.
func callError(err error, watch *silenceWatch) error {
	if watch.fired.Load() {
		return errSilent
	}
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}

// silenceWatch cancels a call once no bytes have moved for longer than its current limit.
type silenceWatch struct {
	last  atomic.Int64 // unix nanoseconds of the last progress
	limit atomic.Int64 // allowed silence in nanoseconds
	fired atomic.Bool
}

func newSilenceWatch(limit time.Duration) *silenceWatch {
	watch := &silenceWatch{}
	watch.expect(limit)
	return watch
}

// expect starts a new period of allowed silence.
// It stores the time before the limit, and run loads them the other way round, so a check never mixes a new limit with an old time.
func (w *silenceWatch) expect(limit time.Duration) {
	w.last.Store(time.Now().UnixNano())
	w.limit.Store(int64(limit))
}

func (w *silenceWatch) progress() {
	w.last.Store(time.Now().UnixNano())
}

func (w *silenceWatch) run(stop <-chan struct{}, cancel context.CancelFunc) {
	ticker := time.NewTicker(silenceCheck)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			limit := w.limit.Load()
			if time.Now().UnixNano()-w.last.Load() > limit {
				w.fired.Store(true)
				cancel()
				return
			}
		}
	}
}

// progressReader reports every read to a silenceWatch, and calls onDone once its last byte has been read.
type progressReader struct {
	reader    io.Reader
	watch     *silenceWatch
	remaining int
	onDone    func()
}

func (r *progressReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		r.watch.progress()
		if r.onDone != nil {
			r.remaining -= n
			if r.remaining <= 0 {
				r.onDone()
				r.onDone = nil
			}
		}
	}
	return n, err
}
