package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/netip"
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
	fakeDNS      *FakeDNS
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
	client.Endpoint = newEndpoint(client.mailbox, runID, "up", "down", client.onFrame, up, down,
		endpointShape{maxBatch: smallBatchBytes, sendParallelism: uploadParallelism, receiveWindow: downloadParallelism})
	return client, nil
}

func (c *Client) onFrame(streamID uint32, frameType byte, payload []byte) {
	c.deliver(streamID, frameType, payload)
}

// routeDownAfter is how long registration keeps failing before the client tries another route.
const routeDownAfter = 20 * time.Second

// keepRegistered registers with the node and refreshes the registration, reporting Connected once accepted.
// It returns when the node has lost this run (for example after a restart or a handover), so the caller can start a new one.
// It returns true when registration has kept failing for routeDownAfter, so the caller can try another route.
func (c *Client) keepRegistered(ctx context.Context, onStatus func(Status)) bool {
	accepted := false
	var failingSince time.Time
	for ctx.Err() == nil {
		attempt := time.Now()
		known, err := c.register(ctx, accepted)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			if failingSince.IsZero() {
				failingSince = attempt
			}
			if time.Since(failingSince) > routeDownAfter {
				log.Printf("registration: %v, giving up on this route", err)
				return true
			}
			log.Printf("registration: %v, retrying", err)
			sleepContext(ctx, time.Duration(2+randomInt(4))*time.Second)
			continue
		}
		failingSince = time.Time{}
		if !accepted {
			accepted = true
			log.Printf("client: node accepted run %s", c.runID)
			onStatus(StatusConnected)
		} else if !known {
			log.Printf("client: node lost run %s, reconnecting", c.runID)
			return false
		}
		select {
		case <-ctx.Done():
		case <-time.After(registrationRefresh):
		case <-c.sender.Stalled:
			log.Printf("client: traffic stalled, checking the node")
		case <-c.mailbox.Recovered:
			// The node ends a session a minute after its client went quiet, so ask at once.
			log.Printf("client: network is back, checking the node")
		case <-c.mailbox.Failing:
			// A route that has gone down is found by registering, which then fails for routeDownAfter.
			log.Printf("client: mailbox calls are failing, checking the route")
		}
	}
	return false
}

func (c *Client) register(ctx context.Context, refresh bool) (bool, error) {
	sealed, err := sealRegistration(registration{
		DeploymentID: c.own.DeploymentID,
		AuthKey:      c.own.AuthKey,
		RunID:        c.runID,
		ClientKey:    c.publicKey[:],
		Refresh:      refresh,
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
	command := socksConnect
	if first[0] == 5 {
		target, command, err = socks5Handshake(buffered)
	} else {
		target, initial, err = httpProxyHandshake(buffered, reader)
	}
	if err != nil {
		conn.Close()
		return
	}
	if command == socksUDPAssociate {
		c.serveDNSAssociation(buffered)
		return
	}
	target = c.realTarget(target)
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

// realTarget turns a fake DNS address back into the name it stands for, so the node resolves it.
func (c *Client) realTarget(target string) string {
	if c.fakeDNS == nil {
		return target
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return target
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return target
	}
	if name, ok := c.fakeDNS.Lookup(address); ok {
		return net.JoinHostPort(name, port)
	}
	return target
}

// serveDNSAssociation answers a SOCKS5 UDP ASSOCIATE for DNS only: queries to port 53 get fake DNS answers,
// other datagrams are dropped so applications fall back to TCP. It lasts as long as the control connection.
func (c *Client) serveDNSAssociation(control net.Conn) {
	defer control.Close()
	if c.fakeDNS == nil {
		control.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		control.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer relay.Close()
	port := relay.LocalAddr().(*net.UDPAddr).Port
	if _, err := control.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, byte(port >> 8), byte(port)}); err != nil {
		return
	}
	go func() {
		io.Copy(io.Discard, control)
		relay.Close()
	}()

	packet := make([]byte, 64*1024)
	for {
		n, sender, err := relay.ReadFromUDP(packet)
		if err != nil {
			return
		}
		header, destinationPort, query, ok := splitSocksDatagram(packet[:n])
		if !ok || destinationPort != 53 {
			continue
		}
		answer, err := c.fakeDNS.Answer(query)
		if err != nil {
			continue
		}
		relay.WriteToUDP(append(append([]byte(nil), header...), answer...), sender)
	}
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
