// Package jftmdb is a minimal client for The Movie Database, enough to find a
// film by title and year and download its poster.
//
// Jellyfin does this through TMDbLib in
// MediaBrowser.Providers/Plugins/Tmdb/; this covers only the two calls that
// artwork needs, using the stdlib so the binary stays dependency-free.
//
// # About the API key
//
// Jellyfin ships a hardcoded key in TmdbUtils.cs. It is deliberately NOT reused
// here. That key is registered to the Jellyfin project, TMDB's terms require an
// application to use its own, and borrowing it would put someone else's key at
// risk of the rate limiting our traffic caused. A key is free; the server
// refuses to enable the provider without one.
package jftmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIBase   = "https://api.themoviedb.org/3"
	defaultImageBase = "https://image.tmdb.org/t/p"

	// DefaultPosterSize is TMDB's w500. Jellyfin defaults to "original", which
	// for a large library means downloading many megabytes per film to render a
	// thumbnail a few hundred pixels wide.
	DefaultPosterSize = "w500"

	DefaultLanguage = "en-US"
)

// ErrNoAPIKey is returned when the provider is used without a key.
var ErrNoAPIKey = errors.New("jftmdb: no API key configured")

// ErrNotFound means the search returned nothing, which is ordinary: plenty of
// files are named in ways no database can match.
var ErrNotFound = errors.New("jftmdb: no match")

// Client talks to TMDB. The zero value is not usable; use New.
type Client struct {
	APIKey     string
	Language   string
	PosterSize string

	// APIBase and ImageBase are overridable so tests can point at a local
	// server instead of the internet.
	APIBase   string
	ImageBase string

	HTTP *http.Client

	// MinInterval throttles outbound requests. TMDB tolerates bursts, but a
	// first scan of a large library is exactly the traffic shape that gets an
	// API key rate-limited.
	MinInterval time.Duration
	lastCall    time.Time
}

func New(apiKey string) *Client {
	return &Client{
		APIKey:     apiKey,
		Language:   DefaultLanguage,
		PosterSize: DefaultPosterSize,
		APIBase:    defaultAPIBase,
		ImageBase:  defaultImageBase,
		HTTP: &http.Client{
			Timeout: 20 * time.Second,
		},
		MinInterval: 60 * time.Millisecond,
	}
}

// Movie is the subset of a TMDB search hit that artwork needs.
type Movie struct {
	ID          int
	Title       string
	Year        int
	PosterPath  string
	Overview    string
	VoteAverage float64
}

type searchResponse struct {
	Results []struct {
		ID          int     `json:"id"`
		Title       string  `json:"title"`
		ReleaseDate string  `json:"release_date"`
		PosterPath  string  `json:"poster_path"`
		Overview    string  `json:"overview"`
		VoteAverage float64 `json:"vote_average"`
	} `json:"results"`
}

// SearchMovie finds the best match for a title, optionally constrained by year.
//
// Passing the year matters: it is the difference between the 1978 and the 2004
// remake, and TMDB ranks by popularity rather than by how well the title
// actually matches.
func (c *Client) SearchMovie(ctx context.Context, title string, year int) (*Movie, error) {
	if c.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	if strings.TrimSpace(title) == "" {
		return nil, ErrNotFound
	}

	q := url.Values{}
	q.Set("api_key", c.APIKey)
	q.Set("query", title)
	q.Set("include_adult", "false")
	if c.Language != "" {
		q.Set("language", c.Language)
	}
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}

	var out searchResponse
	if err := c.getJSON(ctx, "/search/movie?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	if len(out.Results) == 0 {
		// A year is a guess when it came from a filename. Retry without it
		// rather than give up: a mislabelled year is common, a wrong title is
		// not.
		if year > 0 {
			return c.SearchMovie(ctx, title, 0)
		}
		return nil, ErrNotFound
	}

	r := out.Results[0]
	m := &Movie{
		ID:          r.ID,
		Title:       r.Title,
		PosterPath:  r.PosterPath,
		Overview:    r.Overview,
		VoteAverage: r.VoteAverage,
	}
	if len(r.ReleaseDate) >= 4 {
		if y, err := strconv.Atoi(r.ReleaseDate[:4]); err == nil {
			m.Year = y
		}
	}
	if m.PosterPath == "" {
		return m, ErrNotFound
	}
	return m, nil
}

// PosterURL builds the absolute image URL, mirroring TmdbClientManager.GetUrl:
// {imageBase}/{size}{path}, with "original" as the fallback size.
func (c *Client) PosterURL(posterPath string) string {
	if posterPath == "" {
		return ""
	}
	size := c.PosterSize
	if size == "" {
		size = "original"
	}
	base := c.ImageBase
	if base == "" {
		base = defaultImageBase
	}
	return strings.TrimSuffix(base, "/") + "/" + size + posterPath
}

// DownloadPoster fetches an image to dest, writing through a temporary file so
// an interrupted download cannot leave a truncated image in the cache.
func (c *Client) DownloadPoster(ctx context.Context, posterPath, dest string) error {
	u := c.PosterURL(posterPath)
	if u == "" {
		return ErrNotFound
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	c.throttle()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jftmdb: fetching poster: %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".poster-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// Cap the download: a corrupt or hostile response should not fill the disk.
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, 32<<20)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

func (c *Client) getJSON(ctx context.Context, path string, into any) error {
	c.throttle()

	base := c.APIBase
	if base == "" {
		base = defaultAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("jftmdb: API key rejected (HTTP 401)")
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("jftmdb: rate limited (HTTP 429)")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("jftmdb: unexpected status %s", resp.Status)
	}

	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into)
}

// throttle spaces out calls so a first scan does not arrive as a burst.
func (c *Client) throttle() {
	if c.MinInterval <= 0 {
		return
	}
	if wait := c.MinInterval - time.Since(c.lastCall); wait > 0 {
		time.Sleep(wait)
	}
	c.lastCall = time.Now()
}
