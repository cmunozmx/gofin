package jfauth

import (
	"net/http/httptest"
	"testing"
)

func TestParseHeader(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Info
	}{
		{
			"jellyfin web",
			`MediaBrowser Client="Jellyfin Web", Device="Firefox", DeviceId="TW96aWxsYQ==", Version="10.11.0", Token="abc123"`,
			Info{"Jellyfin Web", "Firefox", "TW96aWxsYQ==", "10.11.0", "abc123"},
		},
		{
			"unquoted values",
			`MediaBrowser Client=Kodi, Device=Linux, DeviceId=xyz, Version=1.0, Token=tok`,
			Info{"Kodi", "Linux", "xyz", "1.0", "tok"},
		},
		{
			"emby scheme, mixed case keys",
			`Emby CLIENT="App", deviceid="d1", token="t1"`,
			Info{Client: "App", DeviceID: "d1", Token: "t1"},
		},
		{
			"no scheme",
			`Client="App", Token="t2"`,
			Info{Client: "App", Token: "t2"},
		},
		{
			"comma inside a quoted device name",
			`MediaBrowser Client="Web", Device="Chrome, Windows", Token="t3"`,
			Info{Client: "Web", Device: "Chrome, Windows", Token: "t3"},
		},
		{
			"base64 device id with padding and equals",
			`MediaBrowser DeviceId="YWJjZA==", Token="t4"`,
			Info{DeviceID: "YWJjZA==", Token: "t4"},
		},
		{
			"bare token, no grammar at all",
			`justatoken`,
			Info{Token: "justatoken"},
		},
		{
			"no token present",
			`MediaBrowser Client="Web", Device="Firefox"`,
			Info{Client: "Web", Device: "Firefox"},
		},
		{"empty", ``, Info{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseHeader(c.in); got != c.want {
				t.Errorf("ParseHeader(%q)\n got %+v\nwant %+v", c.in, got, c.want)
			}
		})
	}
}

func TestFromRequestPrecedence(t *testing.T) {
	// Query-param fallback, used by <img> tags and <video> elements that
	// cannot set headers. This is why image and stream URLs must accept it.
	r := httptest.NewRequest("GET", "/Items/abc/Images/Primary?api_key=qtok", nil)
	if got := FromRequest(r).Token; got != "qtok" {
		t.Errorf("query token = %q, want qtok", got)
	}

	// A header token wins over the query parameter.
	r = httptest.NewRequest("GET", "/Items?api_key=qtok", nil)
	r.Header.Set("Authorization", `MediaBrowser Token="htok"`)
	if got := FromRequest(r).Token; got != "htok" {
		t.Errorf("header token = %q, want htok", got)
	}

	// Metadata in the legacy header, token in X-Emby-Token: both get used.
	r = httptest.NewRequest("GET", "/Items", nil)
	r.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Kodi", DeviceId="k1"`)
	r.Header.Set("X-Emby-Token", "etok")
	got := FromRequest(r)
	if got.Client != "Kodi" || got.DeviceID != "k1" || got.Token != "etok" {
		t.Errorf("split headers not merged: %+v", got)
	}
}
