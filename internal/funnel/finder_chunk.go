package funnel

// finder_chunk.go holds the file-chunking helpers extracted from hypothesize.go
// for readability. Pure code motion: no logic changes.

import (
	"sort"

	"github.com/dpoage/bugbot/internal/ingest"
)

// fileChunk is one finder unit's worth of target files plus the language set
// those files span (deduplicated, sorted). The language set selects the
// per-language manifestation blocks in the finder prompt; mixed chunks get the
// union of their languages' blocks.
type fileChunk struct {
	files []string
	langs []ingest.Language
}

// chunkByLanguage groups files by detected language BEFORE chunking, so chunks
// are language-homogeneous where possible and most finder prompts carry
// exactly one manifestation block. Each language's files (kept in input order,
// which Sweep may have heat-ordered) are cut into full chunks of exactly size;
// the per-language tails are then concatenated — still grouped by language, in
// first-seen order — and chunked together, so the only mixed chunks are the
// unavoidable remainders. Chunk-size semantics match chunk(): at most size
// files each, and a non-positive size yields a single chunk of everything.
func chunkByLanguage(files []string, size int) []fileChunk {
	if len(files) == 0 {
		return nil
	}
	if size <= 0 || len(files) <= size {
		return []fileChunk{{files: files, langs: chunkLangs(files)}}
	}

	var order []ingest.Language
	groups := make(map[ingest.Language][]string)
	for _, f := range files {
		l := ingest.DetectLanguage(f)
		if _, ok := groups[l]; !ok {
			order = append(order, l)
		}
		groups[l] = append(groups[l], f)
	}

	var out []fileChunk
	var tails []string
	for _, l := range order {
		g := groups[l]
		for len(g) >= size {
			// Three-index slice so a later append elsewhere can never write into
			// this chunk's backing array.
			out = append(out, fileChunk{files: g[:size:size], langs: []ingest.Language{l}})
			g = g[size:]
		}
		tails = append(tails, g...)
	}
	// The tails are language-contiguous, so chunking them keeps remainders
	// homogeneous whenever they happen to align with a chunk boundary; only the
	// genuinely unavoidable stragglers mix.
	for _, c := range chunk(tails, size) {
		out = append(out, fileChunk{files: c, langs: chunkLangs(c)})
	}

	// Restore global heat priority at chunk granularity: Sweep heat-orders the
	// input (churn x recency, bugbot-sro), and grouping by language must not
	// defer the second-hottest file of another language behind a whole cold
	// group. Sort chunks by the input rank of their hottest (earliest) member;
	// homogeneity is preserved because membership is untouched.
	rank := make(map[string]int, len(files))
	for i, f := range files {
		rank[f] = i
	}
	hottest := func(c fileChunk) int {
		best := len(files)
		for _, f := range c.files {
			if r := rank[f]; r < best {
				best = r
			}
		}
		return best
	}
	sort.SliceStable(out, func(i, j int) bool { return hottest(out[i]) < hottest(out[j]) })
	return out
}

// chunkLangs returns the deduplicated language set of files, sorted for a
// deterministic prompt (the manifestation blocks render in this order).
func chunkLangs(files []string) []ingest.Language {
	seen := make(map[ingest.Language]bool)
	var out []ingest.Language
	for _, f := range files {
		l := ingest.DetectLanguage(f)
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// chunk splits files into slices of at most size elements. The final chunk may
// be shorter. A non-positive size yields a single chunk of everything.
func chunk(files []string, size int) [][]string {
	if size <= 0 || len(files) <= size {
		if len(files) == 0 {
			return nil
		}
		return [][]string{files}
	}
	var out [][]string
	for i := 0; i < len(files); i += size {
		end := i + size
		if end > len(files) {
			end = len(files)
		}
		out = append(out, files[i:end])
	}
	return out
}
