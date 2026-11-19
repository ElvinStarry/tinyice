package netease

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hajimehoshi/go-mp3"
)

func TestOpenStreamFollowsGrowingFileAndCaches(t *testing.T) {
	payload := bytes.Repeat([]byte("stream-cache-"), 32*1024) // 384 KiB
	sum := md5.Sum(payload)
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		half := len(payload) / 2
		_, _ = w.Write(payload[:half])
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = w.Write(payload[half:])
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Cache{HTTP: srv.Client(), StallTimeout: 2 * time.Second}
	lease, err := c.openStream(context.Background(), SongURL{
		URL:  srv.URL,
		Type: "mp3",
		Size: int64(len(payload)),
		MD5:  hex.EncodeToString(sum[:]),
	}, 99, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	first := make([]byte, 100)
	if _, err := io.ReadFull(lease, first); err != nil {
		t.Fatalf("read before download finished: %v", err)
	}
	if !bytes.Equal(first, payload[:len(first)]) {
		t.Fatal("first bytes do not match the source")
	}

	close(release)
	rest, err := io.ReadAll(lease)
	if err != nil {
		t.Fatalf("read to EOF: %v", err)
	}
	got := append(append([]byte(nil), first...), rest...)
	if !bytes.Equal(got, payload) {
		t.Fatalf("streamed %d bytes, want %d", len(got), len(payload))
	}
	if path := c.findCached(99, dir); path == "" {
		t.Fatal("completed stream was not renamed into the cache")
	}
}

func TestOpenStreamReportsDownloadFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 100))
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Cache{HTTP: srv.Client(), StallTimeout: 2 * time.Second}
	lease, err := c.openStream(context.Background(), SongURL{
		URL:  srv.URL,
		Type: "mp3",
		Size: 1000,
	}, 100, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	if _, err := io.ReadAll(lease); err == nil {
		t.Fatal("truncated download was reported as success")
	}
	if path := c.findCached(100, dir); path != "" {
		t.Fatalf("failed download left a cache file at %s", path)
	}
}

func TestOpenStreamStallTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("y"), 10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Cache{HTTP: srv.Client(), StallTimeout: 150 * time.Millisecond}
	lease, err := c.openStream(context.Background(), SongURL{URL: srv.URL, Type: "mp3"}, 101, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	buf := make([]byte, 10)
	if _, err := io.ReadFull(lease, buf); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Read(make([]byte, 1)); !errors.Is(err, ErrStreamStalled) {
		t.Fatalf("read after stall = %v, want ErrStreamStalled", err)
	}
}

func TestOpenStreamCacheHit(t *testing.T) {
	dir := t.TempDir()
	audio := []byte("cached-audio")
	if err := os.WriteFile(filepath.Join(dir, "7.mp3"), audio, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Cache{}
	lease, err := c.OpenStream(context.Background(), nil, 7, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	got, err := io.ReadAll(lease)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, audio) {
		t.Fatalf("cache hit = %q, want %q", got, audio)
	}
}

func TestOpenStreamFeedsMP3DecoderFromPartialFile(t *testing.T) {
	frame := make([]byte, 417) // MPEG-1 Layer III, 128 kbps, 44.1 kHz
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x64})
	payload := bytes.Repeat(frame, 100)
	half := len(payload) / 2

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload[:half])
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write(payload[half:])
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Cache{HTTP: srv.Client(), StallTimeout: 2 * time.Second}
	lease, err := c.openStream(context.Background(), SongURL{
		URL:  srv.URL,
		Type: "mp3",
		Size: int64(len(payload)),
	}, 55, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	dec, err := mp3.NewDecoder(lease)
	if err != nil {
		t.Fatalf("decoder on growing file: %v", err)
	}
	pcm, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decode growing file: %v", err)
	}
	if len(pcm) == 0 {
		t.Fatal("decoder produced no PCM")
	}
	if path := c.findCached(55, dir); path == "" {
		t.Fatal("decoded stream was not committed to the cache")
	}
}

func TestOpenStreamCancelUnblocksRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("z"), 10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	c := &Cache{HTTP: srv.Client(), StallTimeout: 30 * time.Second}
	lease, err := c.openStream(ctx, SongURL{URL: srv.URL, Type: "mp3"}, 102, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	first := make([]byte, 10)
	if _, err := io.ReadFull(lease, first); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := lease.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel = %v, want context.Canceled", err)
	}
}
