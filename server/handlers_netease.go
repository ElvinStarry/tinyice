package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/DatanoiseTV/tinyice/config"
	"github.com/DatanoiseTV/tinyice/logger"
	"github.com/DatanoiseTV/tinyice/netease"
	qrcode "github.com/skip2/go-qrcode"
)

// neteaseCacheJob is the progress of the "cache this playlist" action for
// one AutoDJ mount. Downloads run in the background because a large
// playlist can take minutes; the UI polls GET on the same route.
type neteaseCacheJob struct {
	Mount      string  `json:"mount"`
	Total      int     `json:"total"`
	Done       int     `json:"done"`
	Failed     int     `json:"failed"`
	Cached     int     `json:"cached,omitempty"`
	Active     bool    `json:"active"`
	Error      string  `json:"error,omitempty"`
	IDs        []int64 `json:"ids,omitempty"`
	StartedAt  int64   `json:"started_at,omitempty"`
	FinishedAt int64   `json:"finished_at,omitempty"`
}

// neteaseCacheState is the persistent part of the cache-job store. It
// lives next to the config file so a restart does not lose which mount
// was caching and how far it got.
type neteaseCacheState struct {
	Jobs   map[string]*neteaseCacheJob `json:"jobs"`
	Queued map[string][]int64          `json:"queued,omitempty"`
}

// handleNeteaseStatus reports the stored account and cache settings. It
// never returns the MUSIC_U cookie itself.
func (s *Server) handleNeteaseStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireNeteaseAdmin(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("check") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		_, _ = session.Check(ctx)
	}
	s.writeNeteaseStatus(w, session)
}

func (s *Server) writeNeteaseStatus(w http.ResponseWriter, session *netease.Session) {
	st := session.Status()
	cfg := s.Config.NetEase
	out := map[string]any{
		"logged_in":     st.State == netease.LoginValid,
		"state":         st.State.String(),
		"nickname":      st.Nickname,
		"checked_at":    st.CheckedAt.Unix(),
		"last_error":    st.LastError,
		"cache_enabled": cfg.CacheOn(),
		"cache_dir":     cfg.EffectiveCacheDir(),
		"music_u_set":   st.MusicUSet,
	}
	jsonResponse(w, out)
}

func (s *Server) neteaseCacheStatePath() string {
	if s.Config == nil || s.Config.ConfigPath == "" {
		return ""
	}
	return s.Config.ConfigPath + ".cache-state.json"
}

// loadNeteaseCacheStateLocked restores job snapshots once. An active job
// cannot survive a process restart, so it is marked interrupted instead of
// leaving the UI polling forever.
func (s *Server) loadNeteaseCacheStateLocked() {
	if s.neteaseCacheLoaded {
		return
	}
	s.neteaseCacheLoaded = true
	path := s.neteaseCacheStatePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state neteaseCacheState
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	if s.neteaseCacheJobs == nil {
		s.neteaseCacheJobs = make(map[string]*neteaseCacheJob)
	}
	if s.neteaseCacheQueued == nil {
		s.neteaseCacheQueued = make(map[string][]int64)
	}
	for mount, job := range state.Jobs {
		if job == nil || mount == "" {
			continue
		}
		if job.Active {
			job.Active = false
			if job.Error == "" {
				job.Error = "interrupted by server restart"
			}
			job.FinishedAt = time.Now().Unix()
			s.neteaseCacheQueued[mount] = appendUniqueIDs(s.neteaseCacheQueued[mount], job.IDs...)
		}
		s.neteaseCacheJobs[mount] = job
	}
	if s.neteaseCacheQueued == nil {
		s.neteaseCacheQueued = make(map[string][]int64)
	}
	for mount, ids := range state.Queued {
		if len(ids) > 0 {
			s.neteaseCacheQueued[mount] = appendUniqueIDs(s.neteaseCacheQueued[mount], ids...)
		}
	}
	s.persistNeteaseCacheStateLocked()
}

func (s *Server) persistNeteaseCacheStateLocked() {
	path := s.neteaseCacheStatePath()
	if path == "" {
		return
	}
	state := neteaseCacheState{
		Jobs:   s.neteaseCacheJobs,
		Queued: s.neteaseCacheQueued,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func appendUniqueIDs(dst []int64, ids ...int64) []int64 {
	for _, id := range ids {
		if id <= 0 || containsID(dst, id) {
			continue
		}
		dst = append(dst, id)
	}
	return dst
}

func containsID(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

// handleNeteaseCachePlaylist starts a background download of every NetEase
// track in one AutoDJ's current playlist, or reports the job's progress.
// The route carries the mount as a query parameter injected by the
// /api/autodj/{mount}/netease/cache router.
func (s *Server) handleNeteaseCachePlaylist(w http.ResponseWriter, r *http.Request) {
	session, ok := s.requireNeteaseAdmin(w, r)
	if !ok {
		return
	}
	mount := r.URL.Query().Get("mount")
	if mount == "" {
		jsonError(w, "Missing mount", http.StatusBadRequest)
		return
	}
	streamer := s.StreamerM.GetStreamer(mount)
	if streamer == nil {
		jsonError(w, "AutoDJ is not running; start it once so its playlist can be loaded", http.StatusNotFound)
		return
	}
	if streamer.Source != config.AutoDJSourceNetease {
		jsonError(w, "This AutoDJ does not use the NetEase source", http.StatusBadRequest)
		return
	}

	ids := streamer.NeteasePlaylistIDs()
	s.neteaseCacheMu.Lock()
	s.loadNeteaseCacheStateLocked()
	existing := s.neteaseCacheJobs[mount]
	if existing != nil && r.Method == http.MethodGet {
		snapshot := *existing
		s.neteaseCacheMu.Unlock()
		if !snapshot.Active {
			snapshot.Cached = s.StreamerM.NeteaseCachedCount(ids)
		}
		jsonResponse(w, snapshot)
		return
	}
	s.neteaseCacheMu.Unlock()

	switch r.Method {
	case http.MethodGet:
		jsonResponse(w, neteaseCacheJob{
			Mount:  mount,
			Total:  len(ids),
			Cached: s.StreamerM.NeteaseCachedCount(ids),
		})
	case http.MethodPost:
		if !s.isCSRFSafe(r) {
			jsonError(w, "Forbidden", http.StatusForbidden)
			return
		}
		if !s.Config.NetEase.CacheOn() {
			jsonError(w, "Enable the NetEase download cache before caching a playlist", http.StatusBadRequest)
			return
		}
		if !session.MusicUSet() {
			jsonError(w, "Sign in to NetEase before caching a playlist", http.StatusBadRequest)
			return
		}
		if len(ids) == 0 {
			jsonError(w, "This AutoDJ playlist has no NetEase tracks", http.StatusBadRequest)
			return
		}

		s.neteaseCacheMu.Lock()
		s.loadNeteaseCacheStateLocked()
		if existing := s.neteaseCacheJobs[mount]; existing != nil && existing.Active {
			job := *existing
			s.neteaseCacheMu.Unlock()
			jsonResponse(w, job)
			return
		}
		job := &neteaseCacheJob{
			Mount:     mount,
			Total:     len(ids),
			Active:    true,
			IDs:       append([]int64(nil), ids...),
			StartedAt: time.Now().Unix(),
		}
		if s.neteaseCacheJobs == nil {
			s.neteaseCacheJobs = make(map[string]*neteaseCacheJob)
		}
		s.neteaseCacheJobs[mount] = job
		s.persistNeteaseCacheStateLocked()
		snapshot := *job
		s.neteaseCacheMu.Unlock()

		go s.runNeteaseCacheJob(mount, ids)
		jsonResponse(w, snapshot)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) neteaseCacheJobSnapshot(mount string) (neteaseCacheJob, bool) {
	s.neteaseCacheMu.Lock()
	defer s.neteaseCacheMu.Unlock()
	job := s.neteaseCacheJobs[mount]
	if job == nil {
		return neteaseCacheJob{}, false
	}
	return *job, true
}

func (s *Server) updateNeteaseCacheJob(mount string, fn func(*neteaseCacheJob)) {
	s.neteaseCacheMu.Lock()
	defer s.neteaseCacheMu.Unlock()
	if job := s.neteaseCacheJobs[mount]; job != nil {
		fn(job)
		s.persistNeteaseCacheStateLocked()
	}
}

// runNeteaseCacheJob downloads with a small worker pool. Three concurrent
// requests keep a 200-track playlist moving without hammering the NetEase
// CDN or tripping its rate limits. Each music id is still serialized by
// netease.Cache, so prefetch and playback cannot corrupt the same file.
func (s *Server) runNeteaseCacheJob(mount string, ids []int64) {
	s.runNeteaseCacheJobWith(mount, ids, func(ctx context.Context, id int64) error {
		return s.StreamerM.CacheNeteaseTrack(ctx, id)
	})
}

// runNeteaseCacheJobWith takes the per-track downloader as a parameter so
// progress, worker shutdown and failure accounting are testable without a
// NetEase account or network.
func (s *Server) runNeteaseCacheJobWith(mount string, ids []int64, download func(context.Context, int64) error) {
	defer s.finishNeteaseCacheJob(mount)

	const workers = 3
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	work := make(chan int64)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				trackCtx, trackCancel := context.WithTimeout(ctx, 10*time.Minute)
				err := download(trackCtx, id)
				trackCancel()
				if err != nil {
					if logger.L != nil {
						logger.L.Warnw("NetEase playlist cache failed",
							"mount", mount, "music_id", id, "error", err)
					}
				}
				s.updateNeteaseCacheJob(mount, func(job *neteaseCacheJob) {
					if err != nil {
						job.Failed++
						if job.Error == "" {
							job.Error = fmt.Sprintf("music id %d: %v", id, err)
						}
						return
					}
					job.Done++
				})
			}
		}()
	}

	for _, id := range ids {
		select {
		case work <- id:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return
		}
	}
	close(work)
	wg.Wait()
}

// finishNeteaseCacheJob closes the current batch and immediately starts
// any ids queued by an auto-cache playlist update while it was running.
func (s *Server) finishNeteaseCacheJob(mount string) {
	var next []int64
	s.neteaseCacheMu.Lock()
	if job := s.neteaseCacheJobs[mount]; job != nil {
		job.Active = false
		job.FinishedAt = time.Now().Unix()
	}
	if queued := s.neteaseCacheQueued[mount]; len(queued) > 0 {
		next = append([]int64(nil), queued...)
		delete(s.neteaseCacheQueued, mount)
		job := &neteaseCacheJob{
			Mount:     mount,
			Total:     len(next),
			Active:    true,
			IDs:       append([]int64(nil), next...),
			StartedAt: time.Now().Unix(),
		}
		s.neteaseCacheJobs[mount] = job
	}
	s.persistNeteaseCacheStateLocked()
	s.neteaseCacheMu.Unlock()
	if len(next) > 0 {
		go s.runNeteaseCacheJob(mount, next)
	}
}

// startOrQueueNeteaseCache starts a batch when the mount is idle, or
// appends new playlist ids to the running batch's queue.
func (s *Server) startOrQueueNeteaseCache(mount string, ids []int64) {
	ids = appendUniqueIDs(nil, ids...)
	if len(ids) == 0 {
		return
	}
	s.neteaseCacheMu.Lock()
	s.loadNeteaseCacheStateLocked()
	if job := s.neteaseCacheJobs[mount]; job != nil && job.Active {
		pending := s.neteaseCacheQueued[mount]
		for _, id := range ids {
			if !containsID(job.IDs, id) {
				pending = appendUniqueIDs(pending, id)
			}
		}
		s.neteaseCacheQueued[mount] = pending
		s.persistNeteaseCacheStateLocked()
		s.neteaseCacheMu.Unlock()
		return
	}
	next := appendUniqueIDs(append([]int64(nil), s.neteaseCacheQueued[mount]...), ids...)
	delete(s.neteaseCacheQueued, mount)
	job := &neteaseCacheJob{
		Mount:     mount,
		Total:     len(next),
		Active:    true,
		IDs:       append([]int64(nil), next...),
		StartedAt: time.Now().Unix(),
	}
	s.neteaseCacheJobs[mount] = job
	s.persistNeteaseCacheStateLocked()
	s.neteaseCacheMu.Unlock()
	go s.runNeteaseCacheJob(mount, next)
}

// resumeNeteaseCacheJobs restarts batches that were active when the
// previous process exited. Their ids were moved back to the queue while
// loading the state file; already-cached tracks finish as cache hits.
func (s *Server) resumeNeteaseCacheJobs() {
	if s.Config == nil || !s.Config.NetEase.CacheOn() {
		return
	}
	if session := s.StreamerM.Netease(); session == nil || !session.MusicUSet() {
		return
	}
	type batch struct {
		mount string
		ids   []int64
	}
	var starts []batch

	s.neteaseCacheMu.Lock()
	s.loadNeteaseCacheStateLocked()
	for mount, ids := range s.neteaseCacheQueued {
		if len(ids) == 0 {
			continue
		}
		if job := s.neteaseCacheJobs[mount]; job != nil && job.Active {
			continue
		}
		next := appendUniqueIDs(nil, ids...)
		delete(s.neteaseCacheQueued, mount)
		job := &neteaseCacheJob{
			Mount:     mount,
			Total:     len(next),
			Active:    true,
			IDs:       append([]int64(nil), next...),
			StartedAt: time.Now().Unix(),
		}
		s.neteaseCacheJobs[mount] = job
		starts = append(starts, batch{mount: mount, ids: next})
	}
	s.persistNeteaseCacheStateLocked()
	s.neteaseCacheMu.Unlock()
	for _, b := range starts {
		go s.runNeteaseCacheJob(b.mount, b.ids)
	}
}

func (s *Server) onNeteasePlaylistChanged(mount string, added, removed []int64) {
	if s.Config == nil || !s.Config.NetEase.CacheOn() {
		return
	}
	streamer := s.StreamerM.GetStreamer(mount)
	if streamer == nil || !streamer.AutoCachePlaylist {
		return
	}
	if session := s.StreamerM.Netease(); session == nil || !session.MusicUSet() {
		return
	}
	if len(added) > 0 {
		s.startOrQueueNeteaseCache(mount, added)
	}
	for _, id := range removed {
		s.removeNeteaseCacheIfUnused(id)
	}
}

func (s *Server) removeNeteaseCacheIfUnused(id int64) {
	s.neteaseCacheMu.Lock()
	s.loadNeteaseCacheStateLocked()
	busy := false
	for _, job := range s.neteaseCacheJobs {
		if job.Active && containsID(job.IDs, id) {
			busy = true
			break
		}
	}
	for _, ids := range s.neteaseCacheQueued {
		if containsID(ids, id) {
			busy = true
			break
		}
	}
	s.neteaseCacheMu.Unlock()
	if busy || s.StreamerM.NeteaseIDInUse(id) {
		return
	}
	s.StreamerM.RemoveNeteaseCache(id)
}

// syncAutoCachePlaylists fills the cache for AutoDJs whose playlist is
// already loaded but whose initial refresh happened before sign-in.
func (s *Server) syncAutoCachePlaylists() {
	if s.Config == nil || !s.Config.NetEase.CacheOn() {
		return
	}
	if session := s.StreamerM.Netease(); session == nil || !session.MusicUSet() {
		return
	}
	for _, streamer := range s.StreamerM.GetStreamers() {
		if streamer.Source != config.AutoDJSourceNetease || !streamer.AutoCachePlaylist {
			continue
		}
		if ids := streamer.NeteasePlaylistIDs(); len(ids) > 0 {
			s.startOrQueueNeteaseCache(streamer.OutputMount, ids)
		}
	}
}

// handleNeteaseQR starts a QR login or polls one. Both are POST with a
// JSON body; an empty key starts a new session, a key polls it.
func (s *Server) handleNeteaseQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireNeteaseAdmin(w, r)
	if !ok {
		return
	}
	if !s.isCSRFSafe(r) {
		jsonError(w, "Forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonError(w, "Invalid request body", http.StatusBadRequest)
			return
		}
	}

	if body.Key == "" {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		key, err := session.Client().QRLoginKey(ctx)
		if err != nil {
			jsonError(w, "Could not create a NetEase login key: "+err.Error(), http.StatusBadGateway)
			return
		}
		png, err := qrcode.Encode("https://music.163.com/login?codekey="+key, qrcode.Medium, 512)
		if err != nil {
			jsonError(w, "Could not render the QR code: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{
			"key":        key,
			"qr":         "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
			"expires_in": 180,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	state, nickname, musicU, err := session.Client().QRLoginCheck(ctx, body.Key)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	switch state {
	case netease.QRWaiting:
		jsonResponse(w, map[string]any{"status": "waiting"})
	case netease.QRScanned:
		jsonResponse(w, map[string]any{"status": "scanned", "nickname": nickname})
	case netease.QRSuccess:
		session.SetLogin(musicU, nickname)
		if s.Config.NetEase == nil {
			s.Config.NetEase = &config.NetEaseConfig{}
		}
		s.Config.NetEase.MusicU = musicU
		if s.Config.NetEase.CacheDir == "" {
			s.Config.NetEase.CacheDir = "netease-cache"
		}
		if err := s.Config.SaveConfig(); err != nil {
			jsonError(w, "Login succeeded but the config could not be saved: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.resumeNeteaseCacheJobs()
		s.syncAutoCachePlaylists()
		jsonResponse(w, map[string]any{"status": "success", "nickname": nickname})
	case netease.QRExpired:
		jsonResponse(w, map[string]any{"status": "expired"})
	default:
		jsonError(w, "Unexpected NetEase login state", http.StatusBadGateway)
	}
}

func (s *Server) handleNeteaseLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireNeteaseAdmin(w, r)
	if !ok {
		return
	}
	if !s.isCSRFSafe(r) {
		jsonError(w, "Forbidden", http.StatusForbidden)
		return
	}
	session.Clear()
	if s.Config.NetEase == nil {
		s.Config.NetEase = &config.NetEaseConfig{}
	}
	s.Config.NetEase.MusicU = ""
	if err := s.Config.SaveConfig(); err != nil {
		jsonError(w, "Could not save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeNeteaseStatus(w, session)
}

// handleNeteaseCache updates the download-cache switch and directory.
func (s *Server) handleNeteaseCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	session, ok := s.requireNeteaseAdmin(w, r)
	if !ok {
		return
	}
	if !s.isCSRFSafe(r) {
		jsonError(w, "Forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Enabled *bool   `json:"enabled"`
		Dir     *string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if s.Config.NetEase == nil {
		s.Config.NetEase = &config.NetEaseConfig{}
	}
	if body.Enabled != nil {
		s.Config.NetEase.SetCacheOn(*body.Enabled)
	}
	if body.Dir != nil {
		dir := *body.Dir
		if dir != "" {
			abs, err := filepath.Abs(dir)
			if err != nil {
				jsonError(w, "Invalid cache directory", http.StatusBadRequest)
				return
			}
			dir = abs
		}
		s.Config.NetEase.CacheDir = dir
	}
	if err := s.Config.SaveConfig(); err != nil {
		jsonError(w, "Could not save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.Config.NetEase.CacheOn() {
		s.resumeNeteaseCacheJobs()
		s.syncAutoCachePlaylists()
	}
	s.writeNeteaseStatus(w, session)
}

func (s *Server) requireNeteaseAdmin(w http.ResponseWriter, r *http.Request) (*netease.Session, bool) {
	user, ok := s.checkAuth(r)
	if !ok {
		jsonError(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	if user.Role != config.RoleSuperAdmin {
		jsonError(w, "Forbidden", http.StatusForbidden)
		return nil, false
	}
	if s.StreamerM == nil || s.StreamerM.Netease() == nil {
		jsonError(w, "NetEase support is not available on this server", http.StatusServiceUnavailable)
		return nil, false
	}
	return s.StreamerM.Netease(), true
}
