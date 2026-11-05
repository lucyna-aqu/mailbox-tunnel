package tunnel

import (
	"bytes"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

// newTestLanes builds an endpoint with both lanes in memory; nothing runs, so tests move batches by hand.
func newTestLanes(withExpress bool) (*Endpoint, *Sender) {
	endpoint := &Endpoint{sender: newTestSender(), inboxes: map[uint32]*inbox{}, lanes: map[uint32]int{}}
	if !withExpress {
		return endpoint, nil
	}
	express := newTestSender()
	express.sealedIn = map[uint32]int{}
	endpoint.express = express
	endpoint.sender.hold = func(streamID uint32) bool { return !express.Finished(streamID) }
	return endpoint, express
}

// sealOne takes a batch from a lane the way Run does and returns the streams in it.
func sealOne(sender *Sender) map[uint32]bool {
	included := map[uint32]bool{}
	frames := sender.takeFrames(largeBatchBytes, included)
	if len(frames) > 0 && sender.sealedIn != nil {
		for streamID := range included {
			sender.sealedIn[streamID] = sender.nextSequence
		}
		sender.nextSequence++
	}
	return included
}

func TestANewStreamStartsOnTheExpressLaneAndMovesToBulk(t *testing.T) {
	endpoint, express := newTestLanes(true)
	endpoint.send(1, frameOpen, []byte("example.com:443"))
	endpoint.send(1, frameData, bytes.Repeat([]byte("a"), 200_000))
	endpoint.send(1, frameData, bytes.Repeat([]byte("b"), 100_000))
	if !sealOne(express)[1] {
		t.Fatal("the stream's first frames did not go on the express lane")
	}
	if len(sealOne(endpoint.sender)) != 0 {
		t.Fatal("the bulk lane sent the stream's later frames before the express lane delivered its earlier ones")
	}
	express.updateProgress(1)
	if !sealOne(endpoint.sender)[1] {
		t.Fatal("the bulk lane kept holding the stream after the express lane delivered")
	}
	endpoint.send(1, frameData, []byte("c"))
	if !sealOne(endpoint.sender)[1] || len(sealOne(express)) != 0 {
		t.Fatal("a stream that moved to bulk went back to the express lane")
	}
}

func TestAShortReplyGoesOnlyOnTheExpressLane(t *testing.T) {
	endpoint, express := newTestLanes(true)
	endpoint.send(2, frameData, []byte("reply"))
	endpoint.send(2, frameClose, nil)
	if got := framesIn(express.takeFrames(largeBatchBytes, nil)); len(got) != 2 {
		t.Fatalf("express lane carried %v, want the reply and the close", got)
	}
	if endpoint.sender.pending() != 0 {
		t.Fatal("a short stream left frames on the bulk lane")
	}
	if _, kept := endpoint.lanes[2]; kept {
		t.Fatal("a closed stream's lane was not forgotten")
	}
}

func TestStreamsWithoutTheExpressLaneAreNeverHeld(t *testing.T) {
	endpoint, _ := newTestLanes(false)
	endpoint.send(3, frameData, []byte("before"))
	if !sealOne(endpoint.sender)[3] {
		t.Fatal("a stream on a run without the express lane did not go on the bulk lane")
	}
	endpoint, _ = newTestLanes(true)
	endpoint.lanes[4] = bulkLane
	endpoint.send(4, frameData, []byte("older stream"))
	if !sealOne(endpoint.sender)[4] {
		t.Fatal("a stream that started before the express lane was held")
	}
}

func TestDropClearsBothLanes(t *testing.T) {
	endpoint, express := newTestLanes(true)
	endpoint.send(5, frameData, bytes.Repeat([]byte("x"), expressBytes))
	endpoint.send(5, frameData, []byte("more"))
	endpoint.drop(5, false)
	if express.pending() != 0 || endpoint.sender.pending() != 0 {
		t.Fatal("dropping a stream left its frames on a lane")
	}
	if !express.Finished(5) {
		t.Fatal("a dropped stream still holds the bulk lane")
	}
}

func TestExpressCiphersDifferFromBulkCiphers(t *testing.T) {
	clientPublic, clientPrivate, _ := box.GenerateKey(rand.Reader)
	nodePublic, nodePrivate, _ := box.GenerateKey(rand.Reader)
	up, _, err := newStreamCiphers(clientPrivate, nodePublic[:], "run")
	if err != nil {
		t.Fatal(err)
	}
	expressUp, _, err := newExpressCiphers(clientPrivate, nodePublic[:], "run")
	if err != nil {
		t.Fatal(err)
	}
	sealed := expressUp.seal(0, []byte("hello"))
	if bytes.Equal(sealed, up.seal(0, []byte("hello"))) {
		t.Fatal("the express lane reuses the bulk lane's key")
	}
	if _, err := up.open(0, sealed); err == nil {
		t.Fatal("the bulk key opened an express batch")
	}
	// Both ends derive the same express keys: the client's up is what the node opens with.
	nodeUpForClient, _, err := newExpressCiphers(nodePrivate, clientPublic[:], "run")
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := nodeUpForClient.open(0, sealed); err != nil || string(plain) != "hello" {
		t.Fatalf("the node could not open the client's express batch: %q, %v", plain, err)
	}
}
