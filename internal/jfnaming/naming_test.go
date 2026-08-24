package jfnaming

import "testing"

func TestIsVideoFile(t *testing.T) {
	for _, p := range []string{
		"/m/Arrival.mkv", "/m/Arrival.MKV", "/m/x.avi", "/m/x.mp4",
		"/m/x.m2ts", "/m/x.webm", "/m/x.iso", "/m/x.strm",
	} {
		if !IsVideoFile(p) {
			t.Errorf("IsVideoFile(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"/m/x.txt", "/m/x.nfo", "/m/x.jpg", "/m/x.srt", "/m/x", "/m/x.flac",
	} {
		if IsVideoFile(p) {
			t.Errorf("IsVideoFile(%q) = true, want false", p)
		}
	}
}

func TestIsAudioFile(t *testing.T) {
	for _, p := range []string{
		"/m/t.flac", "/m/t.FLAC", "/m/t.mp3", "/m/t.m4a", "/m/t.opus",
		"/m/t.wav", "/m/t.ogg", "/m/t.wma", "/m/t.ape", "/m/t.dsf",
	} {
		if !IsAudioFile(p) {
			t.Errorf("IsAudioFile(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/m/t.mkv", "/m/t.jpg", "/m/t.txt", "/m/t.log"} {
		if IsAudioFile(p) {
			t.Errorf("IsAudioFile(%q) = true, want false", p)
		}
	}
}

// A few extensions are in both upstream tables. Video wins, matching the order
// upstream's resolvers run in.
func TestOverlappingExtensionsResolveAsVideo(t *testing.T) {
	for _, p := range []string{"/m/x.3gp", "/m/x.rm", "/m/x.nsv", "/m/x.strm"} {
		if !IsVideoFile(p) {
			t.Errorf("%s should resolve as video", p)
		}
	}
}

func TestCleanDateTime(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantYear int
	}{
		// The ordinary shapes.
		{"Arrival (2016)", "Arrival", 2016},
		{"Arrival.2016", "Arrival", 2016},
		{"Arrival_2016", "Arrival", 2016},
		{"Arrival [2016]", "Arrival", 2016},
		{"Arrival - 2016", "Arrival", 2016},
		{"Blade Runner 2049 (2017)", "Blade Runner 2049", 2017},

		// Greedy leading group: the RIGHTMOST qualifying year wins. Getting
		// this backwards renames the film to "" and dates it 2001.
		{"2001 A Space Odyssey (1968)", "2001 A Space Odyssey", 1968},

		// A longer number is not a year -- the (?![0-9]+) alternative.
		{"Movie 20161", "Movie 20161", 0},

		// A date is not a release year -- the (?!\W\d\d\W\d\d) alternative.
		// Treating the first component as a year would retitle the recording.
		{"Recording 2016.01.02", "Recording 2016.01.02", 0},

		// No year at all.
		{"Arrival", "Arrival", 0},
		{"", "", 0},
		{"2016", "2016", 0},

		// Years outside 19xx/20xx are not years.
		{"Movie (1899)", "Movie (1899)", 0},
		{"Movie (2116)", "Movie (2116)", 0},

		// A separator is required between title and year.
		{"Movie2016", "Movie2016", 0},

		// A dangling separator survives, because a SPACE is not in upstream's
		// excluded class [^_\,\.\(\)\[\]\-]: the leading group is allowed to
		// end with one, and .TrimEnd() then strips only the whitespace. So the
		// dash stays. This looks like a bug and is faithful to upstream --
		// "fixing" it here would silently diverge item names from Jellyfin's.
		{"Movie - (2016)", "Movie -", 2016},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := CleanDateTime(c.in)
			if got.Name != c.wantName {
				t.Errorf("Name = %q, want %q", got.Name, c.wantName)
			}
			if got.Year != c.wantYear {
				t.Errorf("Year = %d, want %d", got.Year, c.wantYear)
			}
			if got.HasYear != (c.wantYear != 0) {
				t.Errorf("HasYear = %v, want %v", got.HasYear, c.wantYear != 0)
			}
		})
	}
}

func TestCleanString(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantOK  bool
		whatFor string
	}{
		{"Arrival 1080p", "Arrival", true, "a single release tag"},
		{"Arrival.BluRay", "Arrival", true, "tag after a dot"},
		{"Arrival [1080p]", "Arrival", true, "bracketed tag"},
		{"Arrival - 12", "Arrival", true, "trailing number"},
		{"Arrival-trailer", "Arrival", true, "extra suffix"},
		{"Arrival.sample", "Arrival", true, "sample suffix"},
		{"[Group] Arrival", "Arrival", true, "leading bracket tag"},
		// Nothing to clean.
		{"Arrival", "", false, "a name with no clutter"},
		{"", "", false, "the empty string"},
	}
	for _, c := range cases {
		t.Run(c.whatFor, func(t *testing.T) {
			got, ok := CleanString(c.in)
			if ok != c.wantOK {
				t.Fatalf("CleanString(%q) ok = %v, want %v (got %q)", c.in, ok, c.wantOK, got)
			}
			if ok && got != c.want {
				t.Errorf("CleanString(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The expressions are applied in sequence, each feeding the next, which is what
// strips a whole scene-release suffix rather than only its last tag.
func TestCleanStringIsIterative(t *testing.T) {
	got, ok := CleanString("Arrival.1080p.BluRay.x264")
	if !ok {
		t.Fatal("a scene release should clean")
	}
	if got != "Arrival" {
		t.Errorf("CleanString = %q, want Arrival -- the expressions must be applied in sequence", got)
	}
}

// "[foo].mkv" must not clean to "mkv": that is what the negative lookahead in
// the leading-bracket expression prevents, and it has no RE2 equivalent.
func TestCleanStringLeadingBracketDoesNotEatTheExtension(t *testing.T) {
	if got, ok := CleanString("[foo].mkv"); ok && got == "mkv" {
		t.Error("a leading bracket followed only by an extension must not be stripped")
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		path     string
		isDir    bool
		wantName string
		wantYear int
	}{
		{"/mnt/alberca/Movies/Arrival (2016).mkv", false, "Arrival", 2016},
		{"/mnt/alberca/Movies/Arrival (2016)/Arrival (2016).mkv", false, "Arrival", 2016},
		{"/mnt/alberca/Movies/Arrival (2016)", true, "Arrival", 2016},
		{"/mnt/alberca/Movies/Arrival.2016.1080p.BluRay.x264.mkv", false, "Arrival", 2016},
		{"/mnt/alberca/Movies/Arrival.mkv", false, "Arrival", 0},
		// A directory keeps everything after a dot; stripping ".4" would be
		// wrong for a folder that is not a file.
		{"/mnt/alberca/Movies/Movie 4.4", true, "Movie 4.4", 0},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			got := Resolve(c.path, c.isDir)
			if got.Name != c.wantName {
				t.Errorf("Name = %q, want %q", got.Name, c.wantName)
			}
			if got.Year != c.wantYear {
				t.Errorf("Year = %d, want %d", got.Year, c.wantYear)
			}
		})
	}
}

// A title must never come back empty, or the client shows a blank row.
func TestResolveNeverReturnsAnEmptyName(t *testing.T) {
	for _, p := range []string{"/m/1999.mkv", "/m/2016.mkv", "/m/.mkv"} {
		if got := Resolve(p, false); got.Name == "" {
			t.Errorf("Resolve(%q) produced an empty name", p)
		}
	}
}

func TestParseStack(t *testing.T) {
	cases := []struct {
		in        string
		wantTitle string
		wantNum   string
		wantOK    bool
	}{
		{"Movie cd1.mkv", "Movie", "1", true},
		{"Movie CD2.mkv", "Movie", "2", true},
		{"Movie.part1.mkv", "Movie", "1", true},
		{"Movie-pt2.mkv", "Movie", "2", true},
		{"Movie disc1.mkv", "Movie", "1", true},
		{"Movie disk2.mkv", "Movie", "2", true},
		{"Movie cda.mkv", "Movie", "a", true},
		// Not a stack.
		{"Movie.mkv", "", "", false},
		{"Arrival (2016).mkv", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, ok := ParseStack(c.in)
			if ok != c.wantOK {
				t.Fatalf("ParseStack(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			}
			if !ok {
				return
			}
			if got.Title != c.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, c.wantTitle)
			}
			if got.Number != c.wantNum {
				t.Errorf("Number = %q, want %q", got.Number, c.wantNum)
			}
		})
	}
}

// The parts of one film must share a title, or a two-CD movie shows up twice.
func TestParseStackGroupsParts(t *testing.T) {
	a, ok1 := ParseStack("Movie cd1.mkv")
	b, ok2 := ParseStack("Movie cd2.mkv")
	if !ok1 || !ok2 {
		t.Fatal("both parts should parse")
	}
	if a.Title != b.Title {
		t.Errorf("parts disagree on title: %q vs %q", a.Title, b.Title)
	}
	if a.Number == b.Number {
		t.Error("parts should have different numbers")
	}
}

func TestIsIgnoredDir(t *testing.T) {
	for _, n := range []string{
		"metadata", "Metadata", "extrafanart", "extrathumbs", ".actors",
		"lost+found", "sample", "@eaDir", ".git", ".Trash-1000", "ps3_update",
	} {
		if !IsIgnoredDir(n) {
			t.Errorf("IsIgnoredDir(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"Movies", "Arrival (2016)", "Season 1", "Music"} {
		if IsIgnoredDir(n) {
			t.Errorf("IsIgnoredDir(%q) = true, want false", n)
		}
	}
}

// The sample case is the one that matters: a sample clip is a valid video by
// extension and sits beside the feature, so without this it is indexed as a
// second movie with nearly the same name.
func TestIsIgnoredFile(t *testing.T) {
	for _, n := range []string{
		"sample.mkv", "Sample.avi", "Arrival.sample.mkv", "small.jpg",
		"albumart.jpg", ".DS_Store", "Thumbs.db", ".hidden.mkv",
	} {
		if !IsIgnoredFile(n) {
			t.Errorf("IsIgnoredFile(%q) = false, want true", n)
		}
	}
	for _, n := range []string{
		"Arrival (2016).mkv", "01 Track.flac", "Movie.mp4", "resample.mkv",
	} {
		if IsIgnoredFile(n) {
			t.Errorf("IsIgnoredFile(%q) = true, want false", n)
		}
	}
}

// Extras directories were the single biggest source of bogus items when this
// was first run against a real library: 29 of 97 movies were called
// "Featurettes", one per film that shipped such a folder.
func TestIsExtrasDir(t *testing.T) {
	for _, n := range []string{
		"Featurettes", "featurettes", "Extras", "Behind The Scenes",
		"Deleted Scenes", "Interviews", "Trailers", "Shorts", "Scenes",
		"Samples", "Backdrops", "Specials",
	} {
		if !IsExtrasDir(n) {
			t.Errorf("IsExtrasDir(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"Arrival (2016)", "Season 1", "Disc 1", "Movies"} {
		if IsExtrasDir(n) {
			t.Errorf("IsExtrasDir(%q) = true, want false", n)
		}
	}
}

// A trailer sitting beside the feature must not become a second copy of it.
func TestIsExtraFile(t *testing.T) {
	for _, n := range []string{
		"ONCE_Trailer.mkv", "Movie-trailer.mkv", "Movie.trailer.mkv",
		"Movie sample.mkv", "Movie-featurette.mkv", "Movie-deleted.mkv",
		"Movie-interview.mkv",
	} {
		if !IsExtraFile(n) {
			t.Errorf("IsExtraFile(%q) = false, want true", n)
		}
	}
	for _, n := range []string{
		"Arrival (2016).mkv", "Movie.mkv", "Trailer Park Boys (2001).mkv",
	} {
		if IsExtraFile(n) {
			t.Errorf("IsExtraFile(%q) = true, want false", n)
		}
	}
}
