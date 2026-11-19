package relay

import (
	"testing"
	"time"
)

func TestDecodeWatchdogTimesOutWithoutProgress(t *testing.T) {
	s := &Streamer{State: StatePlaying}
	cancelled := make(chan struct{})
	w := newDecodeWatchdog(s, 50*time.Millisecond, func() { close(cancelled) })
	defer w.Stop()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not cancel a stalled decoder")
	}
	if !w.TimedOut() {
		t.Fatal("watchdog did not report a timeout")
	}
}

func TestDecodeWatchdogIgnoresPause(t *testing.T) {
	s := &Streamer{State: StatePaused}
	cancelled := make(chan struct{})
	w := newDecodeWatchdog(s, 50*time.Millisecond, func() { close(cancelled) })
	defer w.Stop()

	time.Sleep(150 * time.Millisecond)
	if w.TimedOut() {
		t.Fatal("watchdog fired while the AutoDJ was paused")
	}

	s.mu.Lock()
	s.State = StatePlaying
	s.mu.Unlock()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not restart its window after resume")
	}
}
