package jftmdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTMDB stands in for the real API so these tests never touch the network.
func fakeTMDB(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := New("test-key")
	c.APIBase = srv.URL
	c.ImageBase = srv.URL + "/img"
	c.MinInterval = 0 // no throttling in tests
	return c
}

const oneHit = `{"results":[{"id":329865,"title":"Arrival","release_date":"2016-11-10",
"poster_path":"/x.jpg","overview":"Linguist.","vote_average":7.6}]}`

func TestSearchMovie(t *testing.T) {
	var gotQuery, gotYear, gotKey string
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		gotYear = r.URL.Query().Get("year")
		gotKey = r.URL.Query().Get("api_key")
		w.Write([]byte(oneHit))
	})

	m, err := c.SearchMovie(context.Background(), "Arrival", 2016)
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "Arrival" {
		t.Errorf("query = %q", gotQuery)
	}
	if gotYear != "2016" {
		t.Errorf("year = %q, want 2016 -- without it TMDB ranks by popularity "+
			"and a remake outranks the original", gotYear)
	}
	if gotKey != "test-key" {
		t.Errorf("api_key = %q", gotKey)
	}
	if m.ID != 329865 || m.Title != "Arrival" || m.Year != 2016 {
		t.Errorf("got %+v", m)
	}
	if m.PosterPath != "/x.jpg" {
		t.Errorf("PosterPath = %q", m.PosterPath)
	}
}

// A key is required. Without one the client must not send a request at all.
func TestSearchWithoutAPIKey(t *testing.T) {
	var called atomic.Bool
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.Write([]byte(oneHit))
	})
	c.APIKey = ""

	_, err := c.SearchMovie(context.Background(), "Arrival", 2016)
	if !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("err = %v, want ErrNoAPIKey", err)
	}
	if called.Load() {
		t.Error("a request was sent without an API key")
	}
}

// A year parsed out of a filename is a guess. When it finds nothing the search
// retries without it, because a wrong year is common and a wrong title is not.
func TestSearchRetriesWithoutYear(t *testing.T) {
	var calls int32
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if r.URL.Query().Get("year") != "" {
			w.Write([]byte(`{"results":[]}`)) // nothing for that year
			return
		}
		_ = n
		w.Write([]byte(oneHit))
	})

	m, err := c.SearchMovie(context.Background(), "Arrival", 1999)
	if err != nil {
		t.Fatalf("the retry without a year should have matched: %v", err)
	}
	if m.Title != "Arrival" {
		t.Errorf("Title = %q", m.Title)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("made %d calls, want 2 (with year, then without)", calls)
	}
}

func TestSearchNotFound(t *testing.T) {
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[]}`))
	})
	if _, err := c.SearchMovie(context.Background(), "No Such Film", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A hit with no poster is still a miss as far as artwork is concerned.
func TestSearchHitWithoutPoster(t *testing.T) {
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"id":1,"title":"X","release_date":"2000-01-01","poster_path":""}]}`))
	})
	if _, err := c.SearchMovie(context.Background(), "X", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSearchEmptyTitle(t *testing.T) {
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent for an empty title")
	})
	if _, err := c.SearchMovie(context.Background(), "   ", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A rejected key and a rate limit have to be distinguishable, because the
// server stops the whole run on either rather than retrying every item.
func TestSearchHTTPErrors(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "401"},
		{http.StatusTooManyRequests, "429"},
		{http.StatusInternalServerError, "500"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := c.SearchMovie(context.Background(), "Arrival", 0)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %s", err, tc.want)
			}
		})
	}
}

func TestPosterURL(t *testing.T) {
	c := New("k")
	cases := []struct {
		size, path, want string
	}{
		{"w500", "/abc.jpg", "https://image.tmdb.org/t/p/w500/abc.jpg"},
		{"original", "/abc.jpg", "https://image.tmdb.org/t/p/original/abc.jpg"},
		// An empty size falls back to "original", mirroring
		// TmdbClientManager.GetUrl.
		{"", "/abc.jpg", "https://image.tmdb.org/t/p/original/abc.jpg"},
		{"w500", "", ""},
	}
	for _, tc := range cases {
		c.PosterSize = tc.size
		if got := c.PosterURL(tc.path); got != tc.want {
			t.Errorf("PosterURL(%q) with size %q = %q, want %q", tc.path, tc.size, got, tc.want)
		}
	}
}

func TestDownloadPoster(t *testing.T) {
	body := []byte("\xff\xd8\xff\xe0JPEGBYTES")
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/img/w500/x.jpg") {
			t.Errorf("unexpected image path %q", r.URL.Path)
		}
		w.Write(body)
	})

	dest := filepath.Join(t.TempDir(), "nested", "poster.jpg")
	if err := c.DownloadPoster(context.Background(), "/x.jpg", dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("downloaded %q, want %q", got, body)
	}
}

// A failed download must not leave a truncated file behind, or the cache
// serves a broken image forever.
func TestDownloadPosterLeavesNoPartialFile(t *testing.T) {
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	dir := t.TempDir()
	dest := filepath.Join(dir, "poster.jpg")
	if err := c.DownloadPoster(context.Background(), "/x.jpg", dest); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a failed download left a file behind")
	}
	// And no temporary files either.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".poster-") {
			t.Errorf("temporary file %q was not cleaned up", e.Name())
		}
	}
}

func TestDownloadPosterEmptyPath(t *testing.T) {
	c := New("k")
	if err := c.DownloadPoster(context.Background(), "", "/tmp/x.jpg"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestContextCancellation(t *testing.T) {
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.SearchMovie(ctx, "Arrival", 0); err == nil {
		t.Error("a cancelled context should abort the search")
	}
}

// A first scan of a large library is exactly the traffic shape that gets an API
// key rate-limited, so calls are spaced out.
func TestThrottle(t *testing.T) {
	c := fakeTMDB(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(oneHit))
	})
	c.MinInterval = 40 * time.Millisecond

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.SearchMovie(context.Background(), "Arrival", 2016); err != nil {
			t.Fatal(err)
		}
	}
	// Three calls, two gaps.
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("three calls took %v, want at least two throttle intervals", elapsed)
	}
}

func TestDefaults(t *testing.T) {
	c := New("k")
	if c.PosterSize != "w500" {
		t.Errorf("PosterSize = %q; original would download megabytes per film "+
			"to render a thumbnail", c.PosterSize)
	}
	if c.Language != "en-US" {
		t.Errorf("Language = %q", c.Language)
	}
	if c.HTTP.Timeout == 0 {
		t.Error("no HTTP timeout: a hung provider would stall the fetch loop")
	}
	if c.MinInterval <= 0 {
		t.Error("no throttle interval")
	}
}
