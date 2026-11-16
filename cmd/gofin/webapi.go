package main

import (
	"cmp"
	"crypto/rand"
	"io"
	"net/http"
	"slices"
	"strconv"

	"gofin/internal/jfid"
	"gofin/internal/jfscan"
)

// Routes the web interface needs that no native client had asked for.
//
// Each was found the way the README describes: by running the interface
// against this server and reading the "unimplemented endpoint" warnings. The
// shapes come from the upstream controllers under $JF/Jellyfin.Api/Controllers.

func (s *Server) registerWebAPI(mux *apiMux) {
	mux.Handle("GET /Users/{userId}", s.authenticate(s.handleUserByID, false))
	mux.Handle("GET /Playback/BitrateTest", s.authenticate(handleBitrateTest, false))

	// The user menu and the settings pages behind it.
	mux.Handle("GET /Users", s.authenticate(s.handleUserList, false))
	for _, p := range []string{"GET /UserViews/GroupingOptions", "GET /Users/{userId}/GroupingOptions"} {
		mux.Handle(p, s.authenticate(s.handleGroupingOptions, false))
	}
	mux.Handle("GET /Localization/Cultures", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, cultures())
	}, false))

	// The music library's artist tabs and the artist half of search.
	mux.Handle("GET /Artists", s.authenticate(s.handleArtists, false))
	mux.Handle("GET /Artists/AlbumArtists", s.authenticate(s.handleArtists, false))

	// Lists of things the scanner does not record: there are no genres,
	// people or studios to list, and nothing to rank similarity by. Each is
	// the empty list it truthfully is. They are registered rather than left
	// to the catch-all because the interface decodes the body before it
	// reads the status, and a problem document where a list belongs is an
	// error dialog over a library that is otherwise fine.
	for _, p := range []string{"GET /Genres", "GET /MusicGenres", "GET /Persons", "GET /Studios"} {
		mux.Handle(p, s.authenticate(emptyQueryResult, false))
	}
	// One action under six routes upstream (LibraryController.GetSimilarItems).
	for _, p := range []string{
		"GET /Items/{itemId}/Similar", "GET /Movies/{itemId}/Similar", "GET /Albums/{itemId}/Similar",
		"GET /Artists/{itemId}/Similar", "GET /Shows/{itemId}/Similar", "GET /Trailers/{itemId}/Similar",
	} {
		mux.Handle(p, s.authenticate(s.forKnownItem(func(w http.ResponseWriter, r *http.Request, _ jfid.GUID) {
			emptyQueryResult(w, r)
		}), false))
	}
	// Pre-roll videos, asked for at the start of every playback: a list
	// upstream (UserLibraryController.GetIntros), and always an empty one
	// here. /Videos/{id}/Intros gives the same answer from handleStream.
	for _, p := range []string{"GET /Items/{itemId}/Intros", "GET /Users/{userId}/Items/{itemId}/Intros"} {
		mux.Handle(p, s.authenticate(emptyQueryResult, false))
	}
	// The film library's Suggestions tab. A bare array of RecommendationDto.
	mux.Handle("GET /Movies/Recommendations", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []struct{}{})
	}, false))

	// Theme songs and videos. Every item page asks before it draws.
	mux.Handle("GET /Items/{itemId}/ThemeMedia", s.authenticate(s.forKnownItem(func(w http.ResponseWriter, r *http.Request, id jfid.GUID) {
		none := noThemeMedia(id)
		writeJSON(w, 200, AllThemeMediaResult{ThemeVideosResult: none, ThemeSongsResult: none, SoundtrackSongsResult: none})
	}), false))
	for _, p := range []string{"GET /Items/{itemId}/ThemeSongs", "GET /Items/{itemId}/ThemeVideos"} {
		mux.Handle(p, s.authenticate(s.forKnownItem(func(w http.ResponseWriter, r *http.Request, id jfid.GUID) {
			writeJSON(w, 200, noThemeMedia(id))
		}), false))
	}
}

// forKnownItem resolves the {itemId} of a per-item route and answers 400 for
// an id that is not a GUID and 404 for one that names nothing. "Nothing is
// similar to this film" and "there is no such film" are different answers, and
// a per-item route that skips the check gives the first for both.
func (s *Server) forKnownItem(next func(http.ResponseWriter, *http.Request, jfid.GUID)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := jfid.Parse(r.PathValue("itemId"))
		if err != nil {
			problem(w, http.StatusBadRequest, "Malformed item id")
			return
		}
		if !s.itemKnown(id) {
			problem(w, http.StatusNotFound, "Item not found")
			return
		}
		next(w, r, id)
	}
}

// itemKnown reports whether id names a scanned item or a library. Libraries
// are built from configuration and are not in the item store.
func (s *Server) itemKnown(id jfid.GUID) bool {
	if _, ok := s.items.Get(id); ok {
		return true
	}
	for _, l := range s.cfg.Libraries {
		if l.ID(s.cfg.Paths.Data) == id {
			return true
		}
	}
	return false
}

// ThemeMediaResult is upstream's QueryResult<BaseItemDto> plus the item the
// themes belong to.
type ThemeMediaResult struct {
	QueryResult[BaseItemDto]
	OwnerID jfid.GUID `json:"OwnerId"`
}

// AllThemeMediaResult is what /Items/{id}/ThemeMedia returns.
type AllThemeMediaResult struct {
	ThemeVideosResult     ThemeMediaResult `json:"ThemeVideosResult"`
	ThemeSongsResult      ThemeMediaResult `json:"ThemeSongsResult"`
	SoundtrackSongsResult ThemeMediaResult `json:"SoundtrackSongsResult"`
}

// noThemeMedia is the answer for every item: the scanner skips theme-music
// and backdrops folders as extras rather than attaching them to their film.
func noThemeMedia(owner jfid.GUID) ThemeMediaResult {
	return ThemeMediaResult{QueryResult: QueryResult[BaseItemDto]{Items: []BaseItemDto{}}, OwnerID: owner}
}

// handleArtists is ArtistsController.GetArtists and GetAlbumArtists. Both are
// the same list here: an artist is a folder in a music library, and whoever a
// file is filed under is the only artist the scanner knows for it.
func (s *Server) handleArtists(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requestedUser(r)
	if !ok {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}
	ud := s.userDataFor(user.ID)
	q := r.URL.Query()

	// Everything, or everything beneath one library. ParentId is not passed
	// on to the item query: there it would mean "direct children of".
	var pool []jfscan.Item
	if raw := q.Get("ParentId"); raw != "" {
		parent, err := jfid.Parse(raw)
		if err != nil {
			problem(w, http.StatusBadRequest, "Malformed ParentId")
			return
		}
		pool = s.items.Descendants(parent)
	} else {
		pool = s.items.All()
	}
	artists := pool[:0]
	for _, it := range pool {
		if it.Kind == jfscan.KindMusicArtist {
			artists = append(artists, it)
		}
	}

	artists, err := s.filterItems(artists, q, ud)
	if err != nil {
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	// By name unless told otherwise, as upstream; it also gives the map-backed
	// store a stable order for paging.
	slices.SortFunc(artists, func(a, b jfscan.Item) int {
		return cmp.Or(cmp.Compare(sortKey(a), sortKey(b)), cmp.Compare(a.Path, b.Path))
	})
	sortItems(artists, q, ud)

	items := make([]BaseItemDto, 0, len(artists))
	for _, it := range artists {
		items = append(items, s.toItemDto(it, ud))
	}
	total := len(items)
	writeJSON(w, 200, QueryResult[BaseItemDto]{
		Items:            page(items, q),
		TotalRecordCount: total,
		StartIndex:       startIndex(q),
	})
}

// handleUserByID is UserController.GetUserById. The interface loads the
// signed-in user this way, by the id it got at login, rather than through
// /Users/Me -- so without it nothing after the login screen renders.
//
// Upstream answers this for any signed-in user about any other, policy and
// all. Here somebody else's record is an administrator's to read, which is the
// rule every other per-user route in this server already follows.
func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request) {
	id, err := jfid.Parse(r.PathValue("userId"))
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed user id")
		return
	}
	caller := authFrom(r).User
	if id == caller.ID {
		writeJSON(w, 200, toUserDto(caller, s.serverID.String()))
		return
	}
	if !caller.Admin {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}
	u, err := s.store.UserByID(id)
	if err != nil {
		s.log.Error("user by id", "err", err)
		problem(w, http.StatusInternalServerError, "Internal error")
		return
	}
	if u == nil {
		problem(w, http.StatusNotFound, "User not found")
		return
	}
	writeJSON(w, 200, toUserDto(u, s.serverID.String()))
}

// Upstream's bounds and default for the bitrate test, from
// MediaInfoController.GetBitrateTestBytes.
const (
	bitrateTestDefault = 102400
	bitrateTestMax     = 100_000_000
)

// handleBitrateTest sends the requested number of random bytes. The interface
// times the download to pick a streaming bitrate, so the body has to be the
// size asked for and must not compress in transit; what the bytes are does not
// matter. They are streamed rather than built in memory, since a client may
// ask for 100 MB.
func handleBitrateTest(w http.ResponseWriter, r *http.Request) {
	size := bitrateTestDefault
	if raw := r.URL.Query().Get("size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > bitrateTestMax {
			problem(w, http.StatusBadRequest, "The requested size must be between 1 and 100000000")
			return
		}
		size = n
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(size))
	// A measurement that a cache answered would measure nothing.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.CopyN(w, rand.Reader, int64(size))
}

// handleUserList is UserController.GetUsers. An administrator gets everybody;
// anybody else gets themselves, by the same rule as handleUserByID. Upstream's
// isHidden and isDisabled filters select states no user here can be in.
func (s *Server) handleUserList(w http.ResponseWriter, r *http.Request) {
	out := []UserDto{}
	q := r.URL.Query()
	if truthy(q.Get("isHidden")) || truthy(q.Get("isDisabled")) {
		writeJSON(w, 200, out)
		return
	}
	caller := authFrom(r).User
	if !caller.Admin {
		writeJSON(w, 200, append(out, toUserDto(caller, s.serverID.String())))
		return
	}
	users, err := s.store.Users()
	if err != nil {
		s.log.Error("user list", "err", err)
		problem(w, http.StatusInternalServerError, "Internal error")
		return
	}
	for _, u := range users {
		out = append(out, toUserDto(u, s.serverID.String()))
	}
	writeJSON(w, 200, out)
}

// SpecialViewOptionDto is one entry of the grouping options.
type SpecialViewOptionDto struct {
	Name string `json:"Name"`
	ID   string `json:"Id"`
}

// handleGroupingOptions is UserViewsController.GetGroupingOptions: the
// libraries a user may fold into one combined view on the home screen.
// Upstream offers film, show and mixed libraries (UserView.
// IsEligibleForGrouping), ordered by name.
func (s *Server) handleGroupingOptions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requestedUser(r); !ok {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}
	out := []SpecialViewOptionDto{}
	for _, l := range s.cfg.Libraries {
		switch l.Type {
		case "movies", "tvshows", "":
			out = append(out, SpecialViewOptionDto{Name: l.Name, ID: l.ID(s.cfg.Paths.Data).String()})
		}
	}
	slices.SortFunc(out, func(a, b SpecialViewOptionDto) int { return cmp.Compare(a.Name, b.Name) })
	writeJSON(w, 200, out)
}
