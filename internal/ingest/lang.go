package ingest

import (
	"path/filepath"
	"sort"
	"strings"
)

// Language is a coarse, extension-derived classification of a source file. It
// is intentionally a small fixed vocabulary rather than an exhaustive linguist
// database: downstream stages only need to route a file to the right finder
// heuristics, not to render syntax highlighting.
type Language string

const (
	LangGo         Language = "go"
	LangPython     Language = "python"
	LangJavaScript Language = "javascript"
	LangTypeScript Language = "typescript"
	LangRust       Language = "rust"
	LangJava       Language = "java"
	LangC          Language = "c"
	LangCPP        Language = "cpp"
	LangRuby       Language = "ruby"
	LangCSharp     Language = "csharp"
	LangPHP        Language = "php"
	LangSwift      Language = "swift"
	LangKotlin     Language = "kotlin"
	LangShell      Language = "shell"
	LangOther      Language = "other"
)

// extLang maps a lower-cased file extension (including the leading dot) to a
// Language. Anything not present resolves to LangOther.
var extLang = map[string]Language{
	".go":    LangGo,
	".py":    LangPython,
	".pyi":   LangPython,
	".js":    LangJavaScript,
	".mjs":   LangJavaScript,
	".cjs":   LangJavaScript,
	".jsx":   LangJavaScript,
	".ts":    LangTypeScript,
	".tsx":   LangTypeScript,
	".mts":   LangTypeScript,
	".cts":   LangTypeScript,
	".rs":    LangRust,
	".java":  LangJava,
	".c":     LangC,
	".h":     LangC,
	".cc":    LangCPP,
	".cpp":   LangCPP,
	".cxx":   LangCPP,
	".hpp":   LangCPP,
	".hh":    LangCPP,
	".hxx":   LangCPP,
	".rb":    LangRuby,
	".cs":    LangCSharp,
	".php":   LangPHP,
	".swift": LangSwift,
	".kt":    LangKotlin,
	".kts":   LangKotlin,
	".sh":    LangShell,
	".bash":  LangShell,
}

// DetectLanguage classifies a file path by its extension. The path may be
// relative or absolute; only the final extension is inspected. Unknown
// extensions (and extensionless files) classify as LangOther.
func DetectLanguage(path string) Language {
	ext := strings.ToLower(filepath.Ext(path))
	if l, ok := extLang[ext]; ok {
		return l
	}
	return LangOther
}

// binaryExts is a set of extensions we treat as binary without reading content.
// The content sniff in isBinary still catches anything not listed here.
var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true,
	".ico": true, ".webp": true, ".tiff": true, ".svgz": true,
	".pdf": true, ".zip": true, ".gz": true, ".tar": true, ".tgz": true,
	".bz2": true, ".xz": true, ".7z": true, ".rar": true, ".jar": true,
	".war": true, ".class": true, ".o": true, ".a": true, ".so": true,
	".dylib": true, ".dll": true, ".exe": true, ".bin": true, ".obj": true,
	".wasm": true, ".pyc": true, ".pyo": true, ".woff": true, ".woff2": true,
	".ttf": true, ".otf": true, ".eot": true, ".mp3": true, ".mp4": true,
	".wav": true, ".flac": true, ".ogg": true, ".mov": true, ".avi": true,
	".mkv": true, ".webm": true, ".heic": true, ".db": true, ".sqlite": true,
}

// hasBinaryExt reports whether path's extension is a known-binary one.
func hasBinaryExt(path string) bool {
	return binaryExts[strings.ToLower(filepath.Ext(path))]
}

// ExtensionsForLanguage returns the file extensions (with leading dot,
// lowercase) that map to lang in the extLang table, sorted for determinism.
// Doctor and the setup wizard call this to match LSP server configs to a
// language without duplicating the extension→language mapping.
func ExtensionsForLanguage(lang Language) []string {
	var out []string
	for ext, l := range extLang {
		if l == lang {
			out = append(out, ext)
		}
	}
	sort.Strings(out)
	return out
}

// isBinaryContent applies the classic "null byte in the first chunk" heuristic.
// Git uses the same approach: a NUL byte in the leading bytes is a strong
// signal that a file is binary rather than text.
func isBinaryContent(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}
