// Package jfauth parses Jellyfin's authorization grammar.
//
// Clients in the wild send all of these, and a server that rejects any one of
// them locks out some app you cannot control:
//
//	Authorization: MediaBrowser Client="Jellyfin Web", Device="Firefox",
//	               DeviceId="TW96aWxsYS81...", Version="10.11.0", Token="abc"
//	Authorization: Emby Client=Kodi, Device=Linux, DeviceId=xyz, Version=1.0
//	X-Emby-Authorization: <same grammar>
//	X-Emby-Token: <token only>
//	?api_key=<token>  or  ?ApiKey=<token>
//
// The parser is deliberately forgiving: optional scheme, quoted or bare values,
// arbitrary key casing, missing fields, stray whitespace, and values containing
// spaces or '=' (device names do). It never errors -- it returns whatever it
// could recover, and the caller decides whether a token is present.
package jfauth

import (
	"net/http"
	"strings"
)

// Info is what a request tells us about the calling client.
type Info struct {
	Client   string // "Jellyfin Web", "Jellyfin Android", "Kodi"
	Device   string // human-readable device name
	DeviceID string // stable per install; session identity depends on it
	Version  string // client app version
	Token    string // user access token or API key
}

func (i Info) HasToken() bool { return i.Token != "" }

// FromRequest applies the full precedence chain used by Jellyfin.
func FromRequest(r *http.Request) Info {
	var info Info

	// Authorization first, then the legacy header. Parse both so that a client
	// sending metadata in one and a token in the other still works.
	if v := r.Header.Get("Authorization"); v != "" {
		info = merge(info, ParseHeader(v))
	}
	if v := r.Header.Get("X-Emby-Authorization"); v != "" {
		info = merge(info, ParseHeader(v))
	}
	if info.Token == "" {
		if v := strings.TrimSpace(r.Header.Get("X-Emby-Token")); v != "" {
			info.Token = v
		}
	}
	if info.Token == "" {
		if v := strings.TrimSpace(r.Header.Get("X-MediaBrowser-Token")); v != "" {
			info.Token = v
		}
	}
	if info.Token == "" {
		q := r.URL.Query()
		for _, k := range []string{"api_key", "ApiKey", "apikey", "X-Emby-Token"} {
			if v := strings.TrimSpace(q.Get(k)); v != "" {
				info.Token = v
				break
			}
		}
	}
	return info
}

func merge(dst, src Info) Info {
	if dst.Client == "" {
		dst.Client = src.Client
	}
	if dst.Device == "" {
		dst.Device = src.Device
	}
	if dst.DeviceID == "" {
		dst.DeviceID = src.DeviceID
	}
	if dst.Version == "" {
		dst.Version = src.Version
	}
	if dst.Token == "" {
		dst.Token = src.Token
	}
	return dst
}

// ParseHeader handles one header value.
func ParseHeader(v string) Info {
	v = strings.TrimSpace(v)
	if v == "" {
		return Info{}
	}

	// Strip an optional scheme. A bare token with no scheme and no '=' is
	// treated as the token itself, which some older clients do.
	if i := strings.IndexAny(v, " \t"); i > 0 {
		switch strings.ToLower(v[:i]) {
		case "mediabrowser", "emby", "jellyfin":
			v = strings.TrimSpace(v[i+1:])
		}
	} else if !strings.Contains(v, "=") {
		return Info{Token: v}
	}

	var info Info
	for _, part := range splitPairs(v) {
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(part[:eq]))
		val := unquote(strings.TrimSpace(part[eq+1:]))
		switch key {
		case "client":
			info.Client = val
		case "device":
			info.Device = val
		case "deviceid":
			info.DeviceID = val
		case "version":
			info.Version = val
		case "token", "accesstoken", "api_key", "apikey":
			info.Token = val
		}
	}
	return info
}

// splitPairs splits on commas that are not inside quotes. Device names really
// do contain commas -- "Chrome, Windows" shows up in the wild -- so a plain
// strings.Split silently truncates them.
func splitPairs(s string) []string {
	var (
		out    []string
		start  int
		inQuot bool
	)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuot = !inQuot
		case ',':
			if !inQuot {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, strings.TrimSpace(s[start:]))
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	// Clients occasionally URL-encode the device id; leave it alone unless it
	// is unambiguously encoded, since decoding a raw base64 id would corrupt it.
	return strings.TrimSpace(s)
}
