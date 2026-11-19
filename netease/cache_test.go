package netease

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func audioServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(body)
	}))
}

func TestCachePersistsByMusicID(t *testing.T) {
	body := []byte("0123456789abcdef")
	sum := md5.Sum(body)
	ts := audioServer(t, body)
	defer ts.Close()

	dir := t.TempDir()
	c := &Cache{HTTP: ts.Client()}
	info := SongURL{URL: ts.URL + "/song.mp3", Type: "mp3", Size: int64(len(body)), MD5: hex.EncodeToString(sum[:])}

	lease, err := c.fetch(context.Background(), info, 424242, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "424242.mp3")
	if lease.Path != want {
		t.Fatalf("path = %q, want %q", lease.Path, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("content = %q, want %q", got, body)
	}
	lease.Release() // cached files survive release
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("cached file removed on release: %v", err)
	}
	if cached := c.findCached(424242, dir); cached != want {
		t.Fatalf("findCached = %q, want %q", cached, want)
	}
	// A partial file from a crashed download must not count as a hit.
	if err := os.WriteFile(filepath.Join(dir, "424243.mp3.part"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cached := c.findCached(424243, dir); cached != "" {
		t.Fatalf("findCached returned a .part file: %q", cached)
	}
}

func TestCacheDisabledDeletesTempFile(t *testing.T) {
	body := []byte("temporary audio")
	ts := audioServer(t, body)
	defer ts.Close()

	c := &Cache{HTTP: ts.Client()}
	lease, err := c.fetch(context.Background(), SongURL{URL: ts.URL + "/x.mp3", Type: "mp3"}, 7, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	path := lease.Path
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("temp file missing before release: %v", err)
	}
	lease.Release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file still exists after release: %v", err)
	}
}

func TestCacheRejectsBadMD5(t *testing.T) {
	body := []byte("corrupt")
	ts := audioServer(t, body)
	defer ts.Close()

	c := &Cache{HTTP: ts.Client()}
	_, err := c.fetch(context.Background(), SongURL{
		URL: ts.URL + "/x.mp3", Type: "mp3",
		MD5: "00000000000000000000000000000000",
	}, 9, t.TempDir(), true)
	if err == nil {
		t.Fatal("expected md5 mismatch")
	}
}

func TestCacheRemoveLeavesPartialFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"7.mp3", "7.flac", "7.123.part"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := &Cache{}
	if !c.IsCached(7, dir) {
		t.Fatal("IsCached missed the complete file")
	}
	if err := c.Remove(7, dir); err != nil {
		t.Fatal(err)
	}
	if c.IsCached(7, dir) {
		t.Fatal("complete cache files were not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "7.123.part")); err != nil {
		t.Fatal("partial file was removed")
	}
}
