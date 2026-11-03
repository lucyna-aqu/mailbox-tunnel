package tunnel

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
	// A session whose client has taken nothing for this long is ended, so it stops using the user's mailbox.
	clientGoneAfter = time.Minute
)

// NodeServer serves every mailbox registered with it and exits their traffic to the internet.
// Each mailbox has at most one session: a new client run replaces the previous one.
type NodeServer struct {
	registry   *Mailbox
	userConfig MailboxConfig // fronting settings for user mailboxes
	privateKey [32]byte
	publicKey  [32]byte
	mutex      sync.Mutex
	sessions   map[string]*nodeSession // by deployment ID
}

type nodeSession struct {
	runID    string
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
	s.mutex.Lock()
	session, exists := s.sessions[entry.DeploymentID]
	known := exists && session.runID == entry.RunID
	if known {
		session.lastSeen = time.Now()
	}
	s.mutex.Unlock()

	if !known {
		if exists {
			s.endSession(entry.DeploymentID, session.runID, "replaced by a new run")
		}
		var err error
		if session, err = s.startSession(ctx, entry); err != nil {
			log.Printf("session %s: %v", entry.RunID, err)
			return
		}
	}

	state := map[bool]string{true: "known", false: "new"}[known]
	if _, err := session.mailbox.Put(ctx, entry.RunID+":ack", []byte(state), ""); err != nil {
		log.Printf("session %s: ack failed (%v)", entry.RunID, err)
	}
}

func (s *NodeServer) startSession(ctx context.Context, entry registration) (*nodeSession, error) {
	config := s.userConfig
	config.DeploymentID, config.AuthKey = entry.DeploymentID, entry.AuthKey
	up, down, err := newStreamCiphers(&s.privateKey, entry.ClientKey, entry.RunID)
	if err != nil {
		return nil, err
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	session := &nodeSession{runID: entry.RunID, mailbox: NewMailbox(config), cancel: cancel, lastSeen: time.Now()}
	endpoint := NewNodeEndpoint(session.mailbox, entry.RunID, down, up)
	go endpoint.run(sessionCtx)
	go s.watchClient(sessionCtx, entry.DeploymentID, entry.RunID, endpoint.sender)

	s.mutex.Lock()
	s.sessions[entry.DeploymentID] = session
	count := len(s.sessions)
	s.mutex.Unlock()
	log.Printf("session %s started (%d active)", entry.RunID, count)
	return session, nil
}

// watchClient ends the session once its client has stopped taking batches.
func (s *NodeServer) watchClient(ctx context.Context, deploymentID, runID string, sender *Sender) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sender.Stalled:
			if sender.StuckFor() > clientGoneAfter {
				s.endSession(deploymentID, runID, "client stopped answering")
				return
			}
		}
	}
}

// endSession stops the session of deploymentID if it still belongs to runID.
func (s *NodeServer) endSession(deploymentID, runID, reason string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	session, ok := s.sessions[deploymentID]
	if !ok || session.runID != runID {
		return
	}
	session.cancel()
	delete(s.sessions, deploymentID)
	log.Printf("session %s ended: %s (%d active)", runID, reason, len(s.sessions))
}

func (s *NodeServer) SessionCount() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.sessions)
}

func (s *NodeServer) reapIdleSessions() {
	s.mutex.Lock()
	var idle [][2]string
	for deploymentID, session := range s.sessions {
		if time.Since(session.lastSeen) > sessionIdleLimit {
			idle = append(idle, [2]string{deploymentID, session.runID})
		}
	}
	s.mutex.Unlock()
	for _, session := range idle {
		s.endSession(session[0], session[1], "no refresh")
	}
}
