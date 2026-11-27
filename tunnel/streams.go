package tunnel

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	readChunkBytes = 32 * 1024
	maxInboxBytes  = 8 << 20
	maxNodeStreams = 256
)

var (
	errBlockedAddress = errors.New("refused connection to private or local address")
	cgnatPrefix       = netip.MustParsePrefix("100.64.0.0/10")
	benchmarkPrefix   = netip.MustParsePrefix("198.18.0.0/15")
	ipv4Broadcast     = netip.AddrFrom4([4]byte{255, 255, 255, 255})
)

// bytesFromApps and bytesToApps count the traffic relayed for local connections.
var bytesFromApps, bytesToApps atomic.Int64

// TrafficTotals returns how many bytes have been relayed from and to local connections.
func TrafficTotals() (sent, received int64) {
	return bytesFromApps.Load(), bytesToApps.Load()
}

// ResetTrafficTotals zeroes the relayed traffic counters for a new client connection.
func ResetTrafficTotals() {
	bytesFromApps.Store(0)
	bytesToApps.Store(0)
}

// inbox is a bounded per-stream queue, so a slow connection cannot stall the others or exhaust memory.
type inbox struct {
	mutex  sync.Mutex
	items  [][]byte
	bytes  int
	closed bool
	notify chan struct{}
}

func newInbox() *inbox {
	return &inbox{notify: make(chan struct{}, 1)}
}

func (b *inbox) push(data []byte) {
	b.mutex.Lock()
	if !b.closed {
		if b.bytes+len(data) > maxInboxBytes {
			// A stalled stream that never drains is closed rather than allowed to grow without bound.
			b.closed = true
			b.items = nil
			b.bytes = 0
		} else {
			b.items = append(b.items, data)
			b.bytes += len(data)
		}
	}
	b.mutex.Unlock()
	b.signal()
}

func (b *inbox) close() {
	b.mutex.Lock()
	b.closed = true
	b.mutex.Unlock()
	b.signal()
}

func (b *inbox) signal() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

// pop returns the next chunk, or false once the inbox is closed and drained.
func (b *inbox) pop() ([]byte, bool) {
	for {
		b.mutex.Lock()
		if len(b.items) > 0 {
			data := b.items[0]
			b.items[0] = nil
			b.items = b.items[1:]
			b.bytes -= len(data)
			b.mutex.Unlock()
			return data, true
		}
		closed := b.closed
		b.mutex.Unlock()
		if closed {
			return nil, false
		}
		<-b.notify
	}
}

// Endpoint is the state shared by both tunnel ends.
type Endpoint struct {
	sender       *Sender
	receiver     *Receiver
	countTraffic bool
	done         chan struct{}
	ctx          context.Context
	mutex        sync.Mutex
	inboxes      map[uint32]*inbox
}

// endpointShape sets the largest outgoing batch, how much is sealed ahead, and how many calls run in parallel each way.
type endpointShape struct {
	maxBatch        int
	window          int
	sendParallelism int
	receiveWindow   int
}

// newEndpoint uses the key prefixes <base>:<outgoing> and <base>:<incoming>.
func newEndpoint(mailbox *Mailbox, base, outgoing, incoming string, onFrame func(uint32, byte, []byte), sendCipher, receiveCipher streamCipher, shape endpointShape) *Endpoint {
	parser := &frameParser{onFrame: onFrame}
	return &Endpoint{
		sender:   NewSender(mailbox, base+":"+outgoing, sendCipher, shape.maxBatch, shape.sendParallelism, shape.window),
		receiver: &Receiver{mailbox: mailbox, prefix: base + ":" + incoming, onBytes: parser.feed, cipher: receiveCipher, window: shape.receiveWindow},
		done:     make(chan struct{}),
		inboxes:  map[uint32]*inbox{},
	}
}

// run moves batches until ctx ends, then closes every open stream.
func (e *Endpoint) run(ctx context.Context) {
	e.ctx = ctx
	defer close(e.done)
	go e.sender.Run(ctx)
	e.receiver.Run(ctx)
	e.mutex.Lock()
	for _, box := range e.inboxes {
		box.close()
	}
	e.mutex.Unlock()
}

func (e *Endpoint) openStream(streamID uint32) *inbox {
	box := newInbox()
	e.mutex.Lock()
	e.inboxes[streamID] = box
	e.mutex.Unlock()
	return box
}

func (e *Endpoint) tryOpenStream(streamID uint32, limit int) (*inbox, bool) {
	box := newInbox()
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if limit > 0 && len(e.inboxes) >= limit {
		return nil, false
	}
	e.inboxes[streamID] = box
	return box, true
}

func (e *Endpoint) forget(streamID uint32) {
	e.mutex.Lock()
	delete(e.inboxes, streamID)
	e.mutex.Unlock()
}

// deliver routes DATA and CLOSE frames to the stream's inbox.
func (e *Endpoint) deliver(streamID uint32, frameType byte, payload []byte) {
	e.mutex.Lock()
	box := e.inboxes[streamID]
	e.mutex.Unlock()
	// The other end forgets a stream once it closes, so whatever this end still has waiting for it would only be thrown away there.
	// Dropping it is what makes jumping in a video fast: the old download no longer drains through the line first.
	if frameType == frameClose {
		e.sender.Drop(streamID, box != nil)
	}
	if box == nil {
		return
	}
	if frameType == frameData {
		box.push(payload)
	} else {
		box.close()
	}
}

// pump relays one connection both ways until either side closes.
func (e *Endpoint) pump(streamID uint32, conn net.Conn, box *inbox) {
	done := make(chan struct{}, 2)
	streamDone := make(chan struct{})
	go func() {
		buffer := make([]byte, readChunkBytes)
		for {
			n, err := conn.Read(buffer)
			if n > 0 {
				stalledAt := time.Now()
				for e.sender.Backlog() > e.sender.MaxBacklog() {
					select {
					case <-e.done:
						goto readExit
					case <-streamDone:
						goto readExit
					case <-time.After(50 * time.Millisecond):
					}
				}
				if Trace && time.Since(stalledAt) > 50*time.Millisecond {
					log.Printf("trace backlog full for %dms", time.Since(stalledAt).Milliseconds())
				}
				e.sender.Send(streamID, frameData, buffer[:n])
			}
			if err != nil {
				break
			}
		}
	readExit:
		e.sender.Send(streamID, frameClose, nil)
		done <- struct{}{}
	}()
	go func() {
		for {
			data, ok := box.pop()
			if !ok {
				break
			}
			written, err := conn.Write(data)
			if e.countTraffic && written > 0 {
				bytesToApps.Add(int64(written))
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}()
	<-done
	close(streamDone)
	conn.Close()
	box.close()
	e.forget(streamID)
}

// isBlockedExitIP reports whether addr points to loopback, private LAN, link-local/metadata, or other non-public ranges.
func isBlockedExitIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return true
	}
	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified() ||
		cgnatPrefix.Contains(addr) ||
		benchmarkPrefix.Contains(addr) ||
		addr == ipv4Broadcast
}

// NodeEndpoint is the exit side of one client run: it opens real TCP connections for OPEN frames.
type NodeEndpoint struct {
	*Endpoint
	allowPrivateExit bool // tests only
}

// maxBatch is the largest batch the client's mailbox can store; window is how much is sealed ahead of the client.
func NewNodeEndpoint(mailbox *Mailbox, runID string, sendCipher, receiveCipher streamCipher, maxBatch, window int) *NodeEndpoint {
	endpoint := &NodeEndpoint{}
	shape := endpointShape{maxBatch: maxBatch, window: window, sendParallelism: downloadParallelism, receiveWindow: uploadParallelism}
	endpoint.Endpoint = newEndpoint(mailbox, runID, "down", "up", endpoint.onFrame, sendCipher, receiveCipher, shape)
	return endpoint
}

func (n *NodeEndpoint) onFrame(streamID uint32, frameType byte, payload []byte) {
	if frameType == frameOpen {
		box, ok := n.tryOpenStream(streamID, maxNodeStreams)
		if !ok {
			log.Printf("stream %d: too many open streams (%d)", streamID, maxNodeStreams)
			n.sender.Send(streamID, frameClose, nil)
			return
		}
		go n.serve(streamID, string(payload), box)
		return
	}
	n.deliver(streamID, frameType, payload)
}

func (n *NodeEndpoint) serve(streamID uint32, target string, box *inbox) {
	ctx := n.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if !n.allowPrivateExit {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			addrPort, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if isBlockedExitIP(addrPort.Addr()) {
				return errBlockedAddress
			}
			return nil
		}
	}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		log.Printf("stream %d: connect failed (%v)", streamID, err)
		n.sender.Send(streamID, frameClose, nil)
		n.forget(streamID)
		return
	}
	n.pump(streamID, conn, box)
}
