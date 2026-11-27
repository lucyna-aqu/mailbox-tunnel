package tunnel

import (
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	serverValueTTL      = 10 * time.Minute
	serverMaxWait       = 5 * time.Minute
	serverMaxBody       = 16 << 20
	serverMaxTotalBytes = 128 << 20
	serverMaxKeyLen     = 256
)

// MailboxServer is a self-hosted mailbox that speaks the same protocol as the Apps Script one.
// It keeps values in memory for ten minutes, as the script's cache does, so a restart loses them.
type MailboxServer struct {
	authKey string

	mutex      sync.Mutex
	values     map[string]serverValue
	totalBytes int64
	changed    chan struct{} // closed and replaced whenever a value is stored
	swept      time.Time
}

type serverValue struct {
	value   string
	expires time.Time
}

func NewMailboxServer(authKey string) *MailboxServer {
	return &MailboxServer{authKey: authKey, values: map[string]serverValue{}, changed: make(chan struct{})}
}

type serverRequest struct {
	AuthKey     string   `json:"k"`
	Op          string   `json:"op"`
	Key         string   `json:"key"`
	Value       string   `json:"value"`
	ProgressKey string   `json:"progressKey"`
	Progress    *int     `json:"progress"`
	TimeoutMs   int      `json:"timeoutMs"`
	Ack         []string `json:"ack"`
}

func (s *MailboxServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeReply(w, map[string]any{"ok": true})
		return
	}
	body := io.Reader(http.MaxBytesReader(w, r.Body, serverMaxBody))
	if r.Header.Get("Content-Encoding") == "gzip" {
		unpacked, err := gzip.NewReader(body)
		if err != nil {
			writeReply(w, map[string]any{"error": "bad gzip"})
			return
		}
		defer unpacked.Close()
		body = io.LimitReader(unpacked, serverMaxBody)
	}
	var request serverRequest
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		writeReply(w, map[string]any{"error": "bad request"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(request.AuthKey), []byte(s.authKey)) != 1 {
		writeReply(w, map[string]any{"error": "unauthorized"})
		return
	}

	switch request.Op {
	case "ping":
		writeReply(w, map[string]any{"ok": true, "version": 2})
	case "put":
		if request.Key == "" || len(request.Key) > serverMaxKeyLen {
			writeReply(w, map[string]any{"error": "bad key"})
			return
		}
		if !s.store(request.Key, request.Value) {
			writeReply(w, map[string]any{"error": "mailbox full"})
			return
		}
		writeReply(w, map[string]any{"ok": true, "progress": s.progress(request.ProgressKey)})
	case "status":
		writeReply(w, map[string]any{"ok": true, "progress": s.progress(request.ProgressKey)})
	// Read without removing; the receiver acks keys on its next call.
	case "wait":
		s.remove(request.Ack)
		if request.ProgressKey != "" && len(request.ProgressKey) <= serverMaxKeyLen && request.Progress != nil {
			s.store(request.ProgressKey, strconv.Itoa(*request.Progress))
		}
		writeReply(w, map[string]any{"ok": true, "value": s.waitFor(r.Context(), request.Key, request.TimeoutMs, false)})
	case "take":
		writeReply(w, map[string]any{"ok": true, "value": s.waitFor(r.Context(), request.Key, request.TimeoutMs, true)})
	default:
		writeReply(w, map[string]any{"error": "unknown op"})
	}
}

// sweep removes expired values; the caller holds the mutex.
func (s *MailboxServer) sweep(now time.Time) {
	for name, stored := range s.values {
		if now.After(stored.expires) {
			s.totalBytes -= int64(len(name) + len(stored.value))
			delete(s.values, name)
		}
	}
	s.swept = now
}

// store keeps a value and wakes every waiter, sweeping out expired values periodically or when near capacity.
func (s *MailboxServer) store(key, value string) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	now := time.Now()
	if now.Sub(s.swept) > time.Minute {
		s.sweep(now)
	}
	entrySize := int64(len(key) + len(value))
	var oldSize int64
	if prev, ok := s.values[key]; ok {
		oldSize = int64(len(key) + len(prev.value))
	}
	if s.totalBytes-oldSize+entrySize > serverMaxTotalBytes {
		s.sweep(now)
		oldSize = 0
		if prev, ok := s.values[key]; ok {
			oldSize = int64(len(key) + len(prev.value))
		}
		if s.totalBytes-oldSize+entrySize > serverMaxTotalBytes {
			return false
		}
	}
	s.totalBytes = s.totalBytes - oldSize + entrySize
	s.values[key] = serverValue{value: value, expires: now.Add(serverValueTTL)}
	close(s.changed)
	s.changed = make(chan struct{})
	return true
}

func (s *MailboxServer) remove(keys []string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for _, key := range keys {
		if prev, ok := s.values[key]; ok {
			s.totalBytes -= int64(len(key) + len(prev.value))
			delete(s.values, key)
		}
	}
}

// lookup returns a live value; the caller holds the mutex.
func (s *MailboxServer) lookup(key string) (string, bool) {
	stored, ok := s.values[key]
	if !ok || time.Now().After(stored.expires) {
		return "", false
	}
	return stored.value, true
}

// progress returns the number stored at key, or nil, which the client reads as unknown.
func (s *MailboxServer) progress(key string) any {
	if key == "" {
		return nil
	}
	s.mutex.Lock()
	value, ok := s.lookup(key)
	s.mutex.Unlock()
	if !ok {
		return nil
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return nil
	}
	return number
}

// waitFor returns the value at key as soon as it exists, or nil once the timeout passes.
func (s *MailboxServer) waitFor(ctx context.Context, key string, timeoutMs int, remove bool) any {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = waitTimeout
	}
	timer := time.NewTimer(min(timeout, serverMaxWait))
	defer timer.Stop()
	for {
		s.mutex.Lock()
		value, ok := s.lookup(key)
		if ok && remove {
			s.totalBytes -= int64(len(key) + len(value))
			delete(s.values, key)
		}
		changed := s.changed
		s.mutex.Unlock()
		if ok {
			return value
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func writeReply(w http.ResponseWriter, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(payload)
}
