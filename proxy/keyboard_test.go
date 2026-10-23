package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestKeystrokes(t *testing.T) {
	space := keyStroke{keySpace, false}
	for _, tc := range []struct {
		text string
		want []keyStroke
	}{
		{"aZ", []keyStroke{{30, false}, {44, true}}},
		{"1!", []keyStroke{{2, false}, {2, true}}},
		{"&*", []keyStroke{{8, true}, {9, true}}},
		{"|\\", []keyStroke{{43, true}, {43, false}}},
		{"\"'", []keyStroke{{40, true}, {40, false}}},
		{"a b", []keyStroke{{30, false}, space, {48, false}}},
		{"\n\u2028", []keyStroke{{keyEnter, false}, {keyEnter, true}}},
		// Dead keys: accent + letter, or accent + space for the accent itself.
		{"ć", []keyStroke{deadAcute, {46, false}}},
		{"Ś", []keyStroke{deadAcute, {31, true}}},
		{"ö", []keyStroke{deadDiaeresis, {24, false}}},
		{"ñ", []keyStroke{deadTilde, {49, false}}},
		{"^", []keyStroke{deadCircumflex, space}},
		{"~`", []keyStroke{deadTilde, space, deadGrave, space}},
	} {
		got, err := keystrokes(tc.text)
		if err != nil {
			t.Errorf("%q: %v", tc.text, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestKeystrokesRejectsUntypeable(t *testing.T) {
	// The Type Folio US layout has no brackets; Polish ą ę ł ż have no dead key.
	for _, s := range []string{"[", "}", "ł", "ą", "ż", "\t", "€"} {
		if _, err := keystrokes("ok" + s); err == nil {
			t.Errorf("%q: typed, want an error", s)
		}
	}
}

func TestTypeableChars(t *testing.T) {
	chars := typeableChars()
	for _, r := range "aZ09 !?.,;:'\"-_=+/\\|<>()@#$%&*^~`ćńóśźéäüâ" {
		if !strings.ContainsRune(chars, r) {
			t.Errorf("%q missing from typeableChars", r)
		}
	}
	for _, r := range "[]{}\n\u2028ł" {
		if strings.ContainsRune(chars, r) {
			t.Errorf("%q in typeableChars", r)
		}
	}
	if _, err := keystrokes(chars); err != nil {
		t.Errorf("typeableChars cannot be typed: %v", err)
	}
}
