package relay

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// A track switch must invalidate the previous track's bytes for NEW
// subscribers without moving existing listeners. Otherwise a connect (or
// a browser/VLC reconnect) replays up to the burst window of the old song.
func TestBeginAutoDJOutputCheckpointsStaleBuffer(t *testing.T) {
	r := NewRelay(false, nil)
	out := r.GetOrCreateStream("/switch")
	out.Broadcast(bytes.Repeat([]byte{0xAB}, 200*1024), r)
	head := out.Buffer.HeadOffset()

	s := &Streamer{Name: "switch", OutputMount: "/switch", Format: "mp3"}
	sm := NewStreamerManager(r, nil, nil)
	if _, err := sm.beginAutoDJOutput(s); err != nil {
		t.Fatal(err)
	}
	if got := out.GetMinListenerOffset(); got != head {
		t.Fatalf("MinListenerOffset = %d, want %d", got, head)
	}
	offset, _ := out.Subscribe("new-listener", 512*1024)
	defer out.Unsubscribe("new-listener")
	if out.GetMinListenerOffset() <= offset {
		t.Fatalf("new subscriber can still read stale audio (min %d <= offset %d)",
			out.GetMinListenerOffset(), offset)
	}
}

func TestIdleTrackCheckpointsStaleBuffer(t *testing.T) {
	r := NewRelay(false, nil)
	out := r.GetOrCreateStream("/idle-cp")
	out.Broadcast(bytes.Repeat([]byte{0xCD}, 200*1024), r)
	head := out.Buffer.HeadOffset()

	sm := NewStreamerManager(r, nil, nil)
	s := newIdleStreamer("/idle-cp")
	path := writeTestFile(t, "long.wav", makeWavHeader(2, 44100, 16, 10*44100*4))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan idleResult, 1)
	go func() { done <- sm.idleTrack(ctx, s, path, 0, 1) }()

	deadline := time.Now().Add(2 * time.Second)
	for out.GetMinListenerOffset() != head {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("idleTrack did not checkpoint the buffer (min %d, want %d)",
				out.GetMinListenerOffset(), head)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// Every AutoDJ MP3 track must be encoded at one sample rate. A 48 kHz
// source used to make the shine encoder emit 48 kHz frames in the middle
// of a 44.1 kHz Icecast stream; browser decoders stall on the rate change.
func TestStreamFilePinsMP3SampleRate(t *testing.T) {
	r := NewRelay(false, nil)
	out := r.GetOrCreateStream("/rate")
	out.Subscribe("probe", 0) // force the encoder to produce bytes
	defer out.Unsubscribe("probe")

	sm := NewStreamerManager(r, nil, nil)
	s := newIdleStreamer("/rate")
	// 0.5 s of 48 kHz stereo 16-bit audio.
	path := writeTestFile(t, "48k.wav", makeWavHeader(2, 48000, 16, 96000))
	if err := sm.streamFile(context.Background(), s, path, 0, 1, 0); err != nil {
		t.Fatal(err)
	}
	if s.CurrentSampleRate != autoDJMP3SampleRate {
		t.Fatalf("CurrentSampleRate = %d, want %d", s.CurrentSampleRate, autoDJMP3SampleRate)
	}

	buf := make([]byte, 8192)
	n, _, _ := out.Buffer.ReadAt(0, buf)
	if n < 4 {
		t.Fatalf("no encoded bytes captured (n=%d)", n)
	}
	buf = buf[:n]
	frame := -1
	for i := 0; i+4 <= len(buf); i++ {
		if buf[i] == 0xFF && buf[i+1]&0xE0 == 0xE0 {
			if _, rate, _, _, valid := parseMP3Header(buf[i : i+4]); valid {
				if rate != autoDJMP3SampleRate {
					t.Fatalf("encoded frame sample rate = %d, want %d", rate, autoDJMP3SampleRate)
				}
				frame = i
				break
			}
		}
	}
	if frame < 0 {
		t.Fatal("no MP3 frame found in the output")
	}
}
