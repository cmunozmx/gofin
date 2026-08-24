// Package jftime handles the two time representations Jellyfin puts on the
// wire: .NET ticks for durations and positions, and ISO-8601 for instants.
package jftime

import (
	"encoding/json"
	"time"
)

// TicksPerSecond is 10,000,000: one tick is 100 nanoseconds.
//
// Every RunTimeTicks, PlaybackPositionTicks and StartTimeTicks on the wire is
// in these units. Confusing them with milliseconds yields a value 10,000x off,
// which shows up as a progress bar that never moves and resume points that
// snap to zero.
const (
	TicksPerSecond = int64(10_000_000)
	TicksPerMS     = int64(10_000)
	TicksPerMinute = TicksPerSecond * 60
)

// Ticks is a duration in .NET ticks. It marshals as a bare JSON number.
type Ticks int64

func FromDuration(d time.Duration) Ticks { return Ticks(d.Nanoseconds() / 100) }
func (t Ticks) Duration() time.Duration  { return time.Duration(t) * 100 * time.Nanosecond }
func FromSeconds(s float64) Ticks        { return Ticks(s * float64(TicksPerSecond)) }
func (t Ticks) Seconds() float64         { return float64(t) / float64(TicksPerSecond) }

// Time wraps time.Time so it renders the way .NET's default serializer does:
// ISO-8601, UTC, with seven fractional digits. Most clients tolerate fewer,
// but a few string-compare timestamps, so match the original exactly.
type Time struct{ time.Time }

const layout = "2006-01-02T15:04:05.0000000Z07:00"

func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format(layout))
}

func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	// Accept any RFC-3339 precision on input.
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// UnixToTicks converts a Unix time to .NET ticks since 0001-01-01, which is the
// epoch used inside library.db columns.
func UnixToTicks(t time.Time) int64 {
	const epochOffset = int64(621_355_968_000_000_000) // ticks from 0001-01-01 to 1970-01-01
	return t.UTC().UnixNano()/100 + epochOffset
}
