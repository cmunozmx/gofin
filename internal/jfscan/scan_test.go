package jfscan

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gofin/internal/jfid"
)

// tree builds a directory layout from a list of relative paths. A path ending
// in "/" is a directory; anything else is a file with placeholder content.
func tree(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if p[len(p)-1] == '/' {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func scanMovies(t *testing.T, root string) Result {
	t.Helper()
	res, err := Scan(Options{Root: root, CollectionType: "movies", ParentID: jfid.Nil})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func byName(res Result) map[string]Item {
	m := map[string]Item{}
	for _, it := range res.Items {
		m[it.Name] = it
	}
	return m
}

// -------------------------------------------------------------------- movies

func TestScanFlatMovies(t *testing.T) {
	root := tree(t,
		"Arrival (2016).mkv",
		"Blade Runner 2049 (2017).mp4",
		"Dune.2021.1080p.BluRay.x264.mkv",
	)
	res := scanMovies(t, root)
	if len(res.Items) != 3 {
		t.Fatalf("got %d items, want 3: %+v", len(res.Items), res.Items)
	}

	items := byName(res)
	for _, c := range []struct {
		name string
		year int
	}{
		{"Arrival", 2016},
		{"Blade Runner 2049", 2017},
		{"Dune", 2021},
	} {
		it, ok := items[c.name]
		if !ok {
			t.Errorf("no item named %q; got %v", c.name, keys(items))
			continue
		}
		if it.Year != c.year {
			t.Errorf("%s: Year = %d, want %d", c.name, it.Year, c.year)
		}
		if it.Kind != KindMovie {
			t.Errorf("%s: Kind = %s, want Movie", c.name, it.Kind)
		}
		if it.ID.IsZero() {
			t.Errorf("%s: no ID", c.name)
		}
	}
}

// In the folder-per-film layout the DIRECTORY names the movie. The file inside
// is often a far worse scene name, and trusting it produces a library full of
// titles like "Arrival.2016.1080p.BluRay.x264".
func TestScanFolderPerMovieUsesTheDirectoryName(t *testing.T) {
	root := tree(t, "Arrival (2016)/Arrival.2016.1080p.BluRay.x264-GROUP.mkv")
	res := scanMovies(t, root)

	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(res.Items), res.Items)
	}
	if res.Items[0].Name != "Arrival" {
		t.Errorf("Name = %q, want Arrival (the folder should win)", res.Items[0].Name)
	}
	if res.Items[0].Year != 2016 {
		t.Errorf("Year = %d, want 2016", res.Items[0].Year)
	}
}

// A two-part film is one item, not two.
func TestScanStacksMultiPartMovies(t *testing.T) {
	root := tree(t,
		"Movie cd1.mkv",
		"Movie cd2.mkv",
	)
	res := scanMovies(t, root)
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1 stacked item: %+v", len(res.Items), res.Items)
	}
	it := res.Items[0]
	if it.Name != "Movie" {
		t.Errorf("Name = %q, want Movie (without the cd1 suffix)", it.Name)
	}
	if len(it.AdditionalParts) != 1 {
		t.Errorf("AdditionalParts = %v, want the second part", it.AdditionalParts)
	}
	if filepath.Base(it.Path) != "Movie cd1.mkv" {
		t.Errorf("primary part = %q, want cd1", it.Path)
	}
}

// Sample clips are valid video files sitting beside the feature. Without the
// ignore rules they become a second, nearly identically named movie.
func TestScanSkipsSamplesAndJunk(t *testing.T) {
	root := tree(t,
		"Arrival (2016)/Arrival (2016).mkv",
		"Arrival (2016)/sample.mkv",
		"Arrival (2016)/Arrival.sample.mkv",
		"Arrival (2016)/poster.jpg",
		"Arrival (2016)/Arrival.nfo",
		"Arrival (2016)/extrafanart/fanart1.mkv",
		"Arrival (2016)/.hidden.mkv",
		"metadata/junk.mkv",
		"@eaDir/thumb.mkv",
	)
	res := scanMovies(t, root)
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(res.Items), names(res))
	}
	if res.Items[0].Name != "Arrival" {
		t.Errorf("Name = %q, want Arrival", res.Items[0].Name)
	}
}

func TestScanIgnoresNonMedia(t *testing.T) {
	root := tree(t, "readme.txt", "cover.jpg", "notes.nfo", "subs.srt")
	res := scanMovies(t, root)
	if len(res.Items) != 0 {
		t.Errorf("got %d items, want none: %v", len(res.Items), names(res))
	}
}

func TestScanEmptyLibrary(t *testing.T) {
	res := scanMovies(t, t.TempDir())
	if len(res.Items) != 0 {
		t.Errorf("got %d items, want none", len(res.Items))
	}
}

func TestScanMissingRoot(t *testing.T) {
	_, err := Scan(Options{Root: filepath.Join(t.TempDir(), "nope"), CollectionType: "movies"})
	if err == nil {
		t.Error("scanning a missing root should fail")
	}
}

func TestScanRootIsAFile(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "file.mkv")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(Options{Root: f, CollectionType: "movies"}); err == nil {
		t.Error("scanning a file as a root should fail")
	}
}

// Item IDs must be stable across scans, or every rescan invalidates client
// caches, favourites and deep links.
func TestItemIDsAreDeterministic(t *testing.T) {
	root := tree(t, "Arrival (2016).mkv")
	a := scanMovies(t, root)
	b := scanMovies(t, root)

	if len(a.Items) != 1 || len(b.Items) != 1 {
		t.Fatal("expected one item from each scan")
	}
	if a.Items[0].ID != b.Items[0].ID {
		t.Errorf("ID changed between scans: %s vs %s", a.Items[0].ID, b.Items[0].ID)
	}
}

// The ID is derived from the path and the .NET type name, so two different
// kinds at the same path are different items.
func TestNewIDVariesByKind(t *testing.T) {
	p := "/mnt/alberca/Movies/Arrival (2016).mkv"
	if NewID(p, KindMovie) == NewID(p, KindAudio) {
		t.Error("kind must participate in the ID derivation")
	}
	if NewID(p, KindMovie).IsZero() {
		t.Error("ID is zero")
	}
	if DotNetType(KindMovie) != "MediaBrowser.Controller.Entities.Movies.Movie" {
		t.Errorf("Movie type name = %q", DotNetType(KindMovie))
	}
}

// Every top-level item hangs off the library.
func TestScanSetsParentToTheLibrary(t *testing.T) {
	lib := jfid.MustParse("11111111111111111111111111111111")
	root := tree(t, "Arrival (2016).mkv")
	res, err := Scan(Options{Root: root, CollectionType: "movies", ParentID: lib})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].ParentID != lib {
		t.Errorf("ParentId = %s, want the library %s", res.Items[0].ParentID, lib)
	}
}

// A leading article moves to the back for sorting, so "The Matrix" files under M.
func TestSortNameDropsLeadingArticle(t *testing.T) {
	root := tree(t, "The Matrix (1999).mkv", "An Education (2009).mkv", "Arrival (2016).mkv")
	res := scanMovies(t, root)

	got := map[string]string{}
	for _, it := range res.Items {
		got[it.Name] = it.SortName
	}
	if got["The Matrix"] != "matrix" {
		t.Errorf("SortName for The Matrix = %q, want matrix", got["The Matrix"])
	}
	if got["An Education"] != "education" {
		t.Errorf("SortName for An Education = %q, want education", got["An Education"])
	}
}

// A symlink pointing back up must not turn the scan into a hang.
// A symlinked film folder is still a film folder. Libraries are often
// assembled out of links to media living on another volume, and os.ReadDir
// reports a link as a link rather than as the directory it points at.
func TestScanFollowsSymlinkedMovieDirectory(t *testing.T) {
	root := tree(t,
		"Movies/Arrival (2016) [1080p]/arrival.2016.mkv",
		"media/Creep 2 (2017) [1080p] [WEBRip] [5.1] [YTS.MX]/Creep.2.2017.1080p.WEBRip.x264.AAC5.1-[YTS.MX].mp4",
	)
	link := filepath.Join(root, "Movies", "Creep 2 (2017) [1080p]")
	if err := os.Symlink(filepath.Join(root, "media", "Creep 2 (2017) [1080p] [WEBRip] [5.1] [YTS.MX]"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := scanMovies(t, filepath.Join(root, "Movies"))
	items := byName(res)

	// The link's own name is the clean one, so it wins the title just as a
	// real directory's would -- not the scene name of the file inside it.
	it, ok := items["Creep 2"]
	if !ok {
		t.Fatalf("the symlinked movie is missing; got %v", keys(items))
	}
	if it.Year != 2017 {
		t.Errorf("Year = %d, want 2017", it.Year)
	}
	if it.Container != "mp4" {
		t.Errorf("Container = %q, want mp4", it.Container)
	}
	if _, ok := items["Arrival"]; !ok {
		t.Errorf("the plain directory should still scan; got %v", keys(items))
	}
}

// A symlink pointing at a directory already scanned under its real name must
// not produce the film twice.
func TestScanDoesNotDuplicateAliasedDirectory(t *testing.T) {
	root := tree(t, "Movies/Arrival (2016)/arrival.2016.mkv")
	if err := os.Symlink(filepath.Join(root, "Movies", "Arrival (2016)"), filepath.Join(root, "Movies", "Arrival alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := scanMovies(t, filepath.Join(root, "Movies"))
	if len(res.Items) != 1 {
		t.Errorf("got %d items, want 1: %v", len(res.Items), names(res))
	}
}

func TestScanSurvivesSymlinkLoop(t *testing.T) {
	root := tree(t, "Movies/Arrival (2016).mkv")
	loop := filepath.Join(root, "Movies", "loop")
	if err := os.Symlink(root, loop); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	done := make(chan Result, 1)
	go func() {
		res, err := Scan(Options{Root: root, CollectionType: "movies"})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()

	select {
	case res := <-done:
		if len(res.Items) == 0 {
			t.Error("the scan should still find the movie")
		}
	case <-timeout():
		t.Fatal("scan did not terminate: the symlink loop was followed")
	}
}

// --------------------------------------------------------------------- music

func scanMusicLib(t *testing.T, root string) Result {
	t.Helper()
	res, err := Scan(Options{Root: root, CollectionType: "music", ParentID: jfid.Nil})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestScanMusicHierarchy(t *testing.T) {
	root := tree(t,
		"Radiohead/OK Computer/01 Airbag.flac",
		"Radiohead/OK Computer/02 Paranoid Android.flac",
		"Radiohead/In Rainbows/01 15 Step.flac",
	)
	res := scanMusicLib(t, root)

	var artists, albums, tracks int
	for _, it := range res.Items {
		switch it.Kind {
		case KindMusicArtist:
			artists++
		case KindMusicAlbum:
			albums++
		case KindAudio:
			tracks++
		}
	}
	if artists != 1 {
		t.Errorf("got %d artists, want 1", artists)
	}
	if albums != 2 {
		t.Errorf("got %d albums, want 2", albums)
	}
	if tracks != 3 {
		t.Errorf("got %d tracks, want 3", tracks)
	}
}

func TestScanMusicTrackMetadata(t *testing.T) {
	root := tree(t, "Radiohead/OK Computer/02 Paranoid Android.flac")
	res := scanMusicLib(t, root)

	var track *Item
	for i := range res.Items {
		if res.Items[i].Kind == KindAudio {
			track = &res.Items[i]
		}
	}
	if track == nil {
		t.Fatal("no track found")
	}
	if track.Name != "Paranoid Android" {
		t.Errorf("Name = %q, want Paranoid Android", track.Name)
	}
	if track.IndexNumber != 2 {
		t.Errorf("IndexNumber = %d, want 2", track.IndexNumber)
	}
	if track.Album != "OK Computer" {
		t.Errorf("Album = %q, want OK Computer", track.Album)
	}
	if track.AlbumArtist != "Radiohead" {
		t.Errorf("AlbumArtist = %q, want Radiohead", track.AlbumArtist)
	}
	if track.Container != "flac" {
		t.Errorf("Container = %q, want flac", track.Container)
	}
}

// Tracks must sort by position. An album played in alphabetical order is the
// wrong album.
func TestScanMusicTracksSortByNumber(t *testing.T) {
	root := tree(t,
		"A/Album/01 Zebra.flac",
		"A/Album/02 Apple.flac",
	)
	res := scanMusicLib(t, root)

	var tracks []Item
	for _, it := range res.Items {
		if it.Kind == KindAudio {
			tracks = append(tracks, it)
		}
	}
	if len(tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(tracks))
	}
	// Result is sorted by SortName, so track 1 must come first despite "Zebra".
	if tracks[0].Name != "Zebra" {
		t.Errorf("first track = %q, want Zebra (track 1)", tracks[0].Name)
	}
}

// A "Disc 1" directory is a subdivision of the album, not an album of its own.
func TestScanMusicFoldsDiscDirectories(t *testing.T) {
	root := tree(t,
		"Artist/Album/Disc 1/01 One.flac",
		"Artist/Album/Disc 2/01 Two.flac",
	)
	res := scanMusicLib(t, root)

	var albums int
	for _, it := range res.Items {
		if it.Kind == KindMusicAlbum {
			albums++
			if it.Name != "Album" {
				t.Errorf("album name = %q, want Album", it.Name)
			}
		}
	}
	if albums != 1 {
		t.Errorf("got %d albums, want 1 -- disc directories must fold in", albums)
	}
}

// Music with no artist/album folders still resolves to tracks.
func TestScanMusicLooseFiles(t *testing.T) {
	root := tree(t, "Some Track.flac")
	res := scanMusicLib(t, root)

	var tracks int
	for _, it := range res.Items {
		if it.Kind == KindAudio {
			tracks++
		}
	}
	if tracks != 1 {
		t.Errorf("got %d tracks, want 1", tracks)
	}
}

func TestScanMusicIgnoresVideoAndArt(t *testing.T) {
	root := tree(t,
		"Artist/Album/01 Track.flac",
		"Artist/Album/cover.jpg",
		"Artist/Album/albumart.jpg",
		"Artist/Album/video.mkv",
	)
	res := scanMusicLib(t, root)

	for _, it := range res.Items {
		if it.Kind == KindAudio && it.Name != "Track" {
			t.Errorf("unexpected track %q", it.Name)
		}
	}
}

func TestTrackNumber(t *testing.T) {
	cases := []struct {
		in    string
		num   int
		title string
	}{
		{"01 Airbag.flac", 1, "Airbag"},
		{"02 - Paranoid Android.flac", 2, "Paranoid Android"},
		{"1 Track.flac", 1, "Track"},
		{"10.Track.flac", 10, "Track"},
		// Four or more leading digits is a title, not a track number -- a year
		// or a catalogue number.
		{"2016 Something.flac", 0, "2016 Something"},
		{"Airbag.flac", 0, "Airbag"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			n, title := trackNumber(c.in)
			if n != c.num {
				t.Errorf("number = %d, want %d", n, c.num)
			}
			if title != c.title {
				t.Errorf("title = %q, want %q", title, c.title)
			}
		})
	}
}

func TestIsDiscDir(t *testing.T) {
	for _, n := range []string{"Disc 1", "disc1", "CD2", "Disk 3", "Volume 1", "vol 2"} {
		if !isDiscDir(n) {
			t.Errorf("isDiscDir(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"Album", "Disc", "Discovery", "CD Single", "Volume"} {
		if isDiscDir(n) {
			t.Errorf("isDiscDir(%q) = true, want false", n)
		}
	}
}

// An unreadable subdirectory is recorded and the rest of the library still
// scans; one bad directory must not cost the whole library.
func TestScanContinuesPastUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	root := tree(t, "Good/Arrival (2016).mkv", "Bad/")
	bad := filepath.Join(root, "Bad")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(bad, 0o755) })

	res := scanMovies(t, root)
	if len(res.Items) != 1 {
		t.Errorf("got %d items, want the one readable movie", len(res.Items))
	}
	if len(res.Errors) == 0 {
		t.Error("the unreadable directory should be recorded in Errors")
	}
}

// ------------------------------------------------------------------- helpers

func keys(m map[string]Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func names(r Result) []string {
	out := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, it.Name)
	}
	return out
}

func timeout() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Second)
		close(ch)
	}()
	return ch
}

// --------------------------------------- regressions from a real 66-film library

// Scanning a real library first produced 97 movies from 66 folders, 29 of them
// called "Featurettes". An extras directory is not a film folder.
func TestScanSkipsExtrasDirectories(t *testing.T) {
	root := tree(t,
		"Munich (2005)/Munich.2005.1080p.mkv",
		"Munich (2005)/Featurettes/The Making Of.mkv",
		"Munich (2005)/Featurettes/Interview.mkv",
		"Alien (1979)/Alien.mkv",
		"Alien (1979)/Deleted Scenes/cut.mkv",
		"Alien (1979)/Behind The Scenes/bts.mkv",
	)
	res := scanMovies(t, root)

	if len(res.Items) != 2 {
		t.Fatalf("got %d items, want 2: %v", len(res.Items), names(res))
	}
	for _, it := range res.Items {
		if it.Name == "Featurettes" || it.Name == "Deleted Scenes" || it.Name == "Behind The Scenes" {
			t.Errorf("an extras directory was indexed as a film: %q", it.Name)
		}
	}
}

// The ONCE case: a trailer beside the feature became a second item with the
// same folder-derived name.
func TestScanSkipsTrailerBesideFeature(t *testing.T) {
	root := tree(t,
		"ONCE (2006) [DVD] [720p]/ONCE_2007.mkv",
		"ONCE (2006) [DVD] [720p]/ONCE_Trailer.mkv",
	)
	res := scanMovies(t, root)

	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1 (the trailer is an extra): %v", len(res.Items), names(res))
	}
	if res.Items[0].Name != "ONCE" {
		t.Errorf("Name = %q, want ONCE", res.Items[0].Name)
	}
}

// A folder holding several distinct films must not yield that many items all
// sharing the folder's name -- there each file names itself.
func TestScanFolderWithSeveralFilmsNamesEachByFile(t *testing.T) {
	root := tree(t,
		"Star Wars Trilogy - Project 4K/4k77_v1.mkv",
		"Star Wars Trilogy - Project 4K/4k80_v1.mkv",
		"Star Wars Trilogy - Project 4K/4k83_v2.mkv",
	)
	res := scanMovies(t, root)

	if len(res.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(res.Items))
	}
	seen := map[string]bool{}
	for _, it := range res.Items {
		if seen[it.Name] {
			t.Errorf("duplicate name %q: a multi-film folder must not name every file after itself", it.Name)
		}
		seen[it.Name] = true
	}
}

// The single-feature case must still take the folder's name, which is the whole
// point of the folder-per-film layout.
func TestScanSingleFeatureStillUsesFolderName(t *testing.T) {
	root := tree(t,
		"Arrival (2016)/Arrival.2016.1080p.BluRay.x264-GROUP.mkv",
		"Arrival (2016)/Arrival-trailer.mkv",
		"Arrival (2016)/Featurettes/making-of.mkv",
	)
	res := scanMovies(t, root)
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1: %v", len(res.Items), names(res))
	}
	if res.Items[0].Name != "Arrival" {
		t.Errorf("Name = %q, want Arrival", res.Items[0].Name)
	}
}

// A multi-part film in its own folder is still one item, and still takes the
// folder's name.
func TestScanStackInFolderIsOneFeature(t *testing.T) {
	root := tree(t,
		"Movie (2001)/Movie cd1.mkv",
		"Movie (2001)/Movie cd2.mkv",
	)
	res := scanMovies(t, root)
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1: %v", len(res.Items), names(res))
	}
	if res.Items[0].Name != "Movie" || res.Items[0].Year != 2001 {
		t.Errorf("got %q (%d), want Movie (2001)", res.Items[0].Name, res.Items[0].Year)
	}
}

// ------------------------------------------------------------------ artwork

func TestFindPrimaryImage(t *testing.T) {
	root := tree(t,
		"Arrival (2016)/Arrival (2016).mkv",
		"Arrival (2016)/poster.jpg",
	)
	res := scanMovies(t, root)
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(res.Items))
	}
	if res.Items[0].PrimaryImage == "" {
		t.Error("poster.jpg beside the film was not found")
	}
}

// Release groups drop advertising banners beside the media. Showing one as a
// film's poster is worse than showing none, so only conventional names count.
func TestArtworkIgnoresReleaseGroupBanners(t *testing.T) {
	root := tree(t,
		"Passengers (2016) [YTS.AG]/Passengers.mkv",
		"Passengers (2016) [YTS.AG]/WWW.YTS.AG.jpg",
	)
	res := scanMovies(t, root)
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(res.Items))
	}
	if got := res.Items[0].PrimaryImage; got != "" {
		t.Errorf("PrimaryImage = %q, want none -- that file is an advert, not a poster", got)
	}
}

// The conventional names appear capitalised in the wild, and media usually
// lives on a case-sensitive filesystem.
func TestArtworkIsCaseInsensitive(t *testing.T) {
	root := tree(t,
		"Alien (1979)/Alien.mkv",
		"Alien (1979)/Folder.JPG",
	)
	res := scanMovies(t, root)
	if res.Items[0].PrimaryImage == "" {
		t.Error("Folder.JPG was not matched against the conventional name")
	}
}

func TestArtworkForAlbums(t *testing.T) {
	root := tree(t,
		"Radiohead/OK Computer/01 Airbag.flac",
		"Radiohead/OK Computer/cover.jpg",
	)
	res := scanMusicLib(t, root)
	var album *Item
	for i := range res.Items {
		if res.Items[i].Kind == KindMusicAlbum {
			album = &res.Items[i]
		}
	}
	if album == nil {
		t.Fatal("no album")
	}
	if album.PrimaryImage == "" {
		t.Error("cover.jpg in the album directory was not found")
	}
}

func TestNoArtworkLeavesPrimaryImageEmpty(t *testing.T) {
	root := tree(t, "Arrival (2016)/Arrival (2016).mkv")
	res := scanMovies(t, root)
	if got := res.Items[0].PrimaryImage; got != "" {
		t.Errorf("PrimaryImage = %q, want empty", got)
	}
}
