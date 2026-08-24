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
	"testing"
	"time"

	"gofin/internal/jfauth"
	"gofin/internal/jfid"
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

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := defaultConfig()
	cfg.Server.Name = "Test Server"

	store := NewStore()
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
	store.AddUser(admin)
	store.AddUser(viewer)

	srv := &Server{
		cfg:      cfg,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    store,
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
	return e.srv.store.IssueToken(u, jfauthInfoForTest()).Value
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

func TestStoreUserLookup(t *testing.T) {
	e := newTestEnv(t)
	s := e.srv.store

	// Jellyfin logins are case-insensitive on the username.
	for _, name := range []string{"admin", "ADMIN", "Admin"} {
		if u := s.UserByName(name); u == nil || u.ID != e.admin.ID {
			t.Errorf("UserByName(%q) did not find the admin", name)
		}
	}
	if s.UserByName("nobody") != nil {
		t.Error("UserByName found a user that does not exist")
	}
	if u := s.UserByID(e.viewer.ID); u == nil || u.Name != "viewer" {
		t.Error("UserByID did not find the viewer")
	}
	if s.UserByID(jfid.Nil) != nil {
		t.Error("UserByID found a user for the nil GUID")
	}
	if got := len(s.Users()); got != 2 {
		t.Errorf("Users() returned %d, want 2", got)
	}
}

func TestStoreTokens(t *testing.T) {
	e := newTestEnv(t)
	s := e.srv.store

	tok := s.IssueToken(e.admin, jfauthInfoForTest())
	if len(tok.Value) != 32 {
		t.Errorf("token %q is %d chars, want 32 hex chars", tok.Value, len(tok.Value))
	}
	if _, err := hex.DecodeString(tok.Value); err != nil {
		t.Errorf("token is not hex: %v", err)
	}

	gotTok, gotUser := s.Lookup(tok.Value)
	if gotTok == nil || gotUser == nil {
		t.Fatal("Lookup did not find the freshly issued token")
	}
	if gotUser.ID != e.admin.ID {
		t.Error("Lookup returned the wrong user")
	}
	if gotTok.Client != "test" || gotTok.DeviceID != "d1" {
		t.Errorf("token did not record the client metadata: %+v", gotTok)
	}

	if _, u := s.Lookup("0000000000000000"); u != nil {
		t.Error("Lookup accepted an unknown token")
	}
	if _, u := s.Lookup(""); u != nil {
		t.Error("Lookup accepted an empty token")
	}

	// Two tokens for the same user must be distinct.
	if other := s.IssueToken(e.admin, jfauthInfoForTest()); other.Value == tok.Value {
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
	e.srv.store = NewStore()
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
	e.srv.store = NewStore()
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
	for _, path := range []string{"/UserViews", "/Items", "/Library/VirtualFolders", "/nonsense"} {
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
