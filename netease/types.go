package netease

import (
	"strings"
	"time"
)

// QRState is the code returned by the QR login poll endpoint.
type QRState int

const (
	QRExpired QRState = 800
	QRWaiting QRState = 801
	QRScanned QRState = 802
	QRSuccess QRState = 803
)

// LoginState is the last known state of the stored MUSIC_U cookie.
type LoginState int

const (
	LoginUnknown LoginState = iota
	LoginValid
	LoginInvalid
)

func (s LoginState) String() string {
	switch s {
	case LoginValid:
		return "valid"
	case LoginInvalid:
		return "expired"
	default:
		return "unknown"
	}
}

// Track is one song from a playlist or a detail lookup.
type Track struct {
	ID       int64
	Name     string
	Artists  []string
	Album    string
	Duration time.Duration
}

// DisplayTitle is the "Artist - Title" form used by the AutoDJ playlist
// and by the stream metadata.
func (t Track) DisplayTitle() string {
	artist := strings.Join(t.Artists, " / ")
	switch {
	case artist != "" && t.Name != "":
		return artist + " - " + t.Name
	case t.Name != "":
		return t.Name
	case artist != "":
		return artist
	default:
		return "NetEase track"
	}
}

// Playlist is the subset of a NetEase playlist an AutoDJ needs.
type Playlist struct {
	ID       int64
	Name     string
	TrackIDs []int64
}

// SongURL is a playable URL returned by the quality lookup.
type SongURL struct {
	URL   string
	Type  string
	Size  int64
	MD5   string
	Level string
	Code  int
}

type qrKeyResponse struct {
	Code   int    `json:"code"`
	Unikey string `json:"unikey"`
}

type qrCheckResponse struct {
	Code     int    `json:"code"`
	Nickname string `json:"nickname"`
	Message  string `json:"message"`
}

type loginStatusResponse struct {
	Code    int `json:"code"`
	Profile *struct {
		UserID   int64  `json:"userId"`
		Nickname string `json:"nickname"`
	} `json:"profile"`
}

type playlistResponse struct {
	Code     int `json:"code"`
	Playlist *struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		TrackIDs []struct {
			ID int64 `json:"id"`
		} `json:"trackIds"`
		Tracks []songNode `json:"tracks"`
	} `json:"playlist"`
}

type songDetailResponse struct {
	Code  int        `json:"code"`
	Songs []songNode `json:"songs"`
}

type songNode struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	DT   int64  `json:"dt"`
	Ar   []struct {
		Name string `json:"name"`
	} `json:"ar"`
	Al struct {
		Name string `json:"name"`
	} `json:"al"`
}

func (n songNode) track() Track {
	t := Track{ID: n.ID, Name: n.Name, Album: n.Al.Name, Duration: time.Duration(n.DT) * time.Millisecond}
	for _, a := range n.Ar {
		if a.Name != "" {
			t.Artists = append(t.Artists, a.Name)
		}
	}
	return t
}

type songURLResponse struct {
	Code int `json:"code"`
	Data []struct {
		ID    int64  `json:"id"`
		URL   string `json:"url"`
		Type  string `json:"type"`
		Size  int64  `json:"size"`
		MD5   string `json:"md5"`
		Level string `json:"level"`
		Code  int    `json:"code"`
	} `json:"data"`
}
