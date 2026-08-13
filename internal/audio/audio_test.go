package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// oggPage assembles one Ogg page. The CRC is left zero: nothing in this
// package validates it, and a real muxer's checksum would only obscure what
// each fixture is exercising.
func oggPage(headerType byte, granule uint64, seq uint32, payload []byte) []byte {
	page := make([]byte, 27)
	copy(page, oggMagic)
	page[4] = 0 // stream structure version
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], 0xC0FFEE)
	binary.LittleEndian.PutUint32(page[18:22], seq)

	// One lacing value per 255-byte run, as the Ogg segment table demands.
	var table []byte
	remaining := len(payload)
	for remaining >= 255 {
		table = append(table, 255)
		remaining -= 255
	}
	table = append(table, byte(remaining))

	page[26] = byte(len(table))
	page = append(page, table...)
	return append(page, payload...)
}

// opusHeadPacket is the 19-byte OpusHead identification header.
func opusHeadPacket(preSkip uint16) []byte {
	p := make([]byte, 19)
	copy(p, opusMagic)
	p[8] = 1 // version
	p[9] = 1 // channels
	binary.LittleEndian.PutUint16(p[10:12], preSkip)
	binary.LittleEndian.PutUint32(p[12:16], 48000)
	return p
}

// oggOpus builds a minimal but structurally valid Ogg/Opus stream whose
// final granule encodes the given duration.
func oggOpus(preSkip uint16, granule uint64) []byte {
	var b bytes.Buffer
	b.Write(oggPage(0x02, 0, 0, opusHeadPacket(preSkip)))
	b.Write(oggPage(0x00, 0, 1, append([]byte("OpusTags"), make([]byte, 16)...)))
	b.Write(oggPage(0x04, granule, 2, []byte("audio-frames")))
	return b.Bytes()
}

func TestProbe_ReportsOpusAndDuration(t *testing.T) {
	// 312 pre-skip + 3 seconds of 48 kHz samples.
	data := oggOpus(312, 312+3*48000)

	info, err := ProbeBytes(data)
	if err != nil {
		t.Fatalf("ProbeBytes: %v", err)
	}
	if !info.IsOggOpus {
		t.Fatal("IsOggOpus = false, want true")
	}
	if info.Duration != 3*time.Second {
		t.Errorf("Duration = %s, want 3s", info.Duration)
	}
	if info.Seconds() != 3 {
		t.Errorf("Seconds() = %d, want 3", info.Seconds())
	}
}

func TestProbe_RewindsReader(t *testing.T) {
	data := oggOpus(312, 312+48000)
	r := bytes.NewReader(data)

	if _, err := Probe(r); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	// The whole point: the caller uploads the very same reader afterwards,
	// so it has to be back at byte zero with every byte still ahead of it.
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read after probe: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("read %d bytes after probe, want the whole %d-byte stream", len(got), len(data))
	}
}

func TestProbe_NonOpusIsNotAnError(t *testing.T) {
	for name, data := range map[string][]byte{
		"mp3":        append([]byte{0xFF, 0xFB, 0x90, 0x00}, make([]byte, 512)...),
		"empty":      {},
		"ogg-vorbis": oggPage(0x02, 0, 0, append([]byte{1}, []byte("vorbis")...)),
		// An mp3 whose ID3 tag happens to contain the string "OpusHead"
		// must not be mistaken for an Opus stream.
		"mp3-with-opushead-text": append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00OpusHead"), make([]byte, 512)...),
	} {
		t.Run(name, func(t *testing.T) {
			info, err := ProbeBytes(data)
			if err != nil {
				t.Fatalf("ProbeBytes: %v", err)
			}
			if info.IsOggOpus {
				t.Error("IsOggOpus = true, want false")
			}
		})
	}
}

func TestProbe_IgnoresPagesWithoutGranule(t *testing.T) {
	// A trailing page that completes no packet carries granule -1; reading
	// it as a sample count would report a duration of geological length.
	var b bytes.Buffer
	b.Write(oggPage(0x02, 0, 0, opusHeadPacket(312)))
	b.Write(oggPage(0x00, 312+48000, 1, []byte("frames")))
	b.Write(oggPage(0x00, ^uint64(0), 2, []byte("continued")))

	info, err := ProbeBytes(b.Bytes())
	if err != nil {
		t.Fatalf("ProbeBytes: %v", err)
	}
	if info.Duration != time.Second {
		t.Errorf("Duration = %s, want 1s", info.Duration)
	}
}

func TestAvailable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	nonExec := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(nonExec, []byte("data"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cases := map[string]struct {
		path string
		want bool
	}{
		"executable":     {exe, true},
		"missing":        {filepath.Join(dir, "nope"), false},
		"not executable": {nonExec, false},
		"directory":      {dir, false},
		"empty path":     {"", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := NewTranscoder(tc.path).Available(); got != tc.want {
				t.Errorf("Available() = %v, want %v", got, tc.want)
			}
		})
	}

	if (*Transcoder)(nil).Available() {
		t.Error("nil transcoder reported available")
	}
}

func TestToOpus_UnavailableWithoutFFmpeg(t *testing.T) {
	_, err := NewTranscoder(filepath.Join(t.TempDir(), "absent")).
		ToOpus(context.Background(), bytes.NewReader([]byte("audio")))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

// stubFFmpeg writes a shell script standing in for ffmpeg. body receives the
// real argument list, so a stub can honour "-i in out" the way ffmpeg does.
func stubFFmpeg(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs are POSIX-only")
	}
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestToOpus_ReturnsTranscodedBytes(t *testing.T) {
	// The stub copies a canned Opus stream to whatever path is last on the
	// command line, which is where ffmpeg writes its output.
	fixture := writeTemp(t, oggOpus(312, 312+2*48000))
	ffmpeg := stubFFmpeg(t, `for last; do :; done; cp `+fixture+` "$last"`)

	got, err := NewTranscoder(ffmpeg).ToOpus(context.Background(), strings.NewReader("an mp3, allegedly"))
	if err != nil {
		t.Fatalf("ToOpus: %v", err)
	}
	info, err := ProbeBytes(got)
	if err != nil || !info.IsOggOpus {
		t.Fatalf("output is not Ogg/Opus (info=%+v, err=%v)", info, err)
	}
	if info.Seconds() != 2 {
		t.Errorf("Seconds() = %d, want 2", info.Seconds())
	}
}

func TestToOpus_SurfacesFFmpegFailure(t *testing.T) {
	ffmpeg := stubFFmpeg(t, `echo "Invalid data found when processing input" >&2; exit 1`)

	_, err := NewTranscoder(ffmpeg).ToOpus(context.Background(), strings.NewReader("not audio"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Invalid data found") {
		t.Errorf("err = %v, want it to quote ffmpeg's stderr", err)
	}
}

// A stuck ffmpeg must not hold an MCP call open indefinitely.
func TestToOpus_TimesOut(t *testing.T) {
	ffmpeg := stubFFmpeg(t, `sleep 30`)

	start := time.Now()
	_, err := NewTranscoder(ffmpeg, WithTimeout(150*time.Millisecond)).
		ToOpus(context.Background(), strings.NewReader("audio"))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s, want the configured timeout to cut it short", elapsed)
	}
}

func TestWithTimeout_NonPositiveKeepsTheDefault(t *testing.T) {
	if got := NewTranscoder("ffmpeg", WithTimeout(0)).timeout; got != DefaultTimeout {
		t.Errorf("timeout = %s, want %s", got, DefaultTimeout)
	}
	if got := NewTranscoder("ffmpeg").timeout; got != DefaultTimeout {
		t.Errorf("default timeout = %s, want %s", got, DefaultTimeout)
	}
}

func TestToOpus_EmptyOutputIsAnError(t *testing.T) {
	ffmpeg := stubFFmpeg(t, `exit 0`)

	_, err := NewTranscoder(ffmpeg).ToOpus(context.Background(), strings.NewReader("audio"))
	if err == nil || !strings.Contains(err.Error(), "no output") {
		t.Fatalf("err = %v, want a no-output error", err)
	}
}

func TestToOpus_PassesInputThroughAFile(t *testing.T) {
	// Container formats WhatsApp users actually send (m4a) cannot be
	// demuxed from a pipe, so the input must reach ffmpeg as a real path.
	out := writeTemp(t, oggOpus(312, 48312))
	ffmpeg := stubFFmpeg(t, `
in=""
prev=""
for a; do
  if [ "$prev" = "-i" ]; then in="$a"; fi
  prev="$a"
  last="$a"
done
[ -f "$in" ] || exit 3
grep -q PAYLOAD "$in" || exit 4
cp `+out+` "$last"`)

	if _, err := NewTranscoder(ffmpeg).ToOpus(context.Background(), strings.NewReader("PAYLOAD")); err != nil {
		t.Fatalf("ToOpus: %v", err)
	}
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.ogg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}
