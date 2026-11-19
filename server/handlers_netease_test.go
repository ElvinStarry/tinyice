package server

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/DatanoiseTV/tinyice/config"
)

func TestNeteaseCacheJobProgress(t *testing.T) {
	s := &Server{neteaseCacheJobs: map[string]*neteaseCacheJob{
		"/ncm": {Mount: "/ncm", Total: 3, Active: true},
	}}
	s.runNeteaseCacheJobWith("/ncm", []int64{1, 2, 3}, func(_ context.Context, id int64) error {
		if id == 2 {
			return errors.New("download failed")
		}
		return nil
	})
	job, ok := s.neteaseCacheJobSnapshot("/ncm")
	if !ok {
		t.Fatal("job disappeared")
	}
	if job.Active {
		t.Fatal("job still active after all downloads finished")
	}
	if job.Done != 2 || job.Failed != 1 {
		t.Fatalf("progress = %d done / %d failed, want 2/1", job.Done, job.Failed)
	}
	if job.Error == "" {
		t.Fatal("failed download did not record an error")
	}
	if job.FinishedAt == 0 {
		t.Fatal("FinishedAt was not set")
	}
}

func TestNeteaseCacheStatePersistsAndInterruptsActiveJob(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "tinyice.json")
	s := &Server{Config: &config.Config{ConfigPath: cfgPath}}
	s.neteaseCacheMu.Lock()
	s.loadNeteaseCacheStateLocked()
	s.neteaseCacheJobs = map[string]*neteaseCacheJob{
		"/ncm": {
			Mount:  "/ncm",
			Total:  3,
			Done:   1,
			Active: true,
			IDs:    []int64{10, 20, 30},
		},
	}
	s.neteaseCacheQueued = map[string][]int64{"/ncm": {40}}
	s.persistNeteaseCacheStateLocked()
	s.neteaseCacheMu.Unlock()

	restored := &Server{Config: &config.Config{ConfigPath: cfgPath}}
	restored.neteaseCacheMu.Lock()
	restored.loadNeteaseCacheStateLocked()
	job := restored.neteaseCacheJobs["/ncm"]
	queued := restored.neteaseCacheQueued["/ncm"]
	restored.neteaseCacheMu.Unlock()

	if job == nil {
		t.Fatal("job state was not restored")
	}
	if job.Active {
		t.Fatal("restored job remained active")
	}
	if job.Error != "interrupted by server restart" {
		t.Fatalf("error = %q, want interrupted marker", job.Error)
	}
	if !slices.Equal(job.IDs, []int64{10, 20, 30}) {
		t.Fatalf("ids = %v", job.IDs)
	}
	if !slices.Equal(queued, []int64{10, 20, 30, 40}) {
		t.Fatalf("queued = %v", queued)
	}
}

func TestAutoCacheQueuesNewIDsWhileBatchRuns(t *testing.T) {
	s := &Server{
		Config: &config.Config{ConfigPath: filepath.Join(t.TempDir(), "tinyice.json")},
		neteaseCacheJobs: map[string]*neteaseCacheJob{
			"/ncm": {Mount: "/ncm", Total: 1, Active: true, IDs: []int64{1}},
		},
		neteaseCacheQueued: make(map[string][]int64),
	}
	s.startOrQueueNeteaseCache("/ncm", []int64{1, 2, 2, 3})
	s.neteaseCacheMu.Lock()
	queued := append([]int64(nil), s.neteaseCacheQueued["/ncm"]...)
	s.neteaseCacheMu.Unlock()
	if !slices.Equal(queued, []int64{2, 3}) {
		t.Fatalf("queued = %v, want [2 3]", queued)
	}
}
