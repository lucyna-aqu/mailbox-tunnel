package tunnel

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestIsBlockedExitIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"127.255.255.254",
		"::1",
		"::ffff:127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"::ffff:169.254.169.254",
		"fe80::1",
		"fc00::1",
		"100.64.0.1",
		"198.18.0.5",
		"0.0.0.0",
		"::",
		"255.255.255.255",
		"224.0.0.1",
	}
	for _, raw := range blocked {
		addr := netip.MustParseAddr(raw)
		if !isBlockedExitIP(addr) {
			t.Errorf("expected %s to be blocked", raw)
		}
	}

	allowed := []string{
		"1.1.1.1",
		"8.8.8.8",
		"142.250.185.4",
		"2606:4700:4700::1111",
	}
	for _, raw := range allowed {
		addr := netip.MustParseAddr(raw)
		if isBlockedExitIP(addr) {
			t.Errorf("expected public IP %s to be allowed", raw)
		}
	}
}

func TestNodeEndpointBlocksLocalExitTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	var zeroCipher streamCipher
	node := NewNodeEndpoint(nil, "run1", zeroCipher, zeroCipher, smallBatchBytes, relayWindowBytes)
	box := node.openStream(42)
	node.serve(42, listener.Addr().String(), box)

	// serve must refuse 127.0.0.1 and queue a CLOSE frame for stream 42.
	batch, _ := node.sender.takeFrames(largeBatchBytes)
	var closed bool
	parser := &frameParser{onFrame: func(id uint32, frameType byte, _ []byte) {
		if id == 42 && frameType == frameClose {
			closed = true
		}
	}}
	parser.feed(batch)
	if !closed {
		t.Fatal("expected NodeEndpoint.serve to refuse loopback target and send frameClose")
	}
}

func TestNodeServerValidUserDeployment(t *testing.T) {
	appsScriptID := "AKfycbx1234567890abcdef1234567890"

	googleNode := NewNodeServer(NewMailbox(MailboxConfig{DeploymentID: appsScriptID}), MailboxConfig{}, [32]byte{}, [32]byte{})
	if !googleNode.validUserDeployment(appsScriptID) {
		t.Fatal("expected valid Apps Script ID to be accepted")
	}
	for _, bad := range []string{"127.0.0.1:7860", "evil.example.com", "../etc/passwd", "short", ""} {
		if googleNode.validUserDeployment(bad) {
			t.Errorf("expected %q to be rejected on Google node", bad)
		}
	}

	relayHost := "my-relay-8080.app.github.dev"
	relayNode := NewNodeServer(NewMailbox(MailboxConfig{DeploymentID: relayHost}), MailboxConfig{}, [32]byte{}, [32]byte{})
	if !relayNode.validUserDeployment(relayHost) {
		t.Fatal("expected relay's own host to be accepted on relay node")
	}
	if relayNode.validUserDeployment("other.example.com") {
		t.Fatal("expected arbitrary foreign host to be rejected on relay node")
	}
}

func TestInboxClosesWhenExceedingMemoryLimit(t *testing.T) {
	box := newInbox()
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for range 8 {
		box.push(chunk)
	}
	// Pushing one more chunk exceeds maxInboxBytes (8 MiB) and closes the inbox.
	box.push(chunk)
	if _, ok := box.pop(); ok {
		t.Fatal("expected inbox to close and discard buffered data once over maxInboxBytes")
	}
}

func TestPumpExitsBackpressureWaitWhenEndpointCloses(t *testing.T) {
	var zeroCipher streamCipher
	ep := newEndpoint(nil, "run1", "up", "down", func(uint32, byte, []byte) {}, zeroCipher, zeroCipher,
		endpointShape{maxBatch: smallBatchBytes, window: uploadWindowBytes, sendParallelism: 1, receiveWindow: 1})

	// Fill sender backlog above MaxBacklog so pump's reader enters the backpressure wait.
	ep.sender.mutex.Lock()
	ep.sender.retainedBytes = ep.sender.MaxBacklog() + 1024
	ep.sender.mutex.Unlock()

	c1, c2 := net.Pipe()
	defer c2.Close()

	box := ep.openStream(1)
	pumped := make(chan struct{})
	go func() {
		ep.pump(1, c1, box)
		close(pumped)
	}()

	// Write data so c1.Read returns and hits the backpressure loop.
	if _, err := c2.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	// Simulate Endpoint.run finishing; pump must exit promptly instead of leaking.
	close(ep.done)
	select {
	case <-pumped:
	case <-time.After(time.Second):
		t.Fatal("pump hung in backpressure wait after endpoint closed")
	}
}

func TestTrafficAndLatencyReset(t *testing.T) {
	bytesFromApps.Store(100)
	bytesToApps.Store(200)
	recordLatency(50 * time.Millisecond)

	ResetTrafficTotals()
	ResetLatency()

	sent, recv := TrafficTotals()
	if sent != 0 || recv != 0 {
		t.Fatalf("got traffic (%d, %d), want (0, 0)", sent, recv)
	}
	if Latency() != 0 {
		t.Fatalf("got latency %v, want 0", Latency())
	}
}
