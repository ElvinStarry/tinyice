package netease

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonClient(bodies ...string) (*Client, *int) {
	calls := 0
	c := &Client{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := bodies[len(bodies)-1]
		if calls < len(bodies) {
			body = bodies[calls]
		}
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}}
	return c, &calls
}

func TestSongURLUsesBestDowngradedFallback(t *testing.T) {
	c, calls := jsonClient(`{"code":200,"data":[{"url":"https://cdn/a.mp3","type":"mp3","size":123,"md5":"ABCDEF","level":"standard","code":200}]}`)
	got, err := c.SongURL(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != len(songURLQualities) {
		t.Fatalf("requests = %d, want %d", *calls, len(songURLQualities))
	}
	if got.Level != "standard" || got.URL == "" || got.MD5 != "abcdef" {
		t.Fatalf("SongURL = %+v", got)
	}
}

func TestSongURLPrefersHiResOverDowngradedExhigh(t *testing.T) {
	c, calls := jsonClient(
		`{"code":200,"data":[{"url":"https://cdn/a.mp3","type":"mp3","size":123,"level":"standard","code":200}]}`,
		`{"code":200,"data":[{"url":"https://cdn/b.flac","type":"flac","size":456,"level":"hires","code":200}]}`,
	)
	got, err := c.SongURL(context.Background(), 46)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("requests = %d, want 2", *calls)
	}
	if got.Level != "hires" || got.Type != "flac" || got.URL == "" {
		t.Fatalf("SongURL = %+v", got)
	}
}

func TestSongURLFallsBackToHigher(t *testing.T) {
	c, calls := jsonClient(
		`{"code":200,"data":[{"url":"","level":"exhigh","code":404}]}`,
		`{"code":200,"data":[{"url":"https://cdn/b.mp3","type":"mp3","size":456,"level":"higher","code":200}]}`,
	)
	got, err := c.SongURL(context.Background(), 43)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 3 {
		t.Fatalf("requests = %d, want 3", *calls)
	}
	if got.Level != "higher" || got.URL == "" {
		t.Fatalf("SongURL = %+v", got)
	}
}

func TestSongURLFallsBackToHiRes(t *testing.T) {
	c, calls := jsonClient(
		`{"code":200,"data":[{"url":"","level":"exhigh","code":404}]}`,
		`{"code":200,"data":[{"url":"https://cdn/c.flac","type":"flac","size":789,"level":"hires","code":200}]}`,
	)
	got, err := c.SongURL(context.Background(), 45)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("requests = %d, want 2", *calls)
	}
	if got.Level != "hires" || got.Type != "flac" || got.URL == "" {
		t.Fatalf("SongURL = %+v", got)
	}
}

func TestSongURLReportsUnavailableAfterAllLevels(t *testing.T) {
	c, calls := jsonClient(`{"code":200,"data":[{"url":"","code":404}]}`)
	_, err := c.SongURL(context.Background(), 44)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if *calls != len(songURLQualities) {
		t.Fatalf("requests = %d, want %d", *calls, len(songURLQualities))
	}
}
