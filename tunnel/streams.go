package tunnel

import (
	"context"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const readChunkBytes = 32 * 1024

// bytesFromApps and bytesToApps count the traffic relayed for local connections.
var bytesFromApps, bytesToApps atomic.Int64

// TrafficTotals returns how many bytes have been relayed from and to local connections.
func TrafficTotals() (sent, received int64) {
	return bytesFromApps.Load(), bytesToApps.Load()
}

// inbox is an unbounded per-stream queue, so a slow connection cannot stall the others.
type inbox struct {
	mutex  sync.Mutex
	items  [][]byte
	closed bool
	notify chan struct{}
}

func newInbox() *inbox {
	return &inbox{notify: make(chan struct{}, 1)}
}

func (b *inbox) push(data []byte) {
	b.mutex.Lock()
	if !b.closed {
		b.items = append(b.items, data)
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
			b.items = b.items[1:]
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

// A new stream's first bytes go on a small express lane of their own, so they need not queue behind the window a long download has sealed.
// Once a stream has sent expressBytes it moves to the bulk lane, whose frames for it wait until the express lane has delivered all of its earlier ones.
const (
	expressBytes        = 256_000
	expressWindowBytes  = 1_000_000
	expressStatusEvery  = time.Second
	expressReceiveAhead = 1
	bulkLane            = -1
)

// Endpoint is the state shared by both tunnel ends.
type Endpoint struct {
	sender          *Sender
	receiver        *Receiver
	express         *Sender // nil until both ends have agreed on the express lane
	expressReceiver *Receiver
	onFrame         func(uint32, byte, []byte)
	mailbox         *Mailbox
	mutex           sync.Mutex
	inboxes         map[uint32]*inbox
	lanes           map[uint32]int // bytes each stream has sent on the express lane, or bulkLane
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
		onFrame:  onFrame,
		mailbox:  mailbox,
		inboxes:  map[uint32]*inbox{},
		lanes:    map[uint32]int{},
	}
}

// startExpress opens the express lane under the key prefixes <base>:<outgoing>x and <base>:<incoming>x.
// Streams that started before it stay on the bulk lane.
func (e *Endpoint) startExpress(ctx context.Context, base, outgoing, incoming string, sendCipher, receiveCipher streamCipher, maxBatch int) {
	// The lane adds no calls to the mailbox: Google runs at most 30 at once per account, and the bulk lane already uses most of them.
	// Its puts share the bulk lane's slots, and its long poll takes one of the bulk receiver's.
	express := NewSender(e.mailbox, base+":"+outgoing+"x", sendCipher, maxBatch, 1, expressWindowBytes)
	express.slots = e.sender.slots
	express.sealedIn = map[uint32]int{}
	express.statusEvery = expressStatusEvery
	express.onProgress = e.sender.wakeUp
	parser := &frameParser{onFrame: e.onFrame}
	receiver := &Receiver{mailbox: e.mailbox, prefix: base + ":" + incoming + "x", onBytes: parser.feed, cipher: receiveCipher, window: expressReceiveAhead}
	e.sender.mutex.Lock()
	e.sender.hold = func(streamID uint32) bool { return !express.Finished(streamID) }
	e.sender.mutex.Unlock()
	e.receiver.lent.Add(expressReceiveAhead)
	e.mutex.Lock()
	e.express, e.expressReceiver = express, receiver
	e.mutex.Unlock()
	go express.Run(ctx)
	go receiver.Run(ctx)
}

// send queues a frame on the stream's lane: the express lane for its first expressBytes, then the bulk lane.
func (e *Endpoint) send(streamID uint32, frameType byte, data []byte) {
	e.mutex.Lock()
	lane, seen := e.lanes[streamID]
	if !seen {
		lane = bulkLane
		if e.express != nil {
			lane = 0
		}
	}
	sender := e.sender
	if lane != bulkLane && lane+len(data) <= expressBytes {
		lane += len(data)
		sender = e.express
	} else {
		lane = bulkLane
	}
	if frameType == frameClose {
		delete(e.lanes, streamID)
	} else {
		e.lanes[streamID] = lane
	}
	e.mutex.Unlock()
	sender.Send(streamID, frameType, data)
}

// backlog is what the lanes still hold for the receiver.
func (e *Endpoint) backlog() int {
	e.mutex.Lock()
	express := e.express
	e.mutex.Unlock()
	if express == nil {
		return e.sender.Backlog()
	}
	return e.sender.Backlog() + express.Backlog()
}

// drop discards a closed stream's waiting frames on both lanes.
func (e *Endpoint) drop(streamID uint32, stillRunning bool) {
	e.mutex.Lock()
	express := e.express
	delete(e.lanes, streamID)
	e.mutex.Unlock()
	if express != nil {
		express.Drop(streamID, stillRunning)
	}
	e.sender.Drop(streamID, stillRunning)
	e.sender.wakeUp()
}

// run moves batches until ctx ends, then closes every open stream.
func (e *Endpoint) run(ctx context.Context) {
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
		e.drop(streamID, box != nil)
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
	go func() {
		buffer := make([]byte, readChunkBytes)
		for {
			n, err := conn.Read(buffer)
			if n > 0 {
				stalledAt := time.Now()
				for e.backlog() > e.sender.MaxBacklog() {
					time.Sleep(50 * time.Millisecond)
				}
				if Trace && time.Since(stalledAt) > 50*time.Millisecond {
					log.Printf("trace backlog full for %dms", time.Since(stalledAt).Milliseconds())
				}
				e.send(streamID, frameData, buffer[:n])
				bytesFromApps.Add(int64(n))
			}
			if err != nil {
				break
			}
		}
		e.send(streamID, frameClose, nil)
		done <- struct{}{}
	}()
	go func() {
		for {
			data, ok := box.pop()
			if !ok {
				break
			}
			if _, err := conn.Write(data); err != nil {
				break
			}
			bytesToApps.Add(int64(len(data)))
		}
		done <- struct{}{}
	}()
	<-done
	conn.Close()
	box.close()
	e.forget(streamID)
}

// NodeEndpoint is the exit side of one client run: it opens real TCP connections for OPEN frames.
type NodeEndpoint struct {
	*Endpoint
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
		box := n.openStream(streamID)
		go n.serve(streamID, string(payload), box)
		return
	}
	n.deliver(streamID, frameType, payload)
}

func (n *NodeEndpoint) serve(streamID uint32, target string, box *inbox) {
	conn, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		log.Printf("stream %d: connect failed (%v)", streamID, err)
		n.send(streamID, frameClose, nil)
		n.forget(streamID)
		return
	}
	n.pump(streamID, conn, box)
}
