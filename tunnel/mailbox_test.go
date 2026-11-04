package tunnel

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

func TestSilenceWatchCutsASilentCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := newSilenceWatch(time.Second)
	started := time.Now()
	go watch.run(ctx.Done(), cancel)
	<-ctx.Done()
	if !watch.fired.Load() {
		t.Fatal("the watch did not fire")
	}
	if elapsed := time.Since(started); elapsed < time.Second || elapsed > 2*time.Second {
		t.Fatalf("fired after %v, want about 1s", elapsed)
	}
}

func TestSilenceWatchKeepsASlowCall(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := newSilenceWatch(time.Second)
	go watch.run(ctx.Done(), cancel)
	for range 10 {
		time.Sleep(300 * time.Millisecond)
		watch.progress()
	}
	if watch.fired.Load() {
		t.Fatal("a call that kept moving data was cut")
	}
}

func TestSilenceWatchAllowsALongPollBeforeTheAnswer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := newSilenceWatch(500 * time.Millisecond)
	go watch.run(ctx.Done(), cancel)
	watch.expect(2 * time.Second)
	time.Sleep(1500 * time.Millisecond)
	if watch.fired.Load() {
		t.Fatal("the watch cut a long poll inside its allowed wait")
	}
}

func TestProgressReaderSwitchesToTheAnswerLimitAfterTheLastByte(t *testing.T) {
	watch := newSilenceWatch(time.Second)
	reader := &progressReader{reader: bytes.NewReader(make([]byte, 100)), watch: watch, remaining: 100, onDone: func() { watch.expect(5 * time.Second) }}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(watch.limit.Load()); got != 5*time.Second {
		t.Fatalf("limit after the body is %v, want 5s", got)
	}
}
