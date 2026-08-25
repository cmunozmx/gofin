// Package jfid implements Jellyfin's GUID wire format and its deterministic
// item-ID derivation.
//
// Two things here break every client if they are wrong:
//
//  1. Jellyfin serializes GUIDs DASHLESS (.NET "N" format, 32 hex chars) and
//     accepts both dashed and dashless on input.
//
//  2. Item IDs are not random. They are an MD5 over a key derived from the
//     item's path and .NET type name, reinterpreted through .NET's
//     mixed-endian Guid(byte[]) constructor. Reproducing this exactly means an
//     existing Jellyfin library keeps all of its item IDs, so client caches,
//     favourites, playlists and deep links survive the swap.
package jfid

import (
	"crypto/md5"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
)

// GUID holds the 16 bytes in RFC-4122 / big-endian text order, i.e. the order
// they appear in the canonical string form. This is NOT the order .NET's
// Guid(byte[]) constructor consumes; see FromDotNetBytes.
type GUID [16]byte

// Nil is the all-zero GUID.
var Nil GUID

func (g GUID) IsZero() bool { return g == Nil }

// String returns the dashless lowercase form Jellyfin puts on the wire.
func (g GUID) String() string { return hex.EncodeToString(g[:]) }

// Dashed returns the canonical 8-4-4-4-12 form, for logs and SQL.
func (g GUID) Dashed() string {
	s := hex.EncodeToString(g[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// Parse accepts dashed, dashless, and brace-wrapped forms, case-insensitively.
func Parse(s string) (GUID, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return Nil, fmt.Errorf("jfid: bad guid length %d in %q", len(s), s)
	}
	b, err := hex.DecodeString(strings.ToLower(s))
	if err != nil {
		return Nil, fmt.Errorf("jfid: %w", err)
	}
	var g GUID
	copy(g[:], b)
	return g, nil
}

// MustParse is Parse for constants and tests.
func MustParse(s string) GUID {
	g, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return g
}

// MarshalJSON emits the dashless quoted form. A zero GUID emits null, matching
// how Jellyfin renders unset Guid? fields.
func (g GUID) MarshalJSON() ([]byte, error) {
	if g.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(g.String())
}

func (g *GUID) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*g = Nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*g = Nil
		return nil
	}
	parsed, err := Parse(s)
	if err != nil {
		return err
	}
	*g = parsed
	return nil
}

// FromDotNetBytes reinterprets a 16-byte buffer the way .NET's Guid(byte[])
// constructor does: the first three fields are little-endian (int32, int16,
// int16), the trailing eight bytes are copied verbatim. The canonical string
// form then prints those first three fields big-endian, which is why the
// leading groups appear byte-swapped.
//
// Reading an MD5 digest as if it were already a UUID -- the obvious thing to
// do -- produces IDs that are wrong in exactly the first eight bytes, which is
// a nasty bug to chase because the IDs still look plausible.
func FromDotNetBytes(b []byte) GUID {
	if len(b) < 16 {
		panic("jfid: FromDotNetBytes needs 16 bytes")
	}
	var g GUID
	g[0], g[1], g[2], g[3] = b[3], b[2], b[1], b[0]
	g[4], g[5] = b[5], b[4]
	g[6], g[7] = b[7], b[6]
	copy(g[8:], b[8:16])
	return g
}

// ToDotNetBytes is the inverse of FromDotNetBytes, for writing GUIDs into a
// database column that a .NET process also reads.
func (g GUID) ToDotNetBytes() [16]byte {
	var b [16]byte
	b[0], b[1], b[2], b[3] = g[3], g[2], g[1], g[0]
	b[4], b[5] = g[5], g[4]
	b[6], b[7] = g[7], g[6]
	copy(b[8:], g[8:])
	return b
}

// FromDotNetHex parses 32 hex chars that are already in .NET byte order and
// converts them to a GUID.
//
// This is for PRE-10.10 databases only. Jellyfin stored item GUIDs as a
// 16-byte BLOB in library.db, so `hex(guid)` came back byte-swapped and needed
// this. Since 10.10 the data lives in jellyfin.db under EF Core, which maps
// Guid to TEXT -- the column is an ordinary dashed GUID and Parse reads it
// directly. Using this function on a modern database silently byte-swaps a
// value that was already in the right order.
func FromDotNetHex(s string) (GUID, error) {
	b, err := hex.DecodeString(strings.ToLower(strings.ReplaceAll(s, "-", "")))
	if err != nil {
		return Nil, fmt.Errorf("jfid: %w", err)
	}
	if len(b) != 16 {
		return Nil, fmt.Errorf("jfid: want 16 bytes, got %d", len(b))
	}
	return FromDotNetBytes(b), nil
}

// KeyEncoding selects how a key string is turned into bytes before hashing.
//
// !! VERIFY THIS AGAINST A REAL LIBRARY BEFORE TRUSTING MIGRATED IDS !!
//
// Jellyfin's string MD5 helper uses a .NET Encoding to get the bytes. I believe
// it is Encoding.Unicode (UTF-16 little-endian, .NET's native string layout),
// not UTF-8 -- but "believe" is not good enough for something this load-bearing,
// so both are implemented and the choice is a parameter. See TestKnownItemIDs
// for how to pin it down in about two minutes using a real library.db.
type KeyEncoding int

const (
	UTF16LE KeyEncoding = iota // .NET Encoding.Unicode -- the likely one
	UTF8
)

func encodeKey(s string, enc KeyEncoding) []byte {
	if enc == UTF8 {
		return []byte(s)
	}
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// HashString reproduces .NET's `str.GetMD5()` extension: MD5 the encoded bytes
// and feed the digest through the Guid(byte[]) constructor.
func HashString(s string, enc KeyEncoding) GUID {
	sum := md5.Sum(encodeKey(s, enc))
	return FromDotNetBytes(sum[:])
}

// NewItemID reproduces LibraryManager.GetNewItemId.
//
// typeName is the fully-qualified .NET type name of the resolved item, e.g.
// "MediaBrowser.Controller.Entities.Movies.Movie". Those strings are part of the
// ID derivation, so they have to be carried verbatim in the Go port even though
// nothing else needs them -- see itemkind.go for the mapping table.
//
// Path normalization mirrors upstream: a path under the program-data root is
// stored relative to it (so that moving an install does not change every ID),
// and the whole key is lowercased before hashing.
func NewItemID(path, typeName, programDataPath string, enc KeyEncoding) GUID {
	key := path
	if programDataPath != "" && strings.HasPrefix(key, programDataPath) {
		key = key[len(programDataPath):]
	}
	return HashString(strings.ToLower(typeName+key), enc)
}

// ImageTag reproduces ImageProcessor.GetImageCacheTag: MD5 over the image path
// plus its modification time in .NET ticks, rendered as lowercase hex. Clients
// use it purely for cache-busting on /Items/{id}/Images/{type}?tag=...
func ImageTag(path string, modifiedTicks int64, enc KeyEncoding) string {
	sum := md5.Sum(encodeKey(fmt.Sprintf("%s%d", path, modifiedTicks), enc))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- database

// Value implements driver.Valuer so a GUID can be passed straight to a SQL
// query. It stores as the dashless string, which is what SQLite holds and what
// upstream has stored since 10.10.
func (g GUID) Value() (driver.Value, error) { return g.String(), nil }

// Scan implements sql.Scanner so a GUID can be read straight out of a row.
//
// Without this, database/sql refuses a [16]byte destination outright --
// "unsupported Scan, storing driver.Value type string into type *jfid.GUID" --
// and every caller has to remember to convert by hand. Making the type itself
// know how to round-trip removes the chance to forget.
//
// A NULL column scans to the zero GUID rather than an error: every caller
// already treats the zero GUID as absent.
func (g *GUID) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*g = Nil
		return nil
	case string:
		parsed, err := Parse(v)
		if err != nil {
			return fmt.Errorf("scan GUID from %q: %w", v, err)
		}
		*g = parsed
		return nil
	case []byte:
		// SQLite hands TEXT back as []byte through some drivers.
		parsed, err := Parse(string(v))
		if err != nil {
			return fmt.Errorf("scan GUID from %q: %w", v, err)
		}
		*g = parsed
		return nil
	default:
		return fmt.Errorf("scan GUID: unsupported source type %T", src)
	}
}
