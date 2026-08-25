// Package jfscan walks a media root and turns it into items.
//
// It covers the two library types this build configures: movies and music.
// TV needs the episode-expression half of Emby.Naming, which is not ported
// yet, so a tvshows library scans as loose video files rather than silently
// producing nothing.
//
// The scan is deliberately filesystem-only. No metadata provider, no network,
// no tag reading -- everything here comes from the path, which is what makes it
// fast and reproducible. Titles are therefore only as good as the file layout,
// exactly as they are in Jellyfin before its providers run.
package jfscan

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gofin/internal/jfid"
	"gofin/internal/jfnaming"
)

// Kind is an item's type, named as the client expects to see it in Type.
type Kind string

const (
	KindMovie       Kind = "Movie"
	KindVideo       Kind = "Video"
	KindAudio       Kind = "Audio"
	KindMusicAlbum  Kind = "MusicAlbum"
	KindMusicArtist Kind = "MusicArtist"
	KindFolder      Kind = "Folder"
)

// dotNetTypes maps each kind to the fully-qualified .NET type name upstream
// uses. These strings are part of the item-ID derivation, so they have to be
// carried verbatim even though nothing else in a Go port needs them -- this is
// the mapping table jfid.NewItemID's documentation refers to.
var dotNetTypes = map[Kind]string{
	KindMovie:       "MediaBrowser.Controller.Entities.Movies.Movie",
	KindVideo:       "MediaBrowser.Controller.Entities.Video",
	KindAudio:       "MediaBrowser.Controller.Entities.Audio.Audio",
	KindMusicAlbum:  "MediaBrowser.Controller.Entities.Audio.MusicAlbum",
	KindMusicArtist: "MediaBrowser.Controller.Entities.Audio.MusicArtist",
	KindFolder:      "MediaBrowser.Controller.Entities.Folder",
}

// DotNetType returns the .NET type name behind a kind, for ID derivation.
func DotNetType(k Kind) string { return dotNetTypes[k] }

// Item is one scanned entity. It is intentionally storage-shaped rather than
// wire-shaped: turning it into a client DTO is the server's job.
type Item struct {
	ID       jfid.GUID
	ParentID jfid.GUID
	Kind     Kind
	Name     string
	SortName string
	Path     string
	Year     int
	IsFolder bool

	// Container is the file extension without the dot, e.g. "mkv".
	Container string
	Size      int64
	Modified  time.Time

	// Music-specific. Empty for everything else.
	Album       string
	AlbumArtist string
	IndexNumber int // track number, 0 when unknown

	// AdditionalParts holds the second and later files of a multi-part video.
	// The item's own Path is part one.
	AdditionalParts []string

	// PrimaryImage is a local artwork file, empty when none was found. Only
	// the conventional names are accepted -- see findPrimaryImage.
	PrimaryImage string
}

// Result is one library's scan.
type Result struct {
	Items    []Item
	Errors   []error // per-directory failures; a scan continues past them
	Duration time.Duration
}

// Options configures a scan.
type Options struct {
	// Root is the media directory.
	Root string
	// CollectionType is the library's Jellyfin CollectionType: movies, music,
	// or "" for mixed.
	CollectionType string
	// ParentID is the library's own item ID; top-level items hang off it.
	ParentID jfid.GUID
	// MaxDepth guards against a symlink loop turning a scan into a hang.
	MaxDepth int
}

const defaultMaxDepth = 12

// NewID derives an item's ID the way LibraryManager.GetNewItemId does.
func NewID(path string, kind Kind) jfid.GUID {
	return jfid.NewItemID(path, dotNetTypes[kind], "", jfid.UTF16LE)
}

// Scan walks the root and returns everything found.
//
// It never returns a partial-failure error: a directory that cannot be read is
// recorded in Result.Errors and the walk continues, because one unreadable
// subdirectory should not cost the whole library.
func Scan(opts Options) (Result, error) {
	start := time.Now()
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = defaultMaxDepth
	}

	fi, err := os.Stat(opts.Root)
	if err != nil {
		return Result{}, err
	}
	if !fi.IsDir() {
		return Result{}, errors.New("jfscan: root is not a directory")
	}

	var res Result
	switch strings.ToLower(opts.CollectionType) {
	case "music":
		res = scanMusic(opts)
	default:
		// movies, mixed, and -- until the episode expressions are ported --
		// tvshows all resolve as standalone videos.
		res = scanVideos(opts)
	}

	sort.Slice(res.Items, func(i, j int) bool {
		if res.Items[i].SortName != res.Items[j].SortName {
			return res.Items[i].SortName < res.Items[j].SortName
		}
		return res.Items[i].Path < res.Items[j].Path
	})
	res.Duration = time.Since(start)
	return res, nil
}

// ------------------------------------------------------------------- movies

// scanVideos resolves a movies or mixed library.
//
// Two layouts are supported, which is what upstream's resolvers accept:
//
//	Movies/Arrival (2016).mkv                     a bare file
//	Movies/Arrival (2016)/Arrival (2016).mkv      a folder per film
//
// In the folder form the DIRECTORY name wins the title. That matters because
// the file inside is often a scene release with a far worse name, and because
// it is what keeps extras beside the feature from each becoming a movie.
func scanVideos(opts Options) Result {
	var res Result

	walk(opts, &res, func(dir string, depth int, entries []fs.DirEntry) {
		// An extras directory holds material belonging to the film beside it.
		// Its contents are not movies, and indexing them produces one bogus
		// entry per film, all named after the folder.
		if dir != opts.Root && jfnaming.IsExtrasDir(filepath.Base(dir)) {
			return
		}

		// Collect the video files sitting directly in this directory, dropping
		// trailers and samples that live alongside the feature.
		var files []fs.DirEntry
		for _, e := range entries {
			if e.IsDir() || jfnaming.IsIgnoredFile(e.Name()) {
				continue
			}
			if !jfnaming.IsVideoFile(e.Name()) {
				continue
			}
			if jfnaming.IsExtraFile(e.Name()) {
				continue
			}
			files = append(files, e)
		}
		if len(files) == 0 {
			return
		}

		// A directory below the root normally names the film it contains --
		// the file inside is usually a far worse scene name.
		//
		// But only when it holds ONE feature. A folder with several distinct
		// films (a trilogy set, say) would otherwise yield that many items all
		// sharing the folder's name, so there each file names itself.
		folderTitle := ""
		if dir != opts.Root && countFeatures(dir, files) == 1 {
			folderTitle = dir
		}

		for _, grp := range groupStacks(dir, files) {
			var info jfnaming.Result
			if folderTitle != "" {
				info = jfnaming.Resolve(folderTitle, true)
			} else {
				info = jfnaming.Resolve(grp.primary, false)
			}
			// A stacked group's shared title beats a per-file name, since the
			// per-file name still carries "cd1".
			if folderTitle == "" && grp.title != "" {
				r2 := jfnaming.Resolve(grp.title, false)
				info.Name = r2.Name
				if r2.HasYear {
					info.Year, info.HasYear = r2.Year, true
				}
			}

			it := Item{
				ID:              NewID(grp.primary, KindMovie),
				ParentID:        opts.ParentID,
				Kind:            KindMovie,
				Name:            info.Name,
				SortName:        sortName(info.Name),
				Path:            grp.primary,
				Year:            info.Year,
				Container:       container(grp.primary),
				AdditionalParts: grp.extra,
			}
			it.PrimaryImage = findPrimaryImage(grp.primary, false)
			stat(&it)
			res.Items = append(res.Items, it)
		}
	})

	return res
}

// countFeatures reports how many separate films a directory's files amount to,
// counting a multi-part stack as one.
func countFeatures(dir string, files []fs.DirEntry) int {
	return len(groupStacks(dir, files))
}

type stackGroup struct {
	title   string   // shared title when this is a multi-part video
	primary string   // part one
	extra   []string // parts two and up, in order
}

// groupStacks folds "Movie cd1.mkv" and "Movie cd2.mkv" into one item.
func groupStacks(dir string, files []fs.DirEntry) []stackGroup {
	var out []stackGroup
	byTitle := map[string][]string{}
	var order []string

	for _, f := range files {
		full := filepath.Join(dir, f.Name())
		part, ok := jfnaming.ParseStack(f.Name())
		if !ok {
			out = append(out, stackGroup{primary: full})
			continue
		}
		if _, seen := byTitle[part.Title]; !seen {
			order = append(order, part.Title)
		}
		byTitle[part.Title] = append(byTitle[part.Title], full)
	}

	for _, title := range order {
		parts := byTitle[title]
		sort.Strings(parts)
		out = append(out, stackGroup{title: title, primary: parts[0], extra: parts[1:]})
	}
	return out
}

// -------------------------------------------------------------------- music

// scanMusic resolves the conventional music layout:
//
//	Music/Artist/Album/01 Track.flac
//	Music/Artist/Album/Disc 1/01 Track.flac
//
// Artist and album come from the directory names because nothing here reads
// tags. A file directly under the root, with no artist or album folder, still
// becomes a track -- it is just an unattributed one.
func scanMusic(opts Options) Result {
	var res Result

	albums := map[string]*Item{}
	artists := map[string]*Item{}

	walk(opts, &res, func(dir string, depth int, entries []fs.DirEntry) {
		var files []fs.DirEntry
		for _, e := range entries {
			if e.IsDir() || jfnaming.IsIgnoredFile(e.Name()) {
				continue
			}
			if jfnaming.IsAudioFile(e.Name()) {
				files = append(files, e)
			}
		}
		if len(files) == 0 {
			return
		}

		artistDir, albumDir := musicParents(opts.Root, dir)

		parent := opts.ParentID
		var artistItem *Item
		if artistDir != "" {
			artistItem = artists[artistDir]
			if artistItem == nil {
				name := jfnaming.Resolve(artistDir, true).Name
				it := Item{
					ID:       NewID(artistDir, KindMusicArtist),
					ParentID: opts.ParentID,
					Kind:     KindMusicArtist,
					Name:     name,
					SortName: sortName(name),
					Path:     artistDir,
					IsFolder: true,
				}
				stat(&it)
				artists[artistDir] = &it
				artistItem = &it
			}
			parent = artistItem.ID
		}

		var albumItem *Item
		if albumDir != "" {
			albumItem = albums[albumDir]
			if albumItem == nil {
				info := jfnaming.Resolve(albumDir, true)
				it := Item{
					ID:       NewID(albumDir, KindMusicAlbum),
					ParentID: parent,
					Kind:     KindMusicAlbum,
					Name:     info.Name,
					SortName: sortName(info.Name),
					Path:     albumDir,
					Year:     info.Year,
					IsFolder: true,
				}
				if artistItem != nil {
					it.AlbumArtist = artistItem.Name
				}
				it.PrimaryImage = findPrimaryImage(albumDir, true)
				stat(&it)
				albums[albumDir] = &it
				albumItem = &it
			}
			parent = albumItem.ID
		}

		for _, f := range files {
			full := filepath.Join(dir, f.Name())
			track, title := trackNumber(f.Name())
			it := Item{
				ID:          NewID(full, KindAudio),
				ParentID:    parent,
				Kind:        KindAudio,
				Name:        title,
				SortName:    sortName(title),
				Path:        full,
				Container:   container(full),
				IndexNumber: track,
			}
			if albumItem != nil {
				it.Album = albumItem.Name
			}
			if artistItem != nil {
				it.AlbumArtist = artistItem.Name
			}
			// Tracks sort by disc position, not alphabetically: a numbered
			// album played in name order is the wrong album.
			if track > 0 {
				it.SortName = padNumber(track) + " " + it.SortName
			}
			stat(&it)
			res.Items = append(res.Items, it)
		}
	})

	for _, a := range artists {
		res.Items = append(res.Items, *a)
	}
	for _, a := range albums {
		res.Items = append(res.Items, *a)
	}
	return res
}

// musicParents works out which ancestor directories are the artist and the
// album, tolerating an extra "Disc 1" level between album and tracks.
func musicParents(root, dir string) (artistDir, albumDir string) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return "", ""
	}
	parts := strings.Split(rel, string(filepath.Separator))

	// Drop a trailing disc directory: it is a subdivision of the album, not an
	// album of its own.
	if len(parts) > 1 && isDiscDir(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}

	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		// Music/Album/track.flac -- an album with no artist directory.
		return "", filepath.Join(root, parts[0])
	default:
		return filepath.Join(root, parts[0]),
			filepath.Join(root, filepath.Join(parts[:2]...))
	}
}

var discPrefixes = []string{"disc", "disk", "cd", "volume", "vol"}

func isDiscDir(name string) bool {
	l := strings.ToLower(strings.TrimSpace(name))
	for _, p := range discPrefixes {
		if !strings.HasPrefix(l, p) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(l, p))
		rest = strings.TrimLeft(rest, ".-_ ")
		if rest == "" {
			continue
		}
		if _, err := strconv.Atoi(rest); err == nil {
			return true
		}
	}
	return false
}

// trackNumber pulls a leading track number off a filename: "01 Track.flac" and
// "01 - Track.flac" both give 1 and "Track".
func trackNumber(filename string) (int, string) {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))

	i := 0
	for i < len(base) && base[i] >= '0' && base[i] <= '9' {
		i++
	}
	// Two to three leading digits is a track number; more is part of the title.
	if i == 0 || i > 3 {
		return 0, jfnaming.Resolve(filename, false).Name
	}
	n, err := strconv.Atoi(base[:i])
	if err != nil {
		return 0, jfnaming.Resolve(filename, false).Name
	}

	rest := strings.TrimLeft(base[i:], " -_.")
	if rest == "" {
		// The whole name was the number; keep it as the title.
		return n, base
	}
	return n, strings.TrimSpace(rest)
}

func padNumber(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

// --------------------------------------------------------------------- walk

// walk descends the tree, applying the ignore rules and the depth cap, and
// hands each directory's entries to fn.
func walk(opts Options, res *Result, fn func(dir string, depth int, entries []fs.DirEntry)) {
	type frame struct {
		path  string
		depth int
	}
	stack := []frame{{opts.Root, 0}}

	// Guard against symlink loops: a directory already visited is not
	// descended into again.
	seen := map[string]bool{}

	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		real, err := filepath.EvalSymlinks(cur.path)
		if err != nil {
			real = cur.path
		}
		if seen[real] {
			continue
		}
		seen[real] = true

		entries, err := os.ReadDir(cur.path)
		if err != nil {
			res.Errors = append(res.Errors, err)
			continue
		}

		fn(cur.path, cur.depth, entries)

		if cur.depth >= opts.MaxDepth {
			continue
		}
		for _, e := range entries {
			if jfnaming.IsIgnoredDir(e.Name()) {
				continue
			}
			full := filepath.Join(cur.path, e.Name())
			if !isDir(e, full) {
				continue
			}
			stack = append(stack, frame{full, cur.depth + 1})
		}
	}
}

// isDir reports whether an entry is a directory, following symlinks.
//
// os.ReadDir does not follow them -- it reports a link as a link -- so testing
// DirEntry.IsDir alone drops the whole subtree behind a symlinked film folder,
// and the link itself has no extension to be picked up as a video file either.
// Libraries assembled out of links to another volume are common enough that
// losing them silently is not acceptable. Following them is safe here because
// the seen-set above is keyed on the resolved path.
func isDir(e fs.DirEntry, full string) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&fs.ModeSymlink == 0 {
		return false
	}
	fi, err := os.Stat(full)
	return err == nil && fi.IsDir()
}

// ------------------------------------------------------------------- artwork

// artworkNames are the conventional local poster filenames, in preference
// order, matching what Jellyfin's local image provider looks for.
//
// The list is deliberately exact rather than "any .jpg in the folder": release
// groups drop advertising banners beside the media (WWW.YTS.AG.jpg and
// friends), and showing one of those as a film's poster is worse than showing
// no poster at all.
var artworkNames = []string{
	"poster", "folder", "cover", "default", "movie", "front", "album",
}

var artworkExts = []string{".jpg", ".jpeg", ".png", ".webp"}

// findPrimaryImage looks for artwork belonging to an item.
//
// It checks <dir>/<conventional name>.<ext> first, then the sidecar form
// <media file>-poster.<ext> that a bare-file layout uses.
func findPrimaryImage(mediaPath string, isFolder bool) string {
	dir := mediaPath
	if !isFolder {
		dir = filepath.Dir(mediaPath)
	}

	for _, name := range artworkNames {
		for _, e := range artworkExts {
			if p, ok := existsCaseInsensitive(dir, name+e); ok {
				return p
			}
		}
	}

	if !isFolder {
		stem := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
		for _, suffix := range []string{"-poster", "-thumb", ""} {
			for _, e := range artworkExts {
				if p, ok := existsCaseInsensitive(dir, stem+suffix+e); ok {
					return p
				}
			}
		}
	}
	return ""
}

// existsCaseInsensitive finds a file ignoring case, because the conventional
// names appear capitalised in the wild and most media lives on case-sensitive
// filesystems.
func existsCaseInsensitive(dir, name string) (string, bool) {
	if p := filepath.Join(dir, name); fileExists(p) {
		return p, true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(e.Name(), name) {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// -------------------------------------------------------------------- helpers

func stat(it *Item) {
	fi, err := os.Stat(it.Path)
	if err != nil {
		return
	}
	it.Modified = fi.ModTime().UTC()
	if !fi.IsDir() {
		it.Size = fi.Size()
	}
}

func container(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

// sortName lowercases and drops a leading article, which is how Jellyfin files
// "The Matrix" under M.
func sortName(name string) string {
	l := strings.ToLower(strings.TrimSpace(name))
	for _, a := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(l, a) {
			return strings.TrimPrefix(l, a)
		}
	}
	return l
}
