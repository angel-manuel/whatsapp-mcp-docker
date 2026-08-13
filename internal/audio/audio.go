// Package audio owns the one piece of media handling that is not just
// bytes-in/bytes-out: turning whatever a caller uploaded into something
// WhatsApp will actually play as a voice note.
//
// WhatsApp voice notes (PTT) are Ogg/Opus. Everything else — an mp3, an m4a,
// a wav — has to be transcoded first, which means shelling out to ffmpeg.
// ffmpeg is only present in the `-slim` image variant, so the contract is
// deliberately two-sided (see REQUIREMENTS.md, FFMPEG_PATH):
//
//   - ffmpeg present  → arbitrary input is transcoded to Ogg/Opus.
//   - ffmpeg absent   → Opus input is required, and anything else is
//     refused with an actionable error rather than uploaded as an
//     unplayable attachment.
//
// The package also probes Ogg/Opus streams for their duration, so an
// outbound voice note can advertise its length without a second ffprobe
// hop.
package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// OpusMime is the mimetype WhatsApp expects on a voice note.
const OpusMime = "audio/ogg; codecs=opus"

// DefaultTimeout bounds a single transcode. Voice notes are short; a run
// that takes longer than this is stuck, not slow.
const DefaultTimeout = 2 * time.Minute

// stderrTail caps how much of ffmpeg's stderr is quoted back in an error.
// Enough to name the failure, not enough to flood a tool result.
const stderrTail = 2000

// ErrUnavailable is returned by Transcoder.ToOpus when no ffmpeg binary is
// present at the configured path. Callers surface it as "Opus input
// required" rather than as an internal failure: it is a deployment choice
// (the distroless image ships no ffmpeg), not a fault.
var ErrUnavailable = errors.New("audio: ffmpeg is not available")

// Transcoder converts arbitrary audio to Ogg/Opus by shelling out to
// ffmpeg. The zero value is unusable; construct one with NewTranscoder.
type Transcoder struct {
	// path is the ffmpeg binary (config FFMPEG_PATH).
	path string
	// timeout bounds one run. Zero means DefaultTimeout.
	timeout time.Duration
}

// NewTranscoder returns a Transcoder that runs the ffmpeg binary at path.
// An empty path, or a path that does not resolve to an executable, yields a
// Transcoder whose Available reports false — construction never fails, so
// wiring code does not have to decide at startup what only matters at call
// time (an operator may bind-mount ffmpeg into a running container).
func NewTranscoder(path string) *Transcoder {
	return &Transcoder{path: path}
}

// Path returns the configured ffmpeg path, for error messages that need to
// tell an operator which path was probed.
func (t *Transcoder) Path() string {
	if t == nil {
		return ""
	}
	return t.path
}

// Available reports whether the configured ffmpeg binary exists and is
// executable *right now*. It is re-checked per call rather than cached at
// startup, so an image that gains ffmpeg via a mount does not need a
// restart.
func (t *Transcoder) Available() bool {
	if t == nil || t.path == "" {
		return false
	}
	// A bare name (e.g. "ffmpeg") is resolved through PATH; anything with a
	// separator is taken literally, matching exec.Command's own rule.
	if !strings.ContainsRune(t.path, os.PathSeparator) {
		_, err := exec.LookPath(t.path)
		return err == nil
	}
	info, err := os.Stat(t.path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// ToOpus transcodes in to a mono 48 kHz Ogg/Opus stream tuned for voice.
// It returns ErrUnavailable when no ffmpeg binary is present.
//
// Both ends go through temp files rather than pipes on purpose: several
// container formats WhatsApp users actually send (m4a/mp4 in particular)
// cannot be demuxed from a non-seekable stdin, and failing on those would
// defeat the point of having a transcoder at all. /tmp is writable in every
// supported deployment (REQUIREMENTS.md, "read-only root filesystem
// compatible").
func (t *Transcoder) ToOpus(ctx context.Context, in io.Reader) ([]byte, error) {
	if !t.Available() {
		return nil, ErrUnavailable
	}

	inPath, cleanupIn, err := spool(in)
	if err != nil {
		return nil, err
	}
	defer cleanupIn()

	outFile, err := os.CreateTemp("", "wa-audio-out-*.ogg")
	if err != nil {
		return nil, fmt.Errorf("audio: create temp output: %w", err)
	}
	outPath := outFile.Name()
	_ = outFile.Close()
	defer func() { _ = os.Remove(outPath) }()

	timeout := t.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, t.path, ffmpegArgs(inPath, outPath)...) // #nosec G204 -- path is operator config, args are fixed
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil && ctx.Err() == nil {
			return nil, fmt.Errorf("audio: ffmpeg timed out after %s", timeout)
		}
		return nil, fmt.Errorf("audio: ffmpeg failed: %w: %s", err, tail(stderr.String()))
	}

	data, err := os.ReadFile(outPath) // #nosec G304 -- path came from os.CreateTemp
	if err != nil {
		return nil, fmt.Errorf("audio: read transcoded output: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("audio: ffmpeg produced no output: %s", tail(stderr.String()))
	}
	return data, nil
}

// ffmpegArgs builds the voice-note encode. -vn drops any cover art (an mp3
// with embedded artwork would otherwise fail the ogg muxer), and the opus
// settings mirror what WhatsApp's own clients send: mono, 48 kHz, VoIP
// application, 60 ms frames.
func ffmpegArgs(inPath, outPath string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", inPath,
		"-vn",
		"-c:a", "libopus",
		"-b:a", "32k",
		"-ar", "48000",
		"-ac", "1",
		"-application", "voip",
		"-vbr", "on",
		"-compression_level", "10",
		"-frame_duration", "60",
		"-f", "ogg",
		outPath,
	}
}

// spool writes r to a temp file and returns its path plus a cleanup func.
func spool(r io.Reader) (string, func(), error) {
	f, err := os.CreateTemp("", "wa-audio-in-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("audio: create temp input: %w", err)
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("audio: buffer input: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("audio: close temp input: %w", err)
	}
	return path, cleanup, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no stderr output)"
	}
	if len(s) > stderrTail {
		return "..." + s[len(s)-stderrTail:]
	}
	return s
}

// Info describes an audio blob as far as this package can tell from its
// bytes alone.
type Info struct {
	// IsOggOpus reports whether the stream is Ogg-encapsulated Opus, which
	// is the only encoding WhatsApp plays as a voice note.
	IsOggOpus bool
	// Duration is the decoded length, derived from the final Ogg page's
	// granule position. Zero when unknown.
	Duration time.Duration
}

// Seconds returns the duration rounded to whole seconds, which is the
// resolution AudioMessage.Seconds carries.
func (i Info) Seconds() uint32 {
	if i.Duration <= 0 {
		return 0
	}
	return uint32((i.Duration + time.Second/2) / time.Second)
}

const (
	oggMagic  = "OggS"
	opusMagic = "OpusHead"
	// oggHeaderLen is the fixed part of an Ogg page header, up to (not
	// including) the segment table.
	oggHeaderLen = 27
	// opusRate is the granule-position clock. Opus granules are always
	// counted at 48 kHz regardless of the original sample rate.
	opusRate = 48000
	// tailScan is how much of the end of the stream is scanned for the last
	// Ogg page. An Opus page holds at most a few hundred ms, so this is
	// generous by orders of magnitude.
	tailScan = 64 << 10
)

// Probe inspects r and reports whether it carries Ogg/Opus and how long it
// is. It never returns an error for "this is not Opus" — that is a normal
// answer, reported as Info{IsOggOpus: false}. An error means r itself could
// not be read or seeked.
//
// r is left seeked back to its start, so the caller can go on to upload the
// very same reader.
func Probe(r io.ReadSeeker) (Info, error) {
	defer func() { _, _ = r.Seek(0, io.SeekStart) }()

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Info{}, fmt.Errorf("audio: seek to start: %w", err)
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Info{}, fmt.Errorf("audio: read header: %w", err)
	}
	head = head[:n]

	preSkip, ok := opusHead(head)
	if !ok {
		return Info{}, nil
	}

	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return Info{}, fmt.Errorf("audio: seek to end: %w", err)
	}
	scan := int64(tailScan)
	if size < scan {
		scan = size
	}
	if _, err := r.Seek(size-scan, io.SeekStart); err != nil {
		return Info{}, fmt.Errorf("audio: seek to tail: %w", err)
	}
	tailBuf := make([]byte, scan)
	if _, err := io.ReadFull(r, tailBuf); err != nil {
		return Info{}, fmt.Errorf("audio: read tail: %w", err)
	}

	info := Info{IsOggOpus: true}
	if granule, ok := lastGranule(tailBuf); ok && granule > uint64(preSkip) {
		samples := granule - uint64(preSkip)
		info.Duration = time.Duration(samples) * time.Second / opusRate
	}
	return info, nil
}

// ProbeBytes is Probe over an in-memory blob.
func ProbeBytes(data []byte) (Info, error) {
	return Probe(bytes.NewReader(data))
}

// opusHead validates that head starts an Ogg stream whose first packet is an
// OpusHead identification header, and returns its pre-skip (samples of
// encoder delay to discard, which must not be counted as duration).
func opusHead(head []byte) (uint16, bool) {
	if len(head) < oggHeaderLen || string(head[:4]) != oggMagic {
		return 0, false
	}
	idx := bytes.Index(head, []byte(opusMagic))
	// The identification header is the first packet of the first page, so
	// it sits just past the page header + segment table — never deep into
	// the buffer. Requiring that keeps a stray "OpusHead" inside, say, an
	// mp3 tag from being read as a stream header.
	if idx < oggHeaderLen || idx > 128 {
		return 0, false
	}
	// OpusHead layout: magic(8) version(1) channels(1) pre-skip(2 LE).
	if len(head) < idx+12 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(head[idx+10 : idx+12]), true
}

// lastGranule returns the granule position of the last Ogg page in buf. For
// an Opus stream that is the total sample count (including pre-skip), which
// is how the duration is derived without decoding anything.
//
// Pages that complete no packet carry granule -1 and are skipped: reading
// one as a sample count would report a duration of six million years.
func lastGranule(buf []byte) (uint64, bool) {
	const noGranule = ^uint64(0)
	for i := len(buf) - oggHeaderLen; i >= 0; i-- {
		if string(buf[i:i+4]) != oggMagic {
			continue
		}
		if g := binary.LittleEndian.Uint64(buf[i+6 : i+14]); g != noGranule {
			return g, true
		}
	}
	return 0, false
}
