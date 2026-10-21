package main

import (
	"errors"
	"strings"
	"unicode"
)

var nameLigatures = strings.NewReplacer("æ", "ae", "œ", "oe", "ß", "ss", "þ", "th")

// normalizeName produces one ASCII DNS label. Latin accents are folded;
// punctuation and unsupported characters become word separators.
func normalizeName(name string) (string, error) {
	name = nameLigatures.Replace(strings.ToLower(name))
	var slug strings.Builder
	separator := false
	for _, r := range name {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
			continue
		}
		for _, group := range []struct {
			letters string
			base    rune
		}{
			{"àáâãäåāăą", 'a'}, {"çćĉċč", 'c'}, {"ďđ", 'd'},
			{"èéêëēĕėęě", 'e'}, {"ĝğġģ", 'g'}, {"ĥħ", 'h'},
			{"ìíîïĩīĭįı", 'i'}, {"ĵ", 'j'}, {"ķ", 'k'},
			{"ĺļľŀł", 'l'}, {"ñńņňŋ", 'n'}, {"òóôõöøōŏő", 'o'},
			{"ŕŗř", 'r'}, {"śŝşš", 's'}, {"ţťŧ", 't'},
			{"ùúûüũūŭůűų", 'u'}, {"ŵ", 'w'}, {"ýÿŷ", 'y'}, {"źżž", 'z'},
		} {
			if strings.ContainsRune(group.letters, r) {
				r = group.base
				break
			}
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if separator {
				slug.WriteByte('-')
			}
			slug.WriteRune(r)
			separator = false
			if slug.Len() > 63 {
				return "", errors.New("name must produce a subdomain of at most 63 characters")
			}
		} else if slug.Len() > 0 {
			separator = true
		}
	}
	if slug.Len() == 0 {
		return "", errors.New("name must contain at least one Latin letter or digit")
	}
	return slug.String(), nil
}
