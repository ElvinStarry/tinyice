package relay

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DatanoiseTV/tinyice/config"
	"github.com/DatanoiseTV/tinyice/logger"
	"github.com/DatanoiseTV/tinyice/netease"
)

// neteasePathPrefix marks a playlist entry that is resolved through the
// NetEase source instead of the filesystem. The music id is stable across
// playlists, restarts and cache entries.
const neteasePathPrefix = "netease:"

func neteasePath(id int64) string { return neteasePathPrefix + strconv.FormatInt(id, 10) }

// pathAllowedForSource keeps persisted playlists from crossing sources: a
// NetEase AutoDJ must not start by playing leftover local files, and a
// local AutoDJ must not queue virtual NetEase ids it cannot resolve.
func pathAllowedForSource(path, source string) bool {
	_, ncm := parseNeteasePath(path)
	return ncm == (source == config.AutoDJSourceNetease)
}

func parseNeteasePath(path string) (int64, bool) {
	if !strings.HasPrefix(path, neteasePathPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(path, neteasePathPrefix), 10, 64)
	return id, err == nil && id > 0
}

func (s *Streamer) neteaseTrackForPath(path string) (netease.Track, bool) {
	id, ok := parseNeteasePath(path)
	if !ok {
		return netease.Track{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.neteaseTracks[id]
	return t, ok
}

// trackDuration returns a track's wall-clock length for idle pacing.
// NetEase metadata carries an exact duration; local files are inspected
// without decoding.
func (sm *StreamerManager) trackDuration(s *Streamer, path string) (time.Duration, bool) {
	if _, ok := parseNeteasePath(path); ok {
		t, ok := s.neteaseTrackForPath(path)
		if !ok || t.Duration <= 0 {
			return 0, false
		}
		return t.Duration, true
	}
	d, err := estimateAudioDuration(path)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// acquireNetease resolves one music id to a local file (cached or
// temporary) ready for the decoder.
func (sm *StreamerManager) acquireNetease(ctx context.Context, id int64) (*netease.Lease, error) {
	if sm.netease == nil {
		return nil, fmt.Errorf("netease source not configured")
	}
	enabled, dir := sm.neteaseCacheSettings()
	return sm.ncmCache.Acquire(ctx, sm.netease.Client(), id, dir, enabled)
}

func (sm *StreamerManager) neteaseCacheSettings() (enabled bool, dir string) {
	enabled = true
	dir = netease.DefaultCacheDir
	if sm.config != nil && sm.config.NetEase != nil {
		enabled = sm.config.NetEase.CacheOn()
		dir = sm.config.NetEase.EffectiveCacheDir()
	}
	return enabled, dir
}

// openNeteaseStream follows a growing cache file instead of waiting for a
// complete download. A cache hit still returns an ordinary file lease.
func (sm *StreamerManager) openNeteaseStream(ctx context.Context, id int64) (*netease.StreamLease, error) {
	if sm.netease == nil {
		return nil, fmt.Errorf("netease source not configured")
	}
	enabled, dir := sm.neteaseCacheSettings()
	return sm.ncmCache.OpenStream(ctx, sm.netease.Client(), id, dir, enabled)
}

// NeteaseCachedCount reports how many of ids already have a complete
// cache file. The UI uses it to show a playlist's cache coverage without
// starting a job.
func (sm *StreamerManager) NeteaseCachedCount(ids []int64) int {
	enabled, dir := sm.neteaseCacheSettings()
	if !enabled {
		return 0
	}
	return sm.ncmCache.CachedCount(ids, dir)
}

// RemoveNeteaseCache deletes complete cache files for id. Callers must
// first verify that no streamer or prefetch still uses the song.
func (sm *StreamerManager) RemoveNeteaseCache(id int64) {
	enabled, dir := sm.neteaseCacheSettings()
	if !enabled {
		return
	}
	if err := sm.ncmCache.Remove(id, dir); err != nil {
		logger.L.Warnw("NetEase cache delete failed", "music_id", id, "error", err)
	}
}

// NeteaseIDInUse reports whether any AutoDJ's playlist, queue, current
// track, or prefetch holds the music id.
func (sm *StreamerManager) NeteaseIDInUse(id int64) bool {
	for _, s := range sm.GetStreamers() {
		if s.UsesNeteaseID(id) {
			return true
		}
	}
	sm.prefetchMu.Lock()
	defer sm.prefetchMu.Unlock()
	if _, ok := sm.prefetch[id]; ok {
		return true
	}
	_, ok := sm.prefetching[id]
	return ok
}

func (s *Streamer) UsesNeteaseID(id int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if current, ok := parseNeteasePath(s.CurrentFilePath); ok && current == id {
		return true
	}
	for _, p := range s.Playlist {
		if musicID, ok := parseNeteasePath(p.Path); ok && musicID == id {
			return true
		}
	}
	for _, p := range s.Queue {
		if musicID, ok := parseNeteasePath(p); ok && musicID == id {
			return true
		}
	}
	return false
}

// CacheNeteaseTrack downloads one song into the shared cache without
// holding it for playback. With caching enabled the lease releases
// nothing; with caching disabled the temp file is removed immediately.
// The HTTP layer checks the cache switch before starting a batch, so this
// path only serves the "cache playlist" action and prefetch.
func (sm *StreamerManager) CacheNeteaseTrack(ctx context.Context, id int64) error {
	lease, err := sm.acquireNetease(ctx, id)
	if err != nil {
		return err
	}
	lease.Release()
	return nil
}

// neteaseCatalog is the slice of the NetEase client the playlist refresh
// needs. Tests substitute a local catalog.
type neteaseCatalog interface {
	Playlist(context.Context, int64) (netease.Playlist, error)
	SongDetails(context.Context, []int64) ([]netease.Track, error)
}

// refreshNetease rebuilds the source-managed part of the playlist from the
// configured NetEase playlists. The whole refresh is all-or-nothing: a
// transient failure on one playlist keeps the previous working playlist
// instead of silently dropping its tracks.
func (sm *StreamerManager) refreshNetease(ctx context.Context, s *Streamer) error {
	if sm.netease == nil {
		return fmt.Errorf("netease session is not configured")
	}
	return sm.refreshNeteaseWith(ctx, s, sm.netease.Client())
}

func (sm *StreamerManager) refreshNeteaseWith(ctx context.Context, s *Streamer, client neteaseCatalog) error {
	s.mu.RLock()
	ids := append([]int64(nil), s.NeteasePlaylists...)
	s.mu.RUnlock()
	if len(ids) == 0 {
		return fmt.Errorf("no netease playlists configured")
	}

	seen := make(map[int64]struct{})
	var order []int64
	for _, pid := range ids {
		pl, err := client.Playlist(ctx, pid)
		if err != nil {
			return fmt.Errorf("playlist %d: %w", pid, err)
		}
		for _, tid := range pl.TrackIDs {
			if _, ok := seen[tid]; ok {
				continue
			}
			seen[tid] = struct{}{}
			order = append(order, tid)
		}
	}
	if len(order) == 0 {
		return fmt.Errorf("configured netease playlists are empty")
	}

	tracks := make(map[int64]netease.Track, len(order))
	const batch = 200
	for start := 0; start < len(order); start += batch {
		end := start + batch
		if end > len(order) {
			end = len(order)
		}
		details, err := client.SongDetails(ctx, order[start:end])
		if err != nil {
			return fmt.Errorf("song detail: %w", err)
		}
		for _, t := range details {
			tracks[t.ID] = t
		}
	}

	s.mu.Lock()
	oldIDs := make(map[string]int, len(s.Playlist))
	oldSet := make(map[int64]struct{}, len(s.Playlist))
	for _, p := range s.Playlist {
		if id, ok := parseNeteasePath(p.Path); ok {
			oldIDs[p.Path] = p.ID
			oldSet[id] = struct{}{}
		}
	}
	currentPath := s.CurrentFilePath
	playlist := make([]PlaylistSong, 0, len(order))
	newSet := make(map[int64]struct{}, len(order))
	for _, id := range order {
		t, ok := tracks[id]
		if !ok {
			continue
		}
		path := neteasePath(t.ID)
		entryID, ok := oldIDs[path]
		if !ok {
			entryID = s.NextID
			s.NextID++
		}
		playlist = append(playlist, PlaylistSong{Path: path, ID: entryID})
		newSet[t.ID] = struct{}{}
	}
	if len(playlist) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("netEase returned metadata for none of the playlist tracks")
	}
	// The refresh replaces the slice while a track may be playing. Keep
	// the cursor right after that track so the next pick does not jump
	// back to the start of the playlist.
	nextPos := s.CurrentPos
	if _, ok := parseNeteasePath(currentPath); ok {
		nextPos = 0
		for i, p := range playlist {
			if p.Path == currentPath {
				nextPos = i + 1
				break
			}
		}
		if nextPos > len(playlist) {
			nextPos = 0
		}
	}
	s.neteaseTracks = tracks
	s.Playlist = playlist
	s.CurrentPos = nextPos
	s.PlaylistVersion++
	s.mu.Unlock()

	s.broadcastIdle("playlist")
	logger.L.Infow("AutoDJ NetEase playlist refreshed",
		"mount", s.OutputMount, "playlists", ids, "tracks", len(playlist))
	if cb := sm.OnNeteasePlaylistChanged; cb != nil {
		added, removed := diffNeteaseIDs(oldSet, newSet)
		if len(added) > 0 || len(removed) > 0 {
			cb(s.OutputMount, added, removed)
		}
	}
	return nil
}

func diffNeteaseIDs(oldSet, newSet map[int64]struct{}) (added, removed []int64) {
	for id := range newSet {
		if _, ok := oldSet[id]; !ok {
			added = append(added, id)
		}
	}
	for id := range oldSet {
		if _, ok := newSet[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i] < added[j] })
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	return added, removed
}

// runNeteaseRefresher keeps the playlist in sync with the account's
// playlists. Failures retry quickly; successes wait ten minutes.
func (sm *StreamerManager) runNeteaseRefresher(ctx context.Context, s *Streamer) {
	retry := time.Duration(0)
	for {
		if retry > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
		} else {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := sm.refreshNetease(rctx, s)
		cancel()
		if err != nil {
			logger.L.Warnw("AutoDJ NetEase playlist refresh failed",
				"mount", s.OutputMount, "error", err)
			retry = time.Minute
			continue
		}
		retry = 10 * time.Minute
	}
}

const (
	// prefetchLead is how long before the end of the current track the
	// next NetEase download starts. Thirty seconds absorbs typical CDN
	// jitter without holding many files.
	prefetchLead = 30 * time.Second
	// prefetchTTL releases an unused temporary file. The next track
	// normally starts within a minute of the prefetch; ten minutes also
	// covers a short pause without leaving stale temp files around.
	prefetchTTL = 10 * time.Minute
)

type prefetchEntry struct {
	lease *netease.Lease
	timer *time.Timer
}

// nextNeteaseMusicID predicts which track the loop will pick next. It
// returns false for local queue entries, song commands and shuffle, where
// a prefetch would be wasted.
func (s *Streamer) nextNeteaseMusicID(currentPath string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.Queue) > 0 {
		return parseNeteasePath(s.Queue[0])
	}
	if s.Shuffle {
		return 0, false
	}
	idx := -1
	for i, p := range s.Playlist {
		if p.Path == currentPath {
			idx = i
			break
		}
	}
	if idx < 0 {
		return 0, false
	}
	next := idx + 1
	if next >= len(s.Playlist) {
		if !s.Loop {
			return 0, false
		}
		next = 0
	}
	return parseNeteasePath(s.Playlist[next].Path)
}

// scheduleNeteasePrefetch starts a background watcher for the current
// track. When its remaining time reaches prefetchLead, the next NetEase
// track is downloaded (or looked up in the cache) so the transition does
// not stall on the network.
func (sm *StreamerManager) scheduleNeteasePrefetch(s *Streamer, path string) {
	if s.ctx == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				s.mu.RLock()
				current := s.CurrentFilePath
				state := s.State
				start := s.CurrentFileTime
				duration := s.CurrentFileDuration
				paused := time.Duration(s.CurrentPausedNanos.Load())
				s.mu.RUnlock()
				if current != path {
					return
				}
				if state != StatePlaying || start.IsZero() || duration <= 0 {
					continue
				}
				pos := time.Since(start) - paused
				if pos < 0 {
					pos = 0
				}
				if duration-pos > prefetchLead {
					continue
				}
				if id, ok := s.nextNeteaseMusicID(path); ok {
					sm.prefetchNetease(s.ctx, id)
				}
				return
			}
		}
	}()
}

func (sm *StreamerManager) prefetchNetease(parent context.Context, id int64) {
	sm.prefetchMu.Lock()
	_, exists := sm.prefetch[id]
	_, busy := sm.prefetching[id]
	if !exists && !busy {
		if sm.prefetching == nil {
			sm.prefetching = make(map[int64]struct{})
		}
		sm.prefetching[id] = struct{}{}
	}
	sm.prefetchMu.Unlock()
	if exists || busy {
		return
	}
	defer func() {
		sm.prefetchMu.Lock()
		delete(sm.prefetching, id)
		sm.prefetchMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	lease, err := sm.acquireNetease(ctx, id)
	if err != nil {
		logger.L.Debugw("AutoDJ NetEase prefetch failed", "music_id", id, "error", err)
		return
	}
	sm.storePrefetch(id, lease)
}

func (sm *StreamerManager) storePrefetch(id int64, lease *netease.Lease) {
	sm.prefetchMu.Lock()
	if sm.prefetch == nil {
		sm.prefetch = make(map[int64]*prefetchEntry)
	}
	if old := sm.prefetch[id]; old != nil {
		sm.prefetchMu.Unlock()
		lease.Release()
		return
	}
	entry := &prefetchEntry{lease: lease}
	entry.timer = time.AfterFunc(prefetchTTL, func() { sm.dropPrefetch(id, entry) })
	sm.prefetch[id] = entry
	sm.prefetchMu.Unlock()
	logger.L.Debugw("AutoDJ NetEase prefetched next track", "music_id", id, "path", lease.Path)
}

func (sm *StreamerManager) dropPrefetch(id int64, entry *prefetchEntry) {
	sm.prefetchMu.Lock()
	if sm.prefetch[id] != entry {
		sm.prefetchMu.Unlock()
		return
	}
	delete(sm.prefetch, id)
	sm.prefetchMu.Unlock()
	entry.lease.Release()
}

func (sm *StreamerManager) takePrefetch(id int64) *netease.Lease {
	sm.prefetchMu.Lock()
	entry := sm.prefetch[id]
	delete(sm.prefetch, id)
	sm.prefetchMu.Unlock()
	if entry == nil {
		return nil
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	return entry.lease
}
