package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	frameOpen  byte = 1
	frameData  byte = 2
	frameClose byte = 3
	frameHello byte = 4

	frameHeaderSize = 9
	maxBatchBytes   = 70_000
	maxBacklogBytes = 3_000_000
	flushInterval   = 150 * time.Millisecond
	sendParallelism = 6
	receiveWindow   = 6

	retransmitAfter = 6 * time.Second
	statusInterval  = 3 * time.Second
)

// appendFrame encodes one frame as stream id (4) + type (1) + length (4) + data.
func appendFrame(buffer []byte, streamID uint32, frameType byte, data []byte) []byte {
	buffer = binary.BigEndian.AppendUint32(buffer, streamID)
	buffer = append(buffer, frameType)
	buffer = binary.BigEndian.AppendUint32(buffer, uint32(len(data)))
	return append(buffer, data...)
}

// frameParser reassembles frames from the ordered stream of batches.
type frameParser struct {
	buffer  []byte
	onFrame func(streamID uint32, frameType byte, payload []byte)
}

func (p *frameParser) feed(data []byte) {
	p.buffer = append(p.buffer, data...)
	for len(p.buffer) >= frameHeaderSize {
		length := int(binary.BigEndian.Uint32(p.buffer[5:9]))
		end := frameHeaderSize + length
		if len(p.buffer) < end {
			return
		}
		payload := append([]byte(nil), p.buffer[frameHeaderSize:end]...)
		p.onFrame(binary.BigEndian.Uint32(p.buffer[0:4]), p.buffer[4], payload)
		p.buffer = p.buffer[end:]
	}
	if len(p.buffer) == 0 {
		p.buffer = nil
	}
}

type sentBatch struct {
	data    []byte
	putting bool      // a put is in flight
	lastPut time.Time // when the last put finished
}

// Sender batches outgoing frames under sequence keys and keeps each batch until the receiver has taken it.
type Sender struct {
	mailbox   *Mailbox
	prefix    string
	dropEvery int // loss testing

	mutex          sync.Mutex
	buffer         []byte
	nextSequence   int
	retained       map[int]*sentBatch
	retainedBytes  int
	progress       int
	progressSeenAt time.Time
	wake           chan struct{}
	slots          chan struct{}
}

// testDropEvery is set by the -test-drop-every flag.
var testDropEvery int

func NewSender(mailbox *Mailbox, prefix string) *Sender {
	return &Sender{
		mailbox:   mailbox,
		prefix:    prefix,
		dropEvery: testDropEvery,
		retained:  map[int]*sentBatch{},
		wake:      make(chan struct{}, 1),
		slots:     make(chan struct{}, sendParallelism),
	}
}

func (s *Sender) progressKey() string {
	return s.prefix + ":progress"
}

func (s *Sender) Send(streamID uint32, frameType byte, data []byte) {
	s.mutex.Lock()
	s.buffer = appendFrame(s.buffer, streamID, frameType, data)
	s.mutex.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Backlog counts unsent bytes plus bytes the receiver has not taken yet.
func (s *Sender) Backlog() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.buffer) + s.retainedBytes
}

func (s *Sender) pending() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.buffer)
}

func (s *Sender) Run(ctx context.Context) {
	go s.retransmitLoop(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		time.Sleep(flushInterval)
		for s.pending() > 0 {
			if !s.acquireSlot(ctx) {
				return
			}
			s.mutex.Lock()
			size := min(len(s.buffer), maxBatchBytes)
			batch := append([]byte(nil), s.buffer[:size]...)
			s.buffer = s.buffer[size:]
			sequence := s.nextSequence
			s.nextSequence++
			s.retained[sequence] = &sentBatch{data: batch, putting: true}
			s.retainedBytes += len(batch)
			s.mutex.Unlock()
			if s.dropEvery > 0 && sequence%s.dropEvery == s.dropEvery-1 {
				log.Printf("test: dropping first put of %s:%d", s.prefix, sequence)
				s.finishPut(sequence)
				<-s.slots
				continue
			}
			go s.deliver(ctx, sequence, batch)
		}
	}
}

func (s *Sender) acquireSlot(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case s.slots <- struct{}{}:
		return true
	}
}

// deliver puts a batch with a few quick retries.
func (s *Sender) deliver(ctx context.Context, sequence int, batch []byte) {
	defer func() { <-s.slots }()
	defer s.finishPut(sequence)
	key := fmt.Sprintf("%s:%d", s.prefix, sequence)
	for attempt := range 3 {
		progress, err := s.mailbox.Put(ctx, key, batch, s.progressKey())
		if err == nil {
			s.updateProgress(progress)
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("put %s failed (%v)", key, err)
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
}

// finishPut starts the retransmission timer once a put has ended.
func (s *Sender) finishPut(sequence int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if batch, ok := s.retained[sequence]; ok {
		batch.putting = false
		batch.lastPut = time.Now()
	}
}

// updateProgress forgets every batch below the receiver's progress.
func (s *Sender) updateProgress(progress int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.progressSeenAt = time.Now()
	for ; s.progress < progress; s.progress++ {
		if batch, ok := s.retained[s.progress]; ok {
			s.retainedBytes -= len(batch.data)
			delete(s.retained, s.progress)
		}
	}
}

// retransmitLoop re-puts the batch the receiver is stuck on.
func (s *Sender) retransmitLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s.mutex.Lock()
		needsStatus := len(s.retained) > 0 && time.Since(s.progressSeenAt) > statusInterval
		s.mutex.Unlock()
		if needsStatus {
			if progress, err := s.mailbox.Status(ctx, s.progressKey()); err == nil {
				s.updateProgress(progress)
			}
		}

		s.mutex.Lock()
		sequence := s.progress
		head, stuck := s.retained[sequence]
		stuck = stuck && !head.putting && time.Since(head.lastPut) > retransmitAfter
		if stuck {
			head.putting = true
		}
		s.mutex.Unlock()
		if !stuck {
			continue
		}
		if !s.acquireSlot(ctx) {
			return
		}
		log.Printf("retransmitting %s:%d", s.prefix, sequence)
		go s.deliver(ctx, sequence, head.data)
	}
}

// Receiver long-polls a window of sequence keys and delivers batches in order.
// Acks and progress ride along on its wait calls.
type Receiver struct {
	mailbox *Mailbox
	prefix  string
	onBytes func([]byte)

	mutex     sync.Mutex
	acked     []string
	delivered int
}

func (r *Receiver) key(sequence int) string {
	return fmt.Sprintf("%s:%d", r.prefix, sequence)
}

func (r *Receiver) Run(ctx context.Context) {
	pending := map[int]chan []byte{}
	nextToDeliver, nextToRequest := 0, 0
	for {
		for nextToRequest < nextToDeliver+receiveWindow {
			result := make(chan []byte, 1)
			pending[nextToRequest] = result
			go r.fetch(ctx, nextToRequest, result)
			nextToRequest++
		}
		select {
		case <-ctx.Done():
			return
		case batch := <-pending[nextToDeliver]:
			delete(pending, nextToDeliver)
			r.mutex.Lock()
			r.acked = append(r.acked, r.key(nextToDeliver))
			r.delivered = nextToDeliver + 1
			r.mutex.Unlock()
			nextToDeliver++
			r.onBytes(batch)
		}
	}
}

func (r *Receiver) takeAcks() ([]string, int) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	acked := r.acked
	r.acked = nil
	return acked, r.delivered
}

func (r *Receiver) fetch(ctx context.Context, sequence int, result chan<- []byte) {
	key := r.key(sequence)
	for ctx.Err() == nil {
		acked, delivered := r.takeAcks()
		value, err := r.mailbox.Wait(ctx, key, acked, r.prefix+":progress", delivered)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("wait %s failed (%v), retrying", key, err)
				time.Sleep(2 * time.Second)
			}
			continue
		}
		if value != nil {
			result <- value
			return
		}
	}
}
