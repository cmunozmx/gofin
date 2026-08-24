package jfid

import (
	"encoding/json"
	"testing"
)

func TestParseAcceptsBothForms(t *testing.T) {
	dashed := "f1e2d3c4-b5a6-4798-8a9b-0c1d2e3f4a5b"
	dashless := "f1e2d3c4b5a647988a9b0c1d2e3f4a5b"

	a, err := Parse(dashed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(dashless)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("dashed and dashless disagree: %s vs %s", a, b)
	}
	if got := a.String(); got != dashless {
		t.Errorf("String() = %q, want dashless %q", got, dashless)
	}
	if got := a.Dashed(); got != dashed {
		t.Errorf("Dashed() = %q, want %q", got, dashed)
	}
}

func TestJSONIsDashless(t *testing.T) {
	g := MustParse("f1e2d3c4-b5a6-4798-8a9b-0c1d2e3f4a5b")
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	want := `"f1e2d3c4b5a647988a9b0c1d2e3f4a5b"`
	if string(b) != want {
		t.Fatalf("Marshal = %s, want %s", b, want)
	}

	// Round-trip, and accept the dashed form on the way in.
	var back GUID
	if err := json.Unmarshal([]byte(`"f1e2d3c4-b5a6-4798-8a9b-0c1d2e3f4a5b"`), &back); err != nil {
		t.Fatal(err)
	}
	if back != g {
		t.Fatalf("round trip lost data: %s", back)
	}

	var null GUID
	if err := json.Unmarshal([]byte(`null`), &null); err != nil {
		t.Fatal(err)
	}
	if !null.IsZero() {
		t.Error("null should decode to the zero GUID")
	}
}

func TestDotNetByteOrder(t *testing.T) {
	// A .NET Guid built from bytes 00 01 02 ... 0f prints as
	// "03020100-0504-0706-0809-0a0b0c0d0e0f": the first three fields are
	// little-endian, the trailing eight bytes are verbatim.
	raw := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	got := FromDotNetBytes(raw).Dashed()
	want := "03020100-0504-0706-0809-0a0b0c0d0e0f"
	if got != want {
		t.Fatalf("FromDotNetBytes = %s, want %s", got, want)
	}

	// And the inverse must round-trip.
	back := FromDotNetBytes(raw).ToDotNetBytes()
	for i := range raw {
		if back[i] != raw[i] {
			t.Fatalf("round trip differs at byte %d: %d != %d", i, back[i], raw[i])
		}
	}
}

// TestKnownItemIDs is the test that decides whether migration works. It is
// skipped until you fill in real vectors, which takes about two minutes.
//
// WHICH QUERY TO RUN DEPENDS ON THE SERVER VERSION. Jellyfin 10.10 (the
// 20241020 "LibraryDbMigration") folded library.db into the EF Core database
// and changed how a GUID is stored, so the two eras need different commands:
//
// Jellyfin 10.10 and newer -- one jellyfin.db, GUIDs stored as TEXT:
//
//	$ sqlite3 /var/lib/jellyfin/data/jellyfin.db \
//	    "select Id, Path, Type from BaseItems where Path is not null limit 5;"
//
// Id comes back as an ordinary dashed GUID, so paste it in as-is: Parse reads
// it. Note the capitalised Id and Type columns -- EF Core renamed them.
//
// Jellyfin 10.9 and older -- raw SQLite in library.db, GUIDs stored as a
// 16-byte BLOB in .NET byte order:
//
//	$ sqlite3 /var/lib/jellyfin/data/library.db \
//	    "select hex(guid), Path, type from BaseItems where Path is not null limit 5;"
//
// hex(guid) is byte-swapped, so it has to go through FromDotNetHex.
//
// Either way: once one vector matches, every ID in the library will, and an
// in-place migration keeps client caches, favourites and deep links.
func TestKnownItemIDs(t *testing.T) {
	vectors := []struct {
		path     string
		typeName string
		// want is either form: a dashed/dashless GUID as printed by 10.10+,
		// or hex(guid) from a pre-10.10 library.db. dotNetOrder says which.
		want        string
		dotNetOrder bool
	}{
		// 10.10+:
		// {"/media/movies/Arrival (2016)/Arrival (2016).mkv",
		//  "MediaBrowser.Controller.Entities.Movies.Movie",
		//  "f1e2d3c4-b5a6-4798-8a9b-0c1d2e3f4a5b", false},
		//
		// 10.9 and older:
		// {"/media/movies/Arrival (2016)/Arrival (2016).mkv",
		//  "MediaBrowser.Controller.Entities.Movies.Movie",
		//  "...32 hex chars from hex(guid)...", true},
	}
	if len(vectors) == 0 {
		t.Skip("no real vectors yet -- see the comment above; this is the one " +
			"test that must pass before trusting an in-place migration")
	}

	for _, v := range vectors {
		var want GUID
		var err error
		if v.dotNetOrder {
			want, err = FromDotNetHex(v.want)
		} else {
			want, err = Parse(v.want)
		}
		if err != nil {
			t.Fatalf("bad vector %q: %v", v.want, err)
		}

		var matched bool
		for _, enc := range []KeyEncoding{UTF16LE, UTF8} {
			if NewItemID(v.path, v.typeName, "", enc) == want {
				t.Logf("%s matches with encoding %v", v.path, enc)
				matched = true
			}
		}
		if !matched {
			t.Errorf("no encoding reproduces the ID for %s -- the key "+
				"construction in NewItemID is wrong, not just the encoding",
				v.path)
		}
	}
}
