// Package jfprobe reads container metadata with ffprobe.
//
// This is what turns a file on disk into something a client can play: a
// duration for the seek bar, and a stream list so the client can decide whether
// it can direct-play the file or needs a transcode.
//
// Jellyfin does the same thing (MediaBrowser.MediaEncoding/Probing/), just with
// far more post-processing. ffprobe is invoked as a subprocess rather than
// linked, which keeps the static build and means a missing ffprobe degrades to
// "no duration" instead of failing to start.
package jfprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrNoFFprobe means the binary is not installed or not on PATH.
var ErrNoFFprobe = errors.New("jfprobe: ffprobe not found on PATH")

// MediaInfo is what a client needs to play a file.
type MediaInfo struct {
	Container string        `json:"container"`
	Duration  time.Duration `json:"duration"`
	Bitrate   int           `json:"bitrate"`
	Size      int64         `json:"size"`
	Streams   []Stream      `json:"streams"`
}

// Stream is one track inside the container.
type Stream struct {
	Index         int    `json:"index"`
	Type          string `json:"type"` // Video, Audio, Subtitle
	Codec         string `json:"codec"`
	Profile       string `json:"profile,omitempty"`
	Language      string `json:"language,omitempty"`
	Title         string `json:"title,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	Channels      int    `json:"channels,omitempty"`
	SampleRate    int    `json:"sample_rate,omitempty"`
	BitRate       int    `json:"bit_rate,omitempty"`
	IsDefault     bool   `json:"is_default,omitempty"`
	IsForced      bool   `json:"is_forced,omitempty"`
	PixelFormat   string `json:"pixel_format,omitempty"`
	AverageFrames string `json:"average_frames,omitempty"`
}

// VideoStream returns the first video track, or nil.
func (m *MediaInfo) VideoStream() *Stream {
	for i := range m.Streams {
		if m.Streams[i].Type == "Video" {
			return &m.Streams[i]
		}
	}
	return nil
}

// Available reports whether ffprobe can be run at all.
func Available() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// raw mirrors the subset of ffprobe's JSON that matters here.
type raw struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
		Size       string `json:"size"`
	} `json:"format"`
	Streams []struct {
		Index       int    `json:"index"`
		CodecName   string `json:"codec_name"`
		CodecType   string `json:"codec_type"`
		Profile     string `json:"profile"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Channels    int    `json:"channels"`
		SampleRate  string `json:"sample_rate"`
		BitRate     string `json:"bit_rate"`
		PixFmt      string `json:"pix_fmt"`
		AvgFrame    string `json:"avg_frame_rate"`
		Duration    string `json:"duration"`
		Disposition struct {
			Default int `json:"default"`
			Forced  int `json:"forced"`
		} `json:"disposition"`
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
}

// Probe runs ffprobe against a file.
//
// The context bounds it: a damaged file, or one on a stalled network mount, can
// otherwise keep ffprobe running indefinitely and a library scan would never
// finish.
func Probe(ctx context.Context, path string) (*MediaInfo, error) {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, ErrNoFFprobe
	}

	cmd := exec.CommandContext(ctx, bin,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("jfprobe: %w", err)
	}

	var r raw
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("jfprobe: parsing ffprobe output: %w", err)
	}

	info := &MediaInfo{
		// ffprobe reports a comma-joined list of every format the demuxer
		// matched ("matroska,webm"); the first is the one to report.
		Container: firstFormat(r.Format.FormatName),
		Bitrate:   atoi(r.Format.BitRate),
		Size:      atoi64(r.Format.Size),
	}
	if d, err := strconv.ParseFloat(r.Format.Duration, 64); err == nil && d > 0 {
		info.Duration = time.Duration(d * float64(time.Second))
	}

	for _, s := range r.Streams {
		typ := streamType(s.CodecType)
		if typ == "" {
			continue // attachments, data tracks
		}
		st := Stream{
			Index:         s.Index,
			Type:          typ,
			Codec:         s.CodecName,
			Profile:       s.Profile,
			Width:         s.Width,
			Height:        s.Height,
			Channels:      s.Channels,
			SampleRate:    atoi(s.SampleRate),
			BitRate:       atoi(s.BitRate),
			PixelFormat:   s.PixFmt,
			AverageFrames: s.AvgFrame,
			IsDefault:     s.Disposition.Default == 1,
			IsForced:      s.Disposition.Forced == 1,
		}
		st.Language = s.Tags["language"]
		st.Title = s.Tags["title"]
		// Matroska carries per-track bitrate in a BPS tag rather than in the
		// stream header, so a container-level probe reports nothing without it.
		if st.BitRate == 0 {
			st.BitRate = atoi(s.Tags["BPS"])
		}
		info.Streams = append(info.Streams, st)
	}

	if info.Duration == 0 && len(info.Streams) == 0 {
		return nil, errors.New("jfprobe: no usable media information")
	}
	return info, nil
}

// streamType maps ffprobe's codec_type to the capitalised names Jellyfin uses
// on the wire. Anything else is dropped.
func streamType(s string) string {
	switch strings.ToLower(s) {
	case "video":
		return "Video"
	case "audio":
		return "Audio"
	case "subtitle":
		return "Subtitle"
	}
	return ""
}

func firstFormat(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		return s[:i]
	}
	return s
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func atoi64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
