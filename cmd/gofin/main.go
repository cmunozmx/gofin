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
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gofin/internal/jfauth"
	"gofin/internal/jfid"
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
	Users []ConfigUser `json:"users"`
}

type ConfigUser struct {
	Name         string `json:"name"`
	PasswordHash string `json:"password_hash"`
	Password     string `json:"password"` // convenience for bootstrap; hashed on load
	Admin        bool   `json:"admin"`
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
		}
	})

	cfg.Server.BaseURL = strings.TrimSuffix(cfg.Server.BaseURL, "/")
	if cfg.Server.BaseURL != "" && !strings.HasPrefix(cfg.Server.BaseURL, "/") {
		cfg.Server.BaseURL = "/" + cfg.Server.BaseURL
	}
	return cfg, nil
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

// ---------------------------------------------------------------- state

type User struct {
	ID           jfid.GUID
	Name         string
	PasswordHash string
	Admin        bool
	LastLogin    time.Time
}

type Token struct {
	Value    string
	UserID   jfid.GUID
	Client   string
	Device   string
	DeviceID string
	Created  time.Time
}

// Store is the phase-0 stand-in for the SQLite repositories. Replacing this
// with the real thing should not require touching any handler.
type Store struct {
	mu     sync.RWMutex
	users  map[jfid.GUID]*User
	tokens map[string]*Token
}

func NewStore() *Store {
	return &Store{users: map[jfid.GUID]*User{}, tokens: map[string]*Token{}}
}

func (s *Store) AddUser(u *User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
}

func (s *Store) UserByName(name string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if strings.EqualFold(u.Name, name) {
			return u
		}
	}
	return nil
}

func (s *Store) UserByID(id jfid.GUID) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[id]
}

func (s *Store) Users() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	return out
}

func (s *Store) IssueToken(u *User, info jfauth.Info) *Token {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	t := &Token{
		Value:    hex.EncodeToString(raw),
		UserID:   u.ID,
		Client:   info.Client,
		Device:   info.Device,
		DeviceID: info.DeviceID,
		Created:  time.Now(),
	}
	s.mu.Lock()
	s.tokens[t.Value] = t
	s.mu.Unlock()
	return t
}

func (s *Store) Lookup(token string) (*Token, *User) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t := s.tokens[token]
	if t == nil {
		return nil, nil
	}
	return t, s.users[t.UserID]
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
	cfg      *Config
	log      *slog.Logger
	store    *Store
	serverID jfid.GUID
	started  time.Time
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
func problem(w http.ResponseWriter, status int, title string) {
	writeJSON(w, status, map[string]any{
		"type":   fmt.Sprintf("https://tools.ietf.org/html/rfc9110#section-15.5.%d", status-399),
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
		tok, user := s.store.Lookup(info.Token)
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
		out := []UserDto{}
		for _, u := range s.store.Users() {
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

func (s *Server) publicInfo(r *http.Request) PublicSystemInfo {
	local := s.cfg.Server.PublishedURL
	if local == "" {
		local = fmt.Sprintf("http://%s", r.Host)
	}
	return PublicSystemInfo{
		LocalAddress:           local,
		ServerName:             s.cfg.Server.Name,
		Version:                Version,
		ProductName:            ProductName,
		OperatingSystem:        "", // upstream reports "" on modern versions
		ID:                     s.serverID,
		StartupWizardCompleted: len(s.store.Users()) > 0,
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
	user := s.store.UserByName(body.Username)

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

	user.LastLogin = time.Now().UTC()
	tok := s.store.IssueToken(user, info)
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
	_, user := s.store.Lookup(info.Token)
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

	store := NewStore()
	for _, cu := range cfg.Users {
		hash := cu.PasswordHash
		if hash == "" && cu.Password != "" {
			hash = hashPassword(cu.Password)
		}
		// User IDs are derived from the name so that a config-declared user
		// keeps the same ID across restarts and across machines.
		store.AddUser(&User{
			ID:           jfid.HashString("user:"+strings.ToLower(cu.Name), jfid.UTF16LE),
			Name:         cu.Name,
			PasswordHash: hash,
			Admin:        cu.Admin,
		})
	}

	srv := &Server{
		cfg:      cfg,
		log:      log,
		store:    store,
		serverID: jfid.HashString("gofin-server:"+cfg.Server.Name, jfid.UTF16LE),
		started:  time.Now(),
	}

	if *dryRun {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.Port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: media streams and WebSockets are long-lived.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", addr, "users", len(store.Users()), "version", Version)
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
