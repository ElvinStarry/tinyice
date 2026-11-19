package relay

import (
	"context"
	"path/filepath"
	"strconv"
	"time"

	"github.com/DatanoiseTV/tinyice/logger"
)

// idleResult tells the playback loop what to do after time-based pacing.
type idleResult struct {
	// start is true when the caller should open the decoder now. The
	// offset is the active playback position to resume from.
	start bool
	// offset excludes time spent paused, so a track that sat idle for
	// ten minutes while paused still resumes at its real position.
	offset time.Duration
}

type trackMeta struct {
	Display  string
	Artist   string
	Title    string
	Album    string
	Duration time.Duration
}

// metaForPath builds the display metadata for a track. NetEase tracks use
// the playlist metadata; local files fall back to their filename because
// idle pacing must not read audio data.
func (s *Streamer) metaForPath(path string, duration time.Duration) trackMeta {
	if t, ok := s.neteaseTrackForPath(path); ok {
		return trackMeta{
			Display:  t.DisplayTitle(),
			Artist:   joinArtists(t.Artists),
			Title:    t.Name,
			Album:    t.Album,
			Duration: t.Duration,
		}
	}
	base := filepath.Base(path)
	return trackMeta{Display: base, Title: base, Duration: duration}
}

func joinArtists(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	out := names[0]
	for _, n := range names[1:] {
		out += " / " + n
	}
	return out
}

func (s *Streamer) applyTrackMeta(meta trackMeta, path string, pos, id int) {
	s.mu.Lock()
	s.CurrentFile = meta.Display
	s.CurrentArtist = meta.Artist
	s.CurrentTitle = meta.Title
	s.CurrentAlbum = meta.Album
	s.CurrentFilePath = path
	s.CurrentPlayingPos = pos
	s.CurrentPlayingID = id
	s.CurrentSampleRate = 0
	s.CurrentChannels = 2
	s.CurrentFileTime = time.Now()
	s.CurrentPausedNanos.Store(0)
	s.CurrentFileDuration = meta.Duration
	s.mu.Unlock()
	s.broadcastIdle("player")
}

// idleTrack advances one track on the wall clock while the mount has no
// subscribers. It downloads nothing and starts no decoder. If a listener
// joins mid-track, the returned offset is the active elapsed time and the
// caller starts the normal decode/encode chain at that position.
func (sm *StreamerManager) idleTrack(ctx context.Context, s *Streamer, path string, pos, id int) idleResult {
	if out, ok := sm.relay.GetStream(s.OutputMount); ok && out.SubscriberCount() > 0 {
		return idleResult{start: true}
	}
	duration, ok := sm.trackDuration(s, path)
	if !ok || duration <= 0 {
		return idleResult{start: true}
	}

	meta := s.metaForPath(path, duration)
	s.applyTrackMeta(meta, path, pos, id)
	// Start the same prefetch watcher used during real playback. It runs
	// while nobody listens, so the next NetEase track is already cached
	// when the idle clock reaches the track boundary.
	sm.scheduleNeteasePrefetch(s, path)

	s.mu.RLock()
	inject := s.InjectMetadata
	s.mu.RUnlock()

	output, err := sm.beginAutoDJOutput(s)
	if err != nil {
		return idleResult{start: true}
	}
	output.mu.Lock()
	if inject {
		output.CurrentSong = meta.Display
		output.Name = s.Name
		output.Visible = true
	}
	output.Bitrate = strconv.Itoa(s.Bitrate)
	if s.Format == "opus" {
		output.ContentType = "audio/ogg"
	} else {
		output.ContentType = "audio/mpeg"
	}
	output.mu.Unlock()
	output.MarkProducerAlive()
	if inject {
		sm.relay.UpdateMetadata(s.OutputMount, meta.Display)
	}

	logger.L.Debugw("AutoDJ idle pacing (no subscribers)",
		"mount", s.OutputMount, "path", path, "duration", duration)

	var elapsed time.Duration
	remaining := duration
	last := time.Now()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return idleResult{}
		case now := <-ticker.C:
			// Keep the mount out of the HealthMonitor's dead-stream
			// sweep. Idle tracks produce no bytes, but they are alive.
			output.MarkProducerAlive()

			s.mu.RLock()
			state := s.State
			s.mu.RUnlock()
			if state == StatePaused {
				// Time spent paused belongs to neither elapsed nor
				// remaining; record it so the UI position stays
				// honest while parked.
				s.CurrentPausedNanos.Add(int64(now.Sub(last)))
				last = now
				continue
			}
			if state != StatePlaying {
				return idleResult{}
			}
			delta := now.Sub(last)
			last = now
			elapsed += delta
			remaining -= delta
			if output.SubscriberCount() > 0 {
				return idleResult{start: true, offset: elapsed}
			}
			if remaining <= 0 {
				return idleResult{}
			}
		}
	}
}
