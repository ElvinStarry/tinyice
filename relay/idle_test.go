package relay

import (
	"context"
	"testing"
	"time"
)

func newIdleStreamer(mount string) *Streamer {
	return &Streamer{
		Name:        "idle",
		OutputMount: mount,
		Format:      "mp3",
		Bitrate:     128,
		State:       StatePlaying,
		Source:      "local",
		idleCh:      make(chan string, 10),
		stateCh:     make(chan struct{}, 1),
	}
}

func TestIdleTrackAdvancesWithoutDecoder(t *testing.T) {
	r := NewRelay(false, nil)
	sm := NewStreamerManager(r, nil, nil)
	s := newIdleStreamer("/idle")
	// 8000 Hz mono 16-bit, 6400 bytes = 0.4 s.
	path := writeTestFile(t, "short.wav", makeWavHeader(1, 8000, 16, 6400))

	start := time.Now()
	out := sm.idleTrack(context.Background(), s, path, 0, 1)
	elapsed := time.Since(start)
	if out.start {
		t.Fatalf("result = %+v, want the idle wait to finish", out)
	}
	if elapsed < 350*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("idle wait = %v, want about the 0.4 s track duration", elapsed)
	}
	if s.CurrentFile != "short.wav" {
		t.Fatalf("CurrentFile = %q, want the filename", s.CurrentFile)
	}
	if s.CurrentFileDuration < 350*time.Millisecond {
		t.Fatalf("CurrentFileDuration = %v, want ~0.4s", s.CurrentFileDuration)
	}
	if got := r.GetOrCreateStream("/idle").ListenersCount(); got != 0 {
		t.Fatalf("listeners = %d, want 0", got)
	}
}

func TestIdleTrackStartsPlayingWhenListenerJoins(t *testing.T) {
	r := NewRelay(false, nil)
	sm := NewStreamerManager(r, nil, nil)
	s := newIdleStreamer("/idle-join")
	// Two seconds, long enough to observe the listener.
	path := writeTestFile(t, "long.wav", makeWavHeader(1, 8000, 16, 32000))

	done := make(chan idleResult, 1)
	go func() { done <- sm.idleTrack(context.Background(), s, path, 0, 1) }()
	time.Sleep(100 * time.Millisecond)
	stream := r.GetOrCreateStream("/idle-join")
	stream.Subscribe("listener-1", 0)
	defer stream.Unsubscribe("listener-1")

	select {
	case out := <-done:
		if !out.start {
			t.Fatalf("result = %+v, want playback after a listener joined", out)
		}
		// The track started 200 ms before the tick that noticed the
		// listener; the resume offset must reflect that time.
		if out.offset < 100*time.Millisecond || out.offset > time.Second {
			t.Fatalf("resume offset = %v, want the elapsed idle time", out.offset)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idleTrack did not notice the listener")
	}
}

func TestIdleTrackFallsBackForUnknownDuration(t *testing.T) {
	r := NewRelay(false, nil)
	sm := NewStreamerManager(r, nil, nil)
	s := newIdleStreamer("/idle-unknown")
	path := writeTestFile(t, "noise.bin", []byte("not audio"))

	start := time.Now()
	out := sm.idleTrack(context.Background(), s, path, 0, 1)
	if !out.start || out.offset != 0 {
		t.Fatalf("result = %+v, want playback from the start for an unknown duration", out)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("unknown-duration fallback blocked for %v", elapsed)
	}
}

// A configured transcoder subscribes internally. Idle skip must treat it
// as an audience for the source, or every transcoded output starves.
// A listener that joins mid-track resumes at the elapsed position: the
// decoder advances by decoding and discarding PCM, so only the remaining
// audio is encoded.
func TestStreamFileResumesAtOffset(t *testing.T) {
	r := NewRelay(false, nil)
	sm := NewStreamerManager(r, nil, nil)
	s := newIdleStreamer("/resume")
	// 44.1 kHz stereo 16-bit, two seconds.
	path := writeTestFile(t, "two.wav", makeWavHeader(2, 44100, 16, 2*44100*4))

	start := time.Now()
	if err := sm.streamFile(context.Background(), s, path, 0, 1, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 300*time.Millisecond || elapsed > 1200*time.Millisecond {
		t.Fatalf("streamFile took %v for the last 0.5s of a 2s track; the offset was not applied", elapsed)
	}
	if s.CurrentFileDuration < 1900*time.Millisecond {
		t.Fatalf("duration = %v, want ~2s", s.CurrentFileDuration)
	}
}

func TestIdleTrackKeepsProducingForInternalSubscriber(t *testing.T) {
	r := NewRelay(false, nil)
	sm := NewStreamerManager(r, nil, nil)
	s := newIdleStreamer("/idle-internal")
	path := writeTestFile(t, "long.wav", makeWavHeader(1, 8000, 16, 32000))

	stream := r.GetOrCreateStream("/idle-internal")
	stream.SubscribeInternal("transcoder-1", 0)
	defer stream.Unsubscribe("transcoder-1")

	if out := sm.idleTrack(context.Background(), s, path, 0, 1); !out.start || out.offset != 0 {
		t.Fatalf("result = %+v, want immediate playback with an internal subscriber", out)
	}
}
