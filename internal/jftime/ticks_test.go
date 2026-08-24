package jftime

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTickConstants(t *testing.T) {
	// One tick is 100ns. Everything else follows from that, and a wrong
	// constant here is a 10,000x error that looks like a stuck progress bar.
	if TicksPerSecond != 10_000_000 {
		t.Errorf("TicksPerSecond = %d, want 10000000", TicksPerSecond)
	}
	if TicksPerMS != TicksPerSecond/1000 {
		t.Errorf("TicksPerMS = %d, want %d", TicksPerMS, TicksPerSecond/1000)
	}
	if TicksPerMinute != TicksPerSecond*60 {
		t.Errorf("TicksPerMinute = %d, want %d", TicksPerMinute, TicksPerSecond*60)
	}
}

func TestFromDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want Ticks
	}{
		{0, 0},
		{100 * time.Nanosecond, 1},
		{time.Microsecond, 10},
		{time.Millisecond, 10_000},
		{time.Second, 10_000_000},
		{time.Minute, 600_000_000},
		{90 * time.Minute, 54_000_000_000},
		{-time.Second, -10_000_000},
		// Sub-tick resolution truncates rather than rounding.
		{150 * time.Nanosecond, 1},
		{99 * time.Nanosecond, 0},
	}
	for _, c := range cases {
		if got := FromDuration(c.in); got != c.want {
			t.Errorf("FromDuration(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestDurationRoundTrip(t *testing.T) {
	// Any duration that is a whole number of ticks must survive the round trip.
	for _, d := range []time.Duration{
		0,
		time.Second,
		2*time.Hour + 13*time.Minute + 7*time.Second,
		100 * time.Nanosecond,
		-42 * time.Second,
	} {
		if got := FromDuration(d).Duration(); got != d {
			t.Errorf("round trip of %v gave %v", d, got)
		}
	}
}

func TestSeconds(t *testing.T) {
	cases := []struct {
		sec  float64
		want Ticks
	}{
		{0, 0},
		{1, 10_000_000},
		{1.5, 15_000_000},
		{0.0000001, 1},
		{7200, 72_000_000_000},
	}
	for _, c := range cases {
		if got := FromSeconds(c.sec); got != c.want {
			t.Errorf("FromSeconds(%v) = %d, want %d", c.sec, got, c.want)
		}
		if got := c.want.Seconds(); got != c.sec {
			t.Errorf("Ticks(%d).Seconds() = %v, want %v", c.want, got, c.sec)
		}
	}
}

// Ticks must serialize as a bare JSON number. Quoting it makes clients that
// parse RunTimeTicks as a number silently treat the item as zero-length.
func TestTicksMarshalsAsNumber(t *testing.T) {
	b, err := json.Marshal(struct {
		RunTimeTicks Ticks `json:"RunTimeTicks"`
	}{FromDuration(90 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"RunTimeTicks":54000000000}`
	if string(b) != want {
		t.Fatalf("Marshal = %s, want %s", b, want)
	}
}

func TestTimeMarshalsWithSevenFractionalDigits(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			"sub-second precision preserved",
			time.Date(2021, 3, 4, 5, 6, 7, 123456700, time.UTC),
			`"2021-03-04T05:06:07.1234567Z"`,
		},
		{
			// The trailing zeros must be kept: a client that string-compares
			// timestamps needs a fixed width.
			"whole second still gets seven digits",
			time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC),
			`"2021-03-04T05:06:07.0000000Z"`,
		},
		{
			"millisecond precision is zero-padded out to seven",
			time.Date(2000, 1, 1, 0, 0, 0, 500000000, time.UTC),
			`"2000-01-01T00:00:00.5000000Z"`,
		},
		{
			// Anything finer than 100ns is truncated, matching .NET's tick
			// resolution.
			"nanoseconds below tick resolution are dropped",
			time.Date(2021, 3, 4, 5, 6, 7, 123456789, time.UTC),
			`"2021-03-04T05:06:07.1234567Z"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(Time{c.in})
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != c.want {
				t.Errorf("Marshal = %s, want %s", b, c.want)
			}
		})
	}
}

// A non-UTC instant must be converted, not merely relabelled: the same moment
// has to come out with a Z suffix and shifted clock fields.
func TestTimeMarshalConvertsToUTC(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*60*60)
	b, err := json.Marshal(Time{time.Date(2021, 3, 4, 7, 6, 7, 0, zone)})
	if err != nil {
		t.Fatal(err)
	}
	want := `"2021-03-04T05:06:07.0000000Z"`
	if string(b) != want {
		t.Fatalf("Marshal = %s, want %s", b, want)
	}
}

func TestTimeZeroMarshalsNull(t *testing.T) {
	b, err := json.Marshal(Time{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Fatalf("zero Time marshalled as %s, want null", b)
	}
}

// Input precision varies by client, so unmarshalling must be liberal even
// though output is strict.
func TestTimeUnmarshalAcceptsAnyPrecision(t *testing.T) {
	want := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, in := range []string{
		`"2021-03-04T05:06:07Z"`,
		`"2021-03-04T05:06:07.0Z"`,
		`"2021-03-04T05:06:07.000Z"`,
		`"2021-03-04T05:06:07.0000000Z"`,
		`"2021-03-04T07:06:07+02:00"`,
	} {
		var got Time
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Errorf("Unmarshal(%s): %v", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("Unmarshal(%s) = %v, want %v", in, got.Time, want)
		}
	}
}

func TestTimeUnmarshalEmptyAndNull(t *testing.T) {
	for _, in := range []string{`null`, `""`} {
		got := Time{time.Now()}
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Errorf("Unmarshal(%s): %v", in, err)
			continue
		}
		if !got.IsZero() {
			t.Errorf("Unmarshal(%s) = %v, want the zero time", in, got.Time)
		}
	}
}

func TestTimeUnmarshalRejectsGarbage(t *testing.T) {
	for _, in := range []string{`"not a time"`, `12345`, `{}`} {
		var got Time
		if err := json.Unmarshal([]byte(in), &got); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want an error", in)
		}
	}
}

func TestTimeRoundTrip(t *testing.T) {
	in := Time{time.Date(2026, 8, 24, 12, 34, 56, 789012300, time.UTC)}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var back Time
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Equal(in.Time) {
		t.Fatalf("round trip: %v -> %s -> %v", in.Time, b, back.Time)
	}
}

// Vectors computed independently (Python datetime), not from this code.
func TestUnixToTicks(t *testing.T) {
	cases := []struct {
		in   time.Time
		want int64
	}{
		// The .NET epoch offset itself: 0001-01-01 to 1970-01-01.
		{time.Unix(0, 0).UTC(), 621_355_968_000_000_000},
		{time.Date(2021, 3, 4, 5, 6, 7, 123456000, time.UTC), 637_504_311_671_234_560},
		{time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC), 639_231_696_000_000_000},
	}
	for _, c := range cases {
		if got := UnixToTicks(c.in); got != c.want {
			t.Errorf("UnixToTicks(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// A local-zone input must give the same tick count as its UTC equivalent.
func TestUnixToTicksIgnoresZone(t *testing.T) {
	zone := time.FixedZone("UTC-5", -5*60*60)
	a := UnixToTicks(time.Date(2021, 3, 4, 0, 6, 7, 0, zone))
	b := UnixToTicks(time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC))
	if a != b {
		t.Errorf("zone affected the result: %d != %d", a, b)
	}
}
