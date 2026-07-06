package report

import (
	"encoding/json"

	isarif "github.com/dpoage/bugbot/internal/sarif"
)

// BuildSARIF constructs the SARIF document for a report. It is exported so
// tests (and callers wanting the typed form) can inspect the structure before
// serialization.
//
// All finding→SARIF mapping logic lives in internal/sarif.FromFindingsWithOptions;
// this function is a thin adapter that translates Report/Metadata into the
// sarif.Options the canonical emitter expects.
func BuildSARIF(r Report) isarif.Document {
	return isarif.FromFindingsWithOptions(r.Findings, isarif.Options{
		RepoPath: r.Meta.RepoPath,
	})
}

// SARIF renders the report as pretty-printed SARIF 2.1.0 JSON with a trailing
// newline. Marshaling of these fixed structs cannot fail, but the error is
// returned to keep the signature honest and future-proof.
func SARIF(r Report) ([]byte, error) {
	doc := BuildSARIF(r)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
