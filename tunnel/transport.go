package tunnel

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

	frameHeaderSize = 9
	maxBacklogBytes = 3_000_000

	// Each mailbox call costs about a second on Google's side however much it carries, so bulk downloads go in large batches.
	// Uploads stay small, since Iran's slow uplinks would make a large batch hold up everything behind it.
	smallBatchBytes = 70_000
	largeBatchBytes = 500_000

	// The window is how much is sealed ahead of the receiver: enough to keep the line busy, small enough to drain fast after a closed stream.
	// Google's calls take seconds, so its window stays large; a relay answers at once.
	googleWindowBytes = 4_500_000
	relayWindowBytes  = 768_000
	uploadWindowBytes = 512_000

	// Frames that arrive together, such as a TLS server's whole first flight, go out in one batch.
	// Under load no extra wait is needed, since frames gather on their own while every put slot is busy.
	flushInterval = 20 * time.Millisecond

	// Google runs at most 30 executions at once per account, and both tunnel ends run in the user's account.
	// Downloads get 9 parallel calls and uploads 3, which measured about 20% faster than 6 and 6.
	// Uploads lose nothing, since 3 calls already carry more than Iran's uplinks.
	downloadParallelism = 9
	uploadParallelism   = 3

	retransmitAfter  = 6 * time.Second
	statusInterval   = 3 * time.Second
	stallRetransmits = 3
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
// Frames wait in a queue per stream, and batches take one frame from each stream in turn,
// so a short reply never waits behind a long download.
type Sender struct {
	mailbox   *Mailbox
	prefix    string
	cipher    streamCipher
	maxBatch  int
	window    int // most bytes sealed ahead of the receiver
	dropEvery int // loss testing

	mutex           sync.Mutex
	queues          map[uint32][][]byte // encoded frames waiting, by stream
	order           []uint32            // streams with waiting frames, in turn
	pendingBytes    int
	dropped         map[uint32]bool // streams whose other end has closed while this end still runs
	nextSequence    int
	hold            func(streamID uint32) bool // streams whose frames must wait, set on the bulk lane
	sealedIn        map[uint32]int             // the last batch each stream's frames went into, kept on the express lane
	onProgress      func()                     // called when the receiver has taken more
	statusEvery     time.Duration              // how often to ask for progress while batches wait
	retained        map[int]*sentBatch
	retainedBytes   int
	progress        int
	progressSeenAt  time.Time
	progressMovedAt time.Time
	wake            chan struct{}
	slots           chan struct{}
	Stalled         chan struct{}
}

// TestDropEvery is set by the -test-drop-every flag.
var TestDropEvery int

// Trace is set by the -trace flag.
var Trace bool

func NewSender(mailbox *Mailbox, prefix string, cipher streamCipher, maxBatch, parallelism, window int) *Sender {
	return &Sender{
		mailbox:     mailbox,
		prefix:      prefix,
		cipher:      cipher,
		maxBatch:    maxBatch,
		window:      window,
		dropEvery:   TestDropEvery,
		queues:      map[uint32][][]byte{},
		dropped:     map[uint32]bool{},
		retained:    map[int]*sentBatch{},
		statusEvery: statusInterval,
		wake:        make(chan struct{}, 1),
		slots:       make(chan struct{}, parallelism),
		Stalled:     make(chan struct{}, 1),
	}
}

func (s *Sender) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Finished reports whether every frame of the stream has reached the receiver; only the express lane keeps track.
func (s *Sender) Finished(streamID uint32) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, waiting := s.queues[streamID]; waiting {
		return false
	}
	sequence, sealed := s.sealedIn[streamID]
	return !sealed || sequence < s.progress
}

func (s *Sender) progressKey() string {
	return s.prefix + ":progress"
}

func (s *Sender) Send(streamID uint32, frameType byte, data []byte) {
	frame := appendFrame(nil, streamID, frameType, data)
	s.mutex.Lock()
	if s.dropped[streamID] {
		// This end's close is the last frame of a stream, so the mark is no longer needed after it.
		if frameType == frameClose {
			delete(s.dropped, streamID)
		}
		s.mutex.Unlock()
		return
	}
	queue, waiting := s.queues[streamID]
	s.queues[streamID] = append(queue, frame)
	if !waiting {
		s.order = append(s.order, streamID)
	}
	s.pendingBytes += len(frame)
	s.mutex.Unlock()
	s.wakeUp()
}

// Drop discards the frames still waiting for a stream whose other end has closed, since that end would throw them away.
// While this end of the stream still runs, its later frames are discarded too.
func (s *Sender) Drop(streamID uint32, stillRunning bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if queue, ok := s.queues[streamID]; ok {
		for _, frame := range queue {
			s.pendingBytes -= len(frame)
		}
		delete(s.queues, streamID)
		for i, id := range s.order {
			if id == streamID {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	if stillRunning {
		s.dropped[streamID] = true
	}
	delete(s.sealedIn, streamID)
}

// takeFrames builds a batch of whole frames, one frame from each waiting stream in turn; the caller holds the mutex.
// Held streams keep their frames and their place. The streams that went into the batch are added to included when it is not nil.
func (s *Sender) takeFrames(limit int, included map[uint32]bool) []byte {
	var batch []byte
	skipped := 0
	for skipped < len(s.order) {
		streamID := s.order[skipped]
		if s.hold != nil && s.hold(streamID) {
			skipped++
			continue
		}
		queue := s.queues[streamID]
		if len(batch) > 0 && len(batch)+len(queue[0]) > limit {
			break
		}
		batch = append(batch, queue[0]...)
		s.pendingBytes -= len(queue[0])
		if included != nil {
			included[streamID] = true
		}
		s.order = append(s.order[:skipped], s.order[skipped+1:]...)
		if len(queue) > 1 {
			s.queues[streamID] = queue[1:]
			s.order = append(s.order, streamID)
		} else {
			delete(s.queues, streamID)
		}
	}
	return batch
}

// Backlog counts unsent bytes plus bytes the receiver has not taken yet.
func (s *Sender) Backlog() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.pendingBytes + s.retainedBytes
}

// MaxBacklog is the backlog above which writers wait, enough to keep every put slot busy with a full batch.
func (s *Sender) MaxBacklog() int {
	return max(maxBacklogBytes, cap(s.slots)*s.maxBatch+1_000_000)
}

func (s *Sender) pending() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.pendingBytes
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
			// A sealed batch cannot be recalled, so only a window of data is sealed ahead of the receiver.
			// The rest waits in the stream queues, where a closed stream's data can still be dropped and a new reply can go first.
			if s.retainedBytes >= s.window {
				s.mutex.Unlock()
				<-s.slots
				s.waitForRoom(ctx)
				continue
			}
			// The first batch after a quiet spell stays small so the reply starts quickly; bulk follows in large batches.
			limit := s.maxBatch
			if len(s.retained) == 0 {
				limit = min(limit, smallBatchBytes)
			}
			var included map[uint32]bool
			if s.sealedIn != nil {
				included = map[uint32]bool{}
			}
			frames := s.takeFrames(limit, included)
			if len(frames) == 0 {
				// Only held frames are waiting; the express lane wakes this sender when they may go.
				s.mutex.Unlock()
				<-s.slots
				break
			}
			sequence := s.nextSequence
			s.nextSequence++
			for streamID := range included {
				s.sealedIn[streamID] = sequence
			}
			batch := s.cipher.seal(sequence, frames)
			if len(s.retained) == 0 {
				s.progressMovedAt = time.Now()
			}
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

// waitForRoom asks for the receiver's progress while the window is full, since no put replies bring it then.
func (s *Sender) waitForRoom(ctx context.Context) {
	progress, err := s.mailbox.Status(ctx, s.progressKey())
	if err != nil {
		sleepContext(ctx, time.Second)
		return
	}
	s.mutex.Lock()
	moved := progress > s.progress
	s.mutex.Unlock()
	s.updateProgress(progress)
	if !moved {
		// The receiver has taken nothing new, so ask again shortly rather than at once.
		sleepContext(ctx, 200*time.Millisecond)
	}
}

// deliver puts a batch with a few quick retries.
func (s *Sender) deliver(ctx context.Context, sequence int, batch []byte) {
	defer func() { <-s.slots }()
	defer s.finishPut(sequence)
	key := fmt.Sprintf("%s:%d", s.prefix, sequence)
	for attempt := range 3 {
		started := time.Now()
		progress, err := s.mailbox.Put(ctx, key, batch, s.progressKey())
		if Trace {
			log.Printf("trace put %s %dB %dms in-flight %d", key, len(batch), time.Since(started).Milliseconds(), len(s.slots))
		}
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

// StuckFor is how long the receiver has taken nothing while batches were waiting for it.
func (s *Sender) StuckFor() time.Duration {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if len(s.retained) == 0 {
		return 0
	}
	return time.Since(s.progressMovedAt)
}

// updateProgress forgets every batch below the receiver's progress.
func (s *Sender) updateProgress(progress int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.progressSeenAt = time.Now()
	moved := progress > s.progress
	if moved {
		s.progressMovedAt = s.progressSeenAt
	}
	for ; s.progress < progress; s.progress++ {
		if batch, ok := s.retained[s.progress]; ok {
			s.retainedBytes -= len(batch.data)
			delete(s.retained, s.progress)
		}
	}
	for streamID, sequence := range s.sealedIn {
		if sequence < s.progress {
			delete(s.sealedIn, streamID)
		}
	}
	if moved && s.onProgress != nil {
		go s.onProgress()
	}
}

// retransmitLoop re-puts the batch the receiver is stuck on.
// Every stallRetransmits tries on the same batch it signals Stalled, since the receiver may be gone.
func (s *Sender) retransmitLoop(ctx context.Context) {
	ticker := time.NewTicker(min(time.Second, s.statusEvery))
	defer ticker.Stop()
	lastStuck, stuckCount := -1, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s.mutex.Lock()
		needsStatus := len(s.retained) > 0 && time.Since(s.progressSeenAt) > s.statusEvery
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
		if sequence != lastStuck {
			lastStuck, stuckCount = sequence, 0
		}
		stuckCount++
		if stuckCount%stallRetransmits == 0 {
			select {
			case s.Stalled <- struct{}{}:
			default:
			}
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
	cipher  streamCipher
	window  int

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
		for nextToRequest < nextToDeliver+r.window {
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
		started := time.Now()
		value, err := r.mailbox.Wait(ctx, key, acked, r.prefix+":progress", delivered)
		if Trace && value != nil {
			log.Printf("trace got %s %dB after %dms", key, len(value), time.Since(started).Milliseconds())
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("wait %s failed (%v), retrying", key, err)
				time.Sleep(2 * time.Second)
			}
			continue
		}
		if value == nil {
			continue
		}
		plain, err := r.cipher.open(sequence, value)
		if err != nil {
			log.Printf("%s: %v", key, err)
			time.Sleep(2 * time.Second)
			continue
		}
		result <- plain
		return
	}
}
