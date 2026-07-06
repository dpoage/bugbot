package cli

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// commentableLines maps each changed file's post-image path to the set of
// RIGHT-side (new-file) line numbers that appear inside a diff hunk. Inline PR
// review comments can ONLY anchor to such lines, so a finding whose file:line is
// absent here must be routed to the summary instead.
//
// A line is commentable when it is an added ('+') or context (' ') line within a
// hunk — those are the lines the diff actually renders on the right. Deleted
// ('-') lines have no RIGHT-side number and are excluded.
type commentableLines map[string]map[int]bool

// has reports whether path:line is an anchorable RIGHT-side line.
func (c commentableLines) has(path string, line int) bool {
	lines, ok := c[path]
	if !ok {
		return false
	}
	return lines[line]
}

// hunkHeader matches a unified-diff hunk header: @@ -oldStart,oldLen +newStart,newLen @@
// The counts are optional (git omits ",len" when it is 1).
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// fileHeader matches the "+++ b/path" line that names the post-image path of the
// following hunks. "/dev/null" marks a deletion (no RIGHT side to comment on).
var fileHeader = regexp.MustCompile(`^\+\+\+ (.+)$`)

// parseUnifiedDiff parses a raw unified diff (as produced by
// `git diff base...head`) into the commentable RIGHT-side lines per file.
//
// It walks hunks line by line, tracking the running new-file line number from
// each hunk header. Added and context lines advance and are recorded; deleted
// lines do not advance the new-file counter and are not recorded. Renames are
// handled naturally: the "+++ b/<newpath>" header sets the post-image path, so a
// renamed-and-edited file's hunks are attributed to its new path.
func parseUnifiedDiff(diff []byte) commentableLines {
	out := make(commentableLines)
	sc := bufio.NewScanner(bytes.NewReader(diff))
	// Diffs can have long lines; raise the scanner's token cap generously.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var (
		curPath string
		newLine int
		inHunk  bool
	)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case len(line) >= 4 && line[:4] == "+++ ":
			if m := fileHeader.FindStringSubmatch(line); m != nil {
				curPath = normalizeDiffPath(m[1])
				inHunk = false
			}
		case len(line) >= 2 && line[:2] == "@@":
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				newLine, _ = strconv.Atoi(m[1])
				inHunk = true
			}
		case inHunk && curPath != "" && curPath != "/dev/null":
			if len(line) == 0 {
				// A bare empty line inside a hunk is a context line representing an
				// empty new-file line; count it as commentable.
				record(out, curPath, newLine)
				newLine++
				continue
			}
			switch line[0] {
			case '+':
				record(out, curPath, newLine)
				newLine++
			case ' ':
				record(out, curPath, newLine)
				newLine++
			case '-':
				// Deleted line: no RIGHT-side number, do not advance.
			case '\\':
				// "\ No newline at end of file" — metadata, ignore.
			default:
				// Anything else (shouldn't occur in a well-formed hunk) ends the hunk.
				inHunk = false
			}
		}
	}
	return out
}

// record marks path:line commentable, allocating the per-file set on first use.
func record(out commentableLines, path string, line int) {
	set, ok := out[path]
	if !ok {
		set = make(map[int]bool)
		out[path] = set
	}
	set[line] = true
}

// (executeReview calls Repo.UnifiedDiff directly to obtain the bytes parsed
// here; the parser above is the seam tests exercise with canned diffs.)

// normalizeDiffPath strips the conventional "b/" prefix git puts on post-image
// paths and trims any trailing tab-delimited metadata, yielding a repo-relative
// path that matches domain.Finding.File. "/dev/null" is returned as-is.
func normalizeDiffPath(p string) string {
	// git appends a tab + metadata to header paths when they contain spaces.
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "/dev/null" {
		return p
	}
	if len(p) >= 2 && p[:2] == "b/" {
		return p[2:]
	}
	return p
}
