package main

import (
	"net/url"
	"sort"
	"strings"
	"testing"
)

// queryEnv is a scanned library plus the helpers these tests keep needing:
// run an item query as the viewer, and find an item's id by name.
type queryEnv struct {
	*testEnv
	t   *testing.T
	tok string
}

func newQueryEnv(t *testing.T) *queryEnv {
	t.Helper()
	e, _ := scannedEnv(t)
	return &queryEnv{testEnv: e, t: t, tok: e.tokenFor(e.viewer)}
}

func (q *queryEnv) items(query string) []BaseItemDto {
	q.t.Helper()
	w := q.do(q.t, "GET", "/Items?"+query, q.tok, nil)
	if w.Code != 200 {
		q.t.Fatalf("GET /Items?%s = %d: %s", query, w.Code, w.Body.String())
	}
	var res QueryResult[BaseItemDto]
	decodeJSON(q.t, w, &res)
	return res.Items
}

func (q *queryEnv) names(query string) []string {
	q.t.Helper()
	var out []string
	for _, it := range q.items(query) {
		out = append(out, it.Name)
	}
	return out
}

func (q *queryEnv) id(name string) string {
	q.t.Helper()
	for _, it := range q.items("Recursive=true") {
		if it.Name == name {
			return it.ID.String()
		}
	}
	q.t.Fatalf("no item named %q in the scanned library", name)
	return ""
}

func sorted(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

// A recursive query with no ParentId means "everywhere". It used to answer
// with the library folders whatever else was asked, which is how the web
// interface's Favorites tab came to list "Movies" and "Music" as favourite
// episodes.
func TestItemsRecursiveWithoutParentSearchesEverything(t *testing.T) {
	q := newQueryEnv(t)
	got := sorted(q.names("Recursive=true&IncludeItemTypes=Movie"))
	if !equal(got, []string{"Arrival", "Dune"}) {
		t.Errorf("movies everywhere = %v, want [Arrival Dune]", got)
	}
	for _, it := range q.items("Recursive=true") {
		if it.Type == "CollectionFolder" {
			t.Errorf("a recursive query returned the library folder %q", it.Name)
		}
	}
	// Without Recursive, no ParentId is still the top level: the libraries.
	if got := sorted(q.names("")); !equal(got, []string{"Movies", "Music"}) {
		t.Errorf("top level = %v, want the two libraries", got)
	}
}

func TestItemsSearchTerm(t *testing.T) {
	q := newQueryEnv(t)
	// Spelled the way the interface's search page spells it: camelCase keys.
	if got := q.names("recursive=true&searchTerm=ARRIV"); !equal(got, []string{"Arrival"}) {
		t.Errorf("searchTerm=ARRIV = %v, want [Arrival]", got)
	}
	if got := q.names("recursive=true&searchTerm=no-such-title"); len(got) != 0 {
		t.Errorf("a search for nothing returned %v", got)
	}
}

// List parameters arrive comma-separated from the interface's older code and
// as repeated keys from its newer code. Both mean the same list.
func TestItemsTypeFilters(t *testing.T) {
	q := newQueryEnv(t)
	for _, query := range []string{
		"Recursive=true&ExcludeItemTypes=Movie,Audio",
		"recursive=true&excludeItemTypes=Movie&excludeItemTypes=Audio",
	} {
		for _, it := range q.items(query) {
			if it.Type == "Movie" || it.Type == "Audio" {
				t.Errorf("%s returned a %s", query, it.Type)
			}
		}
		if len(q.items(query)) == 0 {
			t.Errorf("%s returned nothing; albums and artists should remain", query)
		}
	}
	for _, it := range q.items("Recursive=true&MediaTypes=Audio") {
		if it.Type != "Audio" {
			t.Errorf("MediaTypes=Audio returned a %s", it.Type)
		}
	}
	if got := q.names("Recursive=true&MediaTypes=Audio"); len(got) != 2 {
		t.Errorf("MediaTypes=Audio = %v, want the two tracks", got)
	}
	if got := q.names("Recursive=true&IncludeItemTypes=LiveTvProgram"); len(got) != 0 {
		t.Errorf("a type this server has none of returned %v", got)
	}
}

func TestItemsFavoriteFilter(t *testing.T) {
	q := newQueryEnv(t)
	arrival := q.id("Arrival")
	if w := q.do(t, "POST", "/UserFavoriteItems/"+arrival, q.tok, nil); w.Code != 200 {
		t.Fatalf("favouriting = %d", w.Code)
	}
	for _, query := range []string{
		"Recursive=true&Filters=IsFavorite",
		"Recursive=true&IsFavorite=true",
		"Recursive=true&Filters=IsFavorite,IsNotFolder",
	} {
		if got := q.names(query); !equal(got, []string{"Arrival"}) {
			t.Errorf("%s = %v, want [Arrival]", query, got)
		}
	}
	for _, name := range q.names("Recursive=true&IsFavorite=false") {
		if name == "Arrival" {
			t.Error("IsFavorite=false returned the favourite")
		}
	}
	// Somebody else's favourites are not this user's.
	adminTok := q.tokenFor(q.admin)
	w := q.do(t, "GET", "/Items?Recursive=true&Filters=IsFavorite", adminTok, nil)
	var res QueryResult[BaseItemDto]
	decodeJSON(t, w, &res)
	if len(res.Items) != 0 {
		t.Errorf("the admin sees %d of the viewer's favourites", len(res.Items))
	}
}

func TestItemsSort(t *testing.T) {
	q := newQueryEnv(t)
	base := "Recursive=true&IncludeItemTypes=Movie&"
	for query, want := range map[string][]string{
		"SortBy=SortName&SortOrder=Ascending":                 {"Arrival", "Dune"},
		"SortBy=SortName&SortOrder=Descending":                {"Dune", "Arrival"},
		"SortBy=ProductionYear,SortName&SortOrder=Descending": {"Dune", "Arrival"},
		"SortBy=ProductionYear&SortOrder=Ascending":           {"Arrival", "Dune"},
		"sortBy=SortName&sortOrder=Descending":                {"Dune", "Arrival"},
		// A key this server cannot sort by is skipped, not fatal.
		"SortBy=CommunityRating,SortName&SortOrder=Descending,Ascending": {"Arrival", "Dune"},
	} {
		if got := q.names(base + query); !equal(got, want) {
			t.Errorf("%s = %v, want %v", query, got, want)
		}
	}
	// Random is a shuffle of the same set, whatever order it lands in.
	if got := sorted(q.names(base + "SortBy=Random")); !equal(got, []string{"Arrival", "Dune"}) {
		t.Errorf("SortBy=Random changed the set: %v", got)
	}
}

// The alphabet picker down the side of a library.
func TestItemsNameRange(t *testing.T) {
	q := newQueryEnv(t)
	base := "Recursive=true&IncludeItemTypes=Movie&SortBy=SortName&"
	for query, want := range map[string][]string{
		"NameStartsWith=D":          {"Dune"},
		"NameStartsWith=d":          {"Dune"},
		"NameLessThan=B":            {"Arrival"},
		"NameStartsWithOrGreater=B": {"Dune"},
		"NameStartsWith=Z":          nil,
	} {
		if got := q.names(base + query); !equal(got, want) {
			t.Errorf("%s = %v, want %v", query, got, want)
		}
	}
}

func TestItemsByRelation(t *testing.T) {
	q := newQueryEnv(t)
	album, artist := q.id("OK Computer"), q.id("Radiohead")

	if got := q.names("Recursive=true&IncludeItemTypes=Audio&AlbumIds=" + album); len(got) != 2 {
		t.Errorf("AlbumIds = %v, want the album's two tracks", got)
	}
	// What the album page asks, verbatim in shape: music videos for the album.
	// There are none; it must not be handed the library folders instead.
	if got := q.names("Recursive=true&IncludeItemTypes=MusicVideo&AlbumIds=" + album); len(got) != 0 {
		t.Errorf("music videos for the album = %v, want none", got)
	}
	for _, key := range []string{"ArtistIds", "AlbumArtistIds"} {
		if got := q.names("Recursive=true&IncludeItemTypes=MusicAlbum&" + key + "=" + artist); !equal(got, []string{"OK Computer"}) {
			t.Errorf("%s = %v, want [OK Computer]", key, got)
		}
	}
	ids := q.id("Arrival") + "," + q.id("Dune")
	if got := sorted(q.names("Ids=" + ids)); !equal(got, []string{"Arrival", "Dune"}) {
		t.Errorf("Ids = %v, want [Arrival Dune]", got)
	}
	if w := q.do(t, "GET", "/Items?Ids=not-a-guid", q.tok, nil); w.Code != 400 {
		t.Errorf("malformed Ids = %d, want 400", w.Code)
	}
}

// Genres, people, studios and tags are not scanned. A query restricted to one
// of them can match nothing -- and must say so, not ignore the restriction and
// return the whole library as if everything were in that genre.
func TestItemsFiltersOnDataNotHeldMatchNothing(t *testing.T) {
	q := newQueryEnv(t)
	some := q.id("Arrival")
	for _, restriction := range []string{
		"GenreIds=" + some, "Genres=Drama", "PersonIds=" + some, "Person=Somebody",
		"StudioIds=" + some, "Studios=A24", "Tags=x", "ContributingArtistIds=" + some,
	} {
		if got := q.names("Recursive=true&" + restriction); len(got) != 0 {
			t.Errorf("%s returned %v, want nothing", restriction, got)
		}
	}
}

func TestItemsYears(t *testing.T) {
	q := newQueryEnv(t)
	if got := q.names("Recursive=true&IncludeItemTypes=Movie&Years=2016,1999"); !equal(got, []string{"Arrival"}) {
		t.Errorf("Years=2016,1999 = %v, want [Arrival]", got)
	}
}

func TestQueryList(t *testing.T) {
	q := url.Values{"A": {"x,y", " z "}, "B": {""}}
	if got := queryList(q, "A"); !equal(got, []string{"x", "y", "z"}) {
		t.Errorf("queryList = %v, want [x y z]", got)
	}
	if got := queryList(q, "B"); len(got) != 0 {
		t.Errorf("an empty value produced %v", got)
	}
	if got := queryList(q, "C"); got != nil {
		t.Errorf("an absent key produced %v", got)
	}
}
