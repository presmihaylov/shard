package sandbox

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// testStall stands in for ExecStallBound, so a stall is seen in milliseconds.
const testStall = 50 * time.Millisecond

// pattern is chunk i of a run, every byte of it i, so a gap or a reorder shows in the bytes.
func pattern(i, size int) []byte {
	return bytes.Repeat([]byte{byte(i)}, size)
}

// write appends count chunks of size bytes and closes done once the last one is in.
func write(b *execBuffer, count, size int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range count {
			b.append(pattern(i, size), false)
		}
	}()

	return done
}

// A client slower than the command still gets every byte, and one 8 MiB batch that takes longer than
// the bound to send is progress all the way, since the bound counts each chunk the client accepts.
func TestASlowFollowerGetsEveryByte(t *testing.T) {
	const count, size = 384, 64 << 10

	b := newExecBuffer(false)
	b.stall = testStall
	b.release()

	var got bytes.Buffer
	streamed := make(chan error, 1)
	go func() {
		streamed <- b.stream(t.Context(), nil, func(c chunk) error {
			time.Sleep(time.Millisecond)
			got.Write(c.data)

			return nil
		})
	}()

	waitFollower(t, b)
	<-write(b, count, size)
	b.close()
	if err := <-streamed; err != nil {
		t.Fatalf("stream returned %v, want the whole output", err)
	}

	var want bytes.Buffer
	for i := range count {
		want.Write(pattern(i, size))
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Errorf("the client got %d bytes, want the %d the command wrote, byte for byte", got.Len(), want.Len())
	}
	if lost := b.lostBytes(); lost != 0 {
		t.Errorf("the buffer lost %d bytes to a client that kept taking them", lost)
	}
}

// A client that accepts nothing for the bound is detached, its attach is ended, and the command runs on
// evicting what nobody took, which the buffer counts as lost.
func TestAFollowerThatTakesNothingIsDetached(t *testing.T) {
	const size = 1 << 20

	b := newExecBuffer(false)
	b.stall = testStall
	b.release()

	emitting := make(chan struct{})
	detached := make(chan struct{})
	streamed := make(chan error, 1)
	go func() {
		streamed <- b.stream(t.Context(), func() { close(detached) }, func(chunk) error {
			close(emitting)
			<-detached

			return errors.New("the socket closed under the write")
		})
	}()

	// The client wedges on the first chunk, so it accepts nothing and owes everything.
	waitFollower(t, b)
	b.append(pattern(0, size), false)
	<-emitting

	start := time.Now()
	<-write(b, 11, size)

	if err := <-streamed; !errors.Is(err, errStalled) {
		t.Fatalf("stream returned %v, want the stall", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the command waited %s on a wedged client, want about the %s bound", took, testStall)
	}
	// Twelve chunks over an 8 MiB cap evict four, and the client accepted none of them.
	if lost := b.lostBytes(); lost != 4*size {
		t.Errorf("the buffer lost %d bytes, want %d", lost, 4*size)
	}
}

// A client cut midway through a batch it copied owes the rest of it, so those bytes count as lost once evicted.
func TestADetachMidBatchCountsTheUnsentBytesLost(t *testing.T) {
	const size = 1 << 20

	b := newExecBuffer(false)
	b.stall = testStall
	b.release()
	<-write(b, 4, size)

	detached := make(chan struct{})
	streamed := make(chan error, 1)
	go func() {
		accepted := 0
		streamed <- b.stream(t.Context(), func() { close(detached) }, func(chunk) error {
			if accepted == 0 {
				accepted++

				return nil
			}
			// A buffer that credits the batch unsent never waits, so never detaches; the wedge gives up rather than hang.
			select {
			case <-detached:
			case <-time.After(5 * time.Second):
			}

			return errors.New("the socket closed under the write")
		})
	}()

	waitFollower(t, b)
	<-write(b, 8, size)

	if err := <-streamed; !errors.Is(err, errStalled) {
		t.Fatalf("stream returned %v, want the stall", err)
	}
	// The first batch held four chunks and the client accepted one; the eviction of all four loses the other three.
	if lost := b.lostBytes(); lost != 3*size {
		t.Errorf("the buffer lost %d bytes, want the %d of the batch the client never accepted", lost, 3*size)
	}
}

// The bound runs from the client's last accepted chunk, so progress just after a write began to wait buys one bound, not two.
func TestTheStallBoundRunsFromTheLastProgress(t *testing.T) {
	const stall = 400 * time.Millisecond

	b := newExecBuffer(false)
	b.stall = stall
	b.release()

	accept := make(chan struct{})
	gone := make(chan struct{})
	var progressed, detachedAt time.Time
	streamed := make(chan error, 1)
	go func() {
		first := true
		streamed <- b.stream(t.Context(), func() { detachedAt = time.Now(); close(gone) }, func(chunk) error {
			if !first {
				select {
				case <-gone:
				case <-time.After(5 * time.Second):
				}

				return errors.New("the socket closed under the write")
			}
			first = false
			<-accept
			progressed = time.Now()

			return nil
		})
	}()

	// A small first chunk, then the cap's worth after it, so accepting the small one is progress that frees too little room.
	waitFollower(t, b)
	b.append(pattern(0, 1<<10), false)
	<-write(b, 7, 1<<20)
	written := make(chan struct{})
	go func() {
		defer close(written)
		b.append(pattern(9, 2<<20), false)
	}()
	waitWaiting(t, b)
	time.Sleep(stall / 10)
	close(accept)

	if err := <-streamed; !errors.Is(err, errStalled) {
		t.Fatalf("stream returned %v, want the stall", err)
	}
	<-written
	if since := detachedAt.Sub(progressed); since > stall*3/2 {
		t.Errorf("the detach came %s after the last progress, want about the %s bound", since, stall)
	}
}

// waitFollower blocks until a stream follows the buffer, so the first write is one it owes.
func waitFollower(t *testing.T, b *execBuffer) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		following := b.follower != nil
		b.mu.Unlock()
		if following {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no stream followed the buffer")
}

// The hold keeps the output for the first attach: the command waits at the cap until it comes.
func TestTheHoldWaitsForTheFirstAttach(t *testing.T) {
	const count, size = 12, 1 << 20

	b := newExecBuffer(true)
	b.release()
	written := write(b, count, size)

	waitWaiting(t, b)

	var got bytes.Buffer
	if err := streamAll(t.Context(), b, written, &got); err != nil {
		t.Fatalf("stream returned %v, want the whole output", err)
	}
	if got.Len() != count*size || b.lostBytes() != 0 {
		t.Errorf("the first attach got %d bytes with %d lost, want all %d and none lost", got.Len(), b.lostBytes(), count*size)
	}
}

// A client that never attaches frees the command after the bound, and a late attach hears what it lost.
func TestTheHoldGivesUpAfterTheBound(t *testing.T) {
	const count, size = 12, 1 << 20

	b := newExecBuffer(true)
	b.stall = testStall
	b.release()
	<-write(b, count, size)

	var got bytes.Buffer
	if err := streamAll(t.Context(), b, closedChan(), &got); err != nil {
		t.Fatalf("stream returned %v, want the tail the buffer kept", err)
	}

	if lost := b.lostBytes(); lost != 4*size {
		t.Errorf("the buffer lost %d bytes, want the %d it evicted before the attach", lost, 4*size)
	}
	if got.Len() != 8*size || got.Bytes()[0] != 4 {
		t.Errorf("the late attach got %d bytes from chunk %d, want the last %d from chunk 4", got.Len(), got.Bytes()[0], 8*size)
	}
}

func closedChan() <-chan struct{} {
	done := make(chan struct{})
	close(done)

	return done
}

// waitWaiting blocks until a write waits for a client, which is the command held at the cap.
func waitWaiting(t *testing.T, b *execBuffer) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, waiting := b.waited(); waiting {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no write waited for a client")
}

// streamAll attaches once the writes are done or held, and collects the output until the buffer closes.
func streamAll(ctx context.Context, b *execBuffer, written <-chan struct{}, got *bytes.Buffer) error {
	streamed := make(chan error, 1)
	go func() {
		streamed <- b.stream(ctx, nil, func(c chunk) error {
			got.Write(c.data)

			return nil
		})
	}()

	<-written
	b.close()

	return <-streamed
}
