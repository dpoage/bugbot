Audit the Python repository at `/testbed` for bugs.

**Scope.** Hunt for defects in files matching these globs (repo-relative); you may read any other file for context:

{scope}

The same globs are in `/task/scope.txt`, one per line.

**What counts.** Real defects: code that behaves incorrectly for some reachable input or state (wrong results, crashes, unhandled edge cases, contract violations). Not style, naming, missing docs, or speculative hardening. Precision matters as much as recall: report only findings you would defend to the maintainer.

**Rules.** Do not modify any file under `/testbed`. Do not use the network to look up the project's issue tracker or later versions of the code.

**Output.** Write your findings to `/logs/agent/findings.json` as a JSON array (write `[]` if you find nothing):

```json
[
  {
    "file": "path/relative/to/testbed.py",
    "line": 123,
    "title": "one-line summary",
    "description": "what goes wrong, for which input, and why"
  }
]
```

`line` is the 1-based line where the defect lives in the current file contents.
