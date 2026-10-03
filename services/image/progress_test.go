package image_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/presmihaylov/shard/services/image"
)

// events follows p to its close and answers what it said.
func events(t *testing.T, p *image.Progress) []image.Event {
	t.Helper()

	var got []image.Event
	if err := p.Follow(t.Context(), func(e image.Event) error {
		got = append(got, e)

		return nil
	}); err != nil {
		t.Fatalf("Follow: %v", err)
	}

	return got
}

func statuses(events []image.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Status
	}

	return out
}

func TestPullReportsTheImageItsLayersAndWhereItWent(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"}, map[string]string{"/etc/motd": "hello"})
	svc := newService(t, server)

	progress := image.NewProgress()
	img, err := svc.Pull(image.WithProgress(t.Context(), progress), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	progress.Close()

	got := events(t, progress)
	want := []string{image.StatusPulling, image.StatusLayer, image.StatusLayer, image.StatusUnpacking, image.StatusUnpacked, image.StatusUnpacked, image.StatusPulled}
	if !slices.Equal(statuses(got), want) {
		t.Fatalf("the pull said %v, want %v", statuses(got), want)
	}

	start, unpacking, end := got[0], got[3], got[6]
	if start.Reference != ref || start.Digest != img.Digest || start.Layers != 2 || start.Bytes != got[1].Bytes+got[2].Bytes {
		t.Errorf("the pull started with %+v, want %s %s, two layers and their bytes", start, ref, img.Digest)
	}
	if unpacking.Reference != ref || unpacking.Digest != img.Digest || unpacking.Layers != 2 {
		t.Errorf("the unpack started with %+v, want %s %s and two layers", unpacking, ref, img.Digest)
	}
	// The downloads land in any order and the unpack goes in manifest order, so the digests compare as a set.
	downloaded := []string{got[1].Digest, got[2].Digest}
	for i, e := range got[4:6] {
		if e.Layer != i+1 || e.Layers != 2 || !slices.Contains(downloaded, e.Digest) {
			t.Errorf("unpack step %d said %+v, want layer %d of 2, one of %v", i+1, e, i+1, downloaded)
		}
	}
	if got[4].Digest == got[5].Digest {
		t.Errorf("both unpack steps named %s", got[4].Digest)
	}
	if end.Path != img.RootFS || end.Digest != img.Digest {
		t.Errorf("the pull ended with %+v, want the rootfs %s", end, img.RootFS)
	}

	// The image is on disk now, so the next pull says so in one line.
	again := image.NewProgress()
	if _, err := svc.Pull(image.WithProgress(t.Context(), again), ref); err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	again.Close()

	cached := events(t, again)
	if len(cached) != 1 || cached[0].Status != image.StatusCached || cached[0].Path != img.RootFS || cached[0].Digest != img.Digest {
		t.Errorf("the second pull said %+v, want one cached event at %s", cached, img.RootFS)
	}
}

func TestAPullWithNoProgressReportsNothing(t *testing.T) {
	server, ref := servedImage(t, "app:1.0", map[string]string{"/etc/hostname": "box"})

	if _, err := newService(t, server).Pull(t.Context(), ref); err != nil {
		t.Errorf("Pull with no progress on the context = %v, want the image", err)
	}
}

// A follower that comes late still hears every event from the first, then each one as it lands.
func TestFollowReplaysThenWaitsForTheNext(t *testing.T) {
	p := image.NewProgress()
	p.Add(image.Event{Status: image.StatusPulling})

	heard := make(chan image.Event)
	done := make(chan error, 1)
	go func() {
		done <- p.Follow(t.Context(), func(e image.Event) error {
			heard <- e

			return nil
		})
	}()

	if e := <-heard; e.Status != image.StatusPulling {
		t.Fatalf("the first event was %+v, want the one added before the follow", e)
	}

	p.Add(image.Event{Status: image.StatusLayer})
	if e := <-heard; e.Status != image.StatusLayer {
		t.Fatalf("the next event was %+v, want the layer", e)
	}

	p.Close()
	p.Add(image.Event{Status: image.StatusPulled})
	if err := <-done; err != nil {
		t.Errorf("Follow after Close = %v, want nil", err)
	}
}

func TestFollowStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	err := image.NewProgress().Follow(ctx, func(image.Event) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Follow on an open log = %v, want the context's end", err)
	}
}

func TestFollowStopsAtTheFirstYieldError(t *testing.T) {
	p := image.NewProgress()
	p.Add(image.Event{Status: image.StatusPulling})
	p.Add(image.Event{Status: image.StatusLayer})
	p.Close()

	gone := errors.New("the client hung up")
	calls := 0
	err := p.Follow(t.Context(), func(image.Event) error {
		calls++

		return gone
	})
	if !errors.Is(err, gone) || calls != 1 {
		t.Errorf("Follow = %v after %d yields, want the yield's error after one", err, calls)
	}
}
