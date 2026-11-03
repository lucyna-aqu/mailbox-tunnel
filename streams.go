package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const readChunkBytes = 32 * 1024

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

// Endpoint is the state shared by both tunnel ends.
type Endpoint struct {
	sender   *Sender
	receiver *Receiver
	mutex    sync.Mutex
	inboxes  map[uint32]*inbox
}

// newEndpoint uses the key prefixes <base>:<outgoing> and <base>:<incoming>.
func newEndpoint(mailbox *Mailbox, base, outgoing, incoming string, onFrame func(uint32, byte, []byte)) *Endpoint {
	parser := &frameParser{onFrame: onFrame}
	return &Endpoint{
		sender:   NewSender(mailbox, base+":"+outgoing),
		receiver: &Receiver{mailbox: mailbox, prefix: base + ":" + incoming, onBytes: parser.feed},
		inboxes:  map[uint32]*inbox{},
	}
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

func (e *Endpoint) register(streamID uint32) *inbox {
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
				for e.sender.Backlog() > maxBacklogBytes {
					time.Sleep(50 * time.Millisecond)
				}
				e.sender.Send(streamID, frameData, buffer[:n])
			}
			if err != nil {
				break
			}
		}
		e.sender.Send(streamID, frameClose, nil)
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
		}
		done <- struct{}{}
	}()
	<-done
	conn.Close()
	box.close()
	e.forget(streamID)
}

// serveNode starts a fresh endpoint for each client run announced on <session>:hello.
func serveNode(ctx context.Context, mailbox *Mailbox, session string) {
	var currentRunID string
	var stopCurrent context.CancelFunc = func() {}
	defer func() { stopCurrent() }()
	for ctx.Err() == nil {
		hello, err := mailbox.Take(ctx, session+":hello")
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("waiting for hello failed (%v), retrying", err)
				time.Sleep(2 * time.Second)
			}
			continue
		}
		runID := string(hello)
		if hello == nil || runID == currentRunID {
			continue
		}
		stopCurrent()
		currentRunID = runID
		endpointCtx, cancel := context.WithCancel(ctx)
		stopCurrent = cancel
		node := NewNode(mailbox, session+"/"+runID)
		go node.run(endpointCtx)
		node.sender.Send(0, frameHello, nil)
		log.Printf("node: client run %s connected", runID)
	}
}

// Node is the exit side: it opens real TCP connections for OPEN frames.
type Node struct {
	*Endpoint
}

func NewNode(mailbox *Mailbox, base string) *Node {
	node := &Node{}
	node.Endpoint = newEndpoint(mailbox, base, "down", "up", node.onFrame)
	return node
}

func (n *Node) onFrame(streamID uint32, frameType byte, payload []byte) {
	if frameType == frameOpen {
		box := n.register(streamID)
		go n.serve(streamID, string(payload), box)
		return
	}
	n.deliver(streamID, frameType, payload)
}

func (n *Node) serve(streamID uint32, target string, box *inbox) {
	conn, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		log.Printf("stream %d: connect failed (%v)", streamID, err)
		n.sender.Send(streamID, frameClose, nil)
		n.forget(streamID)
		return
	}
	n.pump(streamID, conn, box)
}

// Client is the local side: a SOCKS5 server whose connections become streams.
type Client struct {
	*Endpoint
	mailbox      *Mailbox
	session      string
	runID        string
	connected    atomic.Bool
	streamMutex  sync.Mutex
	nextStreamID uint32
}

func NewClient(mailbox *Mailbox, session string) *Client {
	runIDBytes := make([]byte, 4)
	rand.Read(runIDBytes)
	client := &Client{mailbox: mailbox, session: session, runID: hex.EncodeToString(runIDBytes), nextStreamID: 1}
	client.Endpoint = newEndpoint(mailbox, session+"/"+client.runID, "up", "down", client.onFrame)
	return client
}

func (c *Client) onFrame(streamID uint32, frameType byte, payload []byte) {
	if frameType == frameHello {
		if !c.connected.Swap(true) {
			log.Printf("client: node answered (run %s)", c.runID)
		}
		return
	}
	c.deliver(streamID, frameType, payload)
}

// announce sends this run's id to the node until the node answers.
func (c *Client) announce(ctx context.Context) {
	for ctx.Err() == nil && !c.connected.Load() {
		if _, err := c.mailbox.Put(ctx, c.session+":hello", []byte(c.runID), ""); err != nil {
			log.Printf("hello failed (%v), retrying", err)
		}
		for range 15 {
			if ctx.Err() != nil || c.connected.Load() {
				return
			}
			time.Sleep(time.Second)
		}
	}
}

func (c *Client) handle(conn net.Conn) {
	target, err := socks5Handshake(conn)
	if err != nil {
		conn.Close()
		return
	}
	c.streamMutex.Lock()
	streamID := c.nextStreamID
	c.nextStreamID++
	c.streamMutex.Unlock()
	box := c.register(streamID)
	c.sender.Send(streamID, frameOpen, []byte(target))
	c.pump(streamID, conn, box)
}
