package ecosystem

// language.go maps an ingest.Language (the scan-side vocabulary) to the gated
// ecosystem key (infer.go's extension table), so the finder-exec auto gate
// and extension-based gating share one inference rule.
import (
	"github.com/dpoage/bugbot/internal/ingest"
)

// languageExt maps each language with a gated ecosystem to a canonical file
// extension that InferFromExtension resolves through extEcosystem; a language
// absent here (or whose ecosystem has no base-mode probe) maps to "".
var languageExt = map[ingest.Language]string{
	ingest.LangGo:         ".go",
	ingest.LangPython:     ".py",
	ingest.LangJavaScript: ".js",
	ingest.LangTypeScript: ".ts",
	ingest.LangRust:       ".rs",
}

// FromLanguage returns the gated ecosystem for a scan's target language via
// the extension table ("" when the language has no base-mode probe).
func FromLanguage(l ingest.Language) Ecosystem {
	return InferFromExtension(languageExt[l])
}
