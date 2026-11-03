package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	registrationSlots   = 2
	registrationRefresh = 4 * time.Minute
	sessionIdleLimit    = 15 * time.Minute
)

// NodeServer serves every mailbox registered with it and exits their traffic to the internet.
type NodeServer struct {
	registry   *Mailbox
	userConfig MailboxConfig // fronting settings for user mailboxes
	privateKey [32]byte
	publicKey  [32]byte
	mutex      sync.Mutex
	sessions   map[string]*nodeSession
}

type nodeSession struct {
	mailbox  *Mailbox
	cancel   context.CancelFunc
	lastSeen time.Time
}

func NewNodeServer(registry *Mailbox, userConfig MailboxConfig, privateKey, publicKey [32]byte) *NodeServer {
	return &NodeServer{
		registry:   registry,
		userConfig: userConfig,
		privateKey: privateKey,
		publicKey:  publicKey,
		sessions:   map[string]*nodeSession{},
	}
}

func (s *NodeServer) Run(ctx context.Context) {
	for slot := range registrationSlots {
		go s.listen(ctx, fmt.Sprintf("register:%d", slot))
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reapIdleSessions()
		}
	}
}

func (s *NodeServer) listen(ctx context.Context, key string) {
	for ctx.Err() == nil {
		sealed, err := s.registry.Take(ctx, key)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("registry %s: %v", key, err)
				time.Sleep(2 * time.Second)
			}
			continue
		}
		if sealed == nil {
			continue
		}
		entry, err := openRegistration(sealed, &s.publicKey, &s.privateKey)
		if err != nil {
			log.Printf("registry %s: %v", key, err)
			continue
		}
		s.accept(ctx, entry)
	}
}

// accept starts serving a newly registered client run, or refreshes a known one, and acks it in the client's mailbox.
func (s *NodeServer) accept(ctx context.Context, entry registration) {
	id := entry.DeploymentID + "/" + entry.RunID
	s.mutex.Lock()
	session, known := s.sessions[id]
	if known {
		session.lastSeen = time.Now()
	}
	s.mutex.Unlock()

	if !known {
		config := s.userConfig
		config.DeploymentID, config.AuthKey = entry.DeploymentID, entry.AuthKey
		up, down, err := newStreamCiphers(&s.privateKey, entry.ClientKey, entry.RunID)
		if err != nil {
			log.Printf("session %s: %v", entry.RunID, err)
			return
		}
		sessionCtx, cancel := context.WithCancel(ctx)
		session = &nodeSession{mailbox: NewMailbox(config), cancel: cancel, lastSeen: time.Now()}
		endpoint := NewNodeEndpoint(session.mailbox, entry.RunID, down, up)
		go endpoint.run(sessionCtx)
		s.mutex.Lock()
		s.sessions[id] = session
		count := len(s.sessions)
		s.mutex.Unlock()
		log.Printf("session %s started (%d active)", entry.RunID, count)
	}

	state := map[bool]string{true: "known", false: "new"}[known]
	if _, err := session.mailbox.Put(ctx, entry.RunID+":ack", []byte(state), ""); err != nil {
		log.Printf("session %s: ack failed (%v)", entry.RunID, err)
	}
}

func (s *NodeServer) reapIdleSessions() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for id, session := range s.sessions {
		if time.Since(session.lastSeen) > sessionIdleLimit {
			session.cancel()
			delete(s.sessions, id)
			log.Printf("session %s expired (%d active)", id, len(s.sessions))
		}
	}
}
