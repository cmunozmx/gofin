package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// Display preferences.
//
// For a native client these are a few view settings it can live without, and
// for a long time this server answered with defaults and threw writes away.
// The web interface keeps a user's whole configuration here -- theme,
// language, the layout of the home screen, subtitle appearance -- under the id
// "usersettings", in CustomPrefs. Discarded, every one of those settings
// reverts on the next page load.
//
// A row is keyed on (user, preferences id, client), as upstream keys it
// (DisplayPreferencesController). The value is the DTO as JSON.

// maxDisplayPrefsBody bounds a write. Real bodies are a few kilobytes.
const maxDisplayPrefsBody = 1 << 20

func (s *Server) handleGetDisplayPrefs(w http.ResponseWriter, r *http.Request) {
	id, client := r.PathValue("prefsId"), r.URL.Query().Get("client")
	prefs := defaultDisplayPreferences(id, client)

	stored, err := s.store.DisplayPrefs(authFrom(r).User.ID, id, client)
	if err != nil {
		// Defaults are a usable answer; an error here can stop a client
		// before it renders anything at all.
		s.log.Error("reading display preferences", "id", id, "err", err)
	} else if stored != nil {
		if err := json.Unmarshal(stored, &prefs); err != nil {
			s.log.Error("stored display preferences are not valid", "id", id, "err", err)
			prefs = defaultDisplayPreferences(id, client)
		}
	}
	// The key is the request's. A stored body is not trusted to say which
	// preferences it is.
	prefs.ID, prefs.Client = id, client
	if prefs.CustomPrefs == nil {
		prefs.CustomPrefs = map[string]string{} // clients index into it directly
	}
	writeJSON(w, 200, prefs)
}

// handleSetDisplayPrefs stores what the client sent. It answers 204 whatever
// happens: a client writes these in passing, and an error makes some of them
// retry the write in a loop. A body that cannot be stored is logged instead.
func (s *Server) handleSetDisplayPrefs(w http.ResponseWriter, r *http.Request) {
	id, client := r.PathValue("prefsId"), r.URL.Query().Get("client")
	defer w.WriteHeader(http.StatusNoContent)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxDisplayPrefsBody+1))
	if err != nil || len(body) > maxDisplayPrefsBody {
		s.log.Warn("display preferences write not stored: unreadable or too large", "id", id)
		return
	}

	// Decoded over the defaults, so a client that sends only the fields it
	// changed does not zero the rest. CustomPrefs is decoded separately to
	// tell null from a string: null is "not set", and storing it as "" would
	// hand the interface a value where it expects none.
	var in struct {
		DisplayPreferencesDto
		CustomPrefs map[string]*string `json:"CustomPrefs"`
	}
	in.DisplayPreferencesDto = defaultDisplayPreferences(id, client)
	if err := json.Unmarshal(body, &in); err != nil {
		s.log.Warn("display preferences write not stored: not valid JSON", "id", id, "err", err)
		return
	}
	prefs := in.DisplayPreferencesDto
	prefs.ID, prefs.Client = id, client
	prefs.CustomPrefs = make(map[string]string, len(in.CustomPrefs))
	for k, v := range in.CustomPrefs {
		if v != nil {
			prefs.CustomPrefs[k] = *v
		}
	}

	data, err := json.Marshal(prefs)
	if err == nil {
		err = s.store.SetDisplayPrefs(authFrom(r).User.ID, id, client, data)
	}
	if err != nil {
		s.log.Error("storing display preferences", "id", id, "err", err)
	}
}
