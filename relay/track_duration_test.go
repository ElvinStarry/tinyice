package relay

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEstimateWavDuration(t *testing.T) {
	// 8000 Hz mono 16-bit, 16000 bytes of data = 1 second.
	path := writeTestFile(t, "one.wav", makeWavHeader(1, 8000, 16, 16000))
	got, err := estimateAudioDuration(path)
	if err != nil {
		t.Fatal(err)
	}
	if got < 900*time.Millisecond || got > 1100*time.Millisecond {
		t.Fatalf("duration = %v, want ~1s", got)
	}
}

func TestEstimateMP3CBRDuration(t *testing.T) {
	// MPEG-1 Layer III, 128 kbps, 44.1 kHz. At 32000 bytes the CBR
	// estimate is exactly two seconds.
	data := make([]byte, 32000)
	copy(data, []byte{0xFF, 0xFB, 0x90, 0x64})
	path := writeTestFile(t, "two.mp3", data)
	got, err := estimateAudioDuration(path)
	if err != nil {
		t.Fatal(err)
	}
	if got < 1900*time.Millisecond || got > 2100*time.Millisecond {
		t.Fatalf("duration = %v, want ~2s", got)
	}
}

func TestEstimateUnknownFallsBack(t *testing.T) {
	path := writeTestFile(t, "noise.bin", []byte("not audio at all"))
	if _, err := estimateAudioDuration(path); err == nil {
		t.Fatal("expected an error for a non-audio file")
	}
}
