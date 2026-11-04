package tunnel

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func postToServer(t *testing.T, url string, request map[string]any, compress bool) map[string]any {
	t.Helper()
	body, _ := json.Marshal(request)
	if compress {
		var packed bytes.Buffer
		writer := gzip.NewWriter(&packed)
		writer.Write(body)
		writer.Close()
		body = packed.Bytes()
	}
	httpRequest, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if compress {
		httpRequest.Header.Set("Content-Encoding", "gzip")
	}
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var reply map[string]any
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestMailboxServerRejectsAWrongKey(t *testing.T) {
	server := httptest.NewServer(NewMailboxServer("secret"))
	defer server.Close()
	reply := postToServer(t, server.URL, map[string]any{"k": "wrong", "op": "ping"}, false)
	if reply["error"] != "unauthorized" {
		t.Fatalf("got %v, want unauthorized", reply)
	}
}

func TestMailboxServerTakeRemovesAndTimesOut(t *testing.T) {
	server := httptest.NewServer(NewMailboxServer("secret"))
	defer server.Close()
	postToServer(t, server.URL, map[string]any{"k": "secret", "op": "put", "key": "a", "value": "hello"}, true)
	reply := postToServer(t, server.URL, map[string]any{"k": "secret", "op": "take", "key": "a", "timeoutMs": 200}, false)
	if reply["value"] != "hello" {
		t.Fatalf("first take got %v, want hello", reply)
	}
	reply = postToServer(t, server.URL, map[string]any{"k": "secret", "op": "take", "key": "a", "timeoutMs": 200}, false)
	if reply["value"] != nil {
		t.Fatalf("second take got %v, want nothing", reply)
	}
}

func TestMailboxServerWaitKeepsTheValueUntilAcked(t *testing.T) {
	server := httptest.NewServer(NewMailboxServer("secret"))
	defer server.Close()
	go func() {
		time.Sleep(200 * time.Millisecond)
		postToServer(t, server.URL, map[string]any{"k": "secret", "op": "put", "key": "b", "value": "data"}, false)
	}()
	started := time.Now()
	reply := postToServer(t, server.URL, map[string]any{"k": "secret", "op": "wait", "key": "b", "timeoutMs": 5000, "progressKey": "p", "progress": 7}, false)
	if reply["value"] != "data" || time.Since(started) > 2*time.Second {
		t.Fatalf("wait got %v after %v, want data at once after the put", reply, time.Since(started))
	}
	reply = postToServer(t, server.URL, map[string]any{"k": "secret", "op": "put", "key": "c", "value": "x", "progressKey": "p"}, false)
	if reply["progress"] != float64(7) {
		t.Fatalf("progress got %v, want 7", reply["progress"])
	}
	reply = postToServer(t, server.URL, map[string]any{"k": "secret", "op": "wait", "key": "b", "timeoutMs": 200, "ack": []string{"b"}}, false)
	if reply["value"] != nil {
		t.Fatalf("wait after the ack got %v, want nothing", reply)
	}
}
