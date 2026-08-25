// Command gofin is a Jellyfin-wire-compatible media server.
//
// This is phase 0/1 of the port: configuration, identity, the system endpoints
// clients probe before anything else, and the WebSocket handshake. Everything
// else answers 501 with a correctly-shaped ProblemDetails body, so the server
// always builds and boots and "how much is left" is a number you can watch.
//
// Build: CGO_ENABLED=0 go build ./cmd/gofin
// Run:   ./gofin serve --port 8096 --log-level debug
package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gofin/internal/jfauth"
	"gofin/internal/jfdb"
	"gofin/internal/jfid"
	"gofin/internal/jfprobe"
	"gofin/internal/jfscan"
	"gofin/internal/jftime"
	"gofin/internal/jftmdb"
	"gofin/internal/jfws"
)

const (
	// Version is what we report to clients. Reporting a plausible Jellyfin
	// version matters: several clients gate features on it, and anything below
	// roughly 10.9 makes them fall back to legacy endpoints.
	Version     = "10.11.0"
	ProductName = "Jellyfin Server"
)

// ---------------------------------------------------------------- config

type Config struct {
	Server struct {
		Name          string   `json:"name"`
		Bind          string   `json:"bind"`
		Port          int      `json:"port"`
		BaseURL       string   `json:"base_url"`
		PublishedURL  string   `json:"published_url"`
		LocalNetworks []string `json:"local_networks"`
		Autodiscovery bool     `json:"autodiscovery"`
	} `json:"server"`
	Paths struct {
		Data   string `json:"data"`
		Config string `json:"config"`
		Cache  string `json:"cache"`
		Logs   string `json:"logs"`
	} `json:"paths"`
	Web struct {
		Enabled bool   `json:"enabled"`
		Dir     string `json:"dir"`
	} `json:"web"`
	Logging struct {
		Level  string `json:"level"`
		Format string `json:"format"`
	} `json:"logging"`
	Metadata struct {
		TMDB struct {
			Enabled    bool   `json:"enabled"`
			APIKey     string `json:"api_key"`
			Language   string `json:"language"`
			PosterSize string `json:"poster_size"`
		} `json:"tmdb"`
	} `json:"metadata"`
	Users     []ConfigUser `json:"users"`
	Libraries []Library    `json:"libraries"`
}

type ConfigUser struct {
	Name         string `json:"name"`
	PasswordHash string `json:"password_hash"`
	Password     string `json:"password"` // convenience for bootstrap; hashed on load
	Admin        bool   `json:"admin"`
}

// Library is one media root. It corresponds to a Jellyfin "virtual folder":
// Name is what the client shows, Type is the CollectionType that decides which
// metadata providers and item kinds apply.
//
// Upstream allows several locations per virtual folder. One path per library is
// the simplification here; adding a second field later is easier than taking a
// list away, and nothing in the scanner design depends on it being singular.
type Library struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`

	// ReadOnly is a pointer so that "unset" is distinguishable from "false".
	// The zero value of a bool is false, which would make an omitted field mean
	// *writable* -- the dangerous direction. Unset means read-only; making a
	// library writable has to be deliberate. Use IsReadOnly, not this field.
	ReadOnly *bool `json:"readonly"`
}

// IsReadOnly reports the effective setting, defaulting to true when unset.
func (l Library) IsReadOnly() bool { return l.ReadOnly == nil || *l.ReadOnly }

// collectionTypes are the CollectionType values a user-created library may
// carry. Upstream has more (playlists, livetv, folders), but those are server-
// managed rather than pointed at a directory. The empty string means "mixed
// content", which upstream also permits.
var collectionTypes = map[string]bool{
	"":            true, // mixed content
	"movies":      true,
	"tvshows":     true,
	"music":       true,
	"musicvideos": true,
	"homevideos":  true,
	"boxsets":     true,
	"books":       true,
	"mixed":       true,
}

func defaultConfig() *Config {
	var c Config
	c.Server.Name = "Gofin"
	c.Server.Bind = "0.0.0.0"
	c.Server.Port = 8096
	c.Server.Autodiscovery = true
	c.Server.LocalNetworks = []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	c.Paths.Data = "./data"
	c.Paths.Config = "./config"
	c.Paths.Cache = "./cache"
	c.Paths.Logs = "./logs"
	c.Logging.Level = "info"
	c.Logging.Format = "text"
	c.Metadata.TMDB.Language = jftmdb.DefaultLanguage
	c.Metadata.TMDB.PosterSize = jftmdb.DefaultPosterSize
	return &c
}

// load applies the precedence chain: defaults, then file, then GOFIN_* env,
// then explicit flags. Only flags the user actually set override the file,
// which is why this walks flag.Visit rather than reading every flag value.
func load(fs *flag.FlagSet, path string) (*Config, error) {
	cfg := defaultConfig()

	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !(errors.Is(err, os.ErrNotExist) && path == defaultConfigPath()) {
			return nil, fmt.Errorf("reading config %s: %w", path, err)
		}
		if err == nil {
			if err := json.Unmarshal(b, cfg); err != nil {
				return nil, fmt.Errorf("parsing config %s: %w", path, err)
			}
		}
	}

	if v := os.Getenv("GOFIN_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = n
		}
	}
	if v := os.Getenv("GOFIN_BIND"); v != "" {
		cfg.Server.Bind = v
	}
	// Accept Jellyfin's own env vars so this drops into an existing container.
	for env, dst := range map[string]*string{
		"JELLYFIN_DATA_DIR":   &cfg.Paths.Data,
		"JELLYFIN_CONFIG_DIR": &cfg.Paths.Config,
		"JELLYFIN_CACHE_DIR":  &cfg.Paths.Cache,
		"JELLYFIN_LOG_DIR":    &cfg.Paths.Logs,
		"JELLYFIN_WEB_DIR":    &cfg.Web.Dir,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("JELLYFIN_PublishedServerUrl"); v != "" {
		cfg.Server.PublishedURL = v
	}
	// Libraries come through the environment as the same JSON the config file
	// uses. A bespoke mini-syntax here would be a second thing to get right for
	// no benefit, and container users are already writing JSON.
	// An API key is a credential; letting it come from the environment keeps it
	// out of a config file that gets copied around.
	if v := os.Getenv("GOFIN_TMDB_API_KEY"); v != "" {
		cfg.Metadata.TMDB.APIKey = v
		cfg.Metadata.TMDB.Enabled = true
	}
	if v := os.Getenv("GOFIN_LIBRARIES"); v != "" {
		var libs []Library
		if err := json.Unmarshal([]byte(v), &libs); err != nil {
			return nil, fmt.Errorf("parsing GOFIN_LIBRARIES: %w", err)
		}
		cfg.Libraries = libs
	}

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Server.Port, _ = strconv.Atoi(f.Value.String())
		case "bind":
			cfg.Server.Bind = f.Value.String()
		case "base-url":
			cfg.Server.BaseURL = f.Value.String()
		case "datadir":
			cfg.Paths.Data = f.Value.String()
		case "webdir":
			cfg.Web.Dir = f.Value.String()
			cfg.Web.Enabled = true
		case "no-web":
			cfg.Web.Enabled = false
		case "log-level":
			cfg.Logging.Level = f.Value.String()
		case "log-format":
			cfg.Logging.Format = f.Value.String()
		case "library":
			// Repeating -library replaces the configured set rather than
			// appending to it, so the flag layer overrides the file layer the
			// same way every other setting does.
			if g, ok := f.Value.(flag.Getter); ok {
				if libs, ok := g.Get().([]Library); ok {
					cfg.Libraries = libs
				}
			}
		}
	})

	cfg.Server.BaseURL = strings.TrimSuffix(cfg.Server.BaseURL, "/")
	if cfg.Server.BaseURL != "" && !strings.HasPrefix(cfg.Server.BaseURL, "/") {
		cfg.Server.BaseURL = "/" + cfg.Server.BaseURL
	}

	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolving the working directory: %w", err)
	}
	libs, err := normalizeLibraries(cfg.Libraries, wd)
	if err != nil {
		return nil, err
	}
	cfg.Libraries = libs

	return cfg, nil
}

// libraryList collects repeated -library flags. Each occurrence is a
// comma-separated key=value list, which keeps the meaning of every part
// explicit instead of relying on positional order:
//
//	-library 'name=Movies,type=movies,path=/mnt/alberca/Movies,readonly'
//
// A path containing a comma cannot be expressed this way; use the config file
// for that.
type libraryList []Library

func (l *libraryList) String() string {
	names := make([]string, 0, len(*l))
	for _, lib := range *l {
		names = append(names, lib.Name)
	}
	return strings.Join(names, ",")
}

// Get lets load pull the parsed value back out through flag.Getter, rather than
// round-tripping it through String.
func (l *libraryList) Get() any { return []Library(*l) }

func (l *libraryList) Set(s string) error {
	var lib Library
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		key, val, hasVal := strings.Cut(field, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)

		switch key {
		case "name":
			lib.Name = val
		case "path":
			lib.Path = val
		case "type":
			lib.Type = val
		case "readonly", "ro":
			// Bare "readonly" means true; "readonly=false" opts out.
			ro := true
			if hasVal {
				b, err := strconv.ParseBool(val)
				if err != nil {
					return fmt.Errorf("readonly: %w", err)
				}
				ro = b
			}
			lib.ReadOnly = &ro
		case "writable", "rw":
			ro := false
			lib.ReadOnly = &ro
		default:
			return fmt.Errorf("unknown library field %q (want name, path, type, readonly)", key)
		}
	}
	if lib.Path == "" {
		return fmt.Errorf("library %q: path= is required", s)
	}
	*l = append(*l, lib)
	return nil
}

// normalizeLibraries cleans and checks the library list without touching the
// filesystem, so it stays cheap and testable. Anything that needs to stat a
// path lives in validateLibraryPaths instead.
//
// wd is the working directory relative paths resolve against.
func normalizeLibraries(libs []Library, wd string) ([]Library, error) {
	out := make([]Library, 0, len(libs))
	byName := map[string]int{}
	byPath := map[string]int{}

	for i, l := range libs {
		l.Name = strings.TrimSpace(l.Name)
		l.Type = strings.ToLower(strings.TrimSpace(l.Type))
		l.Path = strings.TrimSpace(l.Path)

		if l.Path == "" {
			return nil, fmt.Errorf("library %d (%q): path is required", i, l.Name)
		}
		// An absolute, symlink-free-ish path makes the ID derivation stable and
		// the overlap check below meaningful.
		l.Path = filepath.Clean(abs(l.Path, wd))

		// A missing name defaults to the directory name, which is what a user
		// almost always wants and what upstream's "add library" dialog suggests.
		if l.Name == "" {
			l.Name = filepath.Base(l.Path)
		}
		if !collectionTypes[l.Type] {
			return nil, fmt.Errorf("library %q: unknown type %q (valid: %s)",
				l.Name, l.Type, strings.Join(sortedCollectionTypes(), ", "))
		}

		// Names are the client-visible identity and are matched
		// case-insensitively, so two libraries cannot share one.
		key := strings.ToLower(l.Name)
		if prev, dup := byName[key]; dup {
			return nil, fmt.Errorf("library %q: duplicate name (already used by library %d)", l.Name, prev)
		}
		byName[key] = i

		if prev, dup := byPath[l.Path]; dup {
			return nil, fmt.Errorf("library %q: path %s is already used by library %d", l.Name, l.Path, prev)
		}
		byPath[l.Path] = i

		// Nesting one library inside another makes every item in the overlap
		// resolve twice, under two different parents and two different IDs.
		for _, o := range out {
			if pathContains(o.Path, l.Path) || pathContains(l.Path, o.Path) {
				return nil, fmt.Errorf("library %q (%s) overlaps library %q (%s); "+
					"one media root may not contain another", l.Name, l.Path, o.Name, o.Path)
			}
		}

		// Pin the effective value so `--dry-run` and the startup log show what
		// is actually in force rather than a null.
		ro := l.IsReadOnly()
		l.ReadOnly = &ro

		out = append(out, l)
	}
	return out, nil
}

// pathContains reports whether child is parent or lies beneath it, comparing
// whole path elements so /a/bc is not treated as living inside /a/b.
func pathContains(parent, child string) bool {
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, strings.TrimSuffix(parent, string(filepath.Separator))+string(filepath.Separator))
}

func sortedCollectionTypes() []string {
	out := make([]string, 0, len(collectionTypes))
	for t := range collectionTypes {
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// validateLibraryPaths does the filesystem checks: the path must exist, be a
// directory, and actually be readable by this process.
//
// Failing at startup is the point. A library that is unreadable because a mount
// is not up yet, or because the service user lacks permission, otherwise shows
// up as a silently empty library after a scan that appeared to succeed.
func validateLibraryPaths(libs []Library) error {
	for _, l := range libs {
		fi, err := os.Stat(l.Path)
		if err != nil {
			return fmt.Errorf("library %q: %w", l.Name, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("library %q: %s is not a directory", l.Name, l.Path)
		}
		// Stat succeeding does not mean the directory can be listed; the
		// execute/read bits are separate.
		f, err := os.Open(l.Path)
		if err != nil {
			return fmt.Errorf("library %q: cannot read %s: %w", l.Name, l.Path, err)
		}
		_, err = f.ReadDir(1)
		f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("library %q: cannot list %s: %w", l.Name, l.Path, err)
		}
	}
	return nil
}

func defaultConfigPath() string {
	if v := os.Getenv("GOFIN_CONFIG"); v != "" {
		return v
	}
	return "gofin.json"
}

// ---------------------------------------------------------------- passwords

// Jellyfin stores PBKDF2-SHA512 hashes in a $-delimited format. The iteration
// count here matches current upstream; if you are importing an existing user
// table, read the count out of each stored hash rather than assuming it.
const pbkdf2Iterations = 210_000

func hashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	sum := pbkdf2SHA512([]byte(password), salt, pbkdf2Iterations, 64)
	return fmt.Sprintf("$PBKDF2-SHA512$iterations=%d$%s$%s",
		pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(sum))
}

func verifyPassword(password, stored string) bool {
	parts := strings.Split(strings.TrimPrefix(stored, "$"), "$")
	if len(parts) != 4 || !strings.EqualFold(parts[0], "PBKDF2-SHA512") {
		return false
	}
	iter, err := strconv.Atoi(strings.TrimPrefix(parts[1], "iterations="))
	if err != nil || iter <= 0 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA512([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2SHA512 is RFC 2898 PBKDF2, implemented here to avoid pulling in
// golang.org/x/crypto for thirty lines of code.
func pbkdf2SHA512(password, salt []byte, iter, keyLen int) []byte {
	h := sha512.New
	hashLen := sha512.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)

	for block := 1; block <= blocks; block++ {
		mac := hmac.New(h, password)
		mac.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		mac.Write(buf)
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// seedUsers reconciles the users declared in the config file into the database.
//
// Config declares, the database caches: gofin.json is the source of truth for
// who exists, and this runs on every boot. There is deliberately no user CRUD
// over HTTP, so a user is created, renamed or promoted by editing the config.
//
// The password rule is the subtle part, and it is what lets this replace the
// old trick of rewriting gofin.json with the computed hash:
//
//   - An explicit password_hash in the config always wins.
//   - Otherwise, if the stored hash already verifies against the configured
//     plaintext, keep the stored hash. Without this branch a config carrying
//     only a plaintext password would re-hash with a fresh salt on every boot,
//     churning the row and invalidating nothing but wasting the work.
//   - Otherwise hash the plaintext: either this is first boot, or the password
//     in the config changed.
func seedUsers(store *Store, users []ConfigUser) error {
	for i := range users {
		cu := &users[i]

		// IDs are derived from the name so a config-declared user keeps the
		// same ID across restarts and across machines. Renaming a user
		// therefore creates a new one, which is documented in the example
		// config.
		id := jfid.HashString("user:"+strings.ToLower(cu.Name), jfid.UTF16LE)

		existing, err := store.UserByID(id)
		if err != nil {
			return fmt.Errorf("user %q: %w", cu.Name, err)
		}

		hash := cu.PasswordHash
		switch {
		case hash != "":
			// explicit, use as-is
		case existing != nil && verifyPassword(cu.Password, existing.PasswordHash):
			hash = existing.PasswordHash
		default:
			hash = hashPassword(cu.Password)
		}

		if err := store.UpsertUser(&User{
			ID:           id,
			Name:         cu.Name,
			PasswordHash: hash,
			Admin:        cu.Admin,
		}); err != nil {
			return fmt.Errorf("user %q: %w", cu.Name, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- state

// User and Token live in package jfdb, which owns their storage. Aliased here
// so handlers, DTO builders and tests keep referring to them unqualified.
type (
	User  = jfdb.User
	Token = jfdb.Token
)

// Store is the SQLite-backed repository. It replaced an in-memory map whose
// comment promised that swapping it in "should not require touching any
// handler"; that turned out to be true of the item store but not of this one,
// because every lookup can now fail and a silently swallowed database error on
// the auth path is the worst possible way to find that out.
type Store = jfdb.Store

// ---------------------------------------------------------------- items

// ItemStore is the in-memory item repository. Like Store it is the phase-1
// stand-in for SQLite, and it is replaced wholesale at the end of a scan so a
// rescan is atomic: clients never observe a half-populated library.
type ItemStore struct {
	mu       sync.RWMutex
	byID     map[jfid.GUID]jfscan.Item
	byParent map[jfid.GUID][]jfid.GUID
	scanning bool
	lastScan time.Time
}

func NewItemStore() *ItemStore {
	return &ItemStore{
		byID:     map[jfid.GUID]jfscan.Item{},
		byParent: map[jfid.GUID][]jfid.GUID{},
	}
}

// Replace swaps in a freshly scanned set of items.
func (s *ItemStore) Replace(items []jfscan.Item) {
	byID := make(map[jfid.GUID]jfscan.Item, len(items))
	byParent := map[jfid.GUID][]jfid.GUID{}
	for _, it := range items {
		byID[it.ID] = it
		byParent[it.ParentID] = append(byParent[it.ParentID], it.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID = byID
	s.byParent = byParent
	s.lastScan = time.Now()
}

func (s *ItemStore) Get(id jfid.GUID) (jfscan.Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.byID[id]
	return it, ok
}

// Children returns the items directly under parent, in the order the scan
// produced -- which is already sorted.
func (s *ItemStore) Children(parent jfid.GUID) []jfscan.Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.byParent[parent]
	out := make([]jfscan.Item, 0, len(ids))
	for _, id := range ids {
		if it, ok := s.byID[id]; ok {
			out = append(out, it)
		}
	}
	return out
}

func (s *ItemStore) ChildCount(parent jfid.GUID) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byParent[parent])
}

// All returns every item.
func (s *ItemStore) All() []jfscan.Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]jfscan.Item, 0, len(s.byID))
	for _, it := range s.byID {
		out = append(out, it)
	}
	return out
}

// Descendants returns everything beneath parent, at any depth. "Recently added"
// for a music library has to reach the tracks, which sit two levels down.
func (s *ItemStore) Descendants(parent jfid.GUID) []jfscan.Item {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []jfscan.Item
	seen := map[jfid.GUID]bool{}
	queue := []jfid.GUID{parent}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for _, id := range s.byParent[cur] {
			if it, ok := s.byID[id]; ok {
				out = append(out, it)
				queue = append(queue, id)
			}
		}
	}
	return out
}

func (s *ItemStore) Total() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

func (s *ItemStore) setScanning(v bool) {
	s.mu.Lock()
	s.scanning = v
	s.mu.Unlock()
}

func (s *ItemStore) Scanning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scanning
}

// scanLibraries walks every configured library and replaces the item store.
//
// It runs in the background: a large library takes long enough that doing it
// before the listener opens would look like a server that fails to start, and
// upstream treats a library scan as a background task for the same reason.
func (s *Server) scanLibraries() {
	if s.items.Scanning() {
		s.log.Warn("scan already in progress; ignoring the request")
		return
	}
	s.items.setScanning(true)
	defer s.items.setScanning(false)

	start := time.Now()
	s.log.Info("library scan started", "libraries", len(s.cfg.Libraries))

	var all []jfscan.Item
	for _, l := range s.cfg.Libraries {
		res, err := jfscan.Scan(jfscan.Options{
			Root:           l.Path,
			CollectionType: l.Type,
			ParentID:       l.ID(s.cfg.Paths.Data),
		})
		if err != nil {
			s.log.Error("library scan failed", "library", l.Name, "path", l.Path, "err", err)
			continue
		}
		for _, e := range res.Errors {
			s.log.Warn("unreadable directory during scan", "library", l.Name, "err", e)
		}
		s.log.Info("library scanned", "library", l.Name,
			"items", len(res.Items), "errors", len(res.Errors), "took", res.Duration)
		all = append(all, res.Items...)
	}

	// Adopt any poster already in the cache before publishing the items, so a
	// restart shows artwork immediately instead of waiting on the network.
	for i := range all {
		if all[i].PrimaryImage == "" {
			if p := s.cachedPoster(all[i].ID); p != "" {
				all[i].PrimaryImage = p
			}
		}
	}

	s.items.Replace(all)
	s.log.Info("library scan complete", "items", len(all), "took", time.Since(start))

	if s.tmdb != nil {
		go s.fetchArtwork()
	}
}

// posterDir is where downloaded artwork lives.
//
// It is under the CACHE directory, never beside the media. A library declared
// readonly must stay untouched, and artwork is derived data that can always be
// fetched again -- deleting this directory costs nothing but bandwidth.
func (s *Server) posterDir() string {
	return filepath.Join(s.cfg.Paths.Cache, "images")
}

func (s *Server) posterPath(id jfid.GUID) string {
	return filepath.Join(s.posterDir(), id.String()+".jpg")
}

// missPath marks a title TMDB could not match.
//
// Without it every restart re-queries the network for the same unmatchable
// files -- and a library of scene releases has plenty. The marker is a plain
// empty file so clearing the cache clears the misses too.
func (s *Server) missPath(id jfid.GUID) string {
	return filepath.Join(s.posterDir(), id.String()+".miss")
}

func (s *Server) cachedPoster(id jfid.GUID) string {
	p := s.posterPath(id)
	if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
		return p
	}
	return ""
}

// clearArtworkMisses forgets which titles TMDB could not match, so they are
// looked up again -- the point of doing so is usually that the files have been
// renamed since.
func (s *Server) clearArtworkMisses() {
	entries, err := os.ReadDir(s.posterDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".miss") {
			_ = os.Remove(filepath.Join(s.posterDir(), e.Name()))
		}
	}
}

// fetchArtwork downloads posters for items that have none locally.
//
// It runs in the background after a scan and is deliberately unhurried: the
// client already works without artwork, so there is nothing to be gained by
// hammering the API and something to lose if the key gets rate-limited.
func (s *Server) fetchArtwork() {
	if s.tmdb == nil {
		return
	}
	if !s.artworkRunning.CompareAndSwap(false, true) {
		s.log.Debug("artwork fetch already running")
		return
	}
	defer s.artworkRunning.Store(false)

	if err := os.MkdirAll(s.posterDir(), 0o755); err != nil {
		s.log.Error("cannot create the poster cache", "path", s.posterDir(), "err", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var want []jfscan.Item
	for _, it := range s.items.All() {
		// Only films are looked up: TMDB is a movie database, and music would
		// need MusicBrainz instead.
		if it.Kind != jfscan.KindMovie || it.PrimaryImage != "" {
			continue
		}
		if _, err := os.Stat(s.missPath(it.ID)); err == nil {
			continue // already known to be unmatchable
		}
		want = append(want, it)
	}
	if len(want) == 0 {
		return
	}

	s.log.Info("fetching artwork", "items", len(want), "provider", "tmdb")
	start := time.Now()
	var got, missed, failed int

	for _, it := range want {
		m, err := s.tmdb.SearchMovie(ctx, it.Name, it.Year)
		switch {
		case errors.Is(err, jftmdb.ErrNotFound):
			// Record the miss so the next restart does not ask again.
			_ = os.WriteFile(s.missPath(it.ID), nil, 0o644)
			missed++
			s.log.Debug("no artwork match", "title", it.Name, "year", it.Year)
			continue
		case err != nil:
			failed++
			s.log.Warn("artwork lookup failed", "title", it.Name, "err", err)
			// A rejected key or a rate limit will fail for every remaining
			// item too; stopping keeps the log readable and the key safe.
			if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "429") {
				s.log.Error("stopping artwork fetch", "reason", err)
				return
			}
			continue
		}

		if err := s.tmdb.DownloadPoster(ctx, m.PosterPath, s.posterPath(it.ID)); err != nil {
			failed++
			s.log.Warn("poster download failed", "title", it.Name, "err", err)
			continue
		}
		got++
		s.log.Debug("artwork fetched", "title", it.Name, "matched", m.Title, "year", m.Year)
	}

	s.log.Info("artwork fetch complete",
		"downloaded", got, "unmatched", missed, "failed", failed, "took", time.Since(start))

	// Republish so the new posters appear without waiting for a rescan.
	if got > 0 {
		items := s.items.All()
		for i := range items {
			if items[i].PrimaryImage == "" {
				if p := s.cachedPoster(items[i].ID); p != "" {
					items[i].PrimaryImage = p
				}
			}
		}
		s.items.Replace(items)
	}
}

// ---------------------------------------------------------------- DTOs

// Field names are PascalCase and shapes match upstream exactly. Where a client
// distinguishes null from absent, the field is a pointer or has no omitempty.

type PublicSystemInfo struct {
	LocalAddress           string    `json:"LocalAddress"`
	ServerName             string    `json:"ServerName"`
	Version                string    `json:"Version"`
	ProductName            string    `json:"ProductName"`
	OperatingSystem        string    `json:"OperatingSystem"`
	ID                     jfid.GUID `json:"Id"`
	StartupWizardCompleted bool      `json:"StartupWizardCompleted"`
}

type SystemInfo struct {
	PublicSystemInfo
	PackageName            string `json:"PackageName"`
	HasPendingRestart      bool   `json:"HasPendingRestart"`
	IsShuttingDown         bool   `json:"IsShuttingDown"`
	SupportsLibraryMonitor bool   `json:"SupportsLibraryMonitor"`
	WebSocketPortNumber    int    `json:"WebSocketPortNumber"`
	CanSelfRestart         bool   `json:"CanSelfRestart"`
	CanLaunchWebBrowser    bool   `json:"CanLaunchWebBrowser"`
	ProgramDataPath        string `json:"ProgramDataPath"`
	WebPath                string `json:"WebPath"`
	ItemsByNamePath        string `json:"ItemsByNamePath"`
	CachePath              string `json:"CachePath"`
	LogPath                string `json:"LogPath"`
	InternalMetadataPath   string `json:"InternalMetadataPath"`
	TranscodingTempPath    string `json:"TranscodingTempPath"`
	SystemArchitecture     string `json:"SystemArchitecture"`
}

type UserPolicy struct {
	IsAdministrator                bool     `json:"IsAdministrator"`
	IsHidden                       bool     `json:"IsHidden"`
	IsDisabled                     bool     `json:"IsDisabled"`
	EnableRemoteAccess             bool     `json:"EnableRemoteAccess"`
	EnableAllFolders               bool     `json:"EnableAllFolders"`
	EnabledFolders                 []string `json:"EnabledFolders"`
	EnableMediaPlayback            bool     `json:"EnableMediaPlayback"`
	EnableAudioPlaybackTranscoding bool     `json:"EnableAudioPlaybackTranscoding"`
	EnableVideoPlaybackTranscoding bool     `json:"EnableVideoPlaybackTranscoding"`
	EnablePlaybackRemuxing         bool     `json:"EnablePlaybackRemuxing"`
	EnableContentDeletion          bool     `json:"EnableContentDeletion"`
	EnableContentDownloading       bool     `json:"EnableContentDownloading"`
	EnableSyncTranscoding          bool     `json:"EnableSyncTranscoding"`
	EnableUserPreferenceAccess     bool     `json:"EnableUserPreferenceAccess"`
	EnablePublicSharing            bool     `json:"EnablePublicSharing"`
	RemoteClientBitrateLimit       int      `json:"RemoteClientBitrateLimit"`
	AuthenticationProviderID       string   `json:"AuthenticationProviderId"`
	PasswordResetProviderID        string   `json:"PasswordResetProviderId"`
	SyncPlayAccess                 string   `json:"SyncPlayAccess"`
	InvalidLoginAttemptCount       int      `json:"InvalidLoginAttemptCount"`
	LoginAttemptsBeforeLockout     int      `json:"LoginAttemptsBeforeLockout"`
	MaxActiveSessions              int      `json:"MaxActiveSessions"`
	BlockedTags                    []string `json:"BlockedTags"`
	AccessSchedules                []any    `json:"AccessSchedules"`
	BlockUnratedItems              []string `json:"BlockUnratedItems"`
	EnabledChannels                []string `json:"EnabledChannels"`
	EnableAllChannels              bool     `json:"EnableAllChannels"`
	EnabledDevices                 []string `json:"EnabledDevices"`
	EnableAllDevices               bool     `json:"EnableAllDevices"`
}

type UserConfiguration struct {
	PlayDefaultAudioTrack      bool     `json:"PlayDefaultAudioTrack"`
	SubtitleLanguagePreference string   `json:"SubtitleLanguagePreference"`
	DisplayMissingEpisodes     bool     `json:"DisplayMissingEpisodes"`
	GroupedFolders             []string `json:"GroupedFolders"`
	SubtitleMode               string   `json:"SubtitleMode"`
	DisplayCollectionsView     bool     `json:"DisplayCollectionsView"`
	EnableLocalPassword        bool     `json:"EnableLocalPassword"`
	OrderedViews               []string `json:"OrderedViews"`
	LatestItemsExcludes        []string `json:"LatestItemsExcludes"`
	MyMediaExcludes            []string `json:"MyMediaExcludes"`
	HidePlayedInLatest         bool     `json:"HidePlayedInLatest"`
	RememberAudioSelections    bool     `json:"RememberAudioSelections"`
	RememberSubtitleSelections bool     `json:"RememberSubtitleSelections"`
	EnableNextEpisodeAutoPlay  bool     `json:"EnableNextEpisodeAutoPlay"`
	CastReceiverID             string   `json:"CastReceiverId"`
}

type UserDto struct {
	Name                      string            `json:"Name"`
	ServerID                  string            `json:"ServerId"`
	ID                        jfid.GUID         `json:"Id"`
	HasPassword               bool              `json:"HasPassword"`
	HasConfiguredPassword     bool              `json:"HasConfiguredPassword"`
	HasConfiguredEasyPassword bool              `json:"HasConfiguredEasyPassword"`
	EnableAutoLogin           bool              `json:"EnableAutoLogin"`
	LastLoginDate             *time.Time        `json:"LastLoginDate"`
	LastActivityDate          *time.Time        `json:"LastActivityDate"`
	Configuration             UserConfiguration `json:"Configuration"`
	Policy                    UserPolicy        `json:"Policy"`
}

type SessionInfoDto struct {
	ID                    string    `json:"Id"`
	UserID                jfid.GUID `json:"UserId"`
	UserName              string    `json:"UserName"`
	Client                string    `json:"Client"`
	DeviceName            string    `json:"DeviceName"`
	DeviceID              string    `json:"DeviceId"`
	ApplicationVersion    string    `json:"ApplicationVersion"`
	ServerID              string    `json:"ServerId"`
	LastActivityDate      time.Time `json:"LastActivityDate"`
	SupportsRemoteControl bool      `json:"SupportsRemoteControl"`
	IsActive              bool      `json:"IsActive"`
	PlayableMediaTypes    []string  `json:"PlayableMediaTypes"`
	SupportedCommands     []string  `json:"SupportedCommands"`
}

type AuthenticationResult struct {
	User        UserDto        `json:"User"`
	SessionInfo SessionInfoDto `json:"SessionInfo"`
	AccessToken string         `json:"AccessToken"`
	ServerID    string         `json:"ServerId"`
}

// QueryResult is the envelope every list endpoint returns. Clients read
// TotalRecordCount to drive paging, so it is the total matching the query, not
// the length of Items.
type QueryResult[T any] struct {
	Items            []T `json:"Items"`
	TotalRecordCount int `json:"TotalRecordCount"`
	StartIndex       int `json:"StartIndex"`
}

// BaseItemDto is the shape every item on the wire takes -- libraries, folders,
// movies, tracks. This is the subset needed to render a library view; the full
// upstream DTO has well over a hundred fields and they get added as the things
// that populate them get built.
//
// The slice and map fields have no omitempty on purpose. Clients iterate them
// without null checks, so they must serialize as [] and {} rather than null.
type BaseItemDto struct {
	Name                 string            `json:"Name"`
	ServerID             string            `json:"ServerId"`
	ID                   jfid.GUID         `json:"Id"`
	SortName             string            `json:"SortName"`
	Path                 string            `json:"Path,omitempty"`
	ChildCount           int               `json:"ChildCount"`
	Type                 string            `json:"Type"`
	CollectionType       string            `json:"CollectionType,omitempty"`
	IsFolder             bool              `json:"IsFolder"`
	ParentID             jfid.GUID         `json:"ParentId"`
	MediaType            string            `json:"MediaType"`
	LocationType         string            `json:"LocationType"`
	DisplayPreferencesID string            `json:"DisplayPreferencesId"`
	CanDelete            bool              `json:"CanDelete"`
	CanDownload          bool              `json:"CanDownload"`
	PlayAccess           string            `json:"PlayAccess"`
	UserData             *UserItemDataDto  `json:"UserData,omitempty"`
	ImageTags            map[string]string `json:"ImageTags"`
	BackdropImageTags    []string          `json:"BackdropImageTags"`
	ImageBlurHashes      map[string]any    `json:"ImageBlurHashes"`
	ProviderIDs          map[string]string `json:"ProviderIds"`
	Tags                 []string          `json:"Tags"`
	Genres               []string          `json:"Genres"`
	Studios              []any             `json:"Studios"`
	People               []any             `json:"People"`
	GenreItems           []any             `json:"GenreItems"`
	ExternalUrls         []any             `json:"ExternalUrls"`
	RemoteTrailers       []any             `json:"RemoteTrailers"`
	LockedFields         []string          `json:"LockedFields"`
	LockData             bool              `json:"LockData"`

	// Populated for scanned media. ProductionYear and RunTimeTicks are
	// pointers so an unknown value renders as null rather than as 0 -- a
	// client shows a zero-length item as unplayable, and year 0 as a real year.
	ProductionYear *int     `json:"ProductionYear"`
	RunTimeTicks   *int64   `json:"RunTimeTicks"`
	IndexNumber    *int     `json:"IndexNumber"`
	Container      string   `json:"Container,omitempty"`
	Album          string   `json:"Album,omitempty"`
	AlbumArtist    string   `json:"AlbumArtist,omitempty"`
	Artists        []string `json:"Artists"`
}

// mediaTypes maps a scanned kind to the MediaType clients switch on to pick a
// player.
var mediaTypes = map[jfscan.Kind]string{
	jfscan.KindMovie: "Video",
	jfscan.KindVideo: "Video",
	jfscan.KindAudio: "Audio",
}

// toItemDto renders a scanned item for the wire.
func (s *Server) toItemDto(it jfscan.Item, ud userData) BaseItemDto {
	media := mediaTypes[it.Kind]
	if media == "" {
		media = "Unknown"
	}

	d := BaseItemDto{
		Name:                 it.Name,
		ServerID:             s.serverID.String(),
		ID:                   it.ID,
		SortName:             it.SortName,
		Path:                 it.Path,
		Type:                 string(it.Kind),
		IsFolder:             it.IsFolder,
		ParentID:             it.ParentID,
		MediaType:            media,
		LocationType:         "FileSystem",
		DisplayPreferencesID: it.ID.String(),
		Container:            it.Container,
		Album:                it.Album,
		AlbumArtist:          it.AlbumArtist,
		CanDelete:            false, // media is treated as read-only
		CanDownload:          true,
		PlayAccess:           "Full",
		ImageTags:            map[string]string{},
		BackdropImageTags:    []string{},
		ImageBlurHashes:      map[string]any{},
		ProviderIDs:          map[string]string{},
		Tags:                 []string{},
		Genres:               []string{},
		Studios:              []any{},
		People:               []any{},
		GenreItems:           []any{},
		ExternalUrls:         []any{},
		RemoteTrailers:       []any{},
		LockedFields:         []string{},
		Artists:              []string{},
		UserData:             ud.dto(it.ID),
	}
	if it.Year > 0 {
		y := it.Year
		d.ProductionYear = &y
	}
	if it.IndexNumber > 0 {
		n := it.IndexNumber
		d.IndexNumber = &n
	}
	if it.AlbumArtist != "" {
		d.Artists = []string{it.AlbumArtist}
	}
	if it.IsFolder {
		d.ChildCount = s.items.ChildCount(it.ID)
	}
	// A non-empty ImageTags.Primary is what makes a client request the image at
	// all; an item with no local artwork must not advertise one, or every
	// poster slot fires a request that can only 404.
	if it.PrimaryImage != "" {
		tag := jfid.ImageTag(it.PrimaryImage, jftime.UnixToTicks(it.Modified), jfid.UTF16LE)
		d.ImageTags["Primary"] = tag
	}
	// RunTimeTicks stays nil: nothing here probes container duration, and
	// reporting 0 would make every item look zero-length.
	return d
}

// userData is one user's state for every item they have touched, loaded once
// per request.
//
// The alternative -- looking each item up while rendering it -- is a query per
// row of a listing. Only items with state have a row, so this stays small even
// for a large library.
//
// A nil map is valid and yields zero-valued entries, so an unauthenticated or
// not-yet-loaded path still renders a correctly shaped response instead of
// panicking.
type userData map[jfid.GUID]jfdb.UserData

// dto renders one item's state for the wire. Clients expect the object to be
// present and shaped even for a folder that can have no playback state.
func (u userData) dto(id jfid.GUID) *UserItemDataDto {
	d := &UserItemDataDto{Key: id.String(), ItemID: id}
	if s, ok := u[id]; ok {
		d.PlaybackPositionTicks = s.PlaybackPositionTicks
		d.PlayCount = s.PlayCount
		d.IsFavorite = s.IsFavorite
		d.Played = s.Played
	}
	return d
}

// userDataFor loads a user's state, logging and degrading to an empty set on
// failure. A database hiccup should cost the watched ticks on a listing, not
// the listing itself.
func (s *Server) userDataFor(id jfid.GUID) userData {
	ud, err := s.store.UserDataFor(id)
	if err != nil {
		s.log.Error("loading user data", "user", id.String(), "err", err)
		return nil
	}
	return ud
}

// UserItemDataDto carries per-user playback state. A library folder has none of
// it, but clients still expect the object to be present and shaped.
type UserItemDataDto struct {
	PlaybackPositionTicks int64     `json:"PlaybackPositionTicks"`
	PlayCount             int       `json:"PlayCount"`
	IsFavorite            bool      `json:"IsFavorite"`
	Played                bool      `json:"Played"`
	Key                   string    `json:"Key"`
	ItemID                jfid.GUID `json:"ItemId"`
}

// DisplayPreferencesDto is the client's own UI state -- sort order, view mode,
// image sizes. The server is only a store for it.
//
// It is not optional: a client that asks for its preferences and gets an error
// can fail before it ever renders a library, which is why this answers with
// defaults rather than 501 even though nothing is persisted yet.
type DisplayPreferencesDto struct {
	ID                 string            `json:"Id"`
	Client             string            `json:"Client"`
	ViewType           string            `json:"ViewType"`
	SortBy             string            `json:"SortBy"`
	SortOrder          string            `json:"SortOrder"`
	IndexBy            string            `json:"IndexBy,omitempty"`
	ScrollDirection    string            `json:"ScrollDirection"`
	RememberIndexing   bool              `json:"RememberIndexing"`
	RememberSorting    bool              `json:"RememberSorting"`
	ShowBackdrop       bool              `json:"ShowBackdrop"`
	ShowSidebar        bool              `json:"ShowSidebar"`
	PrimaryImageHeight int               `json:"PrimaryImageHeight"`
	PrimaryImageWidth  int               `json:"PrimaryImageWidth"`
	CustomPrefs        map[string]string `json:"CustomPrefs"`
}

func defaultDisplayPreferences(id, client string) DisplayPreferencesDto {
	return DisplayPreferencesDto{
		ID:                 id,
		Client:             client,
		ViewType:           "",
		SortBy:             "SortName",
		SortOrder:          "Ascending",
		ScrollDirection:    "Horizontal",
		RememberIndexing:   false,
		RememberSorting:    false,
		ShowBackdrop:       true,
		ShowSidebar:        false,
		PrimaryImageHeight: 250,
		PrimaryImageWidth:  250,
		CustomPrefs:        map[string]string{},
	}
}

// collectionFolderType is the .NET type name of a library root. It is part of
// the ID derivation, so it has to be carried verbatim.
const collectionFolderType = "MediaBrowser.Controller.Entities.CollectionFolder"

// virtualFolderPath mirrors upstream's on-disk layout. Jellyfin does not derive
// a library's ID from its media path: it creates a directory per library under
// <data>/root/default/<Name> containing a .mblink that points at the media, and
// the CollectionFolder ID is an MD5 over *that* path.
//
// !! UNVERIFIED, same caveat as jfid.NewItemID !!
//
// This reproduces the layout so the IDs at least can match upstream, but I have
// not confirmed it against a real database. If library IDs must survive a
// migration, check one with:
//
//	sqlite3 library.db "select hex(guid), Path from BaseItems
//	                    where type like '%CollectionFolder%';"
//
// Getting this wrong costs the per-library client state (view mode, sort) and
// nothing else -- item IDs are derived separately.
func (l Library) virtualFolderPath(dataDir string) string {
	return filepath.Join(dataDir, "root", "default", l.Name)
}

func (l Library) ID(dataDir string) jfid.GUID {
	return jfid.NewItemID(l.virtualFolderPath(dataDir), collectionFolderType, "", jfid.UTF16LE)
}

// toViewDto renders one configured library as the CollectionFolder a client
// shows in its sidebar.
func (s *Server) toViewDto(l Library, ud userData) BaseItemDto {
	id := l.ID(s.cfg.Paths.Data)
	return BaseItemDto{
		Name:           l.Name,
		ServerID:       s.serverID.String(),
		ID:             id,
		SortName:       strings.ToLower(l.Name),
		Path:           l.Path,
		Type:           "CollectionFolder",
		CollectionType: l.Type,
		IsFolder:       true,
		MediaType:      "Unknown",
		LocationType:   "FileSystem",
		// Clients key their per-view UI state on this, so it must be stable
		// across restarts -- which it is, being derived from the library name.
		DisplayPreferencesID: id.String(),
		// Deleting a library root is a server-admin operation, and this build
		// treats media as read-only regardless.
		CanDelete:         false,
		CanDownload:       false,
		PlayAccess:        "Full",
		ChildCount:        s.items.ChildCount(id),
		ImageTags:         map[string]string{},
		BackdropImageTags: []string{},
		ImageBlurHashes:   map[string]any{},
		ProviderIDs:       map[string]string{},
		Tags:              []string{},
		Genres:            []string{},
		Studios:           []any{},
		People:            []any{},
		GenreItems:        []any{},
		ExternalUrls:      []any{},
		RemoteTrailers:    []any{},
		LockedFields:      []string{},
		UserData:          ud.dto(id),
	}
}

func (s *Server) views(ud userData) []BaseItemDto {
	out := make([]BaseItemDto, 0, len(s.cfg.Libraries))
	for _, l := range s.cfg.Libraries {
		out = append(out, s.toViewDto(l, ud))
	}
	return out
}

func toUserDto(u *User, serverID string) UserDto {
	d := UserDto{
		Name:                  u.Name,
		ServerID:              serverID,
		ID:                    u.ID,
		HasPassword:           u.PasswordHash != "",
		HasConfiguredPassword: u.PasswordHash != "",
	}
	if !u.LastLogin.IsZero() {
		t := u.LastLogin
		d.LastLoginDate = &t
	}
	d.Policy = UserPolicy{
		IsAdministrator:                u.Admin,
		EnableRemoteAccess:             true,
		EnableAllFolders:               true,
		EnableMediaPlayback:            true,
		EnableAudioPlaybackTranscoding: true,
		EnableVideoPlaybackTranscoding: true,
		EnablePlaybackRemuxing:         true,
		EnableContentDownloading:       true,
		EnableSyncTranscoding:          true,
		EnableUserPreferenceAccess:     true,
		EnableAllChannels:              true,
		EnableAllDevices:               true,
		AuthenticationProviderID:       "Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider",
		PasswordResetProviderID:        "Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider",
		SyncPlayAccess:                 "CreateAndJoinGroups",
		LoginAttemptsBeforeLockout:     -1,
		// Non-nil empty slices: these must serialize as [] not null.
		EnabledFolders:    []string{},
		BlockedTags:       []string{},
		AccessSchedules:   []any{},
		BlockUnratedItems: []string{},
		EnabledChannels:   []string{},
		EnabledDevices:    []string{},
	}
	d.Configuration = UserConfiguration{
		PlayDefaultAudioTrack:     true,
		SubtitleMode:              "Default",
		EnableNextEpisodeAutoPlay: true,
		GroupedFolders:            []string{},
		OrderedViews:              []string{},
		LatestItemsExcludes:       []string{},
		MyMediaExcludes:           []string{},
	}
	return d
}

// ---------------------------------------------------------------- server

type Server struct {
	cfg *Config
	log *slog.Logger
	// db is held only so the process can close it on shutdown; every read and
	// write goes through store.
	db       *jfdb.DB
	store    *Store
	items    *ItemStore
	tmdb     *jftmdb.Client // nil when the provider is disabled
	serverID jfid.GUID
	started  time.Time

	artworkRunning atomic.Bool
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// problem writes ASP.NET-shaped ProblemDetails, which is what clients parse on
// error paths.
// rfc9110Sections maps a status to its section in RFC 9110. The previous
// version computed it as status-399, which is right for 4xx and nonsense for
// everything else: a 501 advertised section-15.5.102, which does not exist.
// Clients read status and title rather than type, so nothing broke -- but it
// was wrong in every 5xx response, which is the set this build returns most.
var rfc9110Sections = map[int]string{
	400: "15.5.1", 401: "15.5.2", 403: "15.5.4", 404: "15.5.5",
	405: "15.5.6", 409: "15.5.10", 415: "15.5.16", 416: "15.5.17",
	422: "15.5.21", 429: "15.5.29",
	500: "15.6.1", 501: "15.6.2", 502: "15.6.3", 503: "15.6.4", 504: "15.6.5",
}

func problem(w http.ResponseWriter, status int, title string) {
	section, ok := rfc9110Sections[status]
	if !ok {
		// Fall back to the class rather than inventing a section number.
		section = "15.5"
		if status >= 500 {
			section = "15.6"
		}
	}
	writeJSON(w, status, map[string]any{
		"type":   "https://tools.ietf.org/html/rfc9110#section-" + section,
		"title":  title,
		"status": status,
	})
}

type ctxKey int

const ctxAuth ctxKey = iota

type authCtx struct {
	User  *User
	Token *Token
	Info  jfauth.Info
}

func (s *Server) authenticate(next http.HandlerFunc, requireAdmin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info := jfauth.FromRequest(r)
		if !info.HasToken() {
			problem(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		tok, user, err := s.store.Lookup(info.Token)
		if err != nil {
			// A database failure is not an authentication failure. Answering
			// 401 would send every client into a re-login loop against a
			// server that is merely broken.
			s.log.Error("token lookup", "err", err)
			problem(w, http.StatusInternalServerError, "Internal error")
			return
		}
		if tok == nil || user == nil {
			problem(w, http.StatusUnauthorized, "Invalid token")
			return
		}
		// 403 when authenticated but not permitted, 401 when not authenticated.
		// Clients branch on the difference: a 401 triggers a re-login prompt,
		// a 403 does not.
		if requireAdmin && !user.Admin {
			problem(w, http.StatusForbidden, "Forbidden")
			return
		}
		ctx := context.WithValue(r.Context(), ctxAuth, &authCtx{User: user, Token: tok, Info: info})
		next(w, r.WithContext(ctx))
	}
}

func authFrom(r *http.Request) *authCtx {
	a, _ := r.Context().Value(ctxAuth).(*authCtx)
	return a
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// --- favicon.ico ------------------------------------------------------
	// Serves a minimal 16x16 PNG favicon when requested.
	// This prevents the 501 Not Implemented error that clients otherwise get.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open("static/favicon.ico")
		if err != nil {
			http.Error(w, "favicon not found", 404)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = io.Copy(w, f)
	})

	// --- unauthenticated probes -------------------------------------------
	// Clients hit these to validate a URL before showing a login form, so they
	// must work before setup and must never require a token.

	mux.HandleFunc("GET /System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.publicInfo(r))
	})
	// Both spellings exist in the wild.
	mux.HandleFunc("GET /System/Ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, ProductName)
	})
	mux.HandleFunc("POST /System/Ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, ProductName)
	})
	mux.HandleFunc("GET /QuickConnect/Enabled", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, false)
	})
	mux.HandleFunc("GET /Branding/Configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"LoginDisclaimer": "", "CustomCss": "", "SplashscreenEnabled": false,
		})
	})
	mux.HandleFunc("GET /Branding/Css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.WriteHeader(200)
	})

	// --- authentication ----------------------------------------------------

	mux.HandleFunc("POST /Users/AuthenticateByName", s.handleAuthenticateByName)

	// Public user list, used to render the login screen's avatar picker. Real
	// Jellyfin gates this on the "enable public user list" setting.
	mux.HandleFunc("GET /Users/Public", func(w http.ResponseWriter, r *http.Request) {
		users, err := s.store.Users()
		if err != nil {
			s.log.Error("public user list", "err", err)
			problem(w, http.StatusInternalServerError, "Internal error")
			return
		}
		out := []UserDto{}
		for _, u := range users {
			out = append(out, toUserDto(u, s.serverID.String()))
		}
		writeJSON(w, 200, out)
	})

	// --- authenticated -----------------------------------------------------

	mux.HandleFunc("GET /Users/Me", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, toUserDto(authFrom(r).User, s.serverID.String()))
	}, false))

	mux.HandleFunc("GET /System/Info", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		wd, _ := os.Getwd()
		writeJSON(w, 200, SystemInfo{
			PublicSystemInfo:       s.publicInfo(r),
			HasPendingRestart:      false,
			SupportsLibraryMonitor: true,
			WebSocketPortNumber:    s.cfg.Server.Port,
			CanSelfRestart:         false,
			ProgramDataPath:        abs(s.cfg.Paths.Data, wd),
			CachePath:              abs(s.cfg.Paths.Cache, wd),
			LogPath:                abs(s.cfg.Paths.Logs, wd),
			InternalMetadataPath:   abs(filepath.Join(s.cfg.Paths.Data, "metadata"), wd),
			TranscodingTempPath:    abs(filepath.Join(s.cfg.Paths.Cache, "transcodes"), wd),
			SystemArchitecture:     "X64",
		})
	}, true))

	mux.HandleFunc("GET /System/Endpoint", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{
			"IsLocal":     isLocalRequest(r),
			"IsInNetwork": isLocalRequest(r),
		})
	}, false))

	mux.HandleFunc("GET /Sessions", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []SessionInfoDto{})
	}, false))

	mux.HandleFunc("POST /Sessions/Capabilities/Full", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}, false))

	// --- libraries and views -----------------------------------------------

	// Both spellings of the same thing. Newer clients call /UserViews with the
	// user in a query parameter; older ones -- Delfin among them -- use the
	// per-user path form. Neither is deprecated enough to skip.
	mux.HandleFunc("GET /UserViews", s.authenticate(s.handleUserViews, false))
	mux.HandleFunc("GET /Users/{userId}/Views", s.authenticate(s.handleUserViews, false))

	mux.HandleFunc("GET /Users/{userId}/Items", s.authenticate(s.handleUserItems, false))
	mux.HandleFunc("GET /Items", s.authenticate(s.handleUserItems, false))

	// These literal segments MUST be registered alongside the {itemId}
	// wildcard. Without them "/Users/{id}/Items/Latest" matches the wildcard,
	// "Latest" is parsed as a GUID, and the client's home screen gets a 400.
	// Go's mux prefers the more specific pattern, so registration order does
	// not matter -- but omitting them silently breaks real endpoints.
	mux.HandleFunc("GET /Users/{userId}/Items/Latest", s.authenticate(s.handleLatestItems, false))
	mux.HandleFunc("GET /Items/Latest", s.authenticate(s.handleLatestItems, false))

	mux.HandleFunc("GET /Users/{userId}/Items/Resume", s.authenticate(s.handleResumeItems, false))
	mux.HandleFunc("GET /Items/Resume", s.authenticate(s.handleResumeItems, false))

	// TV rows. There is no TV support yet, so these are genuinely empty -- but
	// they must answer with an empty QueryResult rather than fall through to
	// the 501 catch-all.
	//
	// The catch-all replies with an RFC 9457 problem document, and a client
	// that expects a typed body decodes it regardless of status: Delfin panics
	// with `unknown field "status", expected one of "Items", "StartIndex",
	// "TotalRecordCount"`. A 501 is correct HTTP and useless to that client.
	// An endpoint a client parses into a struct must return that struct's
	// shape, or no body at all.
	for _, p := range []string{
		"GET /Shows/NextUp", "GET /Users/{userId}/Shows/NextUp",
		"GET /Shows/Upcoming", "GET /Users/{userId}/Shows/Upcoming",
	} {
		mux.HandleFunc(p, s.authenticate(emptyQueryResult, false))
	}

	mux.HandleFunc("GET /Users/{userId}/Items/Root", s.authenticate(s.handleItemsRoot, false))

	mux.HandleFunc("GET /Items/{itemId}", s.authenticate(s.handleItemByID, false))
	mux.HandleFunc("GET /Users/{userId}/Items/{itemId}", s.authenticate(s.handleItemByID, false))

	// --- playback ----------------------------------------------------------

	mux.HandleFunc("GET /Items/{itemId}/PlaybackInfo", s.authenticate(s.handlePlaybackInfo, false))
	mux.HandleFunc("POST /Items/{itemId}/PlaybackInfo", s.authenticate(s.handlePlaybackInfo, false))

	// The stream endpoints are NOT behind s.authenticate. A media player is
	// frequently a separate process (libmpv, ExoPlayer, an <video> element) that
	// cannot attach an Authorization header, so upstream passes the token in the
	// query string -- jfauth.FromRequest already accepts api_key there. Leaving
	// these open matches upstream and is what makes playback work at all.
	// Upstream exposes both /stream and /stream.{container}. Go's mux only
	// allows a wildcard as a WHOLE segment -- "stream.{container}" panics at
	// registration -- so the last segment is captured entire and checked in the
	// handler.
	for _, p := range []string{
		"GET /Videos/{itemId}/{stream}", "HEAD /Videos/{itemId}/{stream}",
		"GET /Audio/{itemId}/{stream}", "HEAD /Audio/{itemId}/{stream}",
	} {
		mux.HandleFunc(p, s.handleStream)
	}

	// Playback state reporting. These now persist, but they keep their old
	// contract of never returning an error: a client that cannot report
	// progress may refuse to start playback, and it will retry a failing call
	// for the whole of playback. A write failure is logged and still answered
	// 204.
	mux.HandleFunc("POST /Sessions/Playing", s.authenticate(s.handlePlaybackStart, false))
	mux.HandleFunc("POST /Sessions/Playing/Progress", s.authenticate(s.handlePlaybackProgress, false))
	mux.HandleFunc("POST /Sessions/Playing/Stopped", s.authenticate(s.handlePlaybackStopped, false))
	mux.HandleFunc("POST /Sessions/Playing/Ping", s.authenticate(s.handlePlaybackPing, false))

	// Marking watched and favourited. Both spellings of each: Delfin and other
	// established clients use the legacy per-user paths, newer ones do not.
	for _, p := range []string{
		"POST /Users/{userId}/FavoriteItems/{itemId}", "POST /UserFavoriteItems/{itemId}",
	} {
		mux.HandleFunc(p, s.authenticate(s.handleSetFavorite(true), false))
	}
	for _, p := range []string{
		"DELETE /Users/{userId}/FavoriteItems/{itemId}", "DELETE /UserFavoriteItems/{itemId}",
	} {
		mux.HandleFunc(p, s.authenticate(s.handleSetFavorite(false), false))
	}
	for _, p := range []string{
		"POST /Users/{userId}/PlayedItems/{itemId}", "POST /UserPlayedItems/{itemId}",
	} {
		mux.HandleFunc(p, s.authenticate(s.handleSetPlayed(true), false))
	}
	for _, p := range []string{
		"DELETE /Users/{userId}/PlayedItems/{itemId}", "DELETE /UserPlayedItems/{itemId}",
	} {
		mux.HandleFunc(p, s.authenticate(s.handleSetPlayed(false), false))
	}

	// Logging out. Tokens outlive the process now, so there has to be a way to
	// end one deliberately.
	mux.HandleFunc("POST /Sessions/Logout", s.authenticate(s.handleLogout, false))
	mux.HandleFunc("DELETE /Sessions/AccessToken", s.authenticate(s.handleRevokeToken, true))

	// Artwork. Unauthenticated on purpose: these URLs go straight into <img>
	// tags, which cannot set an Authorization header, and upstream serves them
	// the same way.
	mux.HandleFunc("GET /Items/{itemId}/Images/{imageType}", s.handleItemImage)
	mux.HandleFunc("GET /Items/{itemId}/Images/{imageType}/{index}", s.handleItemImage)

	// Triggering a rescan. Upstream exposes this as a scheduled task; this is
	// the smallest thing that lets a client ask for one.
	mux.HandleFunc("POST /Library/Refresh", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		go s.scanLibraries()
		w.WriteHeader(http.StatusNoContent)
	}, true))

	// Retry artwork without a full rescan. Clearing the miss markers first
	// makes this also a way to re-try titles TMDB could not match, after
	// renaming the files that caused it.
	mux.HandleFunc("POST /Library/RefreshArtwork", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		if s.tmdb == nil {
			problem(w, http.StatusServiceUnavailable, "No artwork provider is configured")
			return
		}
		if r.URL.Query().Get("retryMissed") == "true" {
			s.clearArtworkMisses()
		}
		go s.fetchArtwork()
		w.WriteHeader(http.StatusNoContent)
	}, true))

	// A user with no avatar is a 404, not a 501: that is the same answer
	// upstream gives, and clients fall back to a generated monogram on it.
	mux.HandleFunc("GET /Users/{userId}/Images/Primary", func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusNotFound, "No primary image")
	})

	// Display preferences are the client's own UI state. Nothing is persisted
	// yet, so every client gets the defaults back; answering with a valid shape
	// matters more than remembering it, because a client that fails to read its
	// preferences can stop before rendering anything at all.
	mux.HandleFunc("GET /DisplayPreferences/{prefsId}", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("prefsId")
		s.log.Debug("display preferences read (not persisted)", "id", id)
		writeJSON(w, 200, defaultDisplayPreferences(id, r.URL.Query().Get("client")))
	}, false))

	// Accepted and discarded. Returning an error here makes some clients retry
	// the write in a loop.
	mux.HandleFunc("POST /DisplayPreferences/{prefsId}", s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.log.Debug("display preferences write discarded", "id", r.PathValue("prefsId"))
		w.WriteHeader(http.StatusNoContent)
	}, false))

	// Features a player probes for and can live without. Each answers 404 with
	// an EMPTY body: "this item has none" is the truth, and an empty body is
	// the one response a client cannot mis-decode. Falling through to the 501
	// catch-all hands them a problem document instead, which is what produced
	// `missing field "IntroStart"` and the trickplay decode failures.
	// (/Videos/{id}/Intros is handled inside handleStream: Go's mux rejects it
	// as a separate pattern alongside the /Videos/{id}/{stream} wildcard.)
	for _, p := range []string{
		"GET /Episode/{itemId}/IntroTimestamps",
		"GET /Users/{userId}/Items/{itemId}/Intros",
		// The last segment is captured whole -- "{index}.jpg" is not a legal
		// wildcard -- so this covers both the manifest and the tile images.
		"GET /Videos/{itemId}/Trickplay/{width}/{tile}",
	} {
		mux.HandleFunc(p, noContent404)
	}

	// --- websocket ---------------------------------------------------------

	mux.HandleFunc("GET /socket", s.handleSocket)

	// --- everything else ---------------------------------------------------

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("unimplemented endpoint", "method", r.Method, "path", r.URL.Path)
		problem(w, http.StatusNotImplemented, "Not implemented in this build")
	})

	var h http.Handler = mux
	h = s.stripBaseURL(h)
	h = s.withCORS(h)
	h = s.withLogging(h)
	return h
}

// requestedUser resolves which user a request is about and enforces that the
// caller may act as them.
//
// The user may arrive as a path segment or as a userId query parameter, and
// -- this is the part that bites -- a client will happily send back a DASHED
// GUID even though the server always emits dashless. Delfin does exactly that:
// it receives bdfc15bd7aff... and asks for /Users/bdfc15bd-7aff-.../Views.
// jfid.Parse accepts both forms, which is the only reason this works.
//
// An empty user reference means "me", which is what the query-parameter form
// omits it for.
func (s *Server) requestedUser(r *http.Request) (*User, bool) {
	caller := authFrom(r).User

	raw := r.PathValue("userId")
	if raw == "" {
		raw = r.URL.Query().Get("userId")
	}
	if raw == "" {
		return caller, true
	}

	id, err := jfid.Parse(raw)
	if err != nil {
		return nil, false
	}
	if id == caller.ID {
		return caller, true
	}
	// Asking about somebody else is an administrator's privilege.
	if !caller.Admin {
		return nil, false
	}
	u, err := s.store.UserByID(id)
	if err != nil {
		s.log.Error("user by id", "err", err)
		return nil, false
	}
	return u, u != nil
}

func (s *Server) handleUserViews(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requestedUser(r)
	if !ok {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}
	views := s.views(s.userDataFor(user.ID))
	writeJSON(w, 200, QueryResult[BaseItemDto]{
		Items:            views,
		TotalRecordCount: len(views),
		StartIndex:       0,
	})
}

// handleUserItems answers the browse query.
//
// At the top level -- no ParentId -- the answer is the library roots, which the
// configuration already gives us. Anything below that needs the scanner, so it
// returns an empty result rather than an error: the client renders an empty
// library, which is true, instead of an error dialog, which would be
// misleading. The WARN keeps the gap visible in the log.
func (s *Server) handleUserItems(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requestedUser(r)
	if !ok {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}
	ud := s.userDataFor(user.ID)

	q := r.URL.Query()
	raw := q.Get("ParentId")
	if raw == "" {
		raw = q.Get("parentId")
	}

	// No parent means the top level, which is the set of libraries.
	if raw == "" {
		views := s.views(ud)
		writeJSON(w, 200, QueryResult[BaseItemDto]{
			Items:            views,
			TotalRecordCount: len(views),
			StartIndex:       0,
		})
		return
	}

	parent, err := jfid.Parse(raw)
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed ParentId")
		return
	}

	children := s.items.Children(parent)
	items := make([]BaseItemDto, 0, len(children))
	for _, c := range children {
		items = append(items, s.toItemDto(c, ud))
	}

	total := len(items)
	items = page(items, q)

	writeJSON(w, 200, QueryResult[BaseItemDto]{
		Items:            items,
		TotalRecordCount: total,
		StartIndex:       startIndex(q),
	})
}

// page applies StartIndex and Limit. TotalRecordCount stays the full count, so
// the client can still size its scrollbar.
func page(items []BaseItemDto, q url.Values) []BaseItemDto {
	start := startIndex(q)
	if start >= len(items) {
		return []BaseItemDto{}
	}
	items = items[start:]

	if l := q.Get("Limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n >= 0 && n < len(items) {
			items = items[:n]
		}
	}
	return items
}

func startIndex(q url.Values) int {
	n, err := strconv.Atoi(q.Get("StartIndex"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// handleLatestItems answers the home screen's "Recently Added" row.
//
// It returns a BARE ARRAY, not a QueryResult -- upstream is inconsistent here,
// and a client handed {"Items":[...]} for this endpoint renders nothing.
func (s *Server) handleLatestItems(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requestedUser(r)
	if !ok {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}
	ud := s.userDataFor(user.ID)
	q := r.URL.Query()

	var pool []jfscan.Item
	if raw := q.Get("ParentId"); raw != "" {
		if parent, err := jfid.Parse(raw); err == nil {
			pool = s.items.Descendants(parent)
		}
	} else {
		pool = s.items.All()
	}

	// Newest first by mtime. Folders are excluded: "recently added" means
	// media, not the directory it happens to live in.
	media := pool[:0:0]
	for _, it := range pool {
		if !it.IsFolder {
			media = append(media, it)
		}
	}
	sort.Slice(media, func(i, j int) bool { return media[i].Modified.After(media[j].Modified) })

	limit := 20
	if n, err := strconv.Atoi(q.Get("Limit")); err == nil && n > 0 {
		limit = n
	}
	if len(media) > limit {
		media = media[:limit]
	}

	out := make([]BaseItemDto, 0, len(media))
	for _, it := range media {
		out = append(out, s.toItemDto(it, ud))
	}
	writeJSON(w, 200, out)
}

// emptyQueryResult answers with a well-formed, empty list.
//
// For endpoints a client parses into a QueryResult but this build has no data
// for. Returning the right shape with nothing in it is both true and safe; a
// 501 problem document is neither, because clients decode the body without
// looking at the status.
func emptyQueryResult(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, QueryResult[BaseItemDto]{
		Items: []BaseItemDto{}, TotalRecordCount: 0, StartIndex: 0,
	})
}

// noContent404 answers 404 with no body at all.
//
// For optional per-item features -- intro markers, trickplay tiles -- that this
// item genuinely does not have. An empty body is the only response that cannot
// be mis-decoded into the caller's expected type.
func noContent404(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

// ------------------------------------------------------- playback reporting

// playbackReport is the shared shape of the /Sessions/Playing* bodies. Clients
// send supersets of this; the fields not named here are ignored.
type playbackReport struct {
	ItemID        jfid.GUID `json:"ItemId"`
	PositionTicks int64     `json:"PositionTicks"`
	IsPaused      bool      `json:"IsPaused"`
	PlaySessionID string    `json:"PlaySessionId"`
}

// decodePlaybackReport reads the body and reports whether it named an item.
//
// It never fails the request. A malformed progress report is worth a log line,
// not a non-2xx that a client will retry for the rest of the film.
func (s *Server) decodePlaybackReport(r *http.Request) (playbackReport, bool) {
	var rep playbackReport
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rep); err != nil {
		s.log.Warn("malformed playback report", "path", r.URL.Path, "err", err)
		return rep, false
	}
	return rep, !rep.ItemID.IsZero()
}

// completionRatio is how far into an item counts as "watched" on stop.
// Upstream uses 90%; the last few minutes are credits.
const completionRatio = 0.9

func (s *Server) handlePlaybackStart(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	if rep, ok := s.decodePlaybackReport(r); ok {
		if err := s.store.SetProgress(a.User.ID, rep.ItemID, rep.PositionTicks, time.Now().UTC()); err != nil {
			s.log.Error("playback start", "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePlaybackProgress(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	if rep, ok := s.decodePlaybackReport(r); ok {
		if err := s.store.SetProgress(a.User.ID, rep.ItemID, rep.PositionTicks, time.Now().UTC()); err != nil {
			s.log.Error("playback progress", "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePlaybackStopped writes the final position, and marks the item watched
// if the user got near enough to the end.
//
// Runtime comes from the probe cache, so an item that has never been probed
// cannot be auto-completed -- it keeps its resume point instead, which is the
// safe way to be wrong.
func (s *Server) handlePlaybackStopped(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	rep, ok := s.decodePlaybackReport(r)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	now := time.Now().UTC()

	if runtime := s.runtimeTicks(rep.ItemID); runtime > 0 &&
		float64(rep.PositionTicks) >= float64(runtime)*completionRatio {
		// Finished: count the play and clear the resume point, so it stops
		// appearing in Continue Watching.
		if err := s.store.SetProgress(a.User.ID, rep.ItemID, 0, now); err != nil {
			s.log.Error("clearing resume point", "err", err)
		}
		if err := s.store.SetPlayed(a.User.ID, rep.ItemID, true, now); err != nil {
			s.log.Error("marking played", "err", err)
		}
	} else if err := s.store.SetProgress(a.User.ID, rep.ItemID, rep.PositionTicks, now); err != nil {
		s.log.Error("playback stopped", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePlaybackPing is the client saying it is still alive. There is nothing
// to record but the token's activity.
func (s *Server) handlePlaybackPing(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	if a.Token != nil {
		if err := s.store.TouchActivity(a.Token.Value, time.Now().UTC()); err != nil {
			s.log.Error("touch activity", "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// runtimeTicks reports an item's duration in .NET ticks, or 0 when it is not
// known.
//
// Reads the probe cache directly rather than going through mediaInfoFor, which
// falls back to running ffprobe: this is on the playback-stopped path, and a
// client reporting the end of a film must not wait on a subprocess. An unprobed
// item simply keeps its resume point instead of being auto-completed, which is
// the safe way to be wrong.
func (s *Server) runtimeTicks(id jfid.GUID) int64 {
	b, err := os.ReadFile(s.probeCachePath(id))
	if err != nil {
		return 0
	}
	var info jfprobe.MediaInfo
	if err := json.Unmarshal(b, &info); err != nil || info.Duration <= 0 {
		return 0
	}
	return int64(info.Duration / 100) // time.Duration is ns; ticks are 100ns
}

// ---------------------------------------------------- favourites and watched

// itemFromPath resolves the {itemId} path value, answering the client directly
// on failure.
func itemFromPath(w http.ResponseWriter, r *http.Request) (jfid.GUID, bool) {
	id, err := jfid.Parse(r.PathValue("itemId"))
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed item id")
		return jfid.Nil, false
	}
	return id, true
}

// writeUserData answers with an item's state after a change. Clients use the
// response to update the row in place rather than refetching the listing.
func (s *Server) writeUserData(w http.ResponseWriter, userID, itemID jfid.GUID) {
	d, err := s.store.UserDataItem(userID, itemID)
	if err != nil {
		s.log.Error("reading back user data", "err", err)
		problem(w, http.StatusInternalServerError, "Internal error")
		return
	}
	writeJSON(w, 200, userData{itemID: d}.dto(itemID))
}

func (s *Server) handleSetFavorite(favorite bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.requestedUser(r)
		if !ok {
			problem(w, http.StatusForbidden, "Forbidden")
			return
		}
		id, ok := itemFromPath(w, r)
		if !ok {
			return
		}
		if err := s.store.SetFavorite(user.ID, id, favorite); err != nil {
			s.log.Error("set favorite", "err", err)
			problem(w, http.StatusInternalServerError, "Internal error")
			return
		}
		s.writeUserData(w, user.ID, id)
	}
}

func (s *Server) handleSetPlayed(played bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.requestedUser(r)
		if !ok {
			problem(w, http.StatusForbidden, "Forbidden")
			return
		}
		id, ok := itemFromPath(w, r)
		if !ok {
			return
		}
		if err := s.store.SetPlayed(user.ID, id, played, time.Now().UTC()); err != nil {
			s.log.Error("set played", "err", err)
			problem(w, http.StatusInternalServerError, "Internal error")
			return
		}
		s.writeUserData(w, user.ID, id)
	}
}

// ---------------------------------------------------------------- logging out

// handleLogout revokes the token the caller presented.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	if a.Token != nil {
		if err := s.store.RevokeToken(a.Token.Value); err != nil {
			s.log.Error("logout", "err", err)
			problem(w, http.StatusInternalServerError, "Internal error")
			return
		}
		s.log.Info("logout", "user", a.User.Name, "client", a.Token.Client)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeToken revokes a named token. Admin only: this ends somebody
// else's session.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	value := r.URL.Query().Get("Id")
	if value == "" {
		problem(w, http.StatusBadRequest, "Missing token id")
		return
	}
	if err := s.store.RevokeToken(value); err != nil {
		s.log.Error("revoke token", "err", err)
		problem(w, http.StatusInternalServerError, "Internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleResumeItems answers "Continue Watching": started but not finished,
// most recently played first.
func (s *Server) handleResumeItems(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requestedUser(r)
	if !ok {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}

	limit := 0 // no limit
	if n, err := strconv.Atoi(r.URL.Query().Get("Limit")); err == nil && n > 0 {
		limit = n
	}

	resume, err := s.store.ResumeItems(user.ID, limit)
	if err != nil {
		s.log.Error("resume items", "err", err)
		problem(w, http.StatusInternalServerError, "Internal error")
		return
	}

	ud := s.userDataFor(user.ID)
	items := make([]BaseItemDto, 0, len(resume))
	for _, d := range resume {
		// A row can outlive its item -- a rescan renamed the file, or the
		// library was removed. Skipping is the expected outcome, not an error.
		it, ok := s.items.Get(d.ItemID)
		if !ok {
			continue
		}
		items = append(items, s.toItemDto(it, ud))
	}

	writeJSON(w, 200, QueryResult[BaseItemDto]{
		Items: items, TotalRecordCount: len(items), StartIndex: 0,
	})
}

// handleItemsRoot returns the synthetic folder every library hangs off.
func (s *Server) handleItemsRoot(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requestedUser(r); !ok {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}
	writeJSON(w, 200, BaseItemDto{
		Name:              "Media Folders",
		ServerID:          s.serverID.String(),
		ID:                jfid.HashString("gofin-root:"+s.cfg.Server.Name, jfid.UTF16LE),
		Type:              "Folder",
		IsFolder:          true,
		ChildCount:        len(s.cfg.Libraries),
		MediaType:         "Unknown",
		LocationType:      "FileSystem",
		ImageTags:         map[string]string{},
		BackdropImageTags: []string{},
		ImageBlurHashes:   map[string]any{},
		ProviderIDs:       map[string]string{},
		Tags:              []string{},
		Genres:            []string{},
		Studios:           []any{},
		People:            []any{},
		GenreItems:        []any{},
		ExternalUrls:      []any{},
		RemoteTrailers:    []any{},
		LockedFields:      []string{},
		Artists:           []string{},
	})
}

// handleItemImage serves local artwork.
//
// An item with no artwork answers 404, never 501. The difference matters: a
// client reads 404 as "no image, draw a placeholder" and stops, while a 501
// reads as a broken server and gets retried. This library has no local posters
// at all -- release groups ship advertising banners instead -- so 404 is the
// common path and it needs to be cheap and final.
func (s *Server) handleItemImage(w http.ResponseWriter, r *http.Request) {
	id, err := jfid.Parse(r.PathValue("itemId"))
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed item id")
		return
	}
	if t := r.PathValue("imageType"); !strings.EqualFold(t, "Primary") {
		// Backdrop, Logo, Thumb and friends all need a provider.
		problem(w, http.StatusNotFound, "No image of that type")
		return
	}

	it, ok := s.items.Get(id)
	if !ok || it.PrimaryImage == "" {
		problem(w, http.StatusNotFound, "No primary image")
		return
	}

	f, err := os.Open(it.PrimaryImage)
	if err != nil {
		problem(w, http.StatusNotFound, "No primary image")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		problem(w, http.StatusNotFound, "No primary image")
		return
	}

	// Artwork only changes when the file does, so it is worth caching hard.
	// This is the one response in the server where that is safe.
	w.Header().Set("Cache-Control", "public, max-age=31536000")
	w.Header().Set("ETag", `"`+jfid.ImageTag(it.PrimaryImage, jftime.UnixToTicks(fi.ModTime()), jfid.UTF16LE)+`"`)
	http.ServeContent(w, r, filepath.Base(it.PrimaryImage), fi.ModTime(), f)
}

// ---------------------------------------------------------------- playback

// MediaStreamDto is one track inside a media source.
type MediaStreamDto struct {
	Codec              string `json:"Codec"`
	Language           string `json:"Language,omitempty"`
	Title              string `json:"Title,omitempty"`
	DisplayTitle       string `json:"DisplayTitle,omitempty"`
	Profile            string `json:"Profile,omitempty"`
	Type               string `json:"Type"`
	Index              int    `json:"Index"`
	IsDefault          bool   `json:"IsDefault"`
	IsForced           bool   `json:"IsForced"`
	IsExternal         bool   `json:"IsExternal"`
	IsInterlaced       bool   `json:"IsInterlaced"`
	IsTextSubtitleStr  bool   `json:"IsTextSubtitleStream"`
	SupportsExternalSt bool   `json:"SupportsExternalStream"`
	Width              int    `json:"Width,omitempty"`
	Height             int    `json:"Height,omitempty"`
	Channels           int    `json:"Channels,omitempty"`
	SampleRate         int    `json:"SampleRate,omitempty"`
	BitRate            int    `json:"BitRate,omitempty"`
	PixelFormat        string `json:"PixelFormat,omitempty"`
	Level              int    `json:"Level,omitempty"`
}

// MediaSourceInfo tells a client how it may play an item. Without one, a client
// has nothing to open and sits on a spinner.
type MediaSourceInfo struct {
	Protocol             string           `json:"Protocol"`
	ID                   string           `json:"Id"`
	Path                 string           `json:"Path"`
	Type                 string           `json:"Type"`
	Container            string           `json:"Container"`
	Size                 *int64           `json:"Size"`
	Name                 string           `json:"Name"`
	IsRemote             bool             `json:"IsRemote"`
	ETag                 string           `json:"ETag,omitempty"`
	RunTimeTicks         *int64           `json:"RunTimeTicks"`
	SupportsTranscoding  bool             `json:"SupportsTranscoding"`
	SupportsDirectStream bool             `json:"SupportsDirectStream"`
	SupportsDirectPlay   bool             `json:"SupportsDirectPlay"`
	IsInfiniteStream     bool             `json:"IsInfiniteStream"`
	RequiresOpening      bool             `json:"RequiresOpening"`
	RequiresClosing      bool             `json:"RequiresClosing"`
	RequiresLooping      bool             `json:"RequiresLooping"`
	SupportsProbing      bool             `json:"SupportsProbing"`
	MediaStreams         []MediaStreamDto `json:"MediaStreams"`
	MediaAttachments     []any            `json:"MediaAttachments"`
	Formats              []string         `json:"Formats"`
	Bitrate              *int             `json:"Bitrate"`
	DirectStreamURL      string           `json:"DirectStreamUrl,omitempty"`
	TranscodingURL       string           `json:"TranscodingUrl,omitempty"`
	VideoType            string           `json:"VideoType,omitempty"`
	DefaultAudioStream   *int             `json:"DefaultAudioStreamIndex"`
	DefaultSubtitleIndex *int             `json:"DefaultSubtitleStreamIndex"`
}

type PlaybackInfoResponse struct {
	MediaSources  []MediaSourceInfo `json:"MediaSources"`
	PlaySessionID string            `json:"PlaySessionId"`
	ErrorCode     *string           `json:"ErrorCode"`
}

// probeCachePath stores a probe result so ffprobe runs once per file rather
// than on every playback request.
func (s *Server) probeCachePath(id jfid.GUID) string {
	return filepath.Join(s.cfg.Paths.Cache, "probe", id.String()+".json")
}

// mediaInfoFor returns container metadata, probing on demand and caching the
// result. A probe failure is not fatal: playback of a direct-playable file does
// not need it, so the caller falls back to what the scan already knew.
func (s *Server) mediaInfoFor(it jfscan.Item) *jfprobe.MediaInfo {
	p := s.probeCachePath(it.ID)
	if b, err := os.ReadFile(p); err == nil {
		var mi jfprobe.MediaInfo
		if json.Unmarshal(b, &mi) == nil {
			return &mi
		}
	}
	if !jfprobe.Available() {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mi, err := jfprobe.Probe(ctx, it.Path)
	if err != nil {
		s.log.Warn("probe failed", "path", it.Path, "err", err)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		if b, err := json.Marshal(mi); err == nil {
			_ = os.WriteFile(p, b, 0o644)
		}
	}
	return mi
}

// mediaSource builds the MediaSourceInfo for an item.
//
// Everything here is direct play: the file is handed over byte for byte, with
// no transcoding. That covers most clients for most files, and it is the only
// honest answer while no encoder is wired up -- claiming SupportsTranscoding
// would make a client ask for a transcode that never arrives.
func (s *Server) mediaSource(it jfscan.Item) MediaSourceInfo {
	container := it.Container
	size := it.Size

	src := MediaSourceInfo{
		Protocol:             "File",
		ID:                   it.ID.String(),
		Path:                 it.Path,
		Type:                 "Default",
		Name:                 it.Name,
		IsRemote:             false,
		SupportsDirectPlay:   true,
		SupportsDirectStream: true,
		SupportsTranscoding:  false,
		SupportsProbing:      true,
		MediaStreams:         []MediaStreamDto{},
		MediaAttachments:     []any{},
		Formats:              []string{},
	}

	if mi := s.mediaInfoFor(it); mi != nil {
		if mi.Container != "" {
			container = mi.Container
		}
		if mi.Size > 0 {
			size = mi.Size
		}
		if mi.Duration > 0 {
			ticks := int64(mi.Duration / 100)
			src.RunTimeTicks = &ticks
		}
		if mi.Bitrate > 0 {
			b := mi.Bitrate
			src.Bitrate = &b
		}
		for _, st := range mi.Streams {
			d := MediaStreamDto{
				Codec:              st.Codec,
				Language:           st.Language,
				Title:              st.Title,
				DisplayTitle:       streamDisplayTitle(st),
				Profile:            st.Profile,
				Type:               st.Type,
				Index:              st.Index,
				IsDefault:          st.IsDefault,
				IsForced:           st.IsForced,
				Width:              st.Width,
				Height:             st.Height,
				Channels:           st.Channels,
				SampleRate:         st.SampleRate,
				BitRate:            st.BitRate,
				PixelFormat:        st.PixelFormat,
				SupportsExternalSt: st.Type == "Subtitle",
				IsTextSubtitleStr:  st.Type == "Subtitle" && isTextSubtitle(st.Codec),
			}
			src.MediaStreams = append(src.MediaStreams, d)
			if src.DefaultAudioStream == nil && st.Type == "Audio" && st.IsDefault {
				i := st.Index
				src.DefaultAudioStream = &i
			}
		}
	}

	src.Container = container
	if size > 0 {
		sz := size
		src.Size = &sz
	}

	// The URL the client actually fetches. Jellyfin puts the token in the query
	// string here because media players are often separate processes that
	// cannot carry the Authorization header.
	kind := "Videos"
	if it.Kind == jfscan.KindAudio {
		kind = "Audio"
	}
	src.DirectStreamURL = fmt.Sprintf("%s/%s/%s/stream.%s?static=true&mediaSourceId=%s",
		s.cfg.Server.BaseURL, kind, it.ID.String(), container, it.ID.String())

	if it.Kind == jfscan.KindMovie || it.Kind == jfscan.KindVideo {
		src.VideoType = "VideoFile"
	}
	return src
}

func isTextSubtitle(codec string) bool {
	switch strings.ToLower(codec) {
	case "subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text":
		return true
	}
	return false
}

// streamDisplayTitle is the label a client shows in its track picker.
func streamDisplayTitle(st jfprobe.Stream) string {
	if st.Title != "" {
		return st.Title
	}
	var parts []string
	if st.Language != "" {
		parts = append(parts, strings.ToUpper(st.Language))
	}
	switch st.Type {
	case "Video":
		if st.Height > 0 {
			parts = append(parts, fmt.Sprintf("%dp", st.Height))
		}
	case "Audio":
		switch st.Channels {
		case 1:
			parts = append(parts, "Mono")
		case 2:
			parts = append(parts, "Stereo")
		case 6:
			parts = append(parts, "5.1")
		case 8:
			parts = append(parts, "7.1")
		}
	}
	if st.Codec != "" {
		parts = append(parts, strings.ToUpper(st.Codec))
	}
	return strings.Join(parts, " ")
}

// handlePlaybackInfo answers "how do I play this".
//
// Both GET and POST reach here. Clients POST a DeviceProfile describing what
// they can decode; this build ignores it, because every answer is direct play
// regardless -- there is no encoder to offer an alternative.
func (s *Server) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	id, err := jfid.Parse(r.PathValue("itemId"))
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed item id")
		return
	}
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	}

	it, ok := s.items.Get(id)
	if !ok {
		problem(w, http.StatusNotFound, "Item not found")
		return
	}
	if it.IsFolder {
		problem(w, http.StatusBadRequest, "Not a playable item")
		return
	}

	writeJSON(w, 200, PlaybackInfoResponse{
		MediaSources:  []MediaSourceInfo{s.mediaSource(it)},
		PlaySessionID: newMessageID(),
	})
}

// handleStream serves the media file itself.
//
// http.ServeContent does the heavy lifting: it handles Range requests, which is
// what makes seeking work, and returns 206 with the right Content-Range. A
// naive io.Copy here would force clients to download from the start every time
// the user drags the scrubber.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	// The segment is "stream" or "stream.<container>"; anything else under
	// /Videos/{id}/ is a different endpoint and must not serve the file.
	seg := r.PathValue("stream")

	// Multi-part videos. Registering this as its own pattern conflicts with the
	// wildcard above -- Go rejects a more specific GET route alongside a
	// broader HEAD one -- so it is handled here. Nothing stacks parts into a
	// single playback yet, so the honest answer is an empty list.
	// Intros -- pre-roll videos -- are the same shape and the same story: none,
	// and a client asking must get an empty list rather than a problem
	// document it will try to decode as one.
	if seg == "AdditionalParts" || seg == "Intros" {
		writeJSON(w, 200, QueryResult[BaseItemDto]{Items: []BaseItemDto{}, TotalRecordCount: 0})
		return
	}

	if seg != "stream" && !strings.HasPrefix(seg, "stream.") {
		s.log.Warn("unimplemented endpoint", "method", r.Method, "path", r.URL.Path)
		problem(w, http.StatusNotImplemented, "Not implemented in this build")
		return
	}

	id, err := jfid.Parse(r.PathValue("itemId"))
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed item id")
		return
	}
	it, ok := s.items.Get(id)
	if !ok || it.IsFolder {
		problem(w, http.StatusNotFound, "Item not found")
		return
	}

	f, err := os.Open(it.Path)
	if err != nil {
		s.log.Error("cannot open media", "path", it.Path, "err", err)
		problem(w, http.StatusNotFound, "Media file unavailable")
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		problem(w, http.StatusNotFound, "Media file unavailable")
		return
	}

	if ct := contentType(it.Container); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Advertising range support explicitly matters for players that check
	// before attempting to seek.
	w.Header().Set("Accept-Ranges", "bytes")

	s.log.Debug("streaming", "item", it.Name, "range", r.Header.Get("Range"))
	http.ServeContent(w, r, filepath.Base(it.Path), fi.ModTime(), f)
}

// contentType maps a container to a MIME type. Getting this wrong makes some
// browsers refuse to play a file they can otherwise decode.
func contentType(container string) string {
	switch strings.ToLower(container) {
	case "mkv", "matroska":
		return "video/x-matroska"
	case "mp4", "m4v", "mov":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "avi":
		return "video/x-msvideo"
	case "ts", "mpegts":
		return "video/mp2t"
	case "flac":
		return "audio/flac"
	case "mp3":
		return "audio/mpeg"
	case "m4a", "aac":
		return "audio/mp4"
	case "ogg", "oga", "opus":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	}
	return ""
}

// handleItemByID serves a single item. Clients fetch this when opening a detail
// page, and a library root has to resolve here too -- it is the item the
// browse view was opened from.
func (s *Server) handleItemByID(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requestedUser(r)
	if !ok {
		problem(w, http.StatusForbidden, "Forbidden")
		return
	}
	id, err := jfid.Parse(r.PathValue("itemId"))
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed item id")
		return
	}
	ud := s.userDataFor(user.ID)

	if it, ok := s.items.Get(id); ok {
		writeJSON(w, 200, s.toItemDto(it, ud))
		return
	}
	// A library root is not in the item store; it is built from configuration.
	for _, l := range s.cfg.Libraries {
		if l.ID(s.cfg.Paths.Data) == id {
			writeJSON(w, 200, s.toViewDto(l, ud))
			return
		}
	}
	problem(w, http.StatusNotFound, "Item not found")
}

func (s *Server) publicInfo(r *http.Request) PublicSystemInfo {
	local := s.cfg.Server.PublishedURL
	if local == "" {
		local = fmt.Sprintf("http://%s", r.Host)
	}

	// On a database error, claim the wizard is done. The alternative sends the
	// client into first-run setup and invites it to create an admin account
	// against a server that cannot currently read the ones it has.
	wizardDone := true
	if users, err := s.store.Users(); err != nil {
		s.log.Error("counting users for public info", "err", err)
	} else {
		wizardDone = len(users) > 0
	}

	return PublicSystemInfo{
		LocalAddress:           local,
		ServerName:             s.cfg.Server.Name,
		Version:                Version,
		ProductName:            ProductName,
		OperatingSystem:        "", // upstream reports "" on modern versions
		ID:                     s.serverID,
		StartupWizardCompleted: wizardDone,
	}
}

func (s *Server) handleAuthenticateByName(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"Username"`
		Pw       string `json:"Pw"`
		Password string `json:"Password"` // legacy SHA1 field, still sent by old clients
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		problem(w, http.StatusBadRequest, "Malformed body")
		return
	}

	info := jfauth.FromRequest(r)
	user, err := s.store.UserByName(body.Username)
	if err != nil {
		s.log.Error("user by name", "err", err)
		problem(w, http.StatusInternalServerError, "Internal error")
		return
	}

	// Constant work regardless of whether the user exists, so that response
	// timing does not enumerate accounts.
	valid := false
	if user != nil {
		valid = verifyPassword(body.Pw, user.PasswordHash)
	} else {
		_ = verifyPassword(body.Pw, hashPassword("dummy"))
	}
	if !valid {
		s.log.Warn("failed login", "user", body.Username, "remote", r.RemoteAddr)
		problem(w, http.StatusUnauthorized, "Invalid username or password")
		return
	}

	// Recorded in the database rather than by mutating the shared *User, which
	// raced every concurrent reader and was lost on restart besides. A failure
	// here is not worth refusing a valid login over.
	now := time.Now().UTC()
	if err := s.store.TouchLogin(user.ID, now); err != nil {
		s.log.Error("recording login", "user", user.Name, "err", err)
	}
	user.LastLogin = now

	tok, err := s.store.IssueToken(user.ID, info.Client, info.Device, info.DeviceID, info.Version)
	if err != nil {
		s.log.Error("issuing token", "user", user.Name, "err", err)
		problem(w, http.StatusInternalServerError, "Internal error")
		return
	}
	s.log.Info("login", "user", user.Name, "client", info.Client, "device", info.Device)

	writeJSON(w, 200, AuthenticationResult{
		User:        toUserDto(user, s.serverID.String()),
		AccessToken: tok.Value,
		ServerID:    s.serverID.String(),
		SessionInfo: SessionInfoDto{
			// Session identity is (userID, client, deviceID): reconnecting the
			// same app must resume the same session, not create a new one.
			ID:                    sessionKey(user.ID, info.Client, info.DeviceID),
			UserID:                user.ID,
			UserName:              user.Name,
			Client:                info.Client,
			DeviceName:            info.Device,
			DeviceID:              info.DeviceID,
			ApplicationVersion:    info.Version,
			ServerID:              s.serverID.String(),
			LastActivityDate:      time.Now().UTC(),
			SupportsRemoteControl: false,
			IsActive:              true,
			PlayableMediaTypes:    []string{},
			SupportedCommands:     []string{},
		},
	})
}

func sessionKey(userID jfid.GUID, client, deviceID string) string {
	return jfid.HashString(
		strings.ToLower(userID.String()+client+deviceID), jfid.UTF16LE).String()
}

// ---------------------------------------------------------------- websocket

type wsEnvelope struct {
	MessageType string `json:"MessageType"`
	MessageID   string `json:"MessageId"`
	Data        any    `json:"Data"`
}

func (s *Server) handleSocket(w http.ResponseWriter, r *http.Request) {
	info := jfauth.FromRequest(r)
	_, user, err := s.store.Lookup(info.Token)
	if err != nil {
		s.log.Error("websocket token lookup", "err", err)
		problem(w, http.StatusInternalServerError, "Internal error")
		return
	}
	if user == nil {
		problem(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	conn, err := jfws.Upgrade(w, r)
	if err != nil {
		s.log.Warn("websocket upgrade failed", "err", err)
		return
	}
	defer conn.Close()

	const keepAliveSeconds = 30
	send := func(msgType string, data any) error {
		b, err := json.Marshal(wsEnvelope{
			MessageType: msgType,
			MessageID:   newMessageID(),
			Data:        data,
		})
		if err != nil {
			return err
		}
		return conn.WriteText(b)
	}

	// Upstream sends ForceKeepAlive immediately on connect; its Data is the
	// timeout in seconds. Clients start their keep-alive timer from this, and
	// go silent without it.
	if err := send("ForceKeepAlive", keepAliveSeconds); err != nil {
		return
	}
	s.log.Info("websocket connected", "user", user.Name, "remote", conn.RemoteAddr())

	for {
		_ = conn.SetReadDeadline(time.Now().Add(keepAliveSeconds * 2 * time.Second))
		msg, err := conn.ReadMessage()
		if err != nil {
			s.log.Info("websocket closed", "user", user.Name, "err", err)
			return
		}

		var in struct {
			MessageType string          `json:"MessageType"`
			Data        json.RawMessage `json:"Data"`
		}
		if err := json.Unmarshal(msg, &in); err != nil {
			continue
		}

		switch in.MessageType {
		case "KeepAlive":
			_ = send("KeepAlive", nil)
		case "SessionsStart", "ScheduledTasksInfoStart", "ActivityLogEntryStart":
			// Data arrives as the string "dueTimeMs,periodMs" -- a comma-joined
			// string, not an object. Parsing it as an object is why dashboards
			// silently stop updating.
			var spec string
			_ = json.Unmarshal(in.Data, &spec)
			due, period := parseIntervalSpec(spec)
			s.log.Debug("ws subscribe", "type", in.MessageType, "due", due, "period", period)
			// TODO: start a ticker publishing the corresponding listener payload.
		case "SessionsStop", "ScheduledTasksInfoStop", "ActivityLogEntryStop":
			// TODO: stop the corresponding ticker.
		default:
			s.log.Debug("unhandled ws message", "type", in.MessageType)
		}
	}
}

func parseIntervalSpec(s string) (due, period time.Duration) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return time.Second, 5 * time.Second
	}
	d, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	p, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	return time.Duration(d) * time.Millisecond, time.Duration(p) * time.Millisecond
}

func newMessageID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	var g jfid.GUID
	copy(g[:], b)
	return g.String()
}

// ---------------------------------------------------------------- middleware

func (s *Server) stripBaseURL(next http.Handler) http.Handler {
	base := s.cfg.Server.BaseURL
	if base == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, base) {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, base)
			if r.URL.Path == "" {
				r.URL.Path = "/"
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Accept, Accept-Language, Authorization, Content-Language, Content-Type, Range, X-Emby-Authorization, X-Emby-Token, X-MediaBrowser-Token")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS")
		h.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, X-Content-Duration")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.status = c
	s.ResponseWriter.WriteHeader(c)
}

// Hijack must be forwarded or the WebSocket upgrade breaks under this wrapper.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("not a Hijacker")
	}
	return hj.Hijack()
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.log.Debug("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start))
	})
}

func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func abs(p, wd string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(wd, p)
}

// ---------------------------------------------------------------- main

func main() {
	fs := flag.NewFlagSet("gofin", flag.ExitOnError)
	var (
		configPath = fs.String("config", defaultConfigPath(), "config file path")
		_          = fs.Int("port", 8096, "HTTP port")
		_          = fs.String("bind", "0.0.0.0", "bind address")
		_          = fs.String("base-url", "", "base path for reverse proxies, e.g. /jellyfin")
		_          = fs.String("datadir", "./data", "data directory")
		_          = fs.String("webdir", "", "serve a jellyfin-web dist from this directory")
		_          = fs.Bool("no-web", true, "headless: API only")
		_          = fs.String("log-level", "info", "trace|debug|info|warn|error")
		_          = fs.String("log-format", "text", "text|json")
		dryRun     = fs.Bool("dry-run", false, "validate configuration and exit")
	)
	var libs libraryList
	fs.Var(&libs, "library", "media library, repeatable: "+
		"'name=Movies,type=movies,path=/mnt/media/Movies,readonly'")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "gofin — Jellyfin-compatible media server\n\n"+
			"Usage:\n  gofin [serve|config init|version] [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}
	_ = fs.Parse(args)

	switch cmd {
	case "version":
		fmt.Printf("gofin %s (Jellyfin API %s compatible)\n", Version, Version)
		return
	case "config":
		b, _ := json.MarshalIndent(defaultConfig(), "", "  ")
		fmt.Println(string(b))
		return
	case "serve":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}

	cfg, err := load(fs, *configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	level := slog.LevelInfo
	switch strings.ToLower(cfg.Logging.Level) {
	case "trace", "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Logging.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	log := slog.New(handler)

	for _, d := range []string{cfg.Paths.Data, cfg.Paths.Config, cfg.Paths.Cache, cfg.Paths.Logs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Error("cannot create directory", "path", d, "err", err)
			os.Exit(1)
		}
	}

	// Media paths are checked before anything else opens: an unreadable library
	// is almost always a mount that is not up yet or a permission problem, and
	// both are far cheaper to see now than as an empty library after a scan.
	if err := validateLibraryPaths(cfg.Libraries); err != nil {
		log.Error("library configuration", "err", err)
		os.Exit(1)
	}
	for _, l := range cfg.Libraries {
		log.Info("library", "name", l.Name, "type", l.Type,
			"path", l.Path, "readonly", l.IsReadOnly())
	}
	if len(cfg.Libraries) == 0 {
		log.Warn("no libraries configured; add a \"libraries\" section to the config " +
			"or pass -library")
	}

	// Before the database, so --dry-run stays a pure read: printing the
	// resolved config must not create data/gofin.db as a side effect.
	if *dryRun {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return
	}

	dbPath := filepath.Join(cfg.Paths.Data, "gofin.db")
	db, err := jfdb.Open(dbPath)
	if err != nil {
		log.Error("cannot open database", "path", dbPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()
	log.Info("database", "path", dbPath)

	store := jfdb.NewStore(db)
	if err := seedUsers(store, cfg.Users); err != nil {
		log.Error("seeding users", "err", err)
		os.Exit(1)
	}

	srv := &Server{
		db:       db,
		cfg:      cfg,
		log:      log,
		store:    store,
		items:    NewItemStore(),
		serverID: jfid.HashString("gofin-server:"+cfg.Server.Name, jfid.UTF16LE),
		started:  time.Now(),
	}

	// The artwork provider is opt-in twice over: it has to be enabled AND
	// given a key. Reaching the network is not something a media server should
	// start doing because a config file gained a field.
	if cfg.Metadata.TMDB.Enabled {
		if cfg.Metadata.TMDB.APIKey == "" {
			log.Warn("tmdb enabled but no api_key set; artwork fetching stays off. " +
				"Get a free key at https://www.themoviedb.org/settings/api and put it " +
				"in metadata.tmdb.api_key or $GOFIN_TMDB_API_KEY")
		} else {
			c := jftmdb.New(cfg.Metadata.TMDB.APIKey)
			if cfg.Metadata.TMDB.Language != "" {
				c.Language = cfg.Metadata.TMDB.Language
			}
			if cfg.Metadata.TMDB.PosterSize != "" {
				c.PosterSize = cfg.Metadata.TMDB.PosterSize
			}
			srv.tmdb = c
			log.Info("artwork provider enabled", "provider", "tmdb",
				"language", c.Language, "poster_size", c.PosterSize)
		}
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.Port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: media streams and WebSockets are long-lived.
		IdleTimeout: 120 * time.Second,
	}

	// Scan in the background: a large library would otherwise look like a
	// server that hangs on startup.
	go srv.scanLibraries()

	seeded, err := store.Users()
	if err != nil {
		log.Error("listing users", "err", err)
		os.Exit(1)
	}

	go func() {
		log.Info("listening", "addr", addr, "users", len(seeded), "version", Version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
