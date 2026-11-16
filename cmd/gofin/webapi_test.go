package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gofin/internal/jfid"
)

// The web interface cannot draw its home screen without this: it looks the
// signed-in user up by id rather than asking for /Users/Me.
func TestUserByID(t *testing.T) {
	e := newTestEnv(t)
	viewerTok := e.tokenFor(e.viewer)
	adminTok := e.tokenFor(e.admin)

	t.Run("own record, dashless and dashed", func(t *testing.T) {
		id := e.viewer.ID.String()
		dashed := id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
		for _, ref := range []string{id, dashed} {
			var got UserDto
			w := e.do(t, "GET", "/Users/"+ref, viewerTok, nil)
			if w.Code != 200 {
				t.Fatalf("GET /Users/%s = %d, want 200", ref, w.Code)
			}
			decodeJSON(t, w, &got)
			if got.ID != e.viewer.ID || got.Name != "viewer" {
				t.Errorf("got user %s %q, want the viewer", got.ID, got.Name)
			}
		}
	})

	t.Run("the literal routes still win", func(t *testing.T) {
		var me UserDto
		decodeJSON(t, e.do(t, "GET", "/Users/Me", viewerTok, nil), &me)
		if me.ID != e.viewer.ID {
			t.Errorf("/Users/Me returned %s", me.ID)
		}
		if w := e.do(t, "GET", "/Users/Public", "", nil); w.Code != 200 {
			t.Errorf("/Users/Public = %d, want 200 without a token", w.Code)
		}
	})

	// Upstream lets any signed-in user read any other user's record, policy
	// and all. gofin keeps the rule the rest of its per-user routes follow:
	// somebody else's record is an administrator's to read.
	t.Run("another user", func(t *testing.T) {
		other := "/Users/" + e.admin.ID.String()
		if w := e.do(t, "GET", other, viewerTok, nil); w.Code != 403 {
			t.Errorf("viewer reading the admin = %d, want 403", w.Code)
		}
		var got UserDto
		w := e.do(t, "GET", "/Users/"+e.viewer.ID.String(), adminTok, nil)
		if w.Code != 200 {
			t.Fatalf("admin reading the viewer = %d, want 200", w.Code)
		}
		decodeJSON(t, w, &got)
		if got.ID != e.viewer.ID {
			t.Errorf("admin got %s, want the viewer", got.ID)
		}
	})

	t.Run("errors", func(t *testing.T) {
		for _, c := range []struct {
			name, path, token string
			want              int
		}{
			{"no token", "/Users/" + e.viewer.ID.String(), "", 401},
			{"malformed id", "/Users/not-a-guid", viewerTok, 400},
			{"unknown user, as admin", "/Users/00000000000000000000000000000001", adminTok, 404},
		} {
			if w := e.do(t, "GET", c.path, c.token, nil); w.Code != c.want {
				t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
			}
		}
	})
}

// The interface times a download of this to choose a streaming bitrate. What
// matters is the byte count and that the body does not compress.
func TestBitrateTest(t *testing.T) {
	e := newTestEnv(t)
	tok := e.tokenFor(e.viewer)

	w := e.do(t, "GET", "/Playback/BitrateTest?Size=5000", tok, nil)
	if w.Code != 200 || w.Body.Len() != 5000 {
		t.Fatalf("Size=5000: status %d with %d bytes, want 200 with 5000", w.Code, w.Body.Len())
	}
	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Length"); got != "5000" {
		t.Errorf("Content-Length = %q, want 5000", got)
	}
	if bytes.Equal(w.Body.Bytes(), make([]byte, 5000)) {
		t.Error("body is all zeroes; it has to be incompressible")
	}

	if w := e.do(t, "GET", "/Playback/BitrateTest", tok, nil); w.Body.Len() != 102400 {
		t.Errorf("default size = %d bytes, want upstream's 102400", w.Body.Len())
	}
	for _, bad := range []string{"0", "-1", "100000001", "abc"} {
		if w := e.do(t, "GET", "/Playback/BitrateTest?size="+bad, tok, nil); w.Code != 400 {
			t.Errorf("size=%s = %d, want 400", bad, w.Code)
		}
	}
	if w := e.do(t, "GET", "/Playback/BitrateTest", "", nil); w.Code != 401 {
		t.Errorf("no token = %d, want 401", w.Code)
	}
}

func getQuery(t *testing.T, e *testEnv, tok, path string) QueryResult[BaseItemDto] {
	t.Helper()
	w := e.do(t, "GET", path, tok, nil)
	if w.Code != 200 {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	var res QueryResult[BaseItemDto]
	decodeJSON(t, w, &res)
	if res.Items == nil {
		t.Fatalf("GET %s: Items is null, want an array", path)
	}
	return res
}

// The music library's Artists and Album Artists tabs, and the artist half of
// search. Artists are real here: the scanner makes one per artist folder.
func TestArtists(t *testing.T) {
	q := newQueryEnv(t)
	for _, path := range []string{"/Artists", "/Artists/AlbumArtists"} {
		res := getQuery(t, q.testEnv, q.tok, path)
		if len(res.Items) != 1 || res.Items[0].Name != "Radiohead" || res.Items[0].Type != "MusicArtist" {
			t.Fatalf("%s = %+v, want the one artist", path, res.Items)
		}
		if res.TotalRecordCount != 1 {
			t.Errorf("%s TotalRecordCount = %d, want 1", path, res.TotalRecordCount)
		}
	}

	// Restricted to a library: the music library has the artist, the film
	// library has none.
	var music, movies string
	for _, v := range q.items("") {
		switch v.CollectionType {
		case "music":
			music = v.ID.String()
		case "movies":
			movies = v.ID.String()
		}
	}
	if n := len(getQuery(t, q.testEnv, q.tok, "/Artists?ParentId="+music).Items); n != 1 {
		t.Errorf("artists in the music library = %d, want 1", n)
	}
	if n := len(getQuery(t, q.testEnv, q.tok, "/Artists?parentId="+movies).Items); n != 0 {
		t.Errorf("artists in the film library = %d, want 0", n)
	}

	// The search page's query, in its own spelling.
	if n := len(getQuery(t, q.testEnv, q.tok, "/Artists?limit=100&searchTerm=radio&enableTotalRecordCount=false").Items); n != 1 {
		t.Errorf("searchTerm=radio found %d artists, want 1", n)
	}
	if n := len(getQuery(t, q.testEnv, q.tok, "/Artists?searchTerm=zzz").Items); n != 0 {
		t.Errorf("searchTerm=zzz found %d artists, want 0", n)
	}
	if w := q.do(t, "GET", "/Artists", "", nil); w.Code != 401 {
		t.Errorf("no token = %d, want 401", w.Code)
	}
}

// Lists of things the scanner does not record. Each must be an empty,
// well-formed QueryResult: the interface decodes the body before it looks at
// the status, so a problem document here is an error dialog on every library.
func TestListsOfDataNotHeldAreEmpty(t *testing.T) {
	q := newQueryEnv(t)
	film := q.id("Arrival")
	for _, path := range []string{
		"/Genres", "/Genres?ParentId=" + film, "/MusicGenres", "/Persons?searchTerm=a", "/Studios",
		"/Items/" + film + "/Similar?limit=12",
		"/Movies/" + film + "/Similar", "/Albums/" + q.id("OK Computer") + "/Similar",
		"/Artists/" + q.id("Radiohead") + "/Similar",
	} {
		res := getQuery(t, q.testEnv, q.tok, path)
		if len(res.Items) != 0 || res.TotalRecordCount != 0 {
			t.Errorf("%s = %d items, total %d; want empty", path, len(res.Items), res.TotalRecordCount)
		}
	}
	// "Similar to an item that does not exist" is not an empty list.
	if w := q.do(t, "GET", "/Items/00000000000000000000000000000001/Similar", q.tok, nil); w.Code != 404 {
		t.Errorf("similar to an unknown item = %d, want 404", w.Code)
	}
	if w := q.do(t, "GET", "/Items/nope/Similar", q.tok, nil); w.Code != 400 {
		t.Errorf("similar to a malformed id = %d, want 400", w.Code)
	}
}

// The film library's Suggestions tab. A bare array, not a QueryResult.
func TestMovieRecommendationsIsAnEmptyArray(t *testing.T) {
	q := newQueryEnv(t)
	w := q.do(t, "GET", "/Movies/Recommendations?categoryLimit=6&itemLimit=8", q.tok, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("body = %s, want []", got)
	}
}

// Every item page asks for theme media before it draws. There is none, and
// the answer is three empty lists that name the item they were asked about.
func TestThemeMedia(t *testing.T) {
	q := newQueryEnv(t)
	film := q.id("Arrival")

	type themeResult struct {
		Items            []BaseItemDto `json:"Items"`
		TotalRecordCount int           `json:"TotalRecordCount"`
		StartIndex       int           `json:"StartIndex"`
		OwnerID          jfid.GUID     `json:"OwnerId"`
	}
	var all struct {
		ThemeVideosResult     *themeResult `json:"ThemeVideosResult"`
		ThemeSongsResult      *themeResult `json:"ThemeSongsResult"`
		SoundtrackSongsResult *themeResult `json:"SoundtrackSongsResult"`
	}
	w := q.do(t, "GET", "/Items/"+film+"/ThemeMedia?inheritFromParent=true", q.tok, nil)
	if w.Code != 200 {
		t.Fatalf("ThemeMedia = %d", w.Code)
	}
	decodeJSON(t, w, &all)
	for name, r := range map[string]*themeResult{
		"ThemeVideosResult": all.ThemeVideosResult, "ThemeSongsResult": all.ThemeSongsResult,
		"SoundtrackSongsResult": all.SoundtrackSongsResult,
	} {
		if r == nil || r.Items == nil || len(r.Items) != 0 || r.TotalRecordCount != 0 {
			t.Errorf("%s = %+v, want an empty list", name, r)
		}
	}
	if all.ThemeSongsResult.OwnerID.String() != film {
		t.Errorf("ThemeSongsResult.OwnerId = %s, want the item asked about", all.ThemeSongsResult.OwnerID)
	}

	for _, part := range []string{"ThemeSongs", "ThemeVideos"} {
		var one themeResult
		w := q.do(t, "GET", "/Items/"+film+"/"+part, q.tok, nil)
		if w.Code != 200 {
			t.Fatalf("%s = %d", part, w.Code)
		}
		decodeJSON(t, w, &one)
		if one.Items == nil || len(one.Items) != 0 || one.OwnerID.String() != film {
			t.Errorf("%s = %+v", part, one)
		}
	}
	if w := q.do(t, "GET", "/Items/00000000000000000000000000000001/ThemeMedia", q.tok, nil); w.Code != 404 {
		t.Errorf("theme media of an unknown item = %d, want 404", w.Code)
	}
}

// Pre-roll videos, asked for at the start of every playback. Upstream answers
// with a QueryResult, and there are never any. The per-user spelling used to
// answer an empty 404, which the web interface reports as a failed request on
// every film; /Videos/{id}/Intros already answered with the list.
func TestIntrosIsAnEmptyList(t *testing.T) {
	q := newQueryEnv(t)
	film := q.id("Arrival")
	for _, path := range []string{
		"/Users/" + q.viewer.ID.String() + "/Items/" + film + "/Intros",
		"/Items/" + film + "/Intros",
		"/Videos/" + film + "/Intros",
	} {
		res := getQuery(t, q.testEnv, q.tok, path)
		if len(res.Items) != 0 || res.TotalRecordCount != 0 {
			t.Errorf("%s = %d items, want none", path, len(res.Items))
		}
	}
}

// The user menu asks for the user list. An administrator sees everybody;
// anybody else sees themselves, by the same rule as /Users/{id}.
func TestUserList(t *testing.T) {
	e := newTestEnv(t)
	names := func(tok string) []string {
		var users []UserDto
		w := e.do(t, "GET", "/Users", tok, nil)
		if w.Code != 200 {
			t.Fatalf("GET /Users = %d", w.Code)
		}
		decodeJSON(t, w, &users)
		var out []string
		for _, u := range users {
			out = append(out, u.Name)
		}
		return sorted(out)
	}
	if got := names(e.tokenFor(e.admin)); !equal(got, []string{"admin", "viewer"}) {
		t.Errorf("admin sees %v, want both users", got)
	}
	if got := names(e.tokenFor(e.viewer)); !equal(got, []string{"viewer"}) {
		t.Errorf("viewer sees %v, want only themselves", got)
	}
	if w := e.do(t, "GET", "/Users", "", nil); w.Code != 401 {
		t.Errorf("no token = %d, want 401", w.Code)
	}
}

// Which libraries can be folded into one "Movies"/"Shows" view on the home
// screen: upstream offers the film, show and mixed ones, by name.
func TestGroupingOptions(t *testing.T) {
	q := newQueryEnv(t)
	for _, path := range []string{"/UserViews/GroupingOptions", "/Users/" + q.viewer.ID.String() + "/GroupingOptions"} {
		var opts []struct{ Name, Id string }
		w := q.do(t, "GET", path, q.tok, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s = %d", path, w.Code)
		}
		decodeJSON(t, w, &opts)
		if len(opts) != 1 || opts[0].Name != "Movies" {
			t.Fatalf("%s = %+v, want only the film library (music cannot be grouped)", path, opts)
		}
		if len(opts[0].Id) != 32 || strings.Contains(opts[0].Id, "-") {
			t.Errorf("Id = %q, want a dashless GUID", opts[0].Id)
		}
	}
}

// The language pickers on the playback and subtitle settings pages. The list
// is upstream's own ISO 639-2 table, parsed by upstream's rules.
func TestCultures(t *testing.T) {
	e := newTestEnv(t)
	type culture struct {
		Name, DisplayName, TwoLetterISOLanguageName string
		ThreeLetterISOLanguageName                  *string
		ThreeLetterISOLanguageNames                 []string
	}
	var all []culture
	// Spelled as the interface spells it, lower case.
	w := e.do(t, "GET", "/Localization/cultures", e.tokenFor(e.viewer), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	decodeJSON(t, w, &all)
	by := map[string]culture{}
	for _, c := range all {
		by[c.DisplayName] = c
	}
	if len(all) < 400 {
		t.Errorf("%d cultures, want upstream's table of several hundred", len(all))
	}
	es := by["Spanish; Castilian"]
	if es.TwoLetterISOLanguageName != "es" || es.ThreeLetterISOLanguageName == nil || *es.ThreeLetterISOLanguageName != "spa" {
		t.Errorf("Spanish = %+v", es)
	}
	// Two three-letter codes: terminology first, bibliographic second.
	if fr := by["French"]; !equal(fr.ThreeLetterISOLanguageNames, []string{"fra", "fre"}) {
		t.Errorf("French three-letter names = %v, want [fra fre]", fr.ThreeLetterISOLanguageNames)
	}
	// No two-letter code is an empty string, not null.
	if ace := by["Achinese"]; ace.TwoLetterISOLanguageName != "" || ace.Name != "Achinese" {
		t.Errorf("Achinese = %+v", ace)
	}
}

// Display preferences are where the web interface keeps a user's settings --
// theme, language, home screen layout. Saved and then forgotten, every one of
// those reverts on the next page load.
func TestDisplayPreferencesPersist(t *testing.T) {
	e := newTestEnv(t)
	viewer, admin := e.tokenFor(e.viewer), e.tokenFor(e.admin)
	body := `{"Id":"ignored","Client":"ignored","SortBy":"DateCreated","SortOrder":"Descending",` +
		`"ShowBackdrop":false,"PrimaryImageWidth":320,"CustomPrefs":{"dashboardTheme":"light","homesection0":"resume","unset":null}}`

	if w := e.do(t, "POST", "/DisplayPreferences/usersettings?client=emby", viewer, strings.NewReader(body)); w.Code != 204 {
		t.Fatalf("POST = %d, want 204", w.Code)
	}
	get := func(tok, path string) DisplayPreferencesDto {
		var got DisplayPreferencesDto
		w := e.do(t, "GET", path, tok, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s = %d", path, w.Code)
		}
		decodeJSON(t, w, &got)
		return got
	}

	got := get(viewer, "/DisplayPreferences/usersettings?client=emby")
	if got.SortBy != "DateCreated" || got.SortOrder != "Descending" || got.ShowBackdrop || got.PrimaryImageWidth != 320 {
		t.Errorf("stored preferences did not come back: %+v", got)
	}
	if got.CustomPrefs["dashboardTheme"] != "light" || got.CustomPrefs["homesection0"] != "resume" {
		t.Errorf("CustomPrefs = %v", got.CustomPrefs)
	}
	// The key is the request's, not whatever the body claimed.
	if got.ID != "usersettings" || got.Client != "emby" {
		t.Errorf("Id/Client = %q/%q, want usersettings/emby", got.ID, got.Client)
	}

	// Keyed on user, preferences id and client: none of these see the above.
	for name, other := range map[string]DisplayPreferencesDto{
		"another user":   get(admin, "/DisplayPreferences/usersettings?client=emby"),
		"another client": get(viewer, "/DisplayPreferences/usersettings?client=other"),
		"another id":     get(viewer, "/DisplayPreferences/somelibrary?client=emby"),
	} {
		if other.SortBy != "SortName" || len(other.CustomPrefs) != 0 {
			t.Errorf("%s sees the viewer's preferences: %+v", name, other)
		}
	}

	// A null preference is one the user has not set. It is not stored as an
	// empty string, which the interface would read as a value.
	if _, ok := got.CustomPrefs["unset"]; ok {
		t.Errorf("a null custom preference came back as %q", got.CustomPrefs["unset"])
	}

	// A body that is not preferences changes nothing -- and is still answered
	// 204, like every other write a client makes in passing: an error here
	// makes some clients retry the write in a loop.
	if w := e.do(t, "POST", "/DisplayPreferences/usersettings?client=emby", viewer, strings.NewReader(`not json`)); w.Code != 204 {
		t.Errorf("malformed body = %d, want 204", w.Code)
	}
	if again := get(viewer, "/DisplayPreferences/usersettings?client=emby"); again.SortBy != "DateCreated" {
		t.Errorf("a malformed write changed the stored preferences: %+v", again)
	}
}

// They live in the database, so they outlast the process.
func TestDisplayPreferencesSurviveRestart(t *testing.T) {
	e := newTestEnv(t)
	tok := e.tokenFor(e.viewer)
	e.do(t, "POST", "/DisplayPreferences/usersettings?client=emby", tok, strings.NewReader(`{"SortBy":"Random","CustomPrefs":{}}`))

	// A second server over the same database, as after a restart.
	again := &Server{cfg: e.srv.cfg, log: e.srv.log, db: e.srv.db, store: e.srv.store, items: NewItemStore(), serverID: e.srv.serverID}
	e.handler = again.routes()
	var got DisplayPreferencesDto
	decodeJSON(t, e.do(t, "GET", "/DisplayPreferences/usersettings?client=emby", tok, nil), &got)
	if got.SortBy != "Random" {
		t.Errorf("SortBy after restart = %q, want Random", got.SortBy)
	}
}

// The playback settings page iterates this list to fill its Chromecast
// picker. Absent, the page throws before it draws; there are no receivers
// configured, so the list is empty, and it must be [] rather than null.
func TestSystemInfoListsCastReceivers(t *testing.T) {
	e := newTestEnv(t)
	var raw map[string]json.RawMessage
	decodeJSON(t, e.do(t, "GET", "/System/Info", e.tokenFor(e.admin), nil), &raw)
	if got := string(raw["CastReceiverApplications"]); got != "[]" {
		t.Errorf("CastReceiverApplications = %q, want []", got)
	}
}
