package relay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DatanoiseTV/tinyice/config"
	"github.com/DatanoiseTV/tinyice/netease"
)

// While idle (no subscribers), the playback clock keeps advancing and the
// next NetEase track is prefetched as the current one approaches its end.
func TestIdleTrackPrefetchesNextNeteaseTrack(t *testing.T) {
	const current, next int64 = 11, 22

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "22.mp3"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{NetEase: &config.NetEaseConfig{CacheDir: dir}}
	sm := NewStreamerManager(NewRelay(false, nil), cfg, netease.NewSession(nil, ""))

	s := newIdleStreamer("/idle-prefetch")
	s.Source = "netease"
	s.Playlist = []PlaylistSong{{Path: neteasePath(current)}, {Path: neteasePath(next)}}
	s.neteaseTracks = map[int64]netease.Track{
		current: {ID: current, Name: "current", Duration: 3 * time.Second},
		next:    {ID: next, Name: "next", Duration: 3 * time.Second},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()

	idleCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan idleResult, 1)
	go func() { done <- sm.idleTrack(idleCtx, s, neteasePath(current), 0, 1) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		sm.prefetchMu.Lock()
		_, prefetched := sm.prefetch[next]
		sm.prefetchMu.Unlock()
		if prefetched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle playback did not prefetch the next NetEase track")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done
}
