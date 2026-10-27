package ecosystem

// Maps ingest.Language to the gated ecosystem via infer.go's extension
// table, so the finder-exec auto gate and extension gating share one rule.
import (
	"github.com/dpoage/bugbot/internal/ingest"
)

// languageExt maps gated languages to a canonical extension that
// InferFromExtension resolves; each entry needs a base-mode probe.
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
