package tunnel

import (
	"bytes"
	"fmt"
	"testing"
)

func newTestSender() *Sender {
	var cipher streamCipher
	return NewSender(nil, "test", cipher, largeBatchBytes, 3, relayWindowBytes)
}

// framesIn lists a batch's frames as "stream:payload".
func framesIn(batch []byte) []string {
	var frames []string
	parser := &frameParser{onFrame: func(streamID uint32, frameType byte, payload []byte) {
		frames = append(frames, fmt.Sprintf("%d:%s", streamID, payload))
	}}
	parser.feed(batch)
	return frames
}

func TestTakeFramesLetsAShortReplyPassALongDownload(t *testing.T) {
	sender := newTestSender()
	for i := range 5 {
		sender.Send(1, frameData, []byte(fmt.Sprintf("bulk%d", i)))
	}
	sender.Send(2, frameData, []byte("reply"))
	got := framesIn(sender.takeFrames(3*(frameHeaderSize+5), nil))
	want := []string{"1:bulk0", "2:reply", "1:bulk1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("first batch %v, want %v", got, want)
	}
	if got := framesIn(sender.takeFrames(largeBatchBytes, nil)); fmt.Sprint(got) != "[1:bulk2 1:bulk3 1:bulk4]" {
		t.Fatalf("second batch %v, want the rest of stream 1 in order", got)
	}
}

func TestDropDiscardsWaitingAndLaterFrames(t *testing.T) {
	sender := newTestSender()
	sender.Send(1, frameData, bytes.Repeat([]byte("x"), 1000))
	sender.Send(2, frameData, []byte("keep"))
	sender.Drop(1, true)
	sender.Send(1, frameData, []byte("late"))
	if got := framesIn(sender.takeFrames(largeBatchBytes, nil)); fmt.Sprint(got) != "[2:keep]" {
		t.Fatalf("batch %v, want only stream 2", got)
	}
	if sender.pending() != 0 {
		t.Fatalf("pending %d bytes, want 0", sender.pending())
	}
	sender.Send(1, frameClose, nil)
	if sender.dropped[1] {
		t.Fatal("the mark stayed after the stream's own close")
	}
}

func TestDropOfAFinishedStreamLeavesNoMark(t *testing.T) {
	sender := newTestSender()
	sender.Send(1, frameData, []byte("tail"))
	sender.Send(1, frameClose, nil)
	sender.Drop(1, false)
	if sender.pending() != 0 || len(sender.dropped) != 0 {
		t.Fatalf("pending %d, marks %d, want both 0", sender.pending(), len(sender.dropped))
	}
}
