package funnel

// experiment.go: exploration-only switches for the finder-recall A/B loop
// (branch explore/finder-recall, bugbot-hqgy). BUGBOT_EXPERIMENTS is a
// comma-separated list; unset means production behavior, byte for byte.
// Winners graduate into real config/defaults; losers are deleted.

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

const (
	// expLogicLens adds the logic/wrong-result lens to BuiltinLenses.
	expLogicLens = "logic-lens"
	// expSoftEmpty replaces the "empty list is expected" paragraph of the
	// finder prompt with one that asks for every confirmed defect.
	expSoftEmpty = "soft-empty"
	// expSizeChunks cuts finder chunks by line budget instead of file count.
	expSizeChunks = "size-chunks"
)

// sizeChunkLines is the line budget per finder chunk under expSizeChunks:
// about three 800-line read_file pages, so a 20-turn finder can read the
// whole chunk and still trace outward.
const sizeChunkLines = 2500

func experimentOn(name string) bool {
	for _, e := range strings.Split(os.Getenv("BUGBOT_EXPERIMENTS"), ",") {
		if strings.TrimSpace(e) == name {
			return true
		}
	}
	return false
}

// logicLens hunts the wrong-result bugs no defect-class lens names: in the
// Harbor spike all three gold bugs claude-code found were of this kind.
var logicLens = Lens{
	Name: "logic/wrong-result",
	Core: "Hunt for wrong-result logic defects: code that runs without error but " +
		"computes the wrong answer. Compare what each function, method, and branch " +
		"is documented or named to do with what it actually does: an input " +
		"combination that falls into a branch meant for another case; inverted, " +
		"incomplete, or overlapping conditions; using the wrong variable, attribute, " +
		"or result (for example a value left over from a discarded iteration); " +
		"arithmetic or aggregation whose operator or value type silently changes " +
		"the meaning; and state updated in the wrong order. Construct the concrete " +
		"input that produces the wrong result and trace it through the code before " +
		"reporting.",
}

const softEmptyParagraph = `Report every concrete defect you can confirm by reading the code; a confirmed bug with a precise location is valuable even when it looks minor. Return an empty list only after you have examined every target file and found nothing you can confirm. Do not pad the list with speculative entries.`

// chunkByLines greedily packs files, in input order, into chunks of at most
// maxFiles files and sizeChunkLines lines; a file over the budget gets a
// chunk of its own. Unreadable files count as zero lines.
func chunkByLines(root string, files []string, maxFiles int) []fileChunk {
	var out []fileChunk
	var cur []string
	curLines := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, fileChunk{files: cur, langs: chunkLangs(cur)})
			cur, curLines = nil, 0
		}
	}
	for _, f := range files {
		n := countLines(filepath.Join(root, f))
		if len(cur) > 0 && (curLines+n > sizeChunkLines || (maxFiles > 0 && len(cur) >= maxFiles)) {
			flush()
		}
		cur = append(cur, f)
		curLines += n
	}
	flush()
	return out
}

func countLines(path string) int {
	fh, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	n := 0
	for sc.Scan() {
		n++
	}
	return n
}
