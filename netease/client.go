package netease

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ErrUnavailable means NetEase answered successfully but has no playable
// URL for the track at any quality level (VIP-only, region locked, removed).
var ErrUnavailable = errors.New("netease: no playable url")

const (
	apiHost = "https://music.163.com"

	// Exhigh is the 320 kbps tier and the preferred request. A lower
	// level returned by the server is accepted so an account without
	// exhigh rights can still play the track. Some catalogue entries
	// expose hires but not exhigh, so hires is the next request.
	qualityExhigh   = "exhigh"
	qualityHiRes    = "hires"
	qualityHigher   = "higher"
	qualityStandard = "standard"
)

type qualityRequest struct {
	level      string
	encodeType string
}

var songURLQualities = []qualityRequest{
	{level: qualityExhigh, encodeType: "mp3"},
	{level: qualityHiRes, encodeType: "flac"},
	{level: qualityHigher, encodeType: "mp3"},
	{level: qualityStandard, encodeType: "mp3"},
}

// qualityRanks only answers "is this better than the level we asked for".
// Unknown levels are accepted as-is: NetEase adds tiers over time and a URL
// is more useful than a false negative.
var qualityRanks = map[string]int{
	"standard": 1,
	"higher":   2,
	"exhigh":   3,
	"lossless": 4,
	"hires":    5,
	"jyeffect": 6,
	"sky":      7,
	"jymaster": 8,
}

func qualityRank(level string) int {
	return qualityRanks[level]
}

func qualitySatisfies(got, requested string) bool {
	if got == "" {
		return true
	}
	gotRank, gotKnown := qualityRanks[got]
	wantRank, wantKnown := qualityRanks[requested]
	if !gotKnown || !wantKnown {
		return true
	}
	return gotRank >= wantRank
}

func betterQuality(a, b SongURL) SongURL {
	if a.URL == "" {
		return b
	}
	if b.URL == "" {
		return a
	}
	if qualityRank(b.Level) > qualityRank(a.Level) {
		return b
	}
	return a
}

// Client is a minimal NetEase web API client. It is safe for concurrent
// use; the cookie can be replaced while requests are in flight.
type Client struct {
	http *http.Client

	mu       sync.RWMutex
	musicU   string
	deviceID string
}

// NewClient creates a client. httpClient may be nil, in which case
// http.DefaultClient is used. Call SetMusicU after a successful QR login.
func NewClient(httpClient *http.Client, musicU string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		http:     httpClient,
		musicU:   musicU,
		deviceID: randomDeviceID(),
	}
}

func randomDeviceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A stable non-random value only costs request de-duplication on
		// the server side; it never authenticates anything.
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// SetMusicU replaces the login cookie. Empty clears it (anonymous).
func (c *Client) SetMusicU(musicU string) {
	c.mu.Lock()
	c.musicU = musicU
	c.mu.Unlock()
}

// MusicU returns the current cookie value (empty when anonymous).
func (c *Client) MusicU() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.musicU
}

// QRLoginKey requests a fresh QR login key.
func (c *Client) QRLoginKey(ctx context.Context) (string, error) {
	var out qrKeyResponse
	if err := c.call(ctx, "/api/login/qrcode/unikey", map[string]any{"type": "3"}, &out); err != nil {
		return "", err
	}
	if out.Code != 200 || out.Unikey == "" {
		return "", fmt.Errorf("netease: qr key rejected (code %d)", out.Code)
	}
	return out.Unikey, nil
}

// QRLoginCheck polls one QR login key. musicU is only non-empty on
// QRSuccess.
func (c *Client) QRLoginCheck(ctx context.Context, key string) (state QRState, nickname, musicU string, err error) {
	var out qrCheckResponse
	header, err := c.callWithHeader(ctx, "/api/login/qrcode/client/login", map[string]any{"key": key, "type": "3"}, &out)
	if err != nil {
		return 0, "", "", err
	}
	switch QRState(out.Code) {
	case QRExpired, QRWaiting, QRScanned:
		return QRState(out.Code), out.Nickname, "", nil
	case QRSuccess:
		for _, cookie := range header.Values("Set-Cookie") {
			first, _, _ := strings.Cut(cookie, ";")
			name, value, ok := strings.Cut(strings.TrimSpace(first), "=")
			if ok && name == "MUSIC_U" && value != "" {
				return QRSuccess, out.Nickname, value, nil
			}
		}
		return 0, "", "", fmt.Errorf("netease: qr login succeeded without MUSIC_U cookie")
	default:
		return 0, "", "", fmt.Errorf("netease: qr check returned code %d %s", out.Code, out.Message)
	}
}

// LoginStatus validates the stored cookie and returns the account
// nickname when the cookie is accepted.
func (c *Client) LoginStatus(ctx context.Context) (valid bool, nickname string, err error) {
	var out loginStatusResponse
	if err := c.call(ctx, "/api/w/nuser/account/get", map[string]any{}, &out); err != nil {
		return false, "", err
	}
	if out.Code != 200 {
		return false, "", nil
	}
	if out.Profile == nil || out.Profile.UserID == 0 {
		return false, "", nil
	}
	return true, out.Profile.Nickname, nil
}

// Playlist returns the complete track-id order of one playlist. NetEase
// truncates the inline "tracks" array for large playlists, so callers
// resolve IDs through SongDetails.
func (c *Client) Playlist(ctx context.Context, id int64) (Playlist, error) {
	var out playlistResponse
	if err := c.call(ctx, "/api/v6/playlist/detail", map[string]any{
		"id": fmt.Sprintf("%d", id),
		"n":  "0",
		"s":  "0",
		"t":  "0",
	}, &out); err != nil {
		return Playlist{}, err
	}
	if out.Code != 200 || out.Playlist == nil {
		return Playlist{}, fmt.Errorf("netease: playlist %d unavailable (code %d)", id, out.Code)
	}
	pl := Playlist{ID: out.Playlist.ID, Name: out.Playlist.Name}
	seen := make(map[int64]struct{}, len(out.Playlist.TrackIDs))
	for _, t := range out.Playlist.TrackIDs {
		if t.ID == 0 {
			continue
		}
		if _, ok := seen[t.ID]; ok {
			continue
		}
		seen[t.ID] = struct{}{}
		pl.TrackIDs = append(pl.TrackIDs, t.ID)
	}
	if len(pl.TrackIDs) == 0 {
		for _, t := range out.Playlist.Tracks {
			if t.ID == 0 {
				continue
			}
			if _, ok := seen[t.ID]; ok {
				continue
			}
			seen[t.ID] = struct{}{}
			pl.TrackIDs = append(pl.TrackIDs, t.ID)
		}
	}
	return pl, nil
}

// SongDetails resolves metadata for up to a few hundred IDs per call.
func (c *Client) SongDetails(ctx context.Context, ids []int64) ([]Track, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	entries := make([]map[string]int64, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, map[string]int64{"id": id})
	}
	body, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	var out songDetailResponse
	if err := c.call(ctx, "/api/v3/song/detail", map[string]any{"c": string(body)}, &out); err != nil {
		return nil, err
	}
	if out.Code != 200 {
		return nil, fmt.Errorf("netease: song detail rejected (code %d)", out.Code)
	}
	tracks := make([]Track, 0, len(out.Songs))
	for _, s := range out.Songs {
		tracks = append(tracks, s.track())
	}
	return tracks, nil
}

// SongURL asks for the best playback URL the account can use. It requests
// exhigh (320 kbps), then hires (FLAC), and retries lower levels when those
// are unavailable. A silent downgrade is kept as a fallback while hires is
// probed, because some tracks expose hires without exhigh.
func (c *Client) SongURL(ctx context.Context, id int64) (SongURL, error) {
	ids, _ := json.Marshal([]string{fmt.Sprintf("%d", id)})
	var fallback SongURL
	var lastCode int
	for _, quality := range songURLQualities {
		var out songURLResponse
		if err := c.call(ctx, "/api/song/enhance/player/url/v1", map[string]any{
			"ids":        string(ids),
			"level":      quality.level,
			"encodeType": quality.encodeType,
		}, &out); err != nil {
			return SongURL{}, err
		}
		if out.Code != 200 || len(out.Data) == 0 {
			lastCode = out.Code
			continue
		}
		d := out.Data[0]
		if d.URL == "" {
			lastCode = d.Code
			continue
		}
		candidate := SongURL{
			URL:   d.URL,
			Type:  strings.ToLower(d.Type),
			Size:  d.Size,
			MD5:   strings.ToLower(d.MD5),
			Level: d.Level,
			Code:  d.Code,
		}
		// A silent downgrade is a playable fallback, but the next request
		// may still find hires (some tracks expose hires without exhigh).
		if qualitySatisfies(candidate.Level, quality.level) {
			return candidate, nil
		}
		fallback = betterQuality(fallback, candidate)
	}
	if fallback.URL != "" {
		return fallback, nil
	}
	return SongURL{}, fmt.Errorf("%w: id %d code %d", ErrUnavailable, id, lastCode)
}

// call posts one eapi request and decodes a plain JSON body.
func (c *Client) call(ctx context.Context, path string, payload any, out any) error {
	_, err := c.callWithHeader(ctx, path, payload, out)
	return err
}

// callWithHeader is call plus the response headers, which the QR success
// case needs for Set-Cookie.
func (c *Client) callWithHeader(ctx context.Context, path string, payload any, out any) (http.Header, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("netease: marshal %s: %w", path, err)
	}
	form := eapiParams(path, body)
	endpoint := strings.TrimPrefix(path, "/api")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiHost+"/eapi"+endpoint, bytes.NewBufferString(form))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "NeteaseMusic/9.1.65.240927161425(9001065);Dalvik/2.1.0 (Linux; U; Android 14; Pixel 8 Build/UP1A.231005.007)")
	req.Header.Set("Referer", apiHost)
	req.Header.Set("Cookie", c.cookieHeader())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("netease: %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("netease: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Header, fmt.Errorf("netease: %s: http %d", path, resp.StatusCode)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return resp.Header, fmt.Errorf("netease: %s: empty response", path)
	}
	if raw[0] != '{' && raw[0] != '[' {
		plain, derr := eapiDecrypt(raw)
		if derr != nil {
			return resp.Header, fmt.Errorf("netease: %s: undecodable response", path)
		}
		raw = plain
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.Header, fmt.Errorf("netease: decode %s: %w", path, err)
	}
	return resp.Header, nil
}

func (c *Client) cookieHeader() string {
	c.mu.RLock()
	musicU := c.musicU
	deviceID := c.deviceID
	c.mu.RUnlock()

	// The mobile protocol is what the eapi signing targets; without these
	// headers the server treats the request as a desktop browser session
	// and some endpoints answer with an HTML error page.
	parts := []string{
		"os=android",
		"appver=9.1.65",
		"osver=14",
		"deviceId=" + url.QueryEscape(deviceID),
	}
	if musicU != "" {
		parts = append(parts, "MUSIC_U="+url.QueryEscape(musicU))
	}
	return strings.Join(parts, "; ")
}
