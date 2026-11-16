// Package jfnaming is a Go port of Emby.Naming, the library Jellyfin uses to
// turn a file path into a title, a year, and a media kind.
//
// # Why this is not a regex translation
//
// Upstream is built almost entirely out of .NET regular expressions. Go's
// regexp package is RE2, which has no backtracking and therefore no lookahead
// and no lookbehind -- and the upstream expressions depend on both:
//
//	(?![0-9]+|\W[0-9]{2}\W[0-9]{2})   negative lookahead, in every CleanDateTime
//	(?<=[\]\)\}])                     lookbehind, in the stacking rules
//
// regexp.MustCompile panics on those, so a copy-paste port does not merely
// misbehave, it fails to start. The functions here reproduce the *semantics*
// by hand, and the upstream expression each one implements is quoted above it
// so the two can be diffed by eye when upstream changes.
//
// The one thing not reproduced is .NET's backtracking search order. Where it
// matters -- CleanDateTime's greedy leading group, which makes the RIGHTMOST
// year win -- that ordering is implemented explicitly and tested.
package jfnaming

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- extensions

// videoExtensions is NamingOptions.VideoFileExtensions, verbatim.
var videoExtensions = newExtSet(
	".001", ".3g2", ".3gp", ".amv", ".asf", ".asx", ".avi", ".bin", ".bivx",
	".divx", ".dv", ".dvr-ms", ".f4v", ".fli", ".flv", ".ifo", ".img", ".iso",
	".m2t", ".m2ts", ".m2v", ".m4v", ".mkv", ".mk3d", ".mov", ".mp4", ".mpe",
	".mpeg", ".mpg", ".mts", ".mxf", ".nrg", ".nsv", ".nuv", ".ogm", ".ogv",
	".pva", ".qt", ".rec", ".rm", ".rmvb", ".strm", ".svq3", ".tp", ".ts",
	".ty", ".viv", ".vob", ".vp3", ".webm", ".wmv", ".wtv", ".xvid",
)

// audioExtensions is NamingOptions.AudioFileExtensions, verbatim.
var audioExtensions = newExtSet(
	".669", ".3gp", ".aa", ".aac", ".aax", ".ac3", ".act", ".adp", ".adplug",
	".adx", ".afc", ".amf", ".aif", ".aifc", ".aiff", ".alac", ".amr", ".ape",
	".ast", ".au", ".awb", ".cda", ".cue", ".dmf", ".dsf", ".dsm", ".dsp",
	".dts", ".dvf", ".eac3", ".ec3", ".far", ".flac", ".gdm", ".gsm", ".gym",
	".hps", ".imf", ".it", ".m15", ".m4a", ".m4b", ".mac", ".med", ".mka",
	".mmf", ".mod", ".mogg", ".mp2", ".mp3", ".mpa", ".mpc", ".mpp", ".mp+",
	".msv", ".nmf", ".nsf", ".nsv", ".oga", ".ogg", ".okt", ".opus", ".pls",
	".ra", ".rf64", ".rm", ".s3m", ".sfx", ".shn", ".sid", ".stm", ".strm",
	".ult", ".uni", ".vox", ".wav", ".wma", ".wv", ".xm", ".xsp", ".ymf",
)

func newExtSet(exts ...string) map[string]bool {
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		m[e] = true
	}
	return m
}

// ext returns the lowercased extension, including the dot.
func ext(path string) string { return strings.ToLower(filepath.Ext(path)) }

// IsVideoFile reports whether the extension is a known video container.
func IsVideoFile(path string) bool { return videoExtensions[ext(path)] }

// IsAudioFile reports whether the extension is a known audio container.
func IsAudioFile(path string) bool { return audioExtensions[ext(path)] }

// IsMediaFile reports whether the file is either.
//
// Note that a handful of extensions -- .3gp, .rm, .nsv, .strm -- appear in both
// upstream tables. Video wins, matching the order upstream resolvers run in.
func IsMediaFile(path string) bool { return IsVideoFile(path) || IsAudioFile(path) }

// ------------------------------------------------------------- CleanDateTime

// Result is the outcome of parsing a file or directory name.
type Result struct {
	Name    string // the title with year and junk removed
	Year    int    // 0 when no year was found
	HasYear bool
}

// Separator classes lifted from the two CleanDateTimes expressions. They differ
// only in whether a space counts as a separator.
const (
	// [_\.\(\)\[\]\-] -- expression 1, exactly one character.
	dateSepStrict = "_.()[]-"
	// [ _\.\(\)\[\]\-]+ -- expression 2, one or more, space included.
	dateSepLoose = " _.()[]-"
	// [^_\,\.\(\)\[\]\-] -- the character before the separator may not be one
	// of these, which stops "Movie - (2016)" from yielding the name "Movie -".
	nameMayNotEndWith = "_,.()[]-"
)

// CleanDateTime splits a trailing year off a name.
//
// It implements NamingOptions.CleanDateTimes:
//
//	(.+[^_\,\.\(\)\[\]\-])[_\.\(\)\[\]\-](19[0-9]{2}|20[0-9]{2})(?![0-9]+|\W[0-9]{2}\W[0-9]{2})([ _\,\.\(\)\[\]\-][^0-9]|).*(19[0-9]{2}|20[0-9]{2})*
//	(.+[^_\,\.\(\)\[\]\-])[ _\.\(\)\[\]\-]+(19[0-9]{2}|20[0-9]{2})(?![0-9]+|\W[0-9]{2}\W[0-9]{2})([ _\,\.\(\)\[\]\-][^0-9]|).*(19[0-9]{2}|20[0-9]{2})*
//
// The trailing groups cannot fail -- the third alternates with empty and the
// fourth is starred -- so the only real constraints are the leading group, the
// separator, and the negative lookahead. Upstream returns on the first
// expression that matches, so strict is tried before loose.
//
// The leading (.+) is GREEDY, which means the rightmost qualifying year wins.
// That is what makes "2001 A Space Odyssey (1968)" resolve to the 1968 film and
// not to a movie called "" made in 2001.
func CleanDateTime(name string) Result {
	if name == "" {
		return Result{Name: name}
	}
	for _, loose := range []bool{false, true} {
		if r, ok := cleanDateTime(name, loose); ok {
			return r
		}
	}
	return Result{Name: name}
}

func cleanDateTime(name string, loose bool) (Result, bool) {
	sepClass := dateSepStrict
	if loose {
		sepClass = dateSepLoose
	}

	// Walk candidate year positions right to left: the greedy leading group
	// makes the last qualifying year the one .NET settles on.
	for i := len(name) - 4; i >= 1; i-- {
		year, ok := yearAt(name, i)
		if !ok {
			continue
		}
		// (?![0-9]+|\W[0-9]{2}\W[0-9]{2})
		if !lookaheadOK(name, i+4) {
			continue
		}

		// Consume the separator run immediately before the year. The strict
		// expression takes exactly one character, the loose one takes as many
		// as are there.
		j := i
		for j > 0 && strings.ContainsRune(sepClass, rune(name[j-1])) {
			j--
			if !loose {
				break
			}
		}
		if j == i {
			continue // no separator at all
		}

		// (.+[^_\,\.\(\)\[\]\-]) -- at least one character, not ending in a
		// separator or comma.
		if j < 1 {
			continue
		}
		if strings.ContainsRune(nameMayNotEndWith, rune(name[j-1])) {
			continue
		}

		return Result{
			Name:    strings.TrimRight(name[:j], " \t\n\v\f\r"),
			Year:    year,
			HasYear: true,
		}, true
	}
	return Result{}, false
}

// yearAt reports whether a four-digit 19xx or 20xx year starts at i.
func yearAt(s string, i int) (int, bool) {
	if i < 0 || i+4 > len(s) {
		return 0, false
	}
	if s[i] != '1' && s[i] != '2' {
		return 0, false
	}
	if s[i:i+2] != "19" && s[i:i+2] != "20" {
		return 0, false
	}
	for k := i + 2; k < i+4; k++ {
		if s[k] < '0' || s[k] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s[i : i+4])
	if err != nil {
		return 0, false
	}
	return n, true
}

// lookaheadOK implements (?![0-9]+|\W[0-9]{2}\W[0-9]{2}) at position i.
//
// The first alternative rejects a longer number, so 20161 is not the year 2016.
// The second rejects a date, so 2016.01.02 is not the year 2016 either -- that
// pattern is a recording timestamp, and treating it as a release year renames
// the item.
func lookaheadOK(s string, i int) bool {
	if i < len(s) && isDigit(s[i]) {
		return false
	}
	// \W[0-9]{2}\W[0-9]{2}
	if i+6 <= len(s) &&
		isNonWord(s[i]) && isDigit(s[i+1]) && isDigit(s[i+2]) &&
		isNonWord(s[i+3]) && isDigit(s[i+4]) && isDigit(s[i+5]) {
		return false
	}
	return true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isNonWord is \W: anything outside [A-Za-z0-9_].
func isNonWord(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_':
		return false
	}
	return true
}

// --------------------------------------------------------------- CleanString

// The release-tag vocabulary from the first CleanStrings expression, verbatim.
const releaseTags = `3d|sbs|tab|hsbs|htab|mvc|HDR|HDC|UHD|UltraHD|4k|ac3|dts|` +
	`custom|dc|divx|divx5|dsr|dsrip|dutch|dvd|dvdrip|dvdscr|dvdscreener|` +
	`screener|dvdivx|cam|fragment|fs|hdtv|hdrip|hdtvrip|internal|limited|multi|` +
	`subs|ntsc|ogg|ogm|pal|pdtv|proper|repack|rerip|retail|cd[1-9]|r5|bd5|bd|se|` +
	`svcd|swedish|german|read.nfo|nfofix|unrated|ws|web-dl|telesync|ts|telecine|` +
	`tc|brrip|bdrip|480p|480i|576p|576i|720p|720i|1080p|1080i|2160p|hrhd|hrhdtv|` +
	`hddvd|bluray|blu-ray|x264|x265|h264|h265|xvid|xvidvd|xxx|www.www|AAC|DTS`

// Each of these mirrors one NamingOptions.CleanStrings expression. Where the
// original used a lookahead it has been rewritten to consume the character
// instead -- harmless, because nothing after it contributes to the "cleaned"
// group -- or split out into a Go-side check.
var (
	// ^\s*(?<cleaned>.+?)[ _\,\.\(\)\[\]\-](tags)(?=[ _\,\.\(\)\[\]\-]|$)
	// The trailing lookahead becomes a consuming alternation.
	cleanTags = regexp.MustCompile(`^\s*(?P<cleaned>.+?)[ _,.()\[\]-](?i:` + releaseTags + `)(?:[ _,.()\[\]-]|$)`)

	// ^\s*(?<cleaned>.+?)((\s*\[[^\]]+\]\s*)+)(\.[^\s]+)?$
	cleanTrailingBrackets = regexp.MustCompile(`^\s*(?P<cleaned>.+?)(?:\s*\[[^\]]+\]\s*)+(?:\.[^\s]+)?$`)

	// ^\s*(?<cleaned>.+?)\WE[0-9]+(-|~)E?[0-9]+(\W|$)
	cleanEpisodeRange = regexp.MustCompile(`^\s*(?P<cleaned>.+?)\WE[0-9]+(?:-|~)E?[0-9]+(?:\W|$)`)

	// ^\s*\[[^\]]+\](?!\.\w+$)\s*(?<cleaned>.+)
	// The negative lookahead is checked in Go; see cleanLeadingBracket.
	leadingBracket    = regexp.MustCompile(`^\s*\[[^\]]+\]\s*(?P<cleaned>.+)$`)
	leadingBracketExt = regexp.MustCompile(`^\.\w+$`)

	// ^\s*(?<cleaned>.+?)\s+-\s+[0-9]+\s*$
	cleanTrailingNumber = regexp.MustCompile(`^\s*(?P<cleaned>.+?)\s+-\s+[0-9]+\s*$`)

	// ^\s*(?<cleaned>.+?)(([-._ ](trailer|sample))|-(scene|clip|behindthescenes|deleted|deletedscene|featurette|short|interview|other|extra))$
	cleanExtraSuffix = regexp.MustCompile(`^\s*(?P<cleaned>.+?)(?:(?:[-._ ](?i:trailer|sample))|-(?i:scene|clip|behindthescenes|deleted|deletedscene|featurette|short|interview|other|extra))$`)
)

// CleanString strips release tags and other clutter from a name.
//
// Upstream applies every expression in order, feeding each result into the
// next, and reports success if ANY of them matched -- see CleanStringParser.
// The iteration is what removes several tags in sequence from a scene release
// like Movie.2016.1080p.BluRay.x264.
func CleanString(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	cleaned := false
	cur := name

	for _, step := range []func(string) (string, bool){
		func(s string) (string, bool) { return submatch(cleanTags, s) },
		func(s string) (string, bool) { return submatch(cleanTrailingBrackets, s) },
		func(s string) (string, bool) { return submatch(cleanEpisodeRange, s) },
		cleanLeadingBracket,
		func(s string) (string, bool) { return submatch(cleanTrailingNumber, s) },
		func(s string) (string, bool) { return submatch(cleanExtraSuffix, s) },
	} {
		if out, ok := step(cur); ok {
			cleaned = true
			cur = out
		}
	}

	if !cleaned {
		return "", false
	}
	return cur, true
}

func submatch(re *regexp.Regexp, s string) (string, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	i := re.SubexpIndex("cleaned")
	if i < 0 || i >= len(m) {
		return "", false
	}
	return strings.TrimSpace(m[i]), true
}

// cleanLeadingBracket implements the one expression whose lookahead cannot be
// folded into RE2:
//
//	^\s*\[[^\]]+\](?!\.\w+$)\s*(?<cleaned>.+)
//
// It strips a leading [group] tag, but only when what follows is not just a
// file extension -- otherwise "[foo].mkv" would clean to "mkv".
func cleanLeadingBracket(s string) (string, bool) {
	m := leadingBracket.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	rest := s[len(m[0])-len(m[leadingBracket.SubexpIndex("cleaned")]):]
	if leadingBracketExt.MatchString(rest) {
		return "", false
	}
	return strings.TrimSpace(m[leadingBracket.SubexpIndex("cleaned")]), true
}

// ------------------------------------------------------------------- resolve

// Resolve turns a path into a title and year, mirroring VideoResolver.Resolve:
// take the filename without extension, run CleanDateTime, then CleanString.
//
// isDir selects whether the extension is stripped -- a directory named
// "Movie 4.4" must not lose its ".4".
func Resolve(path string, isDir bool) Result {
	// Keep the untouched base name: it is the last-resort title, and stripping
	// the extension can leave nothing behind. filepath.Ext(".mkv") is ".mkv",
	// so a dotfile trims down to the empty string.
	orig := filepath.Base(path)

	base := orig
	if !isDir {
		base = strings.TrimSuffix(base, filepath.Ext(base))
	}

	r := CleanDateTime(base)
	if cleaned, ok := CleanString(r.Name); ok {
		r.Name = cleaned
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		// Never return an empty title; a file called "1999.mkv" is better shown
		// under its raw name than as a blank row.
		r.Name = orig
	}
	return r
}

// -------------------------------------------------------------------- stacks

// stackRules implement NamingOptions.VideoFileStackingRules:
//
//	^(?<filename>.*?)(?:(?<=[\]\)\}])|[ _.-]+)[\(\[]?(?<parttype>cd|dvd|part|pt|dis[ck])[ _.-]*(?<number>[0-9]+)[\)\]]?(?:\.[^.]+)?$
//	^(?<filename>.*?)(?:(?<=[\]\)\}])|[ _.-]+)[\(\[]?(?<parttype>cd|dvd|part|pt|dis[ck])[ _.-]*(?<number>[a-d])[\)\]]?(?:\.[^.]+)?$
//
// The (?<=[\]\)\}]) lookbehind has no RE2 equivalent, so each rule is split in
// two: one variant where a closing bracket ends the title and is kept, and one
// where a run of separators ends it and is dropped.
var stackRules = []*regexp.Regexp{
	regexp.MustCompile(`^(?P<filename>.*[\]\)\}])[\(\[]?(?i:cd|dvd|part|pt|dis[ck])[ _.-]*(?P<number>[0-9]+)[\)\]]?(?:\.[^.]+)?$`),
	regexp.MustCompile(`^(?P<filename>.*?)[ _.-]+[\(\[]?(?i:cd|dvd|part|pt|dis[ck])[ _.-]*(?P<number>[0-9]+)[\)\]]?(?:\.[^.]+)?$`),
	regexp.MustCompile(`^(?P<filename>.*[\]\)\}])[\(\[]?(?i:cd|dvd|part|pt|dis[ck])[ _.-]*(?P<number>[a-dA-D])[\)\]]?(?:\.[^.]+)?$`),
	regexp.MustCompile(`^(?P<filename>.*?)[ _.-]+[\(\[]?(?i:cd|dvd|part|pt|dis[ck])[ _.-]*(?P<number>[a-dA-D])[\)\]]?(?:\.[^.]+)?$`),
}

// StackPart describes one file of a multi-part video, e.g. "Movie cd1.mkv".
type StackPart struct {
	Title  string // the shared title, "Movie"
	Number string // the part, "1" or "a"
}

// ParseStack reports whether a filename is one part of a multi-part video.
//
// Grouping the parts is the caller's job: files whose Title matches belong to
// one item, which is what stops a two-CD film showing up as two movies.
func ParseStack(filename string) (StackPart, bool) {
	for _, re := range stackRules {
		m := re.FindStringSubmatch(filename)
		if m == nil {
			continue
		}
		title := strings.TrimSpace(m[re.SubexpIndex("filename")])
		if title == "" {
			continue
		}
		return StackPart{
			Title:  title,
			Number: strings.ToLower(m[re.SubexpIndex("number")]),
		}, true
	}
	return StackPart{}, false
}

// -------------------------------------------------------------------- ignore

// ignoredNames are directory and file names skipped outright, from
// Emby.Server.Implementations/Library/IgnorePatterns.cs.
var ignoredNames = map[string]bool{
	"metadata":                  true,
	"ps3_update":                true,
	"ps3_vprm":                  true,
	"extrafanart":               true,
	"extrathumbs":               true,
	".actors":                   true,
	".wd_tv":                    true,
	"lost+found":                true,
	"sample":                    true,
	"minta":                     true,
	"@eadir":                    true, // Synology thumbnail store
	".@__thumb":                 true,
	"$recycle.bin":              true,
	"system volume information": true,
	".grab":                     true,
	"#recycle":                  true,
}

// ignoredFiles are exact filenames skipped, likewise from upstream.
var ignoredFiles = map[string]bool{
	"small.jpg":    true,
	"albumart.jpg": true,
	"desktop.ini":  true,
	"thumbs.db":    true,
	".ds_store":    true,
}

// sampleRe matches upstream's sample.??? / *.sample.??? globs: a "sample" stem,
// or a name ending in ".sample", with a 1-5 character extension.
var sampleRe = regexp.MustCompile(`(?i)(^|\.)sample$`)

// extrasDirs are the subdirectory names whose contents are extras belonging to
// the film beside them, not films of their own. Taken from the folder names in
// NamingOptions.VideoExtraRules (aggregated upstream as
// AllExtrasTypesFolderNames).
//
// Skipping these matters more than it sounds: a library where every film ships
// a Featurettes folder otherwise gains one bogus movie per film, all sharing
// the title "Featurettes".
var extrasDirs = map[string]bool{
	"backdrops":         true,
	"behind the scenes": true,
	"clips":             true,
	"deleted scenes":    true,
	"extra":             true,
	"extras":            true,
	"featurettes":       true,
	"interviews":        true,
	"other":             true,
	"sample":            true,
	"samples":           true,
	"scenes":            true,
	"shorts":            true,
	"theme":             true,
	"trailer":           true,
	"trailers":          true,
	"specials":          true,
	"theme-music":       true,
	"soundtracks":       true,
}

// IsExtrasDir reports whether a directory holds extras rather than features.
func IsExtrasDir(name string) bool {
	return extrasDirs[strings.ToLower(strings.TrimSpace(name))]
}

// extraSuffix matches the per-file extra markers from the last CleanStrings
// expression -- "Movie-trailer.mkv", "Movie_Trailer.mkv".
var extraSuffix = regexp.MustCompile(`(?i)(?:[-._ ](?:trailer|sample)|-(?:scene|clip|behindthescenes|deleted|deletedscene|featurette|short|interview|other|extra))$`)

// IsExtraFile reports whether a file is an extra by its name alone.
//
// This is how a trailer sitting in the same folder as the feature is kept from
// becoming a second copy of the film.
func IsExtraFile(name string) bool {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	return extraSuffix.MatchString(stem)
}

// IsIgnoredDir reports whether a directory should not be descended into.
func IsIgnoredDir(name string) bool {
	l := strings.ToLower(name)
	if ignoredNames[l] {
		return true
	}
	// Hidden directories are skipped, which also covers .git, .cache and the
	// Trash directories most desktops create.
	return strings.HasPrefix(name, ".")
}

// IsIgnoredFile reports whether a file should be skipped.
//
// Sample clips are the important case: they sit beside the feature, are a
// valid video file by extension, and would otherwise be indexed as a second
// movie with almost the same name.
func IsIgnoredFile(name string) bool {
	l := strings.ToLower(name)
	if ignoredFiles[l] {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	stem := strings.TrimSuffix(l, filepath.Ext(l))
	if e := filepath.Ext(l); len(e) >= 2 && len(e) <= 6 {
		if sampleRe.MatchString(stem) {
			return true
		}
	}
	return false
}
