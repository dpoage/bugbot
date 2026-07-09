package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// DefectKind is the closed taxonomy finders choose from when reporting a
// candidate's defect class. It exists because LLMs are unreliable at
// consistent prose but reliable at picking from a small enum: identity used
// to depend on description-token Jaccard similarity clearing an empirically
// tuned threshold (funnel.mergeSimilarityThreshold), a "prose cliff" that
// silently split or merged defects based on wording. DefectKind, together
// with Subject and the locus, gives cross-lens identity a structural
// foundation instead.
//
// The taxonomy is deliberately small (~10 values). A large enum reintroduces
// the prose-consistency problem it exists to remove: a model choosing among
// 40 fine-grained kinds is exactly as inconsistent as one writing free prose.
// "other" is the deliberate escape hatch for a genuine defect that does not
// fit the other nine — it must never be the majority class in practice, but
// its existence is what keeps the other nine tight.
type DefectKind string

const (
	DefectNilDeref          DefectKind = "nil-deref"
	DefectUncheckedError    DefectKind = "unchecked-error"
	DefectResourceLeak      DefectKind = "resource-leak"
	DefectRace              DefectKind = "race"
	DefectBounds            DefectKind = "bounds"
	DefectInjection         DefectKind = "injection"
	DefectContractViolation DefectKind = "contract-violation"
	DefectLogic             DefectKind = "logic"
	DefectOther             DefectKind = "other"
)

// AllDefectKinds is the closed enum in declaration order. It backs the
// candidate JSON schema's "enum" list (internal/funnel/prompt.go) and
// DefectKind.Valid, so the taxonomy is defined in exactly one place.
var AllDefectKinds = []DefectKind{
	DefectNilDeref,
	DefectUncheckedError,
	DefectResourceLeak,
	DefectRace,
	DefectBounds,
	DefectInjection,
	DefectContractViolation,
	DefectLogic,
	DefectOther,
}

// Valid reports whether k is one of AllDefectKinds.
func (k DefectKind) Valid() bool {
	for _, v := range AllDefectKinds {
		if k == v {
			return true
		}
	}
	return false
}

// NormalizeSubject canonicalizes a finder-reported subject (the symbol at
// fault) the same way locus resolution normalizes a location: strip
// receiver/package qualifiers and lowercase. A model may report a subject as
// "(*Handler).ServeHTTP", "pkg.Foo", or "Foo.Bar" depending on phrasing; all
// three name the same symbol at the identity granularity Fingerprint v3
// needs (the base declaration name), so normalization collapses them to one
// string. Only the final dotted segment is kept — a package or receiver
// qualifier prefix carries no defect identity beyond what the enclosing-symbol
// locus already anchors.
func NormalizeSubject(subject string) string {
	s := strings.TrimSpace(subject)
	s = strings.NewReplacer("(", "", ")", "", "*", "", "&", "").Replace(s)
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// normalizeFilePath lowercases and forward-slash-cleans a repo-relative file
// path. Shared by Fingerprint, FingerprintV3, and LocusKey so all three
// identity schemes agree on file normalization.
func normalizeFilePath(file string) string {
	return strings.ToLower(path.Clean(strings.ReplaceAll(file, "\\", "/")))
}

// fingerprintSchemeV3 namespaces the v3 identity hash. Bump this (and add a
// new scheme constant, never mutate this one) if the v3 input tuple ever
// changes shape.
const fingerprintSchemeV3 = "bugbotFingerprint/v3"

// FingerprintV3 computes the structured, cross-lens dedup identity for a
// finding: the normalized file, the caller-supplied locus anchor (see
// funnel.LocusResolver), the closed defect_kind enum, and the normalized
// subject symbol. Deliberately EXCLUDES lens — v2's Fingerprint baked lens
// into identity, which meant two lenses reporting the same defect only
// converged after clearing the description-Jaccard similarity gate
// (funnel.mergeSimilarityThreshold) in triage's clustering pass. Under v3,
// two candidates from different lenses at the same locus with the same
// defect_kind and subject mint the IDENTICAL fingerprint and collide at
// triage's ordinary exact-fingerprint dedup step, with no reliance on prose
// similarity at all. defect_kind still disambiguates two distinct defects at
// the same locus (e.g. a nil-deref and a resource-leak in the same function)
// from colliding.
//
// kind is assumed valid (see DefectKind.Valid); callers validate finder
// output against the schema before this is called. subject is normalized
// internally via NormalizeSubject, so callers may pass the raw finder string.
func FingerprintV3(file, locus string, kind DefectKind, subject string) string {
	normFile := normalizeFilePath(file)
	normSubject := NormalizeSubject(subject)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s", fingerprintSchemeV3, normFile, locus, string(kind), normSubject)
	return hex.EncodeToString(h.Sum(nil))
}
