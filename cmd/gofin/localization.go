package main

import (
	_ "embed"
	"strings"
	"sync"
)

// The language list behind the web interface's audio and subtitle language
// pickers.
//
// iso6392.txt is upstream's own table, copied unchanged from
// $JF/Emby.Server.Implementations/Localization/iso6392.txt, and parsed by the
// rules of LocalizationManager.LoadCultures. Each line is
//
//	terminology code | bibliographic code | two-letter code | English | French
//
// To update it, copy the file again.

//go:embed iso6392.txt
var iso6392 string

// CultureDto is MediaBrowser.Model.Globalization.CultureDto.
type CultureDto struct {
	Name                     string `json:"Name"`
	DisplayName              string `json:"DisplayName"`
	TwoLetterISOLanguageName string `json:"TwoLetterISOLanguageName"`
	// The first of the three-letter names, or null when there are none.
	ThreeLetterISOLanguageName  *string  `json:"ThreeLetterISOLanguageName"`
	ThreeLetterISOLanguageNames []string `json:"ThreeLetterISOLanguageNames"`
}

var cultures = sync.OnceValue(func() []CultureDto {
	var out []CultureDto
	for _, line := range strings.Split(iso6392, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 5 || strings.TrimSpace(parts[3]) == "" {
			continue
		}
		name, display, two := parts[3], parts[3], strings.TrimSpace(parts[2])
		// A regional two-letter code (pt-BR, zh-TW) is the culture's name.
		if strings.Contains(two, "-") {
			name = two
		}
		// With two three-letter codes, the terminology code comes first and
		// the bibliographic one second.
		three := []string{parts[0]}
		if strings.TrimSpace(parts[1]) != "" {
			three = append(three, parts[1])
		}
		out = append(out, CultureDto{
			Name: name, DisplayName: display, TwoLetterISOLanguageName: two,
			ThreeLetterISOLanguageName: &three[0], ThreeLetterISOLanguageNames: three,
		})
	}
	return out
})
