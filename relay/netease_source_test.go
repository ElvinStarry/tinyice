package relay

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/DatanoiseTV/tinyice/config"
	"github.com/DatanoiseTV/tinyice/netease"
)

type fakeCatalog struct {
	playlists map[int64]netease.Playlist
	tracks    map[int64]netease.Track
	failID    int64
}

func (f *fakeCatalog) Playlist(_ context.Context, id int64) (netease.Playlist, error) {
	if id == f.failID {
		return netease.Playlist{}, errors.New("network down")
	}
	pl, ok := f.playlists[id]
	if !ok {
		return netease.Playlist{}, errors.New("missing playlist")
	}
	return pl, nil
}

func (f *fakeCatalog) SongDetails(_ context.Context, ids []int64) ([]netease.Track, error) {
	out := make([]netease.Track, 0, len(ids))
	for _, id := range ids {
		if t, ok := f.tracks[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func newNeteaseTestStreamer() *Streamer {
	return &Streamer{
		Name:             "ncm",
		OutputMount:      "/ncm",
		Source:           "netease",
		NeteasePlaylists: []int64{1, 2},
		Playlist:         []PlaylistSong{{Path: neteasePath(10), ID: 5}},
		NextID:           6,
		neteaseTracks:    make(map[int64]netease.Track),
		idleCh:           make(chan string, 10),
		stateCh:          make(chan struct{}, 1),
	}
}

func TestNeteasePlaylistIDsDedupesAndSkipsLocalPaths(t *testing.T) {
	s := newNeteaseTestStreamer()
	s.Playlist = []PlaylistSong{
		{Path: neteasePath(10)},
		{Path: "/music/local.mp3"},
		{Path: neteasePath(20)},
		{Path: neteasePath(10)},
		{Path: "netease:not-a-number"},
	}
	if got, want := s.NeteasePlaylistIDs(), []int64{10, 20}; !slices.Equal(got, want) {
		t.Fatalf("NeteasePlaylistIDs = %v, want %v", got, want)
	}
}

func TestRefreshNeteaseDedupesAndPreservesIDs(t *testing.T) {
	s := newNeteaseTestStreamer()
	fake := &fakeCatalog{
		playlists: map[int64]netease.Playlist{
			1: {ID: 1, TrackIDs: []int64{10, 20}},
			2: {ID: 2, TrackIDs: []int64{20, 30}},
		},
		tracks: map[int64]netease.Track{
			10: {ID: 10, Name: "ten"},
			20: {ID: 20, Name: "twenty"},
			30: {ID: 30, Name: "thirty"},
		},
	}
	if err := (&StreamerManager{}).refreshNeteaseWith(context.Background(), s, fake); err != nil {
		t.Fatal(err)
	}
	if len(s.Playlist) != 3 {
		t.Fatalf("playlist length = %d, want 3 (deduplicated): %+v", len(s.Playlist), s.Playlist)
	}
	if s.Playlist[0].Path != neteasePath(10) || s.Playlist[0].ID != 5 {
		t.Fatalf("existing entry lost its stable id: %+v", s.Playlist[0])
	}
	if s.Playlist[1].Path != neteasePath(20) || s.Playlist[2].Path != neteasePath(30) {
		t.Fatalf("order not preserved: %+v", s.Playlist)
	}
	if s.NextID != 8 {
		t.Fatalf("NextID = %d, want 8", s.NextID)
	}
	if len(s.neteaseTracks) != 3 {
		t.Fatalf("metadata map = %d entries, want 3", len(s.neteaseTracks))
	}
}

func TestRefreshNeteaseIsAllOrNothing(t *testing.T) {
	s := newNeteaseTestStreamer()
	before := append([]PlaylistSong(nil), s.Playlist...)
	fake := &fakeCatalog{
		playlists: map[int64]netease.Playlist{1: {ID: 1, TrackIDs: []int64{99}}},
		tracks:    map[int64]netease.Track{99: {ID: 99, Name: "new"}},
		failID:    2,
	}
	if err := (&StreamerManager{}).refreshNeteaseWith(context.Background(), s, fake); err == nil {
		t.Fatal("expected the refresh to fail when one playlist is unavailable")
	}
	if len(s.Playlist) != len(before) || s.Playlist[0].Path != before[0].Path {
		t.Fatalf("failed refresh mutated the playlist: %+v", s.Playlist)
	}
	if len(s.neteaseTracks) != 0 {
		t.Fatalf("failed refresh stored partial metadata: %+v", s.neteaseTracks)
	}
}

func TestNextNeteaseMusicID(t *testing.T) {
	s := &Streamer{
		Playlist: []PlaylistSong{
			{Path: neteasePath(1)},
			{Path: neteasePath(2)},
		},
		Loop: true,
	}
	if id, ok := s.nextNeteaseMusicID(neteasePath(1)); !ok || id != 2 {
		t.Fatalf("next after first = %d/%v, want 2/true", id, ok)
	}
	if id, ok := s.nextNeteaseMusicID(neteasePath(2)); !ok || id != 1 {
		t.Fatalf("next after last with loop = %d/%v, want 1/true", id, ok)
	}
	s.Loop = false
	if _, ok := s.nextNeteaseMusicID(neteasePath(2)); ok {
		t.Fatal("next after the last track must not predict without loop")
	}
	s.Shuffle = true
	if _, ok := s.nextNeteaseMusicID(neteasePath(1)); ok {
		t.Fatal("shuffle has no deterministic next track")
	}
	s.Shuffle = false
	s.Queue = []string{"/tmp/local.mp3"}
	if _, ok := s.nextNeteaseMusicID(neteasePath(1)); ok {
		t.Fatal("a local queue entry needs no NetEase prefetch")
	}
	s.Queue = []string{neteasePath(9)}
	if id, ok := s.nextNeteaseMusicID(neteasePath(1)); !ok || id != 9 {
		t.Fatalf("queued NetEase track = %d/%v, want 9/true", id, ok)
	}
}

func TestPrefetchLeaseHandoff(t *testing.T) {
	sm := NewStreamerManager(NewRelay(false, nil), nil, nil)
	path := writeTestFile(t, "prefetch.mp3", []byte("temporary"))

	sm.storePrefetch(42, &netease.Lease{Path: path})
	got := sm.takePrefetch(42)
	if got == nil || got.Path != path {
		t.Fatalf("takePrefetch = %+v, want the stored lease", got)
	}
	if again := sm.takePrefetch(42); again != nil {
		t.Fatalf("second takePrefetch = %+v, want nil", again)
	}
}

// A prefetched lease is consumed before the cache lookup, so the next
// track starts immediately even when caching is disabled.
func TestNeteaseStreamUsesPrefetchedFile(t *testing.T) {
	const id int64 = 54321
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x64})
	path := writeTestFile(t, "54321.mp3", bytes.Repeat(frame, 20))

	cacheOn := false
	cfg := &config.Config{NetEase: &config.NetEaseConfig{CacheEnabled: &cacheOn, CacheDir: t.TempDir()}}
	session := netease.NewSession(nil, "")
	sm := NewStreamerManager(NewRelay(false, nil), cfg, session)
	sm.storePrefetch(id, &netease.Lease{Path: path})

	s := newNeteaseTestStreamer()
	s.OutputMount = "/ncm-prefetch"
	s.Playlist = nil
	s.neteaseTracks = map[int64]netease.Track{
		id: {ID: id, Name: "prefetched", Artists: []string{"artist"}, Duration: 522 * time.Millisecond},
	}
	if err := sm.streamFile(context.Background(), s, neteasePath(id), -1, -1, 0); err != nil {
		t.Fatal(err)
	}
	if s.CurrentTitle != "prefetched" {
		t.Fatalf("title = %q, want prefetched", s.CurrentTitle)
	}
	if sm.takePrefetch(id) != nil {
		t.Fatal("prefetched lease was not consumed")
	}
}

// A cached file keyed by music id is enough to play a NetEase track with
// no network at all. This exercises the same streamFile path the AutoDJ
// uses, including metadata and the local file lease.
func TestNeteaseStreamUsesCachedFile(t *testing.T) {
	const id int64 = 12345
	frame := make([]byte, 417) // MPEG-1 Layer III, 128 kbps, 44.1 kHz
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x64})
	audio := bytes.Repeat(frame, 50)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "12345.mp3"), audio, 0o600); err != nil {
		t.Fatal(err)
	}
	cacheOn := true
	cfg := &config.Config{NetEase: &config.NetEaseConfig{CacheEnabled: &cacheOn, CacheDir: dir}}
	session := netease.NewSession(nil, "")
	sm := NewStreamerManager(NewRelay(false, nil), cfg, session)

	s := newNeteaseTestStreamer()
	s.OutputMount = "/ncm-stream"
	s.NeteasePlaylists = nil
	s.Playlist = nil
	s.neteaseTracks = map[int64]netease.Track{
		id: {ID: id, Name: "cached", Artists: []string{"artist"}, Album: "album", Duration: 1306 * time.Millisecond},
	}
	if err := sm.streamFile(context.Background(), s, neteasePath(id), -1, -1, 0); err != nil {
		t.Fatal(err)
	}
	if s.CurrentTitle != "cached" || s.CurrentArtist != "artist" {
		t.Fatalf("metadata = %q / %q, want cached / artist", s.CurrentTitle, s.CurrentArtist)
	}
	if s.CurrentFileDuration <= 0 {
		t.Fatalf("duration = %v, want the file estimate", s.CurrentFileDuration)
	}
	if _, err := os.Stat(filepath.Join(dir, "12345.mp3")); err != nil {
		t.Fatalf("cached file disappeared: %v", err)
	}
}

func TestRefreshNeteaseReportsAddedAndRemoved(t *testing.T) {
	s := newNeteaseTestStreamer()
	s.Playlist = []PlaylistSong{
		{Path: neteasePath(10), ID: 1},
		{Path: neteasePath(20), ID: 2},
	}
	s.NeteasePlaylists = []int64{1}
	fake := &fakeCatalog{
		playlists: map[int64]netease.Playlist{1: {ID: 1, TrackIDs: []int64{20, 30}}},
		tracks: map[int64]netease.Track{
			20: {ID: 20, Name: "twenty"},
			30: {ID: 30, Name: "thirty"},
		},
	}
	sm := &StreamerManager{}
	var added, removed []int64
	sm.OnNeteasePlaylistChanged = func(_ string, a, r []int64) {
		added, removed = a, r
	}
	if err := sm.refreshNeteaseWith(context.Background(), s, fake); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(added, []int64{30}) {
		t.Fatalf("added = %v, want [30]", added)
	}
	if !slices.Equal(removed, []int64{10}) {
		t.Fatalf("removed = %v, want [10]", removed)
	}
}
