package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"sync"
	"time"
)

// Client is the local side: a SOCKS5 and HTTP proxy whose connections become streams through the user's own mailbox.
type Client struct {
	*Endpoint
	mailbox      *Mailbox
	registry     *Mailbox
	node         NodeInfo
	own          MailboxConfig
	runID        string
	publicKey    [32]byte
	streamMutex  sync.Mutex
	nextStreamID uint32
}

func NewClient(own MailboxConfig, node NodeInfo) (*Client, error) {
	privateKey, publicKey, err := generateKeyPair()
	if err != nil {
		return nil, err
	}
	runIDBytes := make([]byte, 6)
	if _, err := rand.Read(runIDBytes); err != nil {
		return nil, err
	}
	runID := hex.EncodeToString(runIDBytes)
	up, down, err := newStreamCiphers(&privateKey, node.PublicKey[:], runID)
	if err != nil {
		return nil, err
	}
	registryConfig := own
	registryConfig.DeploymentID, registryConfig.AuthKey = node.DeploymentID, node.AuthKey

	client := &Client{
		mailbox:      NewMailbox(own),
		registry:     NewMailbox(registryConfig),
		node:         node,
		own:          own,
		runID:        runID,
		publicKey:    publicKey,
		nextStreamID: 1,
	}
	client.Endpoint = newEndpoint(client.mailbox, runID, "up", "down", client.onFrame, up, down)
	return client, nil
}

func (c *Client) onFrame(streamID uint32, frameType byte, payload []byte) {
	c.deliver(streamID, frameType, payload)
}

// keepRegistered registers with the node and refreshes the registration.
// It returns when the node has lost this run (for example after a restart), so the caller can start a new one.
func (c *Client) keepRegistered(ctx context.Context) {
	accepted := false
	for ctx.Err() == nil {
		known, err := c.register(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("registration: %v, retrying", err)
				sleepContext(ctx, time.Duration(2+randomInt(4))*time.Second)
			}
			continue
		}
		if !accepted {
			accepted = true
			log.Printf("client: node accepted run %s", c.runID)
		} else if !known {
			log.Printf("client: node lost run %s, reconnecting", c.runID)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(registrationRefresh):
		case <-c.sender.Stalled:
			log.Printf("client: traffic stalled, checking the node")
		}
	}
}

func (c *Client) register(ctx context.Context) (bool, error) {
	sealed, err := sealRegistration(registration{
		DeploymentID: c.own.DeploymentID,
		AuthKey:      c.own.AuthKey,
		RunID:        c.runID,
		ClientKey:    c.publicKey[:],
	}, &c.node.PublicKey)
	if err != nil {
		return false, err
	}
	slot := fmt.Sprintf("register:%d", randomInt(registrationSlots))
	if _, err := c.registry.Put(ctx, slot, sealed, ""); err != nil {
		return false, err
	}
	ack, err := c.mailbox.Take(ctx, c.runID+":ack")
	if err != nil {
		return false, err
	}
	if ack == nil {
		return false, errors.New("node did not answer")
	}
	return string(ack) == "known", nil
}

// handle serves one local connection, speaking SOCKS5 or HTTP proxy depending on its first byte.
func (c *Client) handle(conn net.Conn) {
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		conn.Close()
		return
	}
	buffered := &bufferedConn{Conn: conn, reader: reader}
	var target string
	var initial []byte
	if first[0] == 5 {
		target, err = socks5Handshake(buffered)
	} else {
		target, initial, err = httpProxyHandshake(buffered, reader)
	}
	if err != nil {
		conn.Close()
		return
	}
	c.streamMutex.Lock()
	streamID := c.nextStreamID
	c.nextStreamID++
	c.streamMutex.Unlock()
	box := c.openStream(streamID)
	c.sender.Send(streamID, frameOpen, []byte(target))
	if len(initial) > 0 {
		c.sender.Send(streamID, frameData, initial)
	}
	c.pump(streamID, buffered, box)
}

func randomInt(limit int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}

func sleepContext(ctx context.Context, duration time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(duration):
	}
}
