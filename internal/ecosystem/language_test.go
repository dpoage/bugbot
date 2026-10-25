package ecosystem

import (
	"testing"

	"github.com/dpoage/bugbot/internal/ingest"
)

func TestFromLanguage(t *testing.T) {
	cases := []struct {
		lang ingest.Language
		want Ecosystem
	}{
		{ingest.LangGo, EcosystemGo},
		{ingest.LangPython, EcosystemPython},
		{ingest.LangJavaScript, EcosystemJS},
		{ingest.LangTypeScript, EcosystemJS},
		{ingest.LangRust, EcosystemRust},
		{"java", ""},
		{"c", ""},
		{ingest.LangOther, ""},
		{ingest.LangShell, ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := FromLanguage(tc.lang); got != tc.want {
			t.Errorf("FromLanguage(%q) = %q, want %q", tc.lang, got, tc.want)
		}
	}
}

// Mapped languages need a base-mode probe: the auto gate gates on caps[eco][BaseMode(eco)].
func TestFromLanguage_MappedLanguagesHaveBaseMode(t *testing.T) {
	for lang := range languageExt {
		eco := FromLanguage(lang)
		if eco == "" {
			t.Errorf("FromLanguage(%q) = %q, want a gated ecosystem", lang, eco)
		}
		if BaseMode(eco) == "" {
			t.Errorf("FromLanguage(%q) -> %q has no BaseMode", lang, eco)
		}
	}
}
