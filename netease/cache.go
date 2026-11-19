package netease

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultCacheDir is used when the config leaves the directory empty.
const DefaultCacheDir = "netease-cache"

// ErrStreamStalled means the decoder asked for bytes and the download made
// no progress for the configured stall window. The AutoDJ treats it like
// any other per-track stream failure and moves on.
var ErrStreamStalled = errors.New("netease: cache stream stalled")

// StreamLease is a local file that grows while a decoder reads it. The
// downloader appends HTTP body chunks; the follower blocks at EOF until
// more data arrives or the download finishes. With caching enabled the
// completed .part file is renamed to <id>.<ext>, and the open descriptor
// keeps reading the same inode across the rename.
type StreamLease struct {
	// Path is the temp path returned to callers for logs and hooks. It
	// does not change when the completed file is renamed into the cache.
	Path string

	reader *followReader
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	notify chan struct{}

	mu        sync.Mutex
	err       error
	lastWrite time.Time

	stallTimeout time.Duration
	tmpPath      string
	closeOnce    sync.Once
}

// StreamLease implements io.ReadCloser.
func (s *StreamLease) Read(p []byte) (int, error) { return s.reader.Read(p) }

// Close cancels an in-flight download, closes the read handle and removes
// the .part file. A completed cache file is left in place.
func (s *StreamLease) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.reader != nil && s.reader.file != nil {
			_ = s.reader.file.Close()
		}
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			// A wedged HTTP read must not keep the AutoDJ from advancing.
		}
		if s.tmpPath != "" {
			_ = os.Remove(s.tmpPath)
		}
	})
	return nil
}

func (s *StreamLease) progress() {
	s.mu.Lock()
	s.lastWrite = time.Now()
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *StreamLease) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *StreamLease) state() (err error, lastWrite time.Time, finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		finished = true
	default:
	}
	return s.err, s.lastWrite, finished
}

type followReader struct {
	lease *StreamLease
	file  *os.File
	off   int64
}

func (r *followReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		n, err := r.file.ReadAt(p, r.off)
		if n > 0 {
			r.off += int64(n)
			return n, nil
		}
		if err != nil && err != io.EOF {
			return 0, err
		}

		readErr, lastWrite, finished := r.lease.state()
		if finished {
			if readErr != nil {
				return 0, readErr
			}
			return 0, io.EOF
		}

		stall := r.lease.stallTimeout
		if stall <= 0 {
			stall = 5 * time.Second
		}
		idle := time.Since(lastWrite)
		if idle >= stall {
			return 0, ErrStreamStalled
		}
		timer := time.NewTimer(stall - idle)
		select {
		case <-r.lease.notify:
			timer.Stop()
		case <-r.lease.done:
			timer.Stop()
		case <-r.lease.ctx.Done():
			timer.Stop()
			return 0, r.lease.ctx.Err()
		case <-timer.C:
			// Re-check for data, completion or a full stall window.
		}
	}
}

// Cache downloads playback audio. Files are addressed by music id only,
// so a song appearing in several playlists (or several AutoDJs) is
// downloaded once. When caching is disabled the file goes to the system
// temp directory and is removed when playback ends.
type Cache struct {
	HTTP *http.Client

	// StallTimeout bounds how long a decoder waits for the next write
	// before the track is abandoned. Zero uses 5 seconds.
	StallTimeout time.Duration

	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

// Lease is a local file ready for the decoder.
type Lease struct {
	Path string

	cleanup func()
}

// Release removes a temporary file. A cached file is left in place.
func (l *Lease) Release() {
	if l.cleanup != nil {
		l.cleanup()
	}
}

func (c *Cache) lockFor(id int64) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = make(map[int64]*sync.Mutex)
	}
	m := c.locks[id]
	if m == nil {
		m = new(sync.Mutex)
		c.locks[id] = m
	}
	return m
}

func cacheDir(dir string) string {
	if dir != "" {
		return dir
	}
	return DefaultCacheDir
}

// Acquire returns a playable file for id, downloading it when needed.
// cacheEnabled persists the file under cacheDirectory; otherwise the
// returned lease deletes it on Release.
func (c *Cache) Acquire(ctx context.Context, client *Client, id int64, dir string, cacheEnabled bool) (*Lease, error) {
	lock := c.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	if cacheEnabled {
		if path := c.findCached(id, dir); path != "" {
			return &Lease{Path: path}, nil
		}
	}

	var lastErr error
	for range 3 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := client.SongURL(ctx, id)
		if err != nil {
			lastErr = err
			// Missing URL at every quality will not change on retry;
			// anything else (network, HTTP) might.
			if errors.Is(err, ErrUnavailable) {
				return nil, err
			}
			continue
		}
		lease, err := c.fetch(ctx, info, id, dir, cacheEnabled)
		if err == nil {
			return lease, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrUnavailable
	}
	return nil, fmt.Errorf("netease: download %d: %w", id, lastErr)
}

func (c *Cache) findCached(id int64, dir string) string {
	dir = cacheDir(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := strconv.FormatInt(id, 10) + "."
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, ".part") {
			continue
		}
		path := filepath.Join(dir, name)
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			return path
		}
	}
	return ""
}

// IsCached reports whether a complete file exists for id.
func (c *Cache) IsCached(id int64, dir string) bool {
	return c.findCached(id, dir) != ""
}

// CachedCount reports how many of ids have a complete file. It reads the
// directory once instead of walking it for every id.
func (c *Cache) CachedCount(ids []int64, dir string) int {
	if len(ids) == 0 {
		return 0
	}
	dir = cacheDir(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	wanted := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	have := make(map[int64]struct{}, len(ids))
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".part") {
			continue
		}
		dot := strings.IndexByte(name, '.')
		if dot <= 0 {
			continue
		}
		id, err := strconv.ParseInt(name[:dot], 10, 64)
		if err != nil {
			continue
		}
		if _, ok := wanted[id]; !ok {
			continue
		}
		if st, err := e.Info(); err == nil && st.Size() > 0 {
			have[id] = struct{}{}
		}
	}
	return len(have)
}

// Remove deletes every complete cache file for id. In-progress .part
// files are left to their downloader. It is a no-op when the directory
// does not exist.
func (c *Cache) Remove(id int64, dir string) error {
	dir = cacheDir(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	prefix := strconv.FormatInt(id, 10) + "."
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, ".part") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func cacheLocation(dir string, cacheEnabled bool) (string, error) {
	if cacheEnabled {
		dir = cacheDir(dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		return dir, nil
	}
	dir = filepath.Join(os.TempDir(), "tinyice-netease")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// OpenStream returns a decoder view of the track. A complete cache file
// is opened directly. On a cache miss it resolves the playback URL, starts
// the HTTP download into a .part file and returns immediately; reads then
// follow the growing file. This keeps the first byte latency near the
// network RTT instead of waiting for the whole song.
func (c *Cache) OpenStream(ctx context.Context, client *Client, id int64, dir string, cacheEnabled bool) (*StreamLease, error) {
	if cacheEnabled {
		if path := c.findCached(id, dir); path != "" {
			f, err := os.Open(path)
			if err == nil {
				done := make(chan struct{})
				close(done)
				lease := &StreamLease{
					Path:   path,
					ctx:    ctx,
					cancel: func() {},
					done:   done,
					notify: make(chan struct{}, 1),
				}
				lease.reader = &followReader{lease: lease, file: f}
				return lease, nil
			}
			// An unreadable cache entry must not pin the track in a
			// permanent error state; fall through and download again.
		}
	}

	urlCtx, cancel := context.WithTimeout(ctx, c.stallTimeout())
	info, err := client.SongURL(urlCtx, id)
	cancel()
	if err != nil {
		return nil, err
	}
	return c.openStream(ctx, info, id, dir, cacheEnabled)
}

// openStream is the transport-free half of OpenStream. Tests point
// info.URL at an httptest server.
func (c *Cache) openStream(ctx context.Context, info SongURL, id int64, dir string, cacheEnabled bool) (*StreamLease, error) {
	dir, err := cacheLocation(dir, cacheEnabled)
	if err != nil {
		return nil, err
	}
	finalPath := filepath.Join(dir, strconv.FormatInt(id, 10)+"."+mediaExt(info))
	tmp, err := os.CreateTemp(dir, strconv.FormatInt(id, 10)+".*.part")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()

	rf, err := os.Open(tmpPath)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return nil, err
	}

	dctx, cancel := context.WithCancel(ctx)
	lease := &StreamLease{
		Path:         tmpPath,
		ctx:          dctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		notify:       make(chan struct{}, 1),
		lastWrite:    time.Now(),
		stallTimeout: c.stallTimeout(),
		tmpPath:      tmpPath,
	}
	lease.reader = &followReader{lease: lease, file: rf}
	go lease.download(dctx, c, info, tmp, finalPath, cacheEnabled)
	return lease, nil
}

func (c *Cache) stallTimeout() time.Duration {
	if c.StallTimeout > 0 {
		return c.StallTimeout
	}
	return 5 * time.Second
}

type progressWriter struct {
	file  *os.File
	lease *StreamLease
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if n > 0 {
		w.lease.progress()
	}
	return n, err
}

// download fills the .part file. It retries only before the first body
// byte, because restarting after the decoder consumed audio would replay
// part of the track. A mid-stream failure is reported to the follower and
// the AutoDJ skips to the next entry.
func (s *StreamLease) download(ctx context.Context, c *Cache, info SongURL, tmp *os.File, finalPath string, cacheEnabled bool) {
	defer close(s.done)

	var (
		written int64
		sum     []byte
		err     error
	)
	for attempt := 0; attempt < 3; attempt++ {
		written, sum, err = c.download(ctx, info.URL, progressWriter{file: tmp, lease: s})
		if err == nil {
			break
		}
		if ctx.Err() != nil || written > 0 {
			break
		}
		if _, terr := tmp.Seek(0, 0); terr != nil {
			err = terr
			break
		}
		if terr := tmp.Truncate(0); terr != nil {
			err = terr
			break
		}
	}
	if closeErr := tmp.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err == nil && info.Size > 0 && written != info.Size {
		err = fmt.Errorf("short download: got %d bytes, want %d", written, info.Size)
	}
	if err == nil && info.MD5 != "" {
		got := hex.EncodeToString(sum)
		if got != info.MD5 {
			err = fmt.Errorf("md5 mismatch: got %s, want %s", got, info.MD5)
		}
	}
	if err != nil {
		_ = os.Remove(s.tmpPath)
		s.setErr(err)
		return
	}
	if cacheEnabled {
		if err := os.Rename(s.tmpPath, finalPath); err != nil {
			_ = os.Remove(s.tmpPath)
			s.setErr(err)
			return
		}
	}
	s.setErr(nil)
}

func (c *Cache) fetch(ctx context.Context, info SongURL, id int64, cacheDirectory string, cacheEnabled bool) (*Lease, error) {
	dir, err := cacheLocation(cacheDirectory, cacheEnabled)
	if err != nil {
		return nil, err
	}

	ext := mediaExt(info)
	finalPath := filepath.Join(dir, strconv.FormatInt(id, 10)+"."+ext)
	tmp, err := os.CreateTemp(dir, strconv.FormatInt(id, 10)+".*.part")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()

	written, sum, err := c.download(ctx, info.URL, tmp)
	closeErr := tmp.Close()
	if err == nil && closeErr != nil {
		err = closeErr
	}
	if err == nil && info.Size > 0 && written != info.Size {
		err = fmt.Errorf("short download: got %d bytes, want %d", written, info.Size)
	}
	if err == nil && info.MD5 != "" {
		got := hex.EncodeToString(sum)
		if got != info.MD5 {
			err = fmt.Errorf("md5 mismatch: got %s, want %s", got, info.MD5)
		}
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}

	if cacheEnabled {
		if err := os.Rename(tmpPath, finalPath); err != nil {
			_ = os.Remove(tmpPath)
			return nil, err
		}
		return &Lease{Path: finalPath}, nil
	}
	return &Lease{Path: tmpPath, cleanup: func() { _ = os.Remove(tmpPath) }}, nil
}

func (c *Cache) download(ctx context.Context, rawURL string, dst io.Writer) (int64, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", "NeteaseMusic/9.1.65.240927161425(9001065);Dalvik/2.1.0 (Linux; U; Android 14; Pixel 8 Build/UP1A.231005.007)")
	req.Header.Set("Referer", apiHost)

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	h := md5.New()
	n, err := io.Copy(io.MultiWriter(dst, h), resp.Body)
	if err != nil {
		return n, nil, err
	}
	return n, h.Sum(nil), nil
}

func mediaExt(info SongURL) string {
	switch info.Type {
	case "mp3", "flac", "m4a", "aac":
		return info.Type
	}
	if u := info.URL; u != "" {
		if i := strings.IndexByte(u, '?'); i >= 0 {
			u = u[:i]
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(u)), ".")
		if ext == "mp3" || ext == "flac" || ext == "m4a" || ext == "aac" {
			return ext
		}
	}
	return "mp3"
}
