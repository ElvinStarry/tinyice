package netease

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// DefaultCheckInterval is how often the stored cookie is re-validated.
// A network error leaves the previous state untouched; only an explicit
// "not logged in" answer marks the cookie expired, so a brief outage does
// not make the AutoDJ stop using a perfectly good session.
const DefaultCheckInterval = 10 * time.Minute

// Session owns one NetEase account for the process. It stores the cookie
// in memory and periodically verifies it.
type Session struct {
	client *Client

	mu        sync.RWMutex
	state     LoginState
	nickname  string
	checkedAt time.Time
	lastErr   string
}

// NewSession wraps a client and restores the persisted cookie, if any.
func NewSession(httpClient *http.Client, musicU string) *Session {
	return &Session{
		client: NewClient(httpClient, musicU),
		state:  LoginUnknown,
	}
}

// Client returns the shared API client.
func (s *Session) Client() *Client { return s.client }

// MusicUSet reports whether a cookie is currently configured.
func (s *Session) MusicUSet() bool { return s.client.MusicU() != "" }

// SetLogin installs a cookie that a QR login just produced.
func (s *Session) SetLogin(musicU, nickname string) {
	s.client.SetMusicU(musicU)
	s.mu.Lock()
	s.state = LoginValid
	s.nickname = nickname
	s.checkedAt = time.Now()
	s.lastErr = ""
	s.mu.Unlock()
}

// Clear forgets the cookie. The operator can scan a new QR afterwards.
func (s *Session) Clear() {
	s.client.SetMusicU("")
	s.mu.Lock()
	s.state = LoginInvalid
	s.nickname = ""
	s.checkedAt = time.Now()
	s.lastErr = ""
	s.mu.Unlock()
}

// Status is a snapshot for the admin API.
type Status struct {
	State     LoginState
	Nickname  string
	CheckedAt time.Time
	LastError string
	MusicUSet bool
}

// Status returns a copy of the current login state.
func (s *Session) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Status{
		State:     s.state,
		Nickname:  s.nickname,
		CheckedAt: s.checkedAt,
		LastError: s.lastErr,
		MusicUSet: s.client.MusicU() != "",
	}
}

// Check performs one validation round and updates the cached status.
func (s *Session) Check(ctx context.Context) (bool, error) {
	if !s.MusicUSet() {
		s.mu.Lock()
		s.state = LoginUnknown
		s.checkedAt = time.Now()
		s.lastErr = ""
		s.mu.Unlock()
		return false, nil
	}
	valid, nickname, err := s.client.LoginStatus(ctx)
	now := time.Now()
	s.mu.Lock()
	s.checkedAt = now
	if err != nil {
		s.lastErr = err.Error()
		s.mu.Unlock()
		return false, err
	}
	s.lastErr = ""
	if valid {
		s.state = LoginValid
		if nickname != "" {
			s.nickname = nickname
		}
	} else {
		s.state = LoginInvalid
	}
	s.mu.Unlock()
	return valid, nil
}

// Watch validates the cookie immediately, then every interval until done
// closes. Repeated failures are visible through Status.
func (s *Session) Watch(done <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	check := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = s.Check(ctx)
	}
	check()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			check()
		}
	}
}
