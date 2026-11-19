package relay

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

// estimateAudioDuration returns a track's wall-clock length without
// decoding its audio. The AutoDJ's idle mode uses it to advance the
// playlist while nobody is listening. It understands the containers the
// AutoDJ can decode, except Ogg FLAC, which falls back to real decoding.
//
// Errors are normal: callers treat an unknown duration as "cannot skip
// decoding for this track".
func estimateAudioDuration(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() < 16 {
		return 0, fmt.Errorf("file too short")
	}

	head := make([]byte, 16)
	if _, err := f.ReadAt(head, 0); err != nil {
		return 0, err
	}
	switch {
	case bytes.Equal(head[:4], []byte("RIFF")):
		return wavDuration(f, st.Size())
	case bytes.Equal(head[:4], []byte("fLaC")):
		return flacDuration(f)
	case bytes.Equal(head[:4], []byte("OggS")):
		return oggDuration(f, st.Size())
	case head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return mp3Duration(f, st.Size(), 0)
	case string(head[:3]) == "ID3":
		return mp3Duration(f, st.Size(), id3v2End(head))
	default:
		// A FLAC or Ogg stream may carry a leading ID3 tag.
		window := make([]byte, 64*1024)
		n, _ := f.ReadAt(window, 0)
		window = window[:n]
		if i := bytes.Index(window, []byte("fLaC")); i >= 0 {
			return flacDuration(f)
		}
		if i := bytes.Index(window, []byte("OggS")); i >= 0 {
			return oggDuration(f, st.Size())
		}
		return 0, fmt.Errorf("unknown container")
	}
}

func id3v2End(head []byte) int64 {
	if len(head) < 10 || string(head[:3]) != "ID3" {
		return 0
	}
	size := int64(head[6]&0x7F)<<21 | int64(head[7]&0x7F)<<14 | int64(head[8]&0x7F)<<7 | int64(head[9]&0x7F)
	end := int64(10) + size
	if head[5]&0x10 != 0 {
		end += 10 // footer
	}
	return end
}

func mp3Duration(f *os.File, fileSize, start int64) (time.Duration, error) {
	if start < 0 || start >= fileSize {
		return 0, fmt.Errorf("mp3: invalid start offset")
	}
	const scanLimit = 64 * 1024
	windowSize := int64(scanLimit)
	if fileSize-start < windowSize {
		windowSize = fileSize - start
	}
	window := make([]byte, windowSize)
	if _, err := f.ReadAt(window, start); err != nil {
		return 0, err
	}

	frameOff := -1
	for i := 0; i+4 <= len(window); i++ {
		if window[i] != 0xFF || window[i+1]&0xE0 != 0xE0 {
			continue
		}
		if _, _, _, _, ok := parseMP3Header(window[i : i+4]); ok {
			frameOff = i
			break
		}
	}
	if frameOff < 0 {
		return 0, fmt.Errorf("mp3: no frame header")
	}
	bitrate, sampleRate, samplesPerFrame, sideInfo, ok := parseMP3Header(window[frameOff : frameOff+4])
	if !ok {
		return 0, fmt.Errorf("mp3: invalid frame header")
	}
	audioStart := start + int64(frameOff)

	xingOff := frameOff + 4 + sideInfo
	if xingOff+12 <= len(window) {
		tag := window[xingOff : xingOff+4]
		if bytes.Equal(tag, []byte("Xing")) || bytes.Equal(tag, []byte("Info")) {
			flags := binary.BigEndian.Uint32(window[xingOff+4 : xingOff+8])
			if flags&1 != 0 {
				frames := binary.BigEndian.Uint32(window[xingOff+8 : xingOff+12])
				if frames > 0 {
					secs := float64(frames) * float64(samplesPerFrame) / float64(sampleRate)
					return time.Duration(secs * float64(time.Second)), nil
				}
			}
		}
	}
	// VBRI lives 32 bytes after the frame header.
	vbriOff := frameOff + 4 + 32
	if vbriOff+18 <= len(window) && bytes.Equal(window[vbriOff:vbriOff+4], []byte("VBRI")) {
		frames := binary.BigEndian.Uint32(window[vbriOff+14 : vbriOff+18])
		if frames > 0 {
			secs := float64(frames) * float64(samplesPerFrame) / float64(sampleRate)
			return time.Duration(secs * float64(time.Second)), nil
		}
	}

	// No VBR header: treat the stream as CBR at the first frame's bitrate.
	bytesLeft := fileSize - audioStart
	if bytesLeft <= 0 || bitrate <= 0 {
		return 0, fmt.Errorf("mp3: no audio data")
	}
	secs := float64(bytesLeft) * 8 / float64(bitrate)
	return time.Duration(secs * float64(time.Second)), nil
}

func parseMP3Header(h []byte) (bitrate, sampleRate, samplesPerFrame, sideInfo int, ok bool) {
	if len(h) < 4 || h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return 0, 0, 0, 0, false
	}
	versionBits := (h[1] >> 3) & 0x03 // 0=2.5, 2=2, 3=1
	layerBits := (h[1] >> 1) & 0x03   // 1=Layer III
	if versionBits == 1 || layerBits != 1 {
		return 0, 0, 0, 0, false
	}
	bitrateIdx := int(h[2] >> 4)
	sampleIdx := int(h[2] >> 2 & 0x03)
	padding := int(h[2] >> 1 & 0x01)
	if bitrateIdx < 1 || bitrateIdx > 14 || sampleIdx > 2 {
		return 0, 0, 0, 0, false
	}

	mpeg1 := versionBits == 3
	var bitrates []int
	if mpeg1 {
		bitrates = []int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	} else {
		bitrates = []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
	}
	rates := [][]int{
		{11025, 12000, 8000},
		{0, 0, 0},
		{22050, 24000, 16000},
		{44100, 48000, 32000},
	}
	bitrate = bitrates[bitrateIdx] * 1000
	sampleRate = rates[versionBits][sampleIdx]
	if sampleRate == 0 {
		return 0, 0, 0, 0, false
	}
	if mpeg1 {
		samplesPerFrame = 1152
	} else {
		samplesPerFrame = 576
	}
	mono := h[3]>>6 == 3
	if mpeg1 {
		if mono {
			sideInfo = 17
		} else {
			sideInfo = 32
		}
	} else if mono {
		sideInfo = 9
	} else {
		sideInfo = 17
	}
	_ = padding // padding is already accounted for by the CBR estimate
	return bitrate, sampleRate, samplesPerFrame, sideInfo, true
}

func flacDuration(f *os.File) (time.Duration, error) {
	head := make([]byte, 4+4+34)
	if _, err := f.ReadAt(head, 0); err != nil {
		return 0, err
	}
	if !bytes.Equal(head[:4], []byte("fLaC")) {
		return 0, fmt.Errorf("flac: missing marker")
	}
	blockType := head[4] & 0x7F
	length := int(head[5])<<16 | int(head[6])<<8 | int(head[7])
	if blockType != 0 || length < 34 {
		return 0, fmt.Errorf("flac: first block is not STREAMINFO")
	}
	b := head[8 : 8+34]
	sampleRate := int(b[10])<<12 | int(b[11])<<4 | int(b[12])>>4
	total := int64(b[13]&0x0F)<<32 | int64(b[14])<<24 | int64(b[15])<<16 | int64(b[16])<<8 | int64(b[17])
	if sampleRate == 0 || total == 0 {
		return 0, fmt.Errorf("flac: streaminfo has no duration")
	}
	return time.Duration(float64(total) / float64(sampleRate) * float64(time.Second)), nil
}

func wavDuration(f *os.File, fileSize int64) (time.Duration, error) {
	head := make([]byte, 12)
	if _, err := f.ReadAt(head, 0); err != nil {
		return 0, err
	}
	if !bytes.Equal(head[:4], []byte("RIFF")) || !bytes.Equal(head[8:12], []byte("WAVE")) {
		return 0, fmt.Errorf("wav: not a RIFF/WAVE file")
	}
	var byteRate uint32
	var dataSize uint32
	off := int64(12)
	chunk := make([]byte, 8)
	for off+8 <= fileSize {
		if _, err := f.ReadAt(chunk, off); err != nil {
			return 0, err
		}
		id := string(chunk[:4])
		size := int64(binary.LittleEndian.Uint32(chunk[4:]))
		body := off + 8
		switch id {
		case "fmt ":
			fmtBuf := make([]byte, 16)
			if size >= 16 {
				if _, err := f.ReadAt(fmtBuf, body); err != nil {
					return 0, err
				}
				byteRate = binary.LittleEndian.Uint32(fmtBuf[8:12])
			}
		case "data":
			dataSize = uint32(size)
		}
		if byteRate != 0 && dataSize != 0 {
			break
		}
		off = body + size
		if size%2 != 0 {
			off++
		}
	}
	if byteRate == 0 || dataSize == 0 {
		return 0, fmt.Errorf("wav: missing fmt/data chunk")
	}
	return time.Duration(float64(dataSize) / float64(byteRate) * float64(time.Second)), nil
}

func oggDuration(f *os.File, fileSize int64) (time.Duration, error) {
	headLen := int64(64 * 1024)
	if headLen > fileSize {
		headLen = fileSize
	}
	head := make([]byte, headLen)
	if _, err := f.ReadAt(head, 0); err != nil {
		return 0, err
	}
	body := firstOggPageBody(head)
	if len(body) < 8 {
		return 0, fmt.Errorf("ogg: no BOS body")
	}
	var rate int
	var preSkip int64
	switch {
	case bytes.Equal(body[:8], []byte("OpusHead")):
		if len(body) < 19 {
			return 0, fmt.Errorf("ogg opus: short head")
		}
		preSkip = int64(binary.LittleEndian.Uint16(body[10:12]))
		rate = 48000
	case body[0] == 0x01 && bytes.Equal(body[1:7], []byte("vorbis")):
		if len(body) < 16 {
			return 0, fmt.Errorf("ogg vorbis: short id header")
		}
		rate = int(binary.LittleEndian.Uint32(body[12:16]))
	default:
		return 0, fmt.Errorf("ogg: unsupported codec")
	}
	if rate == 0 {
		return 0, fmt.Errorf("ogg: zero sample rate")
	}

	tailLen := int64(64 * 1024)
	if tailLen > fileSize {
		tailLen = fileSize
	}
	tail := make([]byte, tailLen)
	if _, err := f.ReadAt(tail, fileSize-tailLen); err != nil {
		return 0, err
	}
	granule, ok := lastOggGranule(tail)
	if !ok {
		return 0, fmt.Errorf("ogg: no final granule")
	}
	if granule <= preSkip {
		return 0, fmt.Errorf("ogg: empty stream")
	}
	return time.Duration(float64(granule-preSkip) / float64(rate) * float64(time.Second)), nil
}

// lastOggGranule finds the final page header in a tail buffer. Both call
// sites read at least a page header plus the 8-byte granule position.
func lastOggGranule(buf []byte) (int64, bool) {
	for i := len(buf) - 14; i >= 0; i-- {
		if !bytes.Equal(buf[i:i+4], []byte("OggS")) {
			continue
		}
		g := int64(binary.LittleEndian.Uint64(buf[i+6 : i+14]))
		if g >= 0 {
			return g, true
		}
	}
	return 0, false
}
