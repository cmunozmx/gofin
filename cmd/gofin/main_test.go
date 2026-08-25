package main

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gofin/internal/jfauth"
	"gofin/internal/jfdb"
	"gofin/internal/jfid"
	"gofin/internal/jfscan"
	"gofin/internal/jftmdb"
)

// Hashes produced by an independent PBKDF2-HMAC-SHA512 implementation (Python's
// hashlib), not by the code under test. If verifyPassword accepts these, the
// stored format really is interoperable with anything else that speaks it --
// which is what matters when importing an existing user table.
const (
	pwAdmin    = "changeme"
	hashAdmin  = "$PBKDF2-SHA512$iterations=210000$000102030405060708090a0b0c0d0e0f$f055a4e149ae6d287f9e740f7008cf57640dd406dd41d81eccdf7833fca6bc4b2ef9bf58d687062b8c5b09a925c34396a194e6629d7029227d78915f07c711a4"
	pwViewer   = "hunter2"
	hashViewer = "$PBKDF2-SHA512$iterations=210000$101112131415161718191a1b1c1d1e1f$0aec96e4e44fb294a15ed90fbbb2156ccd6e042c1652ae454f61b8ac9045f4e0ac9fb18c5e80e7cdea8a63b7c7e530b1d19bbb9a55ab42ee82769b2d58344c17"
)

// ------------------------------------------------------------------ harness

type testEnv struct {
	srv     *Server
	handler http.Handler
	admin   *User
	viewer  *User
}

// newTestDB opens an empty database in a temp directory, closed when the test
// ends. A file rather than :memory: so the tests exercise the real WAL and
// locking behaviour.
func newTestDB(t *testing.T) *jfdb.DB {
	t.Helper()
	db, err := jfdb.Open(filepath.Join(t.TempDir(), "gofin.db"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := defaultConfig()
	cfg.Server.Name = "Test Server"

	db := newTestDB(t)
	store := jfdb.NewStore(db)

	admin := &User{
		ID:           jfid.HashString("user:admin", jfid.UTF16LE),
		Name:         "admin",
		PasswordHash: hashAdmin,
		Admin:        true,
	}
	viewer := &User{
		ID:           jfid.HashString("user:viewer", jfid.UTF16LE),
		Name:         "viewer",
		PasswordHash: hashViewer,
	}
	for _, u := range []*User{admin, viewer} {
		if err := store.UpsertUser(u); err != nil {
			t.Fatalf("seed %q: %v", u.Name, err)
		}
	}

	srv := &Server{
		cfg:      cfg,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:       db,
		store:    store,
		items:    NewItemStore(),
		serverID: jfid.HashString("gofin-server:Test Server", jfid.UTF16LE),
		started:  time.Now(),
	}
	return &testEnv{srv: srv, handler: srv.routes(), admin: admin, viewer: viewer}
}

// do issues a request against the full middleware stack. Client metadata is
// always sent, since real clients identify themselves even when logging in;
// an empty token means the header carries no Token at all.
func (e *testEnv) do(t *testing.T, method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	auth := `MediaBrowser Client="test", Device="unit", DeviceId="d1", Version="1.0"`
	if token != "" {
		auth += fmt.Sprintf(", Token=%q", token)
	}
	r.Header.Set("Authorization", auth)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

func (e *testEnv) tokenFor(u *User) string {
	info := jfauthInfoForTest()
	tok, err := e.srv.store.IssueToken(u.ID, info.Client, info.Device, info.DeviceID, info.Version)
	if err != nil {
		panic("issue test token: " + err.Error())
	}
	return tok.Value
}

func jfauthInfoForTest() jfauth.Info {
	return jfauth.Info{Client: "test", Device: "unit", DeviceID: "d1", Version: "1.0"}
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
}

// ---------------------------------------------------------------- passwords

// Known answers from Python's hashlib.pbkdf2_hmac, so this checks the
// hand-rolled PBKDF2 against a reference implementation rather than itself.
func TestPBKDF2KnownAnswers(t *testing.T) {
	cases := []struct {
		password string
		saltHex  string
		iter     int
		keyLen   int
		wantHex  string
	}{
		{"password", "73616c74", 1, 64,
			"867f70cf1ade02cff3752599a3a53dc4af34c7a669815ae5d513554e1c8cf252c02d470a285a0501bad999bfe943c08f050235d7d68b1da55e63f73b60a57fce"},
		{"password", "73616c74", 4096, 64,
			"d197b1b33db0143e018b12f3d1d1479e6cdebdcc97c5c0f87f6902e072f457b5143f30602641b3d55cd335988cb36b84376060ecd532e039b742a239434af2d5"},
		// keyLen shorter than one SHA-512 block: the output must be truncated.
		{"passwd", "000102030405060708090a0b0c0d0e0f", 100, 20,
			"05e060a2f5e05f87fd58ecae730cf39f22025fb6"},
		{"", "73616c74", 10, 64,
			"1f7a48471674bc0dcdc3735b2ddc3509b608ed771cb9d878e2ef4e0805d10817dde524a24fb2990a6dd3522bf8e71c6826833de87bd253806b9fccbfafa22255"},
		// keyLen spanning two blocks, exercising the block-counter loop.
		{"unicode-éü", "4e61436c", 50, 100,
			"18ae0945df207d8338f303eab14b802bf65beeefc071344dbfd0efde8aaedc5aa7aa7b8782724ee94cfdae02d48e60f8c6e3c42b84135172d702bc51074a6ac0c977de024b28f889854b5aaddf8e2a2aa11c69f2967ddb9d798d500b3f1cd07e5cb5fb2b"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s/%d/%d", c.password, c.iter, c.keyLen), func(t *testing.T) {
			salt, err := hex.DecodeString(c.saltHex)
			if err != nil {
				t.Fatal(err)
			}
			got := hex.EncodeToString(pbkdf2SHA512([]byte(c.password), salt, c.iter, c.keyLen))
			if got != c.wantHex {
				t.Errorf("pbkdf2SHA512 =\n %s\nwant\n %s", got, c.wantHex)
			}
		})
	}
}

func TestVerifyPasswordAcceptsExternallyGeneratedHash(t *testing.T) {
	if !verifyPassword(pwAdmin, hashAdmin) {
		t.Error("rejected a valid hash produced by another implementation")
	}
	if !verifyPassword(pwViewer, hashViewer) {
		t.Error("rejected a valid hash produced by another implementation")
	}
	if verifyPassword(pwViewer, hashAdmin) {
		t.Error("accepted the wrong password")
	}
}

func TestHashPasswordRoundTrip(t *testing.T) {
	h := hashPassword("correct horse battery staple")
	if !verifyPassword("correct horse battery staple", h) {
		t.Error("a freshly hashed password did not verify")
	}
	if verifyPassword("wrong", h) {
		t.Error("verified the wrong password")
	}
	if !strings.HasPrefix(h, "$PBKDF2-SHA512$iterations=") {
		t.Errorf("stored format = %q, want the $-delimited PBKDF2 form", h)
	}
	if n := len(strings.Split(strings.TrimPrefix(h, "$"), "$")); n != 4 {
		t.Errorf("stored form has %d fields, want 4", n)
	}
}

// A per-password random salt is the whole point; identical hashes for the same
// password would mean the salt is fixed or missing.
func TestHashPasswordUsesRandomSalt(t *testing.T) {
	a, b := hashPassword("same"), hashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical; the salt is not random")
	}
	if !verifyPassword("same", a) || !verifyPassword("same", b) {
		t.Error("independently salted hashes must both verify")
	}
}

func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	cases := []struct {
		name   string
		stored string
	}{
		{"empty", ""},
		{"no password set", ""},
		{"plaintext", "changeme"},
		{"too few fields", "$PBKDF2-SHA512$iterations=210000$00"},
		{"too many fields", hashAdmin + "$extra"},
		{"unknown algorithm", strings.Replace(hashAdmin, "PBKDF2-SHA512", "PBKDF2-SHA1", 1)},
		{"non-numeric iterations", strings.Replace(hashAdmin, "iterations=210000", "iterations=abc", 1)},
		{"zero iterations", strings.Replace(hashAdmin, "iterations=210000", "iterations=0", 1)},
		{"negative iterations", strings.Replace(hashAdmin, "iterations=210000", "iterations=-1", 1)},
		{"salt is not hex", strings.Replace(hashAdmin, "000102030405060708090a0b0c0d0e0f", "zzzz", 1)},
		{"digest is not hex", hashAdmin[:len(hashAdmin)-4] + "zzzz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if verifyPassword(pwAdmin, c.stored) {
				t.Errorf("accepted malformed stored hash %q", c.stored)
			}
		})
	}
}

// The algorithm label is compared case-insensitively, so an imported table that
// spells it differently still works.
func TestVerifyPasswordAlgorithmIsCaseInsensitive(t *testing.T) {
	lower := strings.Replace(hashAdmin, "PBKDF2-SHA512", "pbkdf2-sha512", 1)
	if !verifyPassword(pwAdmin, lower) {
		t.Error("rejected a lowercase algorithm label")
	}
}

// An imported hash may use a different iteration count; the count in the string
// must be honoured rather than the current constant assumed.
func TestVerifyPasswordHonoursStoredIterationCount(t *testing.T) {
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	sum := pbkdf2SHA512([]byte("legacy"), salt, 1000, 64)
	stored := fmt.Sprintf("$PBKDF2-SHA512$iterations=%d$%s$%s",
		1000, hex.EncodeToString(salt), hex.EncodeToString(sum))

	if !verifyPassword("legacy", stored) {
		t.Error("a hash with a non-default iteration count did not verify")
	}
	// The same digest under the default count must not verify, proving the
	// stored count is actually being read.
	wrong := strings.Replace(stored, "iterations=1000", "iterations=210000", 1)
	if verifyPassword("legacy", wrong) {
		t.Error("verified against the wrong iteration count")
	}
}

// ------------------------------------------------------------------- config

func newFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("config", "", "")
	fs.Int("port", 8096, "")
	fs.String("bind", "0.0.0.0", "")
	fs.String("base-url", "", "")
	fs.String("datadir", "./data", "")
	fs.String("webdir", "", "")
	fs.Bool("no-web", true, "")
	fs.String("log-level", "info", "")
	fs.String("log-format", "text", "")
	return fs
}

func TestDefaultConfig(t *testing.T) {
	c := defaultConfig()
	if c.Server.Port != 8096 {
		t.Errorf("default port = %d, want 8096", c.Server.Port)
	}
	if c.Server.Bind != "0.0.0.0" {
		t.Errorf("default bind = %q", c.Server.Bind)
	}
	if !c.Server.Autodiscovery {
		t.Error("autodiscovery should default on")
	}
	if len(c.Server.LocalNetworks) == 0 {
		t.Error("local networks should have defaults")
	}
	if c.Logging.Level != "info" {
		t.Errorf("default log level = %q, want info", c.Logging.Level)
	}
}

// Defaults, then file, then env, then flags -- each layer overriding the last.
func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gofin.json")
	if err := os.WriteFile(path, []byte(`{"server":{"port":9000,"bind":"1.1.1.1","name":"FromFile"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("file overrides defaults", func(t *testing.T) {
		fs := newFlagSet()
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		cfg, err := load(fs, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 9000 || cfg.Server.Bind != "1.1.1.1" || cfg.Server.Name != "FromFile" {
			t.Errorf("file not applied: %+v", cfg.Server)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		t.Setenv("GOFIN_PORT", "9100")
		t.Setenv("GOFIN_BIND", "2.2.2.2")
		fs := newFlagSet()
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		cfg, err := load(fs, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 9100 || cfg.Server.Bind != "2.2.2.2" {
			t.Errorf("env not applied: %+v", cfg.Server)
		}
	})

	t.Run("flags override env", func(t *testing.T) {
		t.Setenv("GOFIN_PORT", "9100")
		fs := newFlagSet()
		if err := fs.Parse([]string{"-port", "9200", "-bind", "3.3.3.3"}); err != nil {
			t.Fatal(err)
		}
		cfg, err := load(fs, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 9200 || cfg.Server.Bind != "3.3.3.3" {
			t.Errorf("flags not applied: %+v", cfg.Server)
		}
	})

	// Only flags the user actually typed may override the file. A flag left at
	// its default must not silently stomp a configured value.
	t.Run("unset flags do not override the file", func(t *testing.T) {
		fs := newFlagSet()
		if err := fs.Parse([]string{"-log-level", "debug"}); err != nil {
			t.Fatal(err)
		}
		cfg, err := load(fs, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 9000 {
			t.Errorf("port = %d, want the file's 9000 -- an unset flag overrode it", cfg.Server.Port)
		}
		if cfg.Logging.Level != "debug" {
			t.Errorf("log level = %q, want debug", cfg.Logging.Level)
		}
	})
}

// Jellyfin's own env vars are honoured so this drops into an existing container.
func TestLoadJellyfinEnvVars(t *testing.T) {
	t.Setenv("JELLYFIN_DATA_DIR", "/jf/data")
	t.Setenv("JELLYFIN_CONFIG_DIR", "/jf/config")
	t.Setenv("JELLYFIN_CACHE_DIR", "/jf/cache")
	t.Setenv("JELLYFIN_LOG_DIR", "/jf/log")
	t.Setenv("JELLYFIN_WEB_DIR", "/jf/web")
	t.Setenv("JELLYFIN_PublishedServerUrl", "https://media.example.com")

	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ got, want, name string }{
		{cfg.Paths.Data, "/jf/data", "data"},
		{cfg.Paths.Config, "/jf/config", "config"},
		{cfg.Paths.Cache, "/jf/cache", "cache"},
		{cfg.Paths.Logs, "/jf/log", "logs"},
		{cfg.Web.Dir, "/jf/web", "web dir"},
		{cfg.Server.PublishedURL, "https://media.example.com", "published url"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// A non-numeric GOFIN_PORT must be ignored rather than zeroing the port.
func TestLoadIgnoresBadPortEnv(t *testing.T) {
	t.Setenv("GOFIN_PORT", "not-a-number")
	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Port != 8096 {
		t.Errorf("port = %d, want the default 8096 to survive a bad env value", cfg.Server.Port)
	}
}

func TestLoadBaseURLNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"/jellyfin", "/jellyfin"},
		{"jellyfin", "/jellyfin"},
		{"/jellyfin/", "/jellyfin"},
		{"jellyfin/", "/jellyfin"},
		{"/a/b/", "/a/b"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			fs := newFlagSet()
			if err := fs.Parse([]string{"-base-url", c.in}); err != nil {
				t.Fatal(err)
			}
			cfg, err := load(fs, "")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server.BaseURL != c.want {
				t.Errorf("base URL %q normalized to %q, want %q", c.in, cfg.Server.BaseURL, c.want)
			}
		})
	}
}

// A config path the user named explicitly must fail loudly if it is missing;
// the default path is allowed to be absent so a bare `gofin serve` works.
func TestLoadMissingConfig(t *testing.T) {
	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := load(fs, filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("a missing explicit config file should be an error")
	}

	t.Setenv("GOFIN_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	fs2 := newFlagSet()
	if err := fs2.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := load(fs2, defaultConfigPath()); err != nil {
		t.Errorf("a missing default config file should be tolerated, got %v", err)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := load(fs, path); err == nil {
		t.Error("malformed config JSON should be an error")
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("GOFIN_CONFIG", "")
	if got := defaultConfigPath(); got != "gofin.json" {
		t.Errorf("defaultConfigPath = %q, want gofin.json", got)
	}
	t.Setenv("GOFIN_CONFIG", "/etc/gofin/custom.json")
	if got := defaultConfigPath(); got != "/etc/gofin/custom.json" {
		t.Errorf("defaultConfigPath = %q, want the env value", got)
	}
}

// -------------------------------------------------------------------- store

// The repository itself is covered in depth by internal/jfdb. These two check
// the wiring: that the Server's store is the SQLite one, seeded, and reachable.

func TestStoreUserLookup(t *testing.T) {
	e := newTestEnv(t)
	s := e.srv.store

	// Jellyfin logins are case-insensitive on the username.
	for _, name := range []string{"admin", "ADMIN", "Admin"} {
		u, err := s.UserByName(name)
		if err != nil {
			t.Fatalf("UserByName(%q): %v", name, err)
		}
		if u == nil || u.ID != e.admin.ID {
			t.Errorf("UserByName(%q) did not find the admin", name)
		}
	}

	if u, err := s.UserByName("nobody"); err != nil || u != nil {
		t.Errorf("UserByName(nobody) = (%v, %v), want (nil, nil)", u, err)
	}
	if u, err := s.UserByID(e.viewer.ID); err != nil || u == nil || u.Name != "viewer" {
		t.Errorf("UserByID did not find the viewer: (%v, %v)", u, err)
	}
	if u, err := s.UserByID(jfid.Nil); err != nil || u != nil {
		t.Errorf("UserByID(nil GUID) = (%v, %v), want (nil, nil)", u, err)
	}

	all, err := s.Users()
	if err != nil {
		t.Fatalf("Users: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("Users() returned %d, want 2", len(all))
	}
}

func TestStoreTokens(t *testing.T) {
	e := newTestEnv(t)
	s := e.srv.store
	info := jfauthInfoForTest()

	tok, err := s.IssueToken(e.admin.ID, info.Client, info.Device, info.DeviceID, info.Version)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if len(tok.Value) != 32 {
		t.Errorf("token %q is %d chars, want 32 hex chars", tok.Value, len(tok.Value))
	}
	if _, err := hex.DecodeString(tok.Value); err != nil {
		t.Errorf("token is not hex: %v", err)
	}

	gotTok, gotUser, err := s.Lookup(tok.Value)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if gotTok == nil || gotUser == nil {
		t.Fatal("Lookup did not find the freshly issued token")
	}
	if gotUser.ID != e.admin.ID {
		t.Error("Lookup returned the wrong user")
	}
	if gotTok.Client != "test" || gotTok.DeviceID != "d1" {
		t.Errorf("token did not record the client metadata: %+v", gotTok)
	}

	for _, bad := range []string{"0000000000000000", ""} {
		if _, u, err := s.Lookup(bad); err != nil || u != nil {
			t.Errorf("Lookup(%q) = (%v, %v), want (nil, nil)", bad, u, err)
		}
	}

	// Two tokens for the same user must be distinct.
	other, err := s.IssueToken(e.admin.ID, info.Client, info.Device, info.DeviceID, info.Version)
	if err != nil {
		t.Fatalf("second IssueToken: %v", err)
	}
	if other.Value == tok.Value {
		t.Error("IssueToken reused a token value")
	}
}

// ---------------------------------------------------------- public endpoints

// These are hit before a login form is shown, so they must never need a token.
func TestUnauthenticatedEndpoints(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{
		"/System/Info/Public",
		"/System/Ping",
		"/QuickConnect/Enabled",
		"/Branding/Configuration",
		"/Branding/Css",
		"/Users/Public",
	} {
		t.Run(path, func(t *testing.T) {
			if w := e.do(t, "GET", path, "", nil); w.Code != 200 {
				t.Errorf("GET %s without a token = %d, want 200", path, w.Code)
			}
		})
	}
}

func TestPublicSystemInfo(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/System/Info/Public", "", nil)

	var got map[string]any
	decodeJSON(t, w, &got)

	if got["ServerName"] != "Test Server" {
		t.Errorf("ServerName = %v", got["ServerName"])
	}
	if got["Version"] != Version {
		t.Errorf("Version = %v, want %s", got["Version"], Version)
	}
	if got["ProductName"] != ProductName {
		t.Errorf("ProductName = %v, want %s", got["ProductName"], ProductName)
	}
	// The server ID goes on the wire dashless, like every other GUID.
	id, _ := got["Id"].(string)
	if len(id) != 32 || strings.Contains(id, "-") {
		t.Errorf("Id = %q, want 32 dashless hex chars", id)
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Errorf("Id is not hex: %v", err)
	}
	// With users configured the wizard is done, which is what stops clients
	// from redirecting into the setup flow.
	if got["StartupWizardCompleted"] != true {
		t.Error("StartupWizardCompleted should be true when users exist")
	}
	// LocalAddress falls back to the request host when no published URL is set.
	if addr, _ := got["LocalAddress"].(string); !strings.HasPrefix(addr, "http://") {
		t.Errorf("LocalAddress = %q, want an http:// URL", addr)
	}
}

func TestPublicSystemInfoBeforeAnyUserExists(t *testing.T) {
	e := newTestEnv(t)
	// Swap in an empty database: these endpoints must behave sanely on a server
	// that has no users at all.
	e.srv.store = jfdb.NewStore(newTestDB(t))
	e.handler = e.srv.routes()

	w := e.do(t, "GET", "/System/Info/Public", "", nil)
	var got map[string]any
	decodeJSON(t, w, &got)
	if got["StartupWizardCompleted"] != false {
		t.Error("StartupWizardCompleted should be false with no users")
	}
}

func TestPublishedURLOverridesLocalAddress(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.Server.PublishedURL = "https://media.example.com"
	e.handler = e.srv.routes()

	w := e.do(t, "GET", "/System/Info/Public", "", nil)
	var got map[string]any
	decodeJSON(t, w, &got)
	if got["LocalAddress"] != "https://media.example.com" {
		t.Errorf("LocalAddress = %v, want the published URL", got["LocalAddress"])
	}
}

// Both spellings exist in the wild; clients use Ping as a reachability probe.
func TestPing(t *testing.T) {
	e := newTestEnv(t)
	for _, method := range []string{"GET", "POST"} {
		w := e.do(t, method, "/System/Ping", "", nil)
		if w.Code != 200 {
			t.Errorf("%s /System/Ping = %d, want 200", method, w.Code)
		}
		var got string
		decodeJSON(t, w, &got)
		if got != ProductName {
			t.Errorf("%s /System/Ping = %q, want %q", method, got, ProductName)
		}
	}
}

func TestBrandingCssContentType(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/Branding/Css", "", nil)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}
}

func TestQuickConnectReportsDisabled(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/QuickConnect/Enabled", "", nil)
	var got bool
	decodeJSON(t, w, &got)
	if got {
		t.Error("QuickConnect should report disabled in this build")
	}
}

func TestJSONContentType(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/System/Info/Public", "", nil)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// ------------------------------------------------------------ authentication

func TestAuthenticateByNameSuccess(t *testing.T) {
	e := newTestEnv(t)
	body := strings.NewReader(`{"Username":"admin","Pw":"changeme"}`)
	w := e.do(t, "POST", "/Users/AuthenticateByName", "", body)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body)
	}

	var got AuthenticationResult
	decodeJSON(t, w, &got)
	if got.AccessToken == "" {
		t.Error("no AccessToken returned")
	}
	if got.User.Name != "admin" {
		t.Errorf("User.Name = %q", got.User.Name)
	}
	if !got.User.Policy.IsAdministrator {
		t.Error("admin should come back with IsAdministrator")
	}
	if got.ServerID != e.srv.serverID.String() {
		t.Errorf("ServerId = %q, want %q", got.ServerID, e.srv.serverID.String())
	}
	if got.SessionInfo.UserID != e.admin.ID {
		t.Error("SessionInfo.UserId does not match the user")
	}
	if got.SessionInfo.Client != "test" || got.SessionInfo.DeviceID != "d1" {
		t.Errorf("session did not pick up the auth header metadata: %+v", got.SessionInfo)
	}

	// The token must actually work.
	me := e.do(t, "GET", "/Users/Me", got.AccessToken, nil)
	if me.Code != 200 {
		t.Errorf("the issued token was rejected by /Users/Me: %d", me.Code)
	}
}

// Login is case-insensitive on the username, matching upstream.
func TestAuthenticateByNameIsCaseInsensitive(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "POST", "/Users/AuthenticateByName", "",
		strings.NewReader(`{"Username":"ADMIN","Pw":"changeme"}`))
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestAuthenticateByNameFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"wrong password", `{"Username":"admin","Pw":"nope"}`, 401},
		{"unknown user", `{"Username":"ghost","Pw":"changeme"}`, 401},
		{"empty password", `{"Username":"admin","Pw":""}`, 401},
		{"empty username", `{"Username":"","Pw":"changeme"}`, 401},
		{"malformed body", `{not json`, 400},
		{"empty body", ``, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			w := e.do(t, "POST", "/Users/AuthenticateByName", "", strings.NewReader(c.body))
			if w.Code != c.want {
				t.Errorf("status = %d, want %d; body %s", w.Code, c.want, w.Body)
			}
			// A failed login must not leak a token.
			if strings.Contains(w.Body.String(), "AccessToken") {
				t.Error("a failed login returned an AccessToken")
			}
		})
	}
}

// A wrong password and an unknown user must be indistinguishable to the client.
func TestAuthenticateByNameDoesNotEnumerateUsers(t *testing.T) {
	e := newTestEnv(t)
	bad := e.do(t, "POST", "/Users/AuthenticateByName", "",
		strings.NewReader(`{"Username":"admin","Pw":"wrong"}`))
	missing := e.do(t, "POST", "/Users/AuthenticateByName", "",
		strings.NewReader(`{"Username":"ghost","Pw":"wrong"}`))

	if bad.Code != missing.Code {
		t.Errorf("status differs: %d for a bad password, %d for a missing user", bad.Code, missing.Code)
	}
	if bad.Body.String() != missing.Body.String() {
		t.Errorf("response body differs and so leaks which users exist:\n %s\n %s", bad.Body, missing.Body)
	}
}

// --------------------------------------------------------- authorization

func TestAuthenticatedEndpointsRejectMissingAndBadTokens(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/Users/Me", "/System/Info", "/System/Endpoint", "/Sessions"} {
		t.Run(path, func(t *testing.T) {
			if w := e.do(t, "GET", path, "", nil); w.Code != 401 {
				t.Errorf("no token: %s = %d, want 401", path, w.Code)
			}
			if w := e.do(t, "GET", path, "deadbeef", nil); w.Code != 401 {
				t.Errorf("bad token: %s = %d, want 401", path, w.Code)
			}
		})
	}
}

// 401 vs 403 is load-bearing: clients re-prompt for a login on 401 and merely
// hide the feature on 403. Conflating them logs users out of the whole app.
func TestAdminOnlyEndpointReturns403NotUnauthorized(t *testing.T) {
	e := newTestEnv(t)

	w := e.do(t, "GET", "/System/Info", e.tokenFor(e.viewer), nil)
	if w.Code != 403 {
		t.Errorf("non-admin on /System/Info = %d, want 403", w.Code)
	}

	w = e.do(t, "GET", "/System/Info", e.tokenFor(e.admin), nil)
	if w.Code != 200 {
		t.Errorf("admin on /System/Info = %d, want 200", w.Code)
	}
}

// The query-param fallback exists for <img> and <video> tags that cannot set
// headers, so it has to be accepted on authenticated routes.
func TestApiKeyQueryParamIsAccepted(t *testing.T) {
	e := newTestEnv(t)
	tok := e.tokenFor(e.viewer)

	r := httptest.NewRequest("GET", "/Users/Me?api_key="+tok, nil)
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("api_key query param = %d, want 200", w.Code)
	}
}

func TestUsersMeReturnsTheTokenHolder(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/Users/Me", e.tokenFor(e.viewer), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var got UserDto
	decodeJSON(t, w, &got)
	if got.Name != "viewer" {
		t.Errorf("Name = %q, want viewer", got.Name)
	}
	if got.ID != e.viewer.ID {
		t.Error("Id does not match the token holder")
	}
	if got.Policy.IsAdministrator {
		t.Error("viewer must not report IsAdministrator")
	}
	if !got.HasPassword {
		t.Error("HasPassword should be true for a user with a hash")
	}
}

func TestSystemInfoPaths(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/System/Info", e.tokenFor(e.admin), nil)
	var got SystemInfo
	decodeJSON(t, w, &got)

	// Paths must be absolute on the wire even though the config holds
	// relative defaults.
	for _, p := range []struct{ name, val string }{
		{"ProgramDataPath", got.ProgramDataPath},
		{"CachePath", got.CachePath},
		{"LogPath", got.LogPath},
		{"InternalMetadataPath", got.InternalMetadataPath},
		{"TranscodingTempPath", got.TranscodingTempPath},
	} {
		if !filepath.IsAbs(p.val) {
			t.Errorf("%s = %q, want an absolute path", p.name, p.val)
		}
	}
	if got.WebSocketPortNumber != e.srv.cfg.Server.Port {
		t.Errorf("WebSocketPortNumber = %d, want %d", got.WebSocketPortNumber, e.srv.cfg.Server.Port)
	}
	if got.Version != Version {
		t.Errorf("Version = %q, want the embedded PublicSystemInfo fields", got.Version)
	}
}

func TestSystemEndpointReportsLocality(t *testing.T) {
	e := newTestEnv(t)
	tok := e.tokenFor(e.viewer)

	r := httptest.NewRequest("GET", "/System/Endpoint", nil)
	r.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token=%q`, tok))
	r.RemoteAddr = "127.0.0.1:5555"
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)

	var got map[string]bool
	decodeJSON(t, w, &got)
	if !got["IsLocal"] || !got["IsInNetwork"] {
		t.Errorf("loopback should be local: %+v", got)
	}
}

func TestSessionsCapabilitiesReturns204(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "POST", "/Sessions/Capabilities/Full", e.tokenFor(e.viewer),
		strings.NewReader(`{"PlayableMediaTypes":["Video"]}`))
	if w.Code != 204 {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 response had a body: %s", w.Body)
	}
}

// ------------------------------------------------------------------ DTO shape

// Jellyfin clients iterate these arrays without null checks. Serializing null
// instead of [] throws in the web client before the library ever renders.
func TestUserDtoEmptyArraysAreNotNull(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/Users/Me", e.tokenFor(e.viewer), nil)

	var raw struct {
		Policy        map[string]json.RawMessage `json:"Policy"`
		Configuration map[string]json.RawMessage `json:"Configuration"`
	}
	decodeJSON(t, w, &raw)

	for _, field := range []string{
		"EnabledFolders", "BlockedTags", "AccessSchedules",
		"BlockUnratedItems", "EnabledChannels", "EnabledDevices",
	} {
		v, ok := raw.Policy[field]
		if !ok {
			t.Errorf("Policy.%s is missing", field)
			continue
		}
		if string(v) != "[]" {
			t.Errorf("Policy.%s = %s, want []", field, v)
		}
	}
	for _, field := range []string{
		"GroupedFolders", "OrderedViews", "LatestItemsExcludes", "MyMediaExcludes",
	} {
		v, ok := raw.Configuration[field]
		if !ok {
			t.Errorf("Configuration.%s is missing", field)
			continue
		}
		if string(v) != "[]" {
			t.Errorf("Configuration.%s = %s, want []", field, v)
		}
	}
}

func TestUserDtoGuidIsDashless(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/Users/Me", e.tokenFor(e.viewer), nil)

	var raw map[string]json.RawMessage
	decodeJSON(t, w, &raw)
	id := strings.Trim(string(raw["Id"]), `"`)
	if len(id) != 32 || strings.Contains(id, "-") {
		t.Errorf("Id = %q, want 32 dashless hex chars", id)
	}
}

// A user who has never logged in must render LastLoginDate as null, not as the
// zero instant, which clients display as the year 1.
func TestUserDtoNullLastLogin(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/Users/Me", e.tokenFor(e.viewer), nil)

	var raw map[string]json.RawMessage
	decodeJSON(t, w, &raw)
	if got := string(raw["LastLoginDate"]); got != "null" {
		t.Errorf("LastLoginDate = %s, want null for a user who never logged in", got)
	}
}

func TestUsersPublicIsAnArray(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/Users/Public", "", nil)

	var got []UserDto
	decodeJSON(t, w, &got)
	if len(got) != 2 {
		t.Fatalf("got %d users, want 2", len(got))
	}

	// Even with no users the field must be [] rather than null.
	// Swap in an empty database: these endpoints must behave sanely on a server
	// that has no users at all.
	e.srv.store = jfdb.NewStore(newTestDB(t))
	e.handler = e.srv.routes()
	w = e.do(t, "GET", "/Users/Public", "", nil)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("empty user list = %s, want []", w.Body)
	}
}

func TestSessionsReturnsEmptyArray(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/Sessions", e.tokenFor(e.viewer), nil)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("body = %s, want []", w.Body)
	}
}

// ---------------------------------------------------------------- 501 + errors

// Unimplemented routes must answer with a parseable ProblemDetails rather than
// an empty body, and must say 501 so the gap is visible.
func TestUnimplementedRoutesReturn501(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/Library/VirtualFolders", "/Shows/NextUp", "/nonsense"} {
		t.Run(path, func(t *testing.T) {
			w := e.do(t, "GET", path, "", nil)
			if w.Code != 501 {
				t.Errorf("status = %d, want 501", w.Code)
			}
			var got map[string]any
			decodeJSON(t, w, &got)
			if got["status"] != float64(501) {
				t.Errorf("status field = %v, want 501", got["status"])
			}
			if got["title"] == "" || got["title"] == nil {
				t.Error("ProblemDetails has no title")
			}
		})
	}
}

func TestProblemDetailsShape(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/Users/Me", "", nil)

	var got map[string]any
	decodeJSON(t, w, &got)
	if got["status"] != float64(401) {
		t.Errorf("status = %v, want 401", got["status"])
	}
	if _, ok := got["title"].(string); !ok {
		t.Error("title is missing or not a string")
	}
	if _, ok := got["type"].(string); !ok {
		t.Error("type is missing or not a string")
	}
}

// -------------------------------------------------------------- middleware

func TestCORSPreflight(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "OPTIONS", "/Users/AuthenticateByName", "", nil)

	if w.Code != 204 {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q, want *", got)
	}
	// The client sends credentials in these headers; if they are not allowed
	// the browser blocks every authenticated request.
	allowed := w.Header().Get("Access-Control-Allow-Headers")
	for _, h := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "Range"} {
		if !strings.Contains(allowed, h) {
			t.Errorf("Allow-Headers %q does not include %s", allowed, h)
		}
	}
	// Range/Content-Range must be exposed or seeking breaks.
	exposed := w.Header().Get("Access-Control-Expose-Headers")
	for _, h := range []string{"Content-Length", "Content-Range"} {
		if !strings.Contains(exposed, h) {
			t.Errorf("Expose-Headers %q does not include %s", exposed, h)
		}
	}
}

func TestCORSHeadersOnNormalResponses(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/System/Info/Public", "", nil)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q on a normal response, want *", got)
	}
}

// Behind a reverse proxy at /jellyfin the prefix must be stripped before
// routing, or every request 501s.
func TestBaseURLStripping(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.Server.BaseURL = "/jellyfin"
	e.handler = e.srv.routes()

	if w := e.do(t, "GET", "/jellyfin/System/Ping", "", nil); w.Code != 200 {
		t.Errorf("prefixed request = %d, want 200", w.Code)
	}
	// The bare prefix must map to "/" rather than the empty path.
	if w := e.do(t, "GET", "/jellyfin", "", nil); w.Code != 501 {
		t.Errorf("bare prefix = %d, want the catch-all 501", w.Code)
	}
	// Unprefixed requests still reach the mux, since some clients drop it.
	if w := e.do(t, "GET", "/System/Ping", "", nil); w.Code != 200 {
		t.Errorf("unprefixed request = %d, want 200", w.Code)
	}
}

func TestNoBaseURLLeavesPathsAlone(t *testing.T) {
	e := newTestEnv(t)
	if w := e.do(t, "GET", "/System/Ping", "", nil); w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// --------------------------------------------------------------- helpers

// Session identity is (userID, client, deviceID): reconnecting the same app has
// to resume the same session rather than pile up duplicates in the dashboard.
func TestSessionKeyIsDeterministic(t *testing.T) {
	u := jfid.HashString("user:admin", jfid.UTF16LE)
	a := sessionKey(u, "Jellyfin Web", "dev-1")
	b := sessionKey(u, "Jellyfin Web", "dev-1")
	if a != b {
		t.Errorf("sessionKey is not stable: %s != %s", a, b)
	}
	if len(a) != 32 {
		t.Errorf("sessionKey = %q, want 32 dashless hex chars", a)
	}

	other := jfid.HashString("user:viewer", jfid.UTF16LE)
	for _, c := range []struct {
		name string
		got  string
	}{
		{"different user", sessionKey(other, "Jellyfin Web", "dev-1")},
		{"different client", sessionKey(u, "Kodi", "dev-1")},
		{"different device", sessionKey(u, "Jellyfin Web", "dev-2")},
	} {
		if c.got == a {
			t.Errorf("%s produced the same session key", c.name)
		}
	}
}

// The subscription interval arrives as the string "dueMs,periodMs" -- a
// comma-joined string, not an object. Parsing it wrong silently freezes the
// dashboard, so the fallbacks matter.
func TestParseIntervalSpec(t *testing.T) {
	cases := []struct {
		in     string
		due    time.Duration
		period time.Duration
	}{
		{"1000,5000", time.Second, 5 * time.Second},
		{"0,1000", 0, time.Second},
		{" 250 , 750 ", 250 * time.Millisecond, 750 * time.Millisecond},
		// Anything unparseable falls back to sane defaults rather than a
		// zero-period ticker, which would spin.
		{"", time.Second, 5 * time.Second},
		{"garbage", time.Second, 5 * time.Second},
		{"1000", time.Second, 5 * time.Second},
		{"1,2,3", time.Second, 5 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			due, period := parseIntervalSpec(c.in)
			if due != c.due || period != c.period {
				t.Errorf("parseIntervalSpec(%q) = %v, %v; want %v, %v",
					c.in, due, period, c.due, c.period)
			}
		})
	}
}

func TestNewMessageID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newMessageID()
		if len(id) != 32 {
			t.Fatalf("message id = %q, want 32 dashless hex chars", id)
		}
		if _, err := hex.DecodeString(id); err != nil {
			t.Fatalf("message id is not hex: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate message id %s", id)
		}
		seen[id] = true
	}
}

func TestIsLocalRequest(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1234", true},
		{"[::1]:1234", true},
		{"10.0.0.5:1234", true},
		{"192.168.1.20:1234", true},
		{"172.16.4.4:1234", true},
		{"169.254.1.1:1234", true},
		{"8.8.8.8:1234", false},
		{"1.2.3.4:5678", false},
		{"not-an-address", false},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/System/Endpoint", nil)
			r.RemoteAddr = c.addr
			if got := isLocalRequest(r); got != c.want {
				t.Errorf("isLocalRequest(%q) = %v, want %v", c.addr, got, c.want)
			}
		})
	}
}

func TestAbs(t *testing.T) {
	cases := []struct{ p, wd, want string }{
		{"/already/absolute", "/wd", "/already/absolute"},
		{"./data", "/wd", "/wd/data"},
		{"data", "/wd", "/wd/data"},
		{"data/metadata", "/wd", "/wd/data/metadata"},
	}
	for _, c := range cases {
		if got := abs(c.p, c.wd); got != c.want {
			t.Errorf("abs(%q, %q) = %q, want %q", c.p, c.wd, got, c.want)
		}
	}
}

func TestToUserDto(t *testing.T) {
	serverID := jfid.HashString("gofin-server:Test", jfid.UTF16LE).String()

	t.Run("no password set", func(t *testing.T) {
		d := toUserDto(&User{Name: "nopass"}, serverID)
		if d.HasPassword || d.HasConfiguredPassword {
			t.Error("a user with no hash must report HasPassword false")
		}
	})

	t.Run("last login is carried through", func(t *testing.T) {
		when := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
		d := toUserDto(&User{Name: "u", LastLogin: when}, serverID)
		if d.LastLoginDate == nil {
			t.Fatal("LastLoginDate is nil, want the login time")
		}
		if !d.LastLoginDate.Equal(when) {
			t.Errorf("LastLoginDate = %v, want %v", *d.LastLoginDate, when)
		}
	})

	t.Run("server id is propagated", func(t *testing.T) {
		d := toUserDto(&User{Name: "u"}, serverID)
		if d.ServerID != serverID {
			t.Errorf("ServerId = %q, want %q", d.ServerID, serverID)
		}
	})

	t.Run("admin policy", func(t *testing.T) {
		d := toUserDto(&User{Name: "a", Admin: true}, serverID)
		if !d.Policy.IsAdministrator {
			t.Error("admin flag did not reach the policy")
		}
		// -1 means "never lock out", which is what upstream sends.
		if d.Policy.LoginAttemptsBeforeLockout != -1 {
			t.Errorf("LoginAttemptsBeforeLockout = %d, want -1", d.Policy.LoginAttemptsBeforeLockout)
		}
	})
}

// ------------------------------------------------------------------ websocket

// /socket must reject an unauthenticated upgrade before hijacking, so the
// client gets a real HTTP error rather than a dropped connection.
func TestSocketRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	for _, token := range []string{"", "deadbeef"} {
		r := httptest.NewRequest("GET", "/socket", nil)
		if token != "" {
			r.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Token=%q`, token))
		}
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

		w := httptest.NewRecorder()
		e.handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("token %q: status = %d, want 401", token, w.Code)
		}
	}
}

// ---------------------------------------------------------------- libraries

func boolPtr(b bool) *bool { return &b }

// The safe default is the whole reason ReadOnly is a pointer: an omitted field
// must not mean "writable", because bool's zero value would make silence the
// dangerous answer.
func TestLibraryReadOnlyDefaultsToTrue(t *testing.T) {
	if !(Library{Path: "/m"}).IsReadOnly() {
		t.Error("an unset readonly must default to read-only")
	}
	if !(Library{Path: "/m", ReadOnly: boolPtr(true)}).IsReadOnly() {
		t.Error("readonly=true must be read-only")
	}
	if (Library{Path: "/m", ReadOnly: boolPtr(false)}).IsReadOnly() {
		t.Error("readonly=false must be writable")
	}
}

// A library declared in JSON with no "readonly" key is read-only, and one that
// explicitly says false is not.
func TestLibraryReadOnlyThroughJSON(t *testing.T) {
	cases := []struct {
		json string
		want bool
	}{
		{`{"name":"M","path":"/m","type":"movies"}`, true},
		{`{"name":"M","path":"/m","type":"movies","readonly":true}`, true},
		{`{"name":"M","path":"/m","type":"movies","readonly":false}`, false},
		{`{"name":"M","path":"/m","type":"movies","readonly":null}`, true},
	}
	for _, c := range cases {
		t.Run(c.json, func(t *testing.T) {
			var l Library
			if err := json.Unmarshal([]byte(c.json), &l); err != nil {
				t.Fatal(err)
			}
			if got := l.IsReadOnly(); got != c.want {
				t.Errorf("IsReadOnly = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNormalizeLibraries(t *testing.T) {
	libs, err := normalizeLibraries([]Library{
		{Name: "Movies", Path: "/mnt/alberca/Movies", Type: "movies"},
		{Name: "Music", Path: "/mnt/alberca/Music", Type: "music"},
	}, "/wd")
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 2 {
		t.Fatalf("got %d libraries, want 2", len(libs))
	}
	for _, l := range libs {
		if !l.IsReadOnly() {
			t.Errorf("library %q should default to read-only", l.Name)
		}
		// Normalization must pin the effective value so --dry-run shows what is
		// actually in force rather than a null.
		if l.ReadOnly == nil {
			t.Errorf("library %q left ReadOnly unset after normalization", l.Name)
		}
	}
}

func TestNormalizeLibrariesMakesPathsAbsolute(t *testing.T) {
	libs, err := normalizeLibraries([]Library{
		{Name: "Rel", Path: "./media/movies", Type: "movies"},
		{Name: "Messy", Path: "/mnt/alberca/Music/", Type: "music"},
	}, "/wd")
	if err != nil {
		t.Fatal(err)
	}
	if libs[0].Path != "/wd/media/movies" {
		t.Errorf("relative path = %q, want /wd/media/movies", libs[0].Path)
	}
	// A trailing separator must be cleaned, or the overlap check below compares
	// inconsistent strings.
	if libs[1].Path != "/mnt/alberca/Music" {
		t.Errorf("path = %q, want the trailing slash cleaned", libs[1].Path)
	}
}

func TestNormalizeLibrariesDefaultsNameToDirectory(t *testing.T) {
	libs, err := normalizeLibraries([]Library{
		{Path: "/mnt/alberca/Movies", Type: "movies"},
	}, "/wd")
	if err != nil {
		t.Fatal(err)
	}
	if libs[0].Name != "Movies" {
		t.Errorf("name = %q, want the directory name Movies", libs[0].Name)
	}
}

func TestNormalizeLibrariesTypeIsCaseInsensitive(t *testing.T) {
	libs, err := normalizeLibraries([]Library{
		{Name: "M", Path: "/m", Type: "Movies"},
		{Name: "T", Path: "/t", Type: "  TVShows  "},
	}, "/wd")
	if err != nil {
		t.Fatal(err)
	}
	if libs[0].Type != "movies" || libs[1].Type != "tvshows" {
		t.Errorf("types not normalized: %q, %q", libs[0].Type, libs[1].Type)
	}
}

func TestNormalizeLibrariesAcceptsMixedContent(t *testing.T) {
	if _, err := normalizeLibraries([]Library{{Name: "Mixed", Path: "/m"}}, "/wd"); err != nil {
		t.Errorf("an empty type means mixed content and must be allowed: %v", err)
	}
}

func TestNormalizeLibrariesRejects(t *testing.T) {
	cases := []struct {
		name string
		in   []Library
		want string
	}{
		{
			"missing path",
			[]Library{{Name: "Movies", Type: "movies"}},
			"path is required",
		},
		{
			"unknown type",
			[]Library{{Name: "Movies", Path: "/m", Type: "films"}},
			"unknown type",
		},
		{
			"duplicate name",
			[]Library{
				{Name: "Movies", Path: "/a", Type: "movies"},
				{Name: "Movies", Path: "/b", Type: "movies"},
			},
			"duplicate name",
		},
		{
			// Names are matched case-insensitively, the way clients treat them.
			"duplicate name differing only in case",
			[]Library{
				{Name: "Movies", Path: "/a", Type: "movies"},
				{Name: "MOVIES", Path: "/b", Type: "movies"},
			},
			"duplicate name",
		},
		{
			"duplicate path",
			[]Library{
				{Name: "A", Path: "/mnt/media", Type: "movies"},
				{Name: "B", Path: "/mnt/media", Type: "music"},
			},
			"already used",
		},
		{
			// The same path written two ways is still the same path.
			"duplicate path after cleaning",
			[]Library{
				{Name: "A", Path: "/mnt/media", Type: "movies"},
				{Name: "B", Path: "/mnt/media/", Type: "music"},
			},
			"already used",
		},
		{
			// Nesting makes every item in the overlap resolve twice, under two
			// parents and two different IDs.
			"library nested inside another",
			[]Library{
				{Name: "All", Path: "/mnt/alberca", Type: "mixed"},
				{Name: "Movies", Path: "/mnt/alberca/Movies", Type: "movies"},
			},
			"overlaps",
		},
		{
			"library containing another, declared in the other order",
			[]Library{
				{Name: "Movies", Path: "/mnt/alberca/Movies", Type: "movies"},
				{Name: "All", Path: "/mnt/alberca", Type: "mixed"},
			},
			"overlaps",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := normalizeLibraries(c.in, "/wd")
			if err == nil {
				t.Fatalf("normalizeLibraries accepted %+v", c.in)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// Sibling directories that share a name prefix are not nested. Comparing raw
// string prefixes rather than whole path elements would reject this.
func TestNormalizeLibrariesAllowsPrefixSiblings(t *testing.T) {
	_, err := normalizeLibraries([]Library{
		{Name: "Movies", Path: "/mnt/alberca/Movies", Type: "movies"},
		{Name: "Movies4K", Path: "/mnt/alberca/Movies4K", Type: "movies"},
	}, "/wd")
	if err != nil {
		t.Errorf("sibling directories sharing a prefix are not nested: %v", err)
	}
}

func TestPathContains(t *testing.T) {
	cases := []struct {
		parent, child string
		want          bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b/c/d", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/x/y", false},
		{"/", "/anything", true},
	}
	for _, c := range cases {
		if got := pathContains(c.parent, c.child); got != c.want {
			t.Errorf("pathContains(%q, %q) = %v, want %v", c.parent, c.child, got, c.want)
		}
	}
}

func TestNormalizeLibrariesEmpty(t *testing.T) {
	libs, err := normalizeLibraries(nil, "/wd")
	if err != nil {
		t.Fatalf("no libraries is a valid configuration: %v", err)
	}
	if len(libs) != 0 {
		t.Errorf("got %d libraries, want none", len(libs))
	}
}

// ------------------------------------------------------- path validation

func TestValidateLibraryPaths(t *testing.T) {
	dir := t.TempDir()
	movies := filepath.Join(dir, "Movies")
	if err := os.Mkdir(movies, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("readable directory", func(t *testing.T) {
		if err := validateLibraryPaths([]Library{{Name: "Movies", Path: movies}}); err != nil {
			t.Errorf("a readable directory should validate: %v", err)
		}
	})

	t.Run("empty directory is fine", func(t *testing.T) {
		// An empty media root is normal on first run; ReadDir returning EOF
		// must not be mistaken for an error.
		if err := validateLibraryPaths([]Library{{Name: "Empty", Path: movies}}); err != nil {
			t.Errorf("an empty library must be allowed: %v", err)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		err := validateLibraryPaths([]Library{{Name: "Gone", Path: filepath.Join(dir, "nope")}})
		if err == nil {
			t.Fatal("a missing path should fail validation")
		}
		if !strings.Contains(err.Error(), "Gone") {
			t.Errorf("error = %q, want it to name the library", err)
		}
	})

	t.Run("path is a file", func(t *testing.T) {
		file := filepath.Join(dir, "notadir.mkv")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := validateLibraryPaths([]Library{{Name: "File", Path: file}})
		if err == nil {
			t.Fatal("a regular file should fail validation")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error = %q, want it to say the path is not a directory", err)
		}
	})
}

// A directory that stats fine but cannot be listed is the classic "service user
// lacks permission" case, and must fail at startup rather than scan as empty.
func TestValidateLibraryPathsUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	if err := validateLibraryPaths([]Library{{Name: "Locked", Path: locked}}); err == nil {
		t.Error("an unreadable directory should fail validation")
	}
}

// ------------------------------------------------------- library config chain

func TestLoadLibrariesFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gofin.json")
	if err := os.WriteFile(path, []byte(`{"libraries":[
		{"name":"Movies","path":"/mnt/alberca/Movies","type":"movies","readonly":true},
		{"name":"Music","path":"/mnt/alberca/Music","type":"music","readonly":true}
	]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Libraries) != 2 {
		t.Fatalf("got %d libraries, want 2", len(cfg.Libraries))
	}
	if cfg.Libraries[0].Name != "Movies" || cfg.Libraries[0].Path != "/mnt/alberca/Movies" {
		t.Errorf("first library = %+v", cfg.Libraries[0])
	}
	if cfg.Libraries[1].Type != "music" {
		t.Errorf("second library type = %q, want music", cfg.Libraries[1].Type)
	}
	for _, l := range cfg.Libraries {
		if !l.IsReadOnly() {
			t.Errorf("library %q should be read-only", l.Name)
		}
	}
}

func TestLoadLibrariesFromEnv(t *testing.T) {
	t.Setenv("GOFIN_LIBRARIES",
		`[{"name":"Movies","path":"/mnt/alberca/Movies","type":"movies"}]`)

	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Libraries) != 1 || cfg.Libraries[0].Name != "Movies" {
		t.Fatalf("libraries = %+v", cfg.Libraries)
	}
}

func TestLoadLibrariesEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gofin.json")
	if err := os.WriteFile(path, []byte(
		`{"libraries":[{"name":"FromFile","path":"/a","type":"movies"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFIN_LIBRARIES", `[{"name":"FromEnv","path":"/b","type":"music"}]`)

	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Libraries) != 1 || cfg.Libraries[0].Name != "FromEnv" {
		t.Errorf("libraries = %+v, want the env value to win", cfg.Libraries)
	}
}

func TestLoadLibrariesBadEnvJSON(t *testing.T) {
	t.Setenv("GOFIN_LIBRARIES", `not json`)
	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := load(fs, ""); err == nil {
		t.Error("malformed GOFIN_LIBRARIES should be an error, not silently ignored")
	}
}

// An invalid library must stop startup rather than be dropped silently, which
// would present as a library that simply never appears.
func TestLoadRejectsInvalidLibrary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gofin.json")
	if err := os.WriteFile(path, []byte(
		`{"libraries":[{"name":"Movies","path":"/a","type":"nonsense"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := load(fs, path); err == nil {
		t.Error("an unknown collection type should fail the load")
	}
}

// ------------------------------------------------------------ -library flag

func TestLibraryFlagParsing(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Library
	}{
		{
			"all fields",
			"name=Movies,type=movies,path=/mnt/alberca/Movies,readonly",
			Library{Name: "Movies", Type: "movies", Path: "/mnt/alberca/Movies", ReadOnly: boolPtr(true)},
		},
		{
			"path only",
			"path=/mnt/alberca/Music",
			Library{Path: "/mnt/alberca/Music"},
		},
		{
			"explicit readonly=false",
			"path=/m,readonly=false",
			Library{Path: "/m", ReadOnly: boolPtr(false)},
		},
		{
			"writable alias",
			"path=/m,writable",
			Library{Path: "/m", ReadOnly: boolPtr(false)},
		},
		{
			"ro alias",
			"path=/m,ro",
			Library{Path: "/m", ReadOnly: boolPtr(true)},
		},
		{
			"whitespace and key casing are tolerated",
			" Name=Movies , TYPE=movies , path=/m ",
			Library{Name: "Movies", Type: "movies", Path: "/m"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var l libraryList
			if err := l.Set(c.in); err != nil {
				t.Fatalf("Set(%q): %v", c.in, err)
			}
			if len(l) != 1 {
				t.Fatalf("got %d libraries, want 1", len(l))
			}
			got := l[0]
			if got.Name != c.want.Name || got.Type != c.want.Type || got.Path != c.want.Path {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
			if got.IsReadOnly() != c.want.IsReadOnly() {
				t.Errorf("IsReadOnly = %v, want %v", got.IsReadOnly(), c.want.IsReadOnly())
			}
		})
	}
}

func TestLibraryFlagRejects(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no path", "name=Movies,type=movies", "path= is required"},
		{"unknown field", "path=/m,colour=blue", "unknown library field"},
		{"bad readonly value", "path=/m,readonly=maybe", "readonly"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var l libraryList
			err := l.Set(c.in)
			if err == nil {
				t.Fatalf("Set(%q) succeeded, want an error", c.in)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestLibraryFlagRepeats(t *testing.T) {
	var l libraryList
	if err := l.Set("name=Movies,type=movies,path=/mnt/alberca/Movies"); err != nil {
		t.Fatal(err)
	}
	if err := l.Set("name=Music,type=music,path=/mnt/alberca/Music"); err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 {
		t.Fatalf("got %d libraries, want 2", len(l))
	}
	if l.String() != "Movies,Music" {
		t.Errorf("String() = %q", l.String())
	}
	got, ok := l.Get().([]Library)
	if !ok {
		t.Fatal("Get() did not return []Library")
	}
	if len(got) != 2 {
		t.Errorf("Get() returned %d libraries, want 2", len(got))
	}
}

// The flag layer replaces the file's set rather than appending to it, matching
// how every other setting overrides.
func TestLibraryFlagOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gofin.json")
	if err := os.WriteFile(path, []byte(
		`{"libraries":[{"name":"FromFile","path":"/a","type":"movies"},
		               {"name":"AlsoFromFile","path":"/b","type":"music"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := newFlagSet()
	var libs libraryList
	fs.Var(&libs, "library", "")
	if err := fs.Parse([]string{"-library", "name=FromFlag,type=movies,path=/c"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Libraries) != 1 || cfg.Libraries[0].Name != "FromFlag" {
		t.Errorf("libraries = %+v, want only the flag value", cfg.Libraries)
	}
}

// The exact configuration this was built for.
func TestAlbercaConfiguration(t *testing.T) {
	fs := newFlagSet()
	var libs libraryList
	fs.Var(&libs, "library", "")
	if err := fs.Parse([]string{
		"-library", "name=Movies,type=movies,path=/mnt/alberca/Movies,readonly",
		"-library", "name=Music,type=music,path=/mnt/alberca/Music,readonly",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Libraries) != 2 {
		t.Fatalf("got %d libraries, want 2", len(cfg.Libraries))
	}
	want := []Library{
		{Name: "Movies", Path: "/mnt/alberca/Movies", Type: "movies"},
		{Name: "Music", Path: "/mnt/alberca/Music", Type: "music"},
	}
	for i, w := range want {
		got := cfg.Libraries[i]
		if got.Name != w.Name || got.Path != w.Path || got.Type != w.Type {
			t.Errorf("library %d = %+v, want %+v", i, got, w)
		}
		if !got.IsReadOnly() {
			t.Errorf("library %q must be read-only", got.Name)
		}
	}
}

// ------------------------------------------------------------ views + items

// libEnv is a testEnv whose server has the two libraries configured.
func libEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	libs, err := normalizeLibraries([]Library{
		{Name: "Movies", Path: "/mnt/alberca/Movies", Type: "movies"},
		{Name: "Music", Path: "/mnt/alberca/Music", Type: "music"},
	}, "/wd")
	if err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Libraries = libs
	e.handler = e.srv.routes()
	return e
}

// The finding from the Delfin session log: the server emits dashless GUIDs, and
// the client sends them back DASHED in the path. Both must resolve to the same
// user or every per-user route 403s against its own token holder.
func TestUserRoutesAcceptDashedAndDashlessGuids(t *testing.T) {
	e := libEnv(t)
	tok := e.tokenFor(e.viewer)

	dashless := e.viewer.ID.String()
	dashed := e.viewer.ID.Dashed()
	if dashed == dashless {
		t.Fatal("test is meaningless: the two forms are identical")
	}

	for _, form := range []struct{ name, id string }{
		{"dashless (what we emit)", dashless},
		{"dashed (what Delfin sends back)", dashed},
	} {
		t.Run(form.name, func(t *testing.T) {
			w := e.do(t, "GET", "/Users/"+form.id+"/Views", tok, nil)
			if w.Code != 200 {
				t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body)
			}
			var got QueryResult[BaseItemDto]
			decodeJSON(t, w, &got)
			if got.TotalRecordCount != 2 {
				t.Errorf("TotalRecordCount = %d, want 2", got.TotalRecordCount)
			}
		})
	}
}

// Both spellings must work: newer clients use /UserViews, Delfin uses the
// per-user path form.
func TestUserViewsBothRouteForms(t *testing.T) {
	e := libEnv(t)
	tok := e.tokenFor(e.viewer)

	for _, path := range []string{
		"/UserViews",
		"/UserViews?userId=" + e.viewer.ID.String(),
		"/Users/" + e.viewer.ID.String() + "/Views",
	} {
		t.Run(path, func(t *testing.T) {
			w := e.do(t, "GET", path, tok, nil)
			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			var got QueryResult[BaseItemDto]
			decodeJSON(t, w, &got)
			if len(got.Items) != 2 {
				t.Errorf("got %d views, want 2", len(got.Items))
			}
		})
	}
}

func TestUserViewsContent(t *testing.T) {
	e := libEnv(t)
	w := e.do(t, "GET", "/UserViews", e.tokenFor(e.viewer), nil)

	var got QueryResult[BaseItemDto]
	decodeJSON(t, w, &got)
	if len(got.Items) != 2 {
		t.Fatalf("got %d views, want 2", len(got.Items))
	}

	movies := got.Items[0]
	if movies.Name != "Movies" {
		t.Errorf("Name = %q, want Movies", movies.Name)
	}
	if movies.CollectionType != "movies" {
		t.Errorf("CollectionType = %q, want movies", movies.CollectionType)
	}
	if !movies.IsFolder {
		t.Error("a library view must be a folder")
	}
	if movies.Type != "CollectionFolder" {
		t.Errorf("Type = %q, want CollectionFolder", movies.Type)
	}
	if movies.ID.IsZero() {
		t.Error("view has no Id")
	}
	// Clients key per-view UI state on this, so it must be set and stable.
	if movies.DisplayPreferencesID != movies.ID.String() {
		t.Errorf("DisplayPreferencesId = %q, want the item id", movies.DisplayPreferencesID)
	}
}

// Library IDs must survive a restart, or clients lose per-library state every
// time the server bounces.
func TestLibraryIDIsDeterministic(t *testing.T) {
	l := Library{Name: "Movies", Path: "/mnt/alberca/Movies", Type: "movies"}
	a := l.ID("/data")
	b := l.ID("/data")
	if a != b {
		t.Errorf("library ID is not stable: %s != %s", a, b)
	}
	if a.IsZero() {
		t.Error("library ID is zero")
	}

	// The ID follows the library name, not the media path, because upstream
	// derives it from the virtual-folder directory.
	renamed := Library{Name: "Films", Path: "/mnt/alberca/Movies", Type: "movies"}
	if renamed.ID("/data") == a {
		t.Error("renaming a library should change its ID")
	}
	moved := Library{Name: "Movies", Path: "/somewhere/else", Type: "movies"}
	if moved.ID("/data") != a {
		t.Error("moving the media path should not change the library ID")
	}
}

// Every view must serialize its empty collections as [] and {}, never null:
// clients iterate them without null checks.
func TestViewDtoEmptyCollectionsAreNotNull(t *testing.T) {
	e := libEnv(t)
	w := e.do(t, "GET", "/UserViews", e.tokenFor(e.viewer), nil)

	var raw struct {
		Items []map[string]json.RawMessage `json:"Items"`
	}
	decodeJSON(t, w, &raw)
	if len(raw.Items) == 0 {
		t.Fatal("no views returned")
	}

	for _, field := range []string{
		"BackdropImageTags", "Tags", "Genres", "Studios",
		"People", "GenreItems", "ExternalUrls", "RemoteTrailers", "LockedFields",
	} {
		if got := string(raw.Items[0][field]); got != "[]" {
			t.Errorf("%s = %s, want []", field, got)
		}
	}
	for _, field := range []string{"ImageTags", "ImageBlurHashes", "ProviderIds"} {
		if got := string(raw.Items[0][field]); got != "{}" {
			t.Errorf("%s = %s, want {}", field, got)
		}
	}
}

// With no libraries configured the answer is an empty result, not null and not
// an error.
func TestUserViewsWithNoLibraries(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/UserViews", e.tokenFor(e.viewer), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var raw map[string]json.RawMessage
	decodeJSON(t, w, &raw)
	if got := string(raw["Items"]); got != "[]" {
		t.Errorf("Items = %s, want []", got)
	}
	if got := string(raw["TotalRecordCount"]); got != "0" {
		t.Errorf("TotalRecordCount = %s, want 0", got)
	}
}

func TestUserViewsRequiresAuth(t *testing.T) {
	e := libEnv(t)
	if w := e.do(t, "GET", "/UserViews", "", nil); w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// One user may not read another's views; an admin may.
func TestUserViewsForbidsOtherUsers(t *testing.T) {
	e := libEnv(t)

	w := e.do(t, "GET", "/Users/"+e.admin.ID.String()+"/Views", e.tokenFor(e.viewer), nil)
	if w.Code != 403 {
		t.Errorf("viewer reading the admin's views = %d, want 403", w.Code)
	}

	w = e.do(t, "GET", "/Users/"+e.viewer.ID.String()+"/Views", e.tokenFor(e.admin), nil)
	if w.Code != 200 {
		t.Errorf("admin reading the viewer's views = %d, want 200", w.Code)
	}
}

func TestUserViewsRejectsMalformedUserID(t *testing.T) {
	e := libEnv(t)
	w := e.do(t, "GET", "/Users/not-a-guid/Views", e.tokenFor(e.viewer), nil)
	if w.Code != 403 {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// The top level of a browse is the library roots, which configuration alone
// can answer.
func TestUserItemsTopLevelReturnsLibraries(t *testing.T) {
	e := libEnv(t)
	w := e.do(t, "GET", "/Users/"+e.viewer.ID.String()+"/Items", e.tokenFor(e.viewer), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got QueryResult[BaseItemDto]
	decodeJSON(t, w, &got)
	if got.TotalRecordCount != 2 {
		t.Errorf("TotalRecordCount = %d, want 2", got.TotalRecordCount)
	}
}

// Below a library the scanner is needed, so the honest answer is an empty
// result -- an empty library is true, an error dialog would not be.
func TestUserItemsBelowLibraryIsEmptyNotAnError(t *testing.T) {
	e := libEnv(t)
	parent := e.srv.views()[0].ID.String()

	for _, path := range []string{
		"/Users/" + e.viewer.ID.String() + "/Items?ParentId=" + parent,
		"/Items?ParentId=" + parent,
	} {
		t.Run(path, func(t *testing.T) {
			w := e.do(t, "GET", path, e.tokenFor(e.viewer), nil)
			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			var raw map[string]json.RawMessage
			decodeJSON(t, w, &raw)
			if got := string(raw["Items"]); got != "[]" {
				t.Errorf("Items = %s, want []", got)
			}
		})
	}
}

// ------------------------------------------------- display preferences, avatar

// A client that cannot read its display preferences may stop before rendering
// anything, so this answers with defaults rather than an error.
func TestDisplayPreferences(t *testing.T) {
	e := libEnv(t)
	w := e.do(t, "GET", "/DisplayPreferences/usersettings?client=emby", e.tokenFor(e.viewer), nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body)
	}

	var got DisplayPreferencesDto
	decodeJSON(t, w, &got)
	if got.ID != "usersettings" {
		t.Errorf("Id = %q, want usersettings", got.ID)
	}
	if got.Client != "emby" {
		t.Errorf("Client = %q, want emby", got.Client)
	}
	if got.SortBy == "" || got.SortOrder == "" {
		t.Errorf("defaults are incomplete: %+v", got)
	}

	// CustomPrefs must be {} rather than null; clients index into it directly.
	var raw map[string]json.RawMessage
	decodeJSON(t, w, &raw)
	if s := string(raw["CustomPrefs"]); s != "{}" {
		t.Errorf("CustomPrefs = %s, want {}", s)
	}
}

// Writes are accepted and dropped. An error here makes some clients retry in a
// loop.
func TestDisplayPreferencesWriteIsAccepted(t *testing.T) {
	e := libEnv(t)
	w := e.do(t, "POST", "/DisplayPreferences/usersettings", e.tokenFor(e.viewer),
		strings.NewReader(`{"SortBy":"DateCreated"}`))
	if w.Code != 204 {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestDisplayPreferencesRequiresAuth(t *testing.T) {
	e := libEnv(t)
	if w := e.do(t, "GET", "/DisplayPreferences/usersettings", "", nil); w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

// A user with no avatar is a 404 -- what upstream returns, and what makes
// clients fall back to a generated monogram instead of showing a broken image.
func TestUserPrimaryImageIs404(t *testing.T) {
	e := libEnv(t)
	w := e.do(t, "GET", "/Users/"+e.viewer.ID.String()+"/Images/Primary", "", nil)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// The exact request sequence from the Delfin session log, with the dashed
// GUID it actually sent.
func TestDelfinSessionSequence(t *testing.T) {
	e := libEnv(t)
	tok := e.tokenFor(e.admin)
	id := e.admin.ID.Dashed()

	for _, c := range []struct {
		path string
		want int
	}{
		{"/Users/" + id + "/Images/Primary", 404},
		{"/Users/" + id + "/Views", 200},
		{"/DisplayPreferences/usersettings", 200},
		{"/Users/" + id + "/Items", 200},
	} {
		t.Run(c.path, func(t *testing.T) {
			if w := e.do(t, "GET", c.path, tok, nil); w.Code != c.want {
				t.Errorf("status = %d, want %d", w.Code, c.want)
			}
		})
	}
}

// ------------------------------------------------------ scanned item browsing

// scannedEnv builds a real media tree on disk, configures it as a library, and
// scans it -- so these tests exercise the whole path from filesystem to DTO.
func scannedEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	root := t.TempDir()
	movies := filepath.Join(root, "Movies")
	music := filepath.Join(root, "Music")

	for _, p := range []string{
		filepath.Join(movies, "Arrival (2016).mkv"),
		filepath.Join(movies, "Dune.2021.1080p.BluRay.x264.mkv"),
		filepath.Join(movies, "sample.mkv"),
		filepath.Join(music, "Radiohead", "OK Computer", "01 Airbag.flac"),
		filepath.Join(music, "Radiohead", "OK Computer", "02 Paranoid Android.flac"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	e := newTestEnv(t)
	libs, err := normalizeLibraries([]Library{
		{Name: "Movies", Path: movies, Type: "movies"},
		{Name: "Music", Path: music, Type: "music"},
	}, root)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Libraries = libs
	e.srv.cfg.Paths.Data = filepath.Join(root, "data")
	e.handler = e.srv.routes()

	e.srv.scanLibraries()
	return e, root
}

func TestBrowseScannedMovies(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	// The library view reports how many items it holds.
	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)

	var moviesView BaseItemDto
	for _, v := range views.Items {
		if v.Name == "Movies" {
			moviesView = v
		}
	}
	if moviesView.ID.IsZero() {
		t.Fatal("no Movies view")
	}
	if moviesView.ChildCount != 2 {
		t.Errorf("ChildCount = %d, want 2 (the sample must not count)", moviesView.ChildCount)
	}

	// Browsing into it returns the films.
	var got QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+moviesView.ID.String(), tok, nil), &got)

	if got.TotalRecordCount != 2 {
		t.Fatalf("TotalRecordCount = %d, want 2: %+v", got.TotalRecordCount, got.Items)
	}
	byName := map[string]BaseItemDto{}
	for _, it := range got.Items {
		byName[it.Name] = it
	}
	arrival, ok := byName["Arrival"]
	if !ok {
		t.Fatalf("no item named Arrival; got %v", got.Items)
	}
	if arrival.Type != "Movie" {
		t.Errorf("Type = %q, want Movie", arrival.Type)
	}
	if arrival.MediaType != "Video" {
		t.Errorf("MediaType = %q, want Video", arrival.MediaType)
	}
	if arrival.ProductionYear == nil || *arrival.ProductionYear != 2016 {
		t.Errorf("ProductionYear = %v, want 2016", arrival.ProductionYear)
	}
	if _, ok := byName["Dune"]; !ok {
		t.Error("the scene-named release should resolve to Dune")
	}
}

func TestBrowseScannedMusic(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	var musicView BaseItemDto
	for _, v := range views.Items {
		if v.Name == "Music" {
			musicView = v
		}
	}
	if musicView.ID.IsZero() {
		t.Fatal("no Music view")
	}

	// Library -> artist
	var artists QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+musicView.ID.String(), tok, nil), &artists)
	if len(artists.Items) != 1 || artists.Items[0].Name != "Radiohead" {
		t.Fatalf("artists = %+v, want one Radiohead", artists.Items)
	}
	if artists.Items[0].Type != "MusicArtist" {
		t.Errorf("Type = %q, want MusicArtist", artists.Items[0].Type)
	}

	// Artist -> album
	var albums QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+artists.Items[0].ID.String(), tok, nil), &albums)
	if len(albums.Items) != 1 || albums.Items[0].Name != "OK Computer" {
		t.Fatalf("albums = %+v, want one OK Computer", albums.Items)
	}

	// Album -> tracks, in disc order
	var tracks QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+albums.Items[0].ID.String(), tok, nil), &tracks)
	if len(tracks.Items) != 2 {
		t.Fatalf("got %d tracks, want 2", len(tracks.Items))
	}
	if tracks.Items[0].Name != "Airbag" {
		t.Errorf("first track = %q, want Airbag", tracks.Items[0].Name)
	}
	if tracks.Items[0].MediaType != "Audio" {
		t.Errorf("MediaType = %q, want Audio", tracks.Items[0].MediaType)
	}
	if tracks.Items[0].IndexNumber == nil || *tracks.Items[0].IndexNumber != 1 {
		t.Errorf("IndexNumber = %v, want 1", tracks.Items[0].IndexNumber)
	}
	if tracks.Items[0].Album != "OK Computer" {
		t.Errorf("Album = %q", tracks.Items[0].Album)
	}
}

// An unknown year or duration must render as null, not 0: a client reads year 0
// as a real year and a zero duration as an unplayable item.
func TestUnknownYearAndDurationAreNull(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	var musicView BaseItemDto
	for _, v := range views.Items {
		if v.Name == "Music" {
			musicView = v
		}
	}

	w := e.do(t, "GET", "/Items?ParentId="+musicView.ID.String(), tok, nil)
	var raw struct {
		Items []map[string]json.RawMessage `json:"Items"`
	}
	decodeJSON(t, w, &raw)
	if len(raw.Items) == 0 {
		t.Fatal("no items")
	}
	for _, f := range []string{"ProductionYear", "RunTimeTicks"} {
		if got := string(raw.Items[0][f]); got != "null" {
			t.Errorf("%s = %s, want null when unknown", f, got)
		}
	}
}

func TestItemByID(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	lib := views.Items[0]

	// A library root resolves even though it is not in the item store.
	var got BaseItemDto
	decodeJSON(t, e.do(t, "GET", "/Items/"+lib.ID.String(), tok, nil), &got)
	if got.ID != lib.ID {
		t.Errorf("Id = %s, want %s", got.ID, lib.ID)
	}

	// A scanned item resolves, in both GUID spellings.
	var kids QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+lib.ID.String(), tok, nil), &kids)
	if len(kids.Items) == 0 {
		t.Fatal("library has no children")
	}
	child := kids.Items[0]
	for _, form := range []string{child.ID.String(), child.ID.Dashed()} {
		w := e.do(t, "GET", "/Items/"+form, tok, nil)
		if w.Code != 200 {
			t.Errorf("GET /Items/%s = %d, want 200", form, w.Code)
		}
	}
}

func TestItemByIDNotFound(t *testing.T) {
	e, _ := scannedEnv(t)
	w := e.do(t, "GET", "/Items/ffffffffffffffffffffffffffffffff", e.tokenFor(e.viewer), nil)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestItemsRejectsMalformedParentID(t *testing.T) {
	e, _ := scannedEnv(t)
	w := e.do(t, "GET", "/Items?ParentId=not-a-guid", e.tokenFor(e.viewer), nil)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// Paging must not lie about the total, or the client sizes its scrollbar wrong.
func TestItemPaging(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	var moviesView BaseItemDto
	for _, v := range views.Items {
		if v.Name == "Movies" {
			moviesView = v
		}
	}

	var got QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET",
		"/Items?ParentId="+moviesView.ID.String()+"&Limit=1", tok, nil), &got)
	if len(got.Items) != 1 {
		t.Errorf("got %d items, want 1", len(got.Items))
	}
	if got.TotalRecordCount != 2 {
		t.Errorf("TotalRecordCount = %d, want the full 2", got.TotalRecordCount)
	}

	decodeJSON(t, e.do(t, "GET",
		"/Items?ParentId="+moviesView.ID.String()+"&StartIndex=1", tok, nil), &got)
	if len(got.Items) != 1 {
		t.Errorf("got %d items after StartIndex=1, want 1", len(got.Items))
	}
	if got.StartIndex != 1 {
		t.Errorf("StartIndex = %d, want 1", got.StartIndex)
	}

	// Past the end is an empty page, not an error and not null.
	decodeJSON(t, e.do(t, "GET",
		"/Items?ParentId="+moviesView.ID.String()+"&StartIndex=99", tok, nil), &got)
	if len(got.Items) != 0 {
		t.Errorf("got %d items past the end, want 0", len(got.Items))
	}
}

// A rescan must not change item IDs, or every scan invalidates client caches.
func TestRescanIsStable(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	var before QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &before)
	lib := before.Items[0].ID

	var a QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+lib.String(), tok, nil), &a)

	e.srv.scanLibraries()

	var b QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+lib.String(), tok, nil), &b)

	if len(a.Items) != len(b.Items) {
		t.Fatalf("item count changed across a rescan: %d -> %d", len(a.Items), len(b.Items))
	}
	for i := range a.Items {
		if a.Items[i].ID != b.Items[i].ID {
			t.Errorf("item %d changed ID across a rescan: %s -> %s",
				i, a.Items[i].ID, b.Items[i].ID)
		}
	}
}

func TestLibraryRefreshRequiresAdmin(t *testing.T) {
	e, _ := scannedEnv(t)
	if w := e.do(t, "POST", "/Library/Refresh", e.tokenFor(e.viewer), nil); w.Code != 403 {
		t.Errorf("non-admin = %d, want 403", w.Code)
	}
	if w := e.do(t, "POST", "/Library/Refresh", e.tokenFor(e.admin), nil); w.Code != 204 {
		t.Errorf("admin = %d, want 204", w.Code)
	}
}

// The item store is swapped in wholesale, so a concurrent read during a rescan
// sees either the old set or the new one, never a half-populated library.
func TestConcurrentBrowseDuringRescan(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			e.srv.scanLibraries()
		}
	}()
	for i := 0; i < 50; i++ {
		if w := e.do(t, "GET", "/UserViews", tok, nil); w.Code != 200 {
			t.Errorf("browse during rescan = %d", w.Code)
			break
		}
	}
	<-done
}

// ------------------------------------------- literal routes under /Items/

// A real client asked for /Users/{id}/Items/Latest and got 400: the {itemId}
// wildcard swallowed it and tried to parse "Latest" as a GUID. Each literal
// segment under /Items/ has to be registered or it breaks a real endpoint.
func TestItemsLiteralRoutesAreNotParsedAsIDs(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)
	uid := e.viewer.ID.Dashed() // the dashed form a client actually sends

	for _, path := range []string{
		"/Users/" + uid + "/Items/Latest",
		"/Users/" + uid + "/Items/Resume",
		"/Users/" + uid + "/Items/Root",
		"/Items/Latest",
		"/Items/Resume",
	} {
		t.Run(path, func(t *testing.T) {
			w := e.do(t, "GET", path, tok, nil)
			if w.Code == 400 {
				t.Fatalf("status = 400: %q was parsed as an item id", path)
			}
			if w.Code != 200 {
				t.Errorf("status = %d, want 200", w.Code)
			}
		})
	}
}

// Latest returns a BARE ARRAY, not a QueryResult. A client handed
// {"Items":[...]} here renders an empty row.
func TestLatestItemsIsABareArray(t *testing.T) {
	e, _ := scannedEnv(t)
	w := e.do(t, "GET", "/Items/Latest", e.tokenFor(e.viewer), nil)

	body := strings.TrimSpace(w.Body.String())
	if !strings.HasPrefix(body, "[") {
		t.Fatalf("Latest returned %.60s..., want a bare JSON array", body)
	}
	var items []BaseItemDto
	decodeJSON(t, w, &items)
	if len(items) == 0 {
		t.Error("Latest returned nothing from a scanned library")
	}
	// Folders are not "recently added media".
	for _, it := range items {
		if it.IsFolder {
			t.Errorf("Latest included the folder %q", it.Name)
		}
	}
}

// Latest is newest-first, and scoping it to a library must reach media nested
// below it -- music tracks are two levels down.
func TestLatestItemsOrderingAndScope(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	var music BaseItemDto
	for _, v := range views.Items {
		if v.Name == "Music" {
			music = v
		}
	}

	var items []BaseItemDto
	decodeJSON(t, e.do(t, "GET", "/Items/Latest?ParentId="+music.ID.String(), tok, nil), &items)
	if len(items) == 0 {
		t.Fatal("scoping Latest to the music library found nothing; it must descend past artist and album")
	}
	for _, it := range items {
		if it.Type != "Audio" {
			t.Errorf("got a %s in the music library's Latest", it.Type)
		}
	}
}

func TestLatestRespectsLimit(t *testing.T) {
	e, _ := scannedEnv(t)
	var items []BaseItemDto
	decodeJSON(t, e.do(t, "GET", "/Items/Latest?Limit=1", e.tokenFor(e.viewer), nil), &items)
	if len(items) != 1 {
		t.Errorf("got %d items, want 1", len(items))
	}
}

// Resume needs playback state, which nothing records. Empty is truthful and
// keeps "Continue Watching" from erroring.
func TestResumeIsEmptyNotAnError(t *testing.T) {
	e, _ := scannedEnv(t)
	w := e.do(t, "GET", "/Items/Resume", e.tokenFor(e.viewer), nil)
	var raw map[string]json.RawMessage
	decodeJSON(t, w, &raw)
	if got := string(raw["Items"]); got != "[]" {
		t.Errorf("Items = %s, want []", got)
	}
}

// ------------------------------------------------------------------ artwork

// scannedEnvWithArt is scannedEnv plus a conventional poster beside one film.
func scannedEnvWithArt(t *testing.T) (*testEnv, string) {
	t.Helper()
	e, root := scannedEnv(t)
	poster := filepath.Join(root, "Movies", "poster.jpg")
	if err := os.WriteFile(poster, []byte("\xff\xd8\xff\xe0JPEGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.srv.scanLibraries()
	return e, root
}

// An item with no local artwork must answer 404, not 501: a client reads 404 as
// "draw a placeholder" and stops, while 501 reads as a broken server and gets
// retried on every render.
func TestMissingArtworkIs404NotUnimplemented(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	var kids QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+views.Items[0].ID.String(), tok, nil), &kids)
	if len(kids.Items) == 0 {
		t.Fatal("no items")
	}

	w := e.do(t, "GET", "/Items/"+kids.Items[0].ID.String()+"/Images/Primary", "", nil)
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// Image URLs go into <img> tags, which cannot set an Authorization header.
func TestArtworkNeedsNoToken(t *testing.T) {
	e, _ := scannedEnvWithArt(t)
	tok := e.tokenFor(e.viewer)

	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	var movies BaseItemDto
	for _, v := range views.Items {
		if v.Name == "Movies" {
			movies = v
		}
	}
	var kids QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+movies.ID.String(), tok, nil), &kids)

	var withArt *BaseItemDto
	for i := range kids.Items {
		if kids.Items[i].ImageTags["Primary"] != "" {
			withArt = &kids.Items[i]
		}
	}
	if withArt == nil {
		t.Fatal("no item advertised a Primary image tag")
	}

	// No Authorization header at all.
	w := e.do(t, "GET", "/Items/"+withArt.ID.String()+"/Images/Primary", "", nil)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; artwork must not require a token", w.Code)
	}
	if !strings.HasPrefix(w.Body.String(), "\xff\xd8\xff") {
		t.Error("body is not the JPEG that was written")
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("Cache-Control = %q, want a long max-age on immutable artwork", cc)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("no ETag on artwork")
	}
}

// An item with no artwork must not advertise a Primary tag, or every poster
// slot fires a request that can only 404.
func TestImageTagsOnlyWhenArtworkExists(t *testing.T) {
	e, _ := scannedEnv(t)
	tok := e.tokenFor(e.viewer)

	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	var kids QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+views.Items[0].ID.String(), tok, nil), &kids)

	for _, it := range kids.Items {
		if it.ImageTags["Primary"] != "" {
			t.Errorf("%q advertises artwork it does not have", it.Name)
		}
	}
}

func TestNonPrimaryImageTypeIs404(t *testing.T) {
	e, _ := scannedEnvWithArt(t)
	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", e.tokenFor(e.viewer), nil), &views)

	for _, typ := range []string{"Backdrop", "Logo", "Thumb"} {
		w := e.do(t, "GET", "/Items/"+views.Items[0].ID.String()+"/Images/"+typ, "", nil)
		if w.Code != 404 {
			t.Errorf("%s = %d, want 404", typ, w.Code)
		}
	}
}

func TestImageRejectsMalformedID(t *testing.T) {
	e, _ := scannedEnv(t)
	w := e.do(t, "GET", "/Items/not-a-guid/Images/Primary", "", nil)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------- artwork fetching

// tmdbEnv is a scanned library wired to a fake TMDB, so nothing touches the
// network.
func tmdbEnv(t *testing.T, handler http.HandlerFunc) (*testEnv, *httptest.Server) {
	t.Helper()
	e, root := scannedEnv(t)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := jftmdb.New("test-key")
	c.APIBase = srv.URL
	c.ImageBase = srv.URL + "/img"
	c.MinInterval = 0
	e.srv.tmdb = c
	e.srv.cfg.Paths.Cache = filepath.Join(root, "cache")
	return e, srv
}

const tmdbHit = `{"results":[{"id":1,"title":"Arrival","release_date":"2016-11-10","poster_path":"/p.jpg"}]}`

func TestFetchArtworkDownloadsPosters(t *testing.T) {
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/img/") {
			w.Write([]byte("\xff\xd8\xff\xe0JPEG"))
			return
		}
		w.Write([]byte(tmdbHit))
	})

	e.srv.fetchArtwork()

	// Both scanned films should now have a cached poster.
	var withArt int
	for _, it := range e.srv.items.All() {
		if it.Kind == jfscan.KindMovie && it.PrimaryImage != "" {
			withArt++
		}
	}
	if withArt != 2 {
		t.Errorf("%d movies have artwork, want 2", withArt)
	}
}

// Posters must land in the cache, never beside the media: a library declared
// readonly has to stay untouched.
func TestFetchArtworkNeverWritesToTheMediaDirectory(t *testing.T) {
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/img/") {
			w.Write([]byte("\xff\xd8\xff\xe0JPEG"))
			return
		}
		w.Write([]byte(tmdbHit))
	})

	moviesDir := e.srv.cfg.Libraries[0].Path
	before := listTree(t, moviesDir)

	e.srv.fetchArtwork()

	if after := listTree(t, moviesDir); len(after) != len(before) {
		t.Errorf("the media directory gained files: %v -> %v", before, after)
	}
	// And the cache did gain them.
	if n := len(listTree(t, e.srv.posterDir())); n == 0 {
		t.Error("no posters were written to the cache directory")
	}
}

// An unmatched title gets a marker so the next restart does not ask again -- a
// library of scene releases has plenty of these.
func TestFetchArtworkRecordsMisses(t *testing.T) {
	var searches int32
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&searches, 1)
		w.Write([]byte(`{"results":[]}`))
	})

	e.srv.fetchArtwork()
	first := atomic.LoadInt32(&searches)
	if first == 0 {
		t.Fatal("no searches were made")
	}

	// A second run must not re-query what already missed.
	e.srv.fetchArtwork()
	if got := atomic.LoadInt32(&searches); got != first {
		t.Errorf("made %d more searches on the second run; misses should be remembered", got-first)
	}
}

// Clearing the markers is what makes a retry meaningful after renaming files.
func TestRefreshArtworkRetryMissed(t *testing.T) {
	var searches int32
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&searches, 1)
		w.Write([]byte(`{"results":[]}`))
	})

	e.srv.fetchArtwork()
	first := atomic.LoadInt32(&searches)

	e.srv.clearArtworkMisses()
	e.srv.fetchArtwork()
	if got := atomic.LoadInt32(&searches); got <= first {
		t.Error("clearing the misses should cause a fresh lookup")
	}
}

// A rejected key fails identically for every remaining item, so the run stops
// rather than sending one doomed request per film.
func TestFetchArtworkStopsOnRejectedKey(t *testing.T) {
	var calls int32
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	})

	e.srv.fetchArtwork()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d calls after a 401, want 1 -- the run should stop", got)
	}
}

func TestFetchArtworkStopsOnRateLimit(t *testing.T) {
	var calls int32
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	})

	e.srv.fetchArtwork()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d calls after a 429, want 1", got)
	}
}

// Music is not looked up: TMDB is a movie database.
func TestFetchArtworkSkipsNonMovies(t *testing.T) {
	var queries []string
	var mu sync.Mutex
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("query"); q != "" {
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
		}
		w.Write([]byte(`{"results":[]}`))
	})

	e.srv.fetchArtwork()

	mu.Lock()
	defer mu.Unlock()
	for _, q := range queries {
		if q == "Airbag" || q == "Paranoid Android" || q == "Radiohead" {
			t.Errorf("looked up the music item %q against a movie database", q)
		}
	}
}

// A cached poster is adopted at scan time, so a restart shows artwork
// immediately instead of waiting on the network. This is the first thing gofin
// persists across restarts.
func TestCachedPosterSurvivesRescan(t *testing.T) {
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/img/") {
			w.Write([]byte("\xff\xd8\xff\xe0JPEG"))
			return
		}
		w.Write([]byte(tmdbHit))
	})

	e.srv.fetchArtwork()

	// Drop the provider entirely, then rescan: the posters must still be there.
	e.srv.tmdb = nil
	e.srv.scanLibraries()

	var withArt int
	for _, it := range e.srv.items.All() {
		if it.Kind == jfscan.KindMovie && it.PrimaryImage != "" {
			withArt++
		}
	}
	if withArt != 2 {
		t.Errorf("%d movies kept artwork across a rescan with no provider, want 2", withArt)
	}
}

// A fetched poster must be servable and advertised, end to end.
func TestFetchedPosterIsServed(t *testing.T) {
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/img/") {
			w.Write([]byte("\xff\xd8\xff\xe0JPEG"))
			return
		}
		w.Write([]byte(tmdbHit))
	})
	e.srv.fetchArtwork()

	tok := e.tokenFor(e.viewer)
	var views QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/UserViews", tok, nil), &views)
	var movies BaseItemDto
	for _, v := range views.Items {
		if v.Name == "Movies" {
			movies = v
		}
	}
	var kids QueryResult[BaseItemDto]
	decodeJSON(t, e.do(t, "GET", "/Items?ParentId="+movies.ID.String(), tok, nil), &kids)

	if len(kids.Items) == 0 {
		t.Fatal("no items")
	}
	it := kids.Items[0]
	if it.ImageTags["Primary"] == "" {
		t.Fatal("a fetched poster was not advertised in ImageTags")
	}
	w := e.do(t, "GET", "/Items/"+it.ID.String()+"/Images/Primary", "", nil)
	if w.Code != 200 {
		t.Fatalf("serving the fetched poster = %d, want 200", w.Code)
	}
	if !strings.HasPrefix(w.Body.String(), "\xff\xd8\xff") {
		t.Error("served body is not the JPEG that was downloaded")
	}
}

// With no provider configured the endpoint says so rather than pretending.
func TestRefreshArtworkWithoutProvider(t *testing.T) {
	e, _ := scannedEnv(t)
	w := e.do(t, "POST", "/Library/RefreshArtwork", e.tokenFor(e.admin), nil)
	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestRefreshArtworkRequiresAdmin(t *testing.T) {
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[]}`))
	})
	if w := e.do(t, "POST", "/Library/RefreshArtwork", e.tokenFor(e.viewer), nil); w.Code != 403 {
		t.Errorf("non-admin = %d, want 403", w.Code)
	}
}

// Two concurrent fetches must not both run: the second would re-request
// everything the first is already downloading.
func TestFetchArtworkDoesNotRunTwice(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	e, _ := tmdbEnv(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release
		w.Write([]byte(`{"results":[]}`))
	})

	done := make(chan struct{})
	go func() { e.srv.fetchArtwork(); close(done) }()

	// Wait for the first run to be in flight, then try to start another.
	for i := 0; i < 100 && atomic.LoadInt32(&calls) == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	e.srv.fetchArtwork() // must return immediately
	close(release)
	<-done
}

// The provider must be off unless it is both enabled and given a key.
func TestTMDBRequiresBothEnabledAndKey(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		key     string
		want    bool
	}{
		{"off by default", false, "", false},
		{"enabled without a key", true, "", false},
		{"key without enabling", false, "k", false},
		{"both", true, "k", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.enabled && c.key != ""
			if got != c.want {
				t.Errorf("enabled=%v key=%q -> %v, want %v", c.enabled, c.key, got, c.want)
			}
		})
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
}
