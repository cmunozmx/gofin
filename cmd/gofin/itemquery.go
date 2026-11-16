package main

import (
	"cmp"
	"math/rand/v2"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"gofin/internal/jfid"
	"gofin/internal/jfscan"
)

// The item query: the part of ItemsController.GetItems that decides which
// items a request means and in what order.
//
// Native clients got by on ParentId, Recursive and IncludeItemTypes. The web
// interface asks for much more of the surface -- a search term, the user's
// favourites, a sort order, the letter picked from the alphabet strip -- and
// asks most of it without a ParentId at all. Answering only the first three
// did not just lose the refinement: a query the handler did not understand
// came back as the library folders, so the interface listed "Movies" and
// "Music" as the user's favourite episodes.
//
// Upstream: $JF/Jellyfin.Api/Controllers/ItemsController.cs, and
// InternalItemsQuery for what each parameter means.

// queryList reads a list parameter. The interface's older code sends lists
// comma-separated in one value (IncludeItemTypes=Movie,Episode); its newer,
// generated code repeats the key (includeItemTypes=Movie&includeItemTypes=
// Episode). Both mean the same list, and mixing them is legal.
func queryList(q url.Values, key string) []string {
	var out []string
	for _, v := range q[key] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func queryIDs(q url.Values, key string) (map[jfid.GUID]bool, error) {
	vals := queryList(q, key)
	if len(vals) == 0 {
		return nil, nil
	}
	out := make(map[jfid.GUID]bool, len(vals))
	for _, v := range vals {
		id, err := jfid.Parse(v)
		if err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, nil
}

func lowerSet(vals []string) map[string]bool {
	if len(vals) == 0 {
		return nil
	}
	out := make(map[string]bool, len(vals))
	for _, v := range vals {
		out[strings.ToLower(v)] = true
	}
	return out
}

// filtersOnDataNotHeld are restrictions to something the scanner does not
// record. Nothing in the library can be shown to satisfy one, so a query that
// carries one matches nothing. Ignoring it instead would answer "everything in
// this genre" with the whole library.
var filtersOnDataNotHeld = []string{
	"GenreIds", "Genres", "PersonIds", "Person", "StudioIds", "Studios",
	"Tags", "OfficialRatings", "ContributingArtistIds",
}

// errBadQueryID is returned for an id parameter that is not a GUID.
type errBadQueryID struct{ param string }

func (e errBadQueryID) Error() string { return "Malformed " + e.param }

// filterItems keeps the items of pool that satisfy the query's restrictions.
// Order is preserved.
func (s *Server) filterItems(pool []jfscan.Item, q url.Values, ud userData) ([]jfscan.Item, error) {
	for _, key := range filtersOnDataNotHeld {
		if len(queryList(q, key)) > 0 {
			return nil, nil
		}
	}

	include := lowerSet(queryList(q, "IncludeItemTypes"))
	exclude := lowerSet(queryList(q, "ExcludeItemTypes"))
	media := lowerSet(queryList(q, "MediaTypes"))
	flags := lowerSet(queryList(q, "Filters"))
	// Likes and dislikes are not recorded, so neither can be satisfied.
	if flags["likes"] || flags["dislikes"] {
		return nil, nil
	}

	// The boolean spellings of two of the flags. Unlike the flags they can
	// also ask for the negative.
	favorite, hasFavorite := queryBool(q, "IsFavorite")
	played, hasPlayed := queryBool(q, "IsPlayed")

	term := strings.ToLower(strings.TrimSpace(q.Get("SearchTerm")))
	startsWith := strings.ToLower(q.Get("NameStartsWith"))
	orGreater := strings.ToLower(q.Get("NameStartsWithOrGreater"))
	lessThan := strings.ToLower(q.Get("NameLessThan"))

	var years map[int]bool
	for _, y := range queryList(q, "Years") {
		if n, err := strconv.Atoi(y); err == nil {
			if years == nil {
				years = map[int]bool{}
			}
			years[n] = true
		}
	}

	albums, err := queryIDs(q, "AlbumIds")
	if err != nil {
		return nil, errBadQueryID{"AlbumIds"}
	}
	// An artist is a folder; "by this artist" is "somewhere beneath it".
	// ArtistIds and AlbumArtistIds are the same question here, since the
	// scanner knows only the artist a file is filed under.
	var underArtist map[jfid.GUID]bool
	for _, key := range []string{"ArtistIds", "AlbumArtistIds"} {
		artists, err := queryIDs(q, key)
		if err != nil {
			return nil, errBadQueryID{key}
		}
		for id := range artists {
			if underArtist == nil {
				underArtist = map[jfid.GUID]bool{}
			}
			for _, d := range s.items.Descendants(id) {
				underArtist[d.ID] = true
			}
		}
	}

	out := make([]jfscan.Item, 0, len(pool))
	for _, it := range pool {
		kind := strings.ToLower(string(it.Kind))
		if include != nil && !include[kind] {
			continue
		}
		if exclude[kind] {
			continue
		}
		if media != nil && !media[strings.ToLower(mediaTypes[it.Kind])] {
			continue
		}
		state := ud[it.ID]
		if (flags["isfavorite"] && !state.IsFavorite) ||
			(flags["isplayed"] && !state.Played) ||
			(flags["isunplayed"] && state.Played) ||
			(flags["isresumable"] && state.PlaybackPositionTicks == 0) ||
			(flags["isfolder"] && !it.IsFolder) ||
			(flags["isnotfolder"] && it.IsFolder) {
			continue
		}
		if hasFavorite && state.IsFavorite != favorite {
			continue
		}
		if hasPlayed && state.Played != played {
			continue
		}
		if term != "" && !strings.Contains(strings.ToLower(it.Name), term) {
			continue
		}
		name := sortKey(it)
		if (startsWith != "" && !strings.HasPrefix(name, startsWith)) ||
			(orGreater != "" && name < orGreater) ||
			(lessThan != "" && name >= lessThan) {
			continue
		}
		if years != nil && !years[it.Year] {
			continue
		}
		if albums != nil && !albums[it.ParentID] {
			continue
		}
		if underArtist != nil && !underArtist[it.ID] {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// queryBool reads an optional boolean. The second result is false when the
// parameter is absent or unparseable, which leaves the question unasked.
func queryBool(q url.Values, key string) (value, ok bool) {
	raw := q.Get(key)
	if raw == "" {
		return false, false
	}
	b, err := strconv.ParseBool(raw)
	return b, err == nil
}

// sortKey is the name an item sorts and ranges by.
func sortKey(it jfscan.Item) string {
	if it.SortName != "" {
		return strings.ToLower(it.SortName)
	}
	return strings.ToLower(it.Name)
}

// itemComparers are the SortBy keys this server can honour, by lower-cased
// name. A key that is not here -- CommunityRating, Runtime, anything else that
// would need metadata the scanner does not collect -- is skipped, which leaves
// the remaining keys to decide the order.
func itemComparers(ud userData) map[string]func(a, b jfscan.Item) int {
	byName := func(a, b jfscan.Item) int { return cmp.Compare(sortKey(a), sortKey(b)) }
	byYear := func(a, b jfscan.Item) int { return cmp.Compare(a.Year, b.Year) }
	byAdded := func(a, b jfscan.Item) int { return a.Modified.Compare(b.Modified) }
	byArtist := func(a, b jfscan.Item) int {
		return cmp.Compare(strings.ToLower(a.AlbumArtist), strings.ToLower(b.AlbumArtist))
	}
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	return map[string]func(a, b jfscan.Item) int{
		"sortname":             byName,
		"name":                 byName,
		"productionyear":       byYear,
		"premieredate":         byYear,
		"datecreated":          byAdded,
		"datelastcontentadded": byAdded,
		"indexnumber":          func(a, b jfscan.Item) int { return cmp.Compare(a.IndexNumber, b.IndexNumber) },
		"album":                func(a, b jfscan.Item) int { return cmp.Compare(strings.ToLower(a.Album), strings.ToLower(b.Album)) },
		"albumartist":          byArtist,
		"artist":               byArtist,
		"dateplayed":           func(a, b jfscan.Item) int { return ud[a.ID].LastPlayed.Compare(ud[b.ID].LastPlayed) },
		"playcount":            func(a, b jfscan.Item) int { return cmp.Compare(ud[a.ID].PlayCount, ud[b.ID].PlayCount) },
		"isfavoriteorliked":    func(a, b jfscan.Item) int { return cmp.Compare(flag(ud[a.ID].IsFavorite), flag(ud[b.ID].IsFavorite)) },
		// Upstream's IsFolder comparer puts folders first.
		"isfolder": func(a, b jfscan.Item) int { return cmp.Compare(flag(!a.IsFolder), flag(!b.IsFolder)) },
	}
}

// sortItems orders items by the query's SortBy keys, each with its own
// SortOrder; when there are fewer orders than keys the last one given applies
// to the rest, as upstream does. With no SortBy the order is left as it is.
func sortItems(items []jfscan.Item, q url.Values, ud userData) {
	keys := queryList(q, "SortBy")
	if len(keys) == 0 {
		return
	}
	orders := queryList(q, "SortOrder")
	known := itemComparers(ud)

	type step struct {
		compare func(a, b jfscan.Item) int
		desc    bool
	}
	var steps []step
	for i, key := range keys {
		key = strings.ToLower(key)
		if key == "random" {
			// A shuffle, then nothing: keys after Random have no ties left
			// to break in any way a client could observe.
			rand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
			break
		}
		compare, ok := known[key]
		if !ok {
			continue
		}
		order := ""
		switch {
		case i < len(orders):
			order = orders[i]
		case len(orders) > 0:
			order = orders[len(orders)-1]
		}
		steps = append(steps, step{compare, strings.EqualFold(order, "Descending")})
	}
	if len(steps) == 0 {
		return
	}
	slices.SortStableFunc(items, func(a, b jfscan.Item) int {
		for _, st := range steps {
			c := st.compare(a, b)
			if st.desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
}

// itemPool is the set of items a query starts from, before any restriction.
// top reports the one case that is not a set of scanned items at all: no
// ParentId and no Recursive is the top level, which is the libraries.
func (s *Server) itemPool(q url.Values) (pool []jfscan.Item, top bool, err error) {
	if ids := queryList(q, "Ids"); len(ids) > 0 {
		for _, raw := range ids {
			id, err := jfid.Parse(raw)
			if err != nil {
				return nil, false, errBadQueryID{"Ids"}
			}
			if it, ok := s.items.Get(id); ok {
				pool = append(pool, it)
			}
		}
		return pool, false, nil
	}

	recursive := truthy(q.Get("Recursive"))
	raw := q.Get("ParentId")
	if raw == "" {
		if !recursive {
			return nil, true, nil
		}
		// Everywhere. The store is a map, so give the result an order of
		// its own: a client that asks for no sort still expects the same
		// answer twice, or its paging skips and repeats items.
		pool = s.items.All()
		slices.SortFunc(pool, func(a, b jfscan.Item) int {
			return cmp.Or(cmp.Compare(sortKey(a), sortKey(b)), cmp.Compare(a.Path, b.Path))
		})
		return pool, false, nil
	}

	parent, err := jfid.Parse(raw)
	if err != nil {
		return nil, false, errBadQueryID{"ParentId"}
	}
	// A music library is Artist/Album/Track, several levels deep. Clients
	// browsing its Albums or Songs tab ask for the whole subtree with
	// Recursive=true and then filter by IncludeItemTypes. Returning only the
	// direct children leaves that query looking at artist folders, which
	// never match, so the library renders empty.
	if recursive {
		return s.items.Descendants(parent), false, nil
	}
	return s.items.Children(parent), false, nil
}
