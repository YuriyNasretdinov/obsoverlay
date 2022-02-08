package main_test

import (
	"testing"

	"github.com/mxmCherry/translit/ruicao"
	"github.com/pemistahl/lingua-go"
	"golang.org/x/text/transform"
)

func TestTranslit(t *testing.T) {
	ru := ruicao.ToLatin()
	phrase := "Сам комрадс толк лайк зыс ин зе чат"

	want := "Sam komrads tolk laik zys in ze chat"
	got, _, _ := transform.String(ru.Transformer(), phrase)

	if got != want {
		t.Errorf("Transliterate(%q) = %q, want %q", phrase, got, want)
	}
}

func TestLangDetect(t *testing.T) {
	languages := []lingua.Language{
		lingua.English,
		lingua.French,
		lingua.German,
		lingua.Spanish,
		lingua.Russian,
	}

	detector := lingua.NewLanguageDetectorBuilder().
		FromLanguages(languages...).
		Build()

	testCases := []struct {
		phrase string
		want   lingua.Language
	}{
		{phrase: "Good comrads speak English on English streams despite the accent", want: lingua.English},
		{phrase: "Сам комрадс толк лайк зыс ин зе чат", want: lingua.Russian},
		{phrase: "Некоторые другие товарищи пишут просто по-русски", want: lingua.Russian},
		{phrase: "Ozer comrads cen spik laik thet az vell", want: lingua.English},
	}

	for _, tc := range testCases {
		got, exists := detector.DetectLanguageOf(tc.phrase)
		if !exists {
			t.Errorf("Language must exist for phrase %q", tc.phrase)
		} else if got != tc.want {
			t.Errorf("LanguageOf(%q) = %v, want %v", tc.phrase, got, tc.want)
		}
	}
}
