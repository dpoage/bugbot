package store

import (
	"errors"
	"fmt"

	"modernc.org/sqlite"
)

// SQLite extended result codes that callers most often need to disambiguate.
//
// These are the SQLite "extended" result codes; an extended code is the primary
// code OR'd with a secondary byte. See https://www.sqlite.org/rescode.html.
//
// We keep a local copy of the ones we inspect rather than importing
// modernc.org/sqlite/lib (which is the C-symbol surface and drags in the entire
// C transpile unit). The values are part of SQLite's stable public ABI.
const (
	// Primary sqlite result codes (1..N).
	sqliteOK        = 0
	sqliteERROR     = 1
	sqliteBUSY      = 5
	sqliteFULL      = 13
	sqliteIOERR     = 10
	sqliteIOERRRead = (sqliteIOERR | (1 << 8))

	// sqliteIOERR | (2<<8) — VFS read() returned fewer bytes than requested.
	// 522 = 10 | (2 << 8) is the symptom of a checkpoint-vs-read race on a
	// pure-Go SQLite driver: a concurrent autocheckpoint truncates/rewrites
	// the main db file while a reader's page read lands past the new end.
	sqliteIOERRShortRead = 522

	// SQLITE_CONSTRAINT (19) extended subcodes. The most common ones that
	// surface in our schema are PRIMARYKEY (1555), UNIQUE (2067), and
	// FOREIGNKEY (787). We map them so a log line says
	// "SQLITE_CONSTRAINT_PRIMARYKEY" instead of "SQLITE_0x613".
	sqliteCONSTRAINTPrimaryKey = 1555
	sqliteCONSTRAINTUnique     = 2067
	sqliteCONSTRAINTForeignKey = 787
	sqliteCONSTRAINTNotNull    = 1299
	sqliteCONSTRAINTCheck      = 275
)

// sqliteCodeName returns a stable, human-friendly name for a SQLite extended
// result code, or "SQLITE_0x<hex>" when the code is not in the table. The
// names match the SQLite source's #define'd identifiers so logs read
// consistently with upstream documentation.
func sqliteCodeName(code int) string {
	switch code {
	case sqliteOK:
		return "SQLITE_OK"
	case sqliteERROR:
		return "SQLITE_ERROR"
	case sqliteBUSY:
		return "SQLITE_BUSY"
	case sqliteFULL:
		return "SQLITE_FULL"
	case sqliteIOERR:
		return "SQLITE_IOERR"
	case sqliteIOERRRead:
		return "SQLITE_IOERR_READ"
	case sqliteIOERRShortRead:
		return "SQLITE_IOERR_SHORT_READ"
	case sqliteCONSTRAINTPrimaryKey:
		return "SQLITE_CONSTRAINT_PRIMARYKEY"
	case sqliteCONSTRAINTUnique:
		return "SQLITE_CONSTRAINT_UNIQUE"
	case sqliteCONSTRAINTForeignKey:
		return "SQLITE_CONSTRAINT_FOREIGNKEY"
	case sqliteCONSTRAINTNotNull:
		return "SQLITE_CONSTRAINT_NOTNULL"
	case sqliteCONSTRAINTCheck:
		return "SQLITE_CONSTRAINT_CHECK"
	default:
		// Unknown / library-internal subcode: render the hex so a log
		// reader can still tell apart IOERR (10..) from BUSY (5) and
		// FULL (13) classes at a glance.
		return fmt.Sprintf("SQLITE_0x%x", code)
	}
}

// isIOErr reports whether code is in the SQLITE_IOERR class (primary=10,
// any secondary byte). Anything matching this class is a VFS/disk-level
// short read, write, or fsync failure — distinct from BUSY (lock) and FULL
// (disk full). On a pure-Go modernc.org/sqlite store, IOERR_SHORT_READ
// (522) is the tell-tale of a checkpoint-vs-read race.
func isIOErr(code int) bool {
	return (code & 0xff) == sqliteIOERR
}

// isIOErrShortRead reports whether code is exactly SQLITE_IOERR_SHORT_READ
// (522). This is the specific failure mode observed in the field; other
// IOERR variants (READ=266, WRITE=778, etc.) have different root causes.
func isIOErrShortRead(code int) bool {
	return code == sqliteIOERRShortRead
}

// annotateErr wraps a returned store error with the on-disk path, the op
// name, and (when the underlying error is a modernc.org/sqlite *sqlite.Error)
// its extended result code. The wrapper is itself an error and unwraps to the
// original via errors.Unwrap, so existing call sites and tests that compare
// against the raw error continue to work.
//
// When err is not a *sqlite.Error, the code is reported as -1 and the name as
// "SQLITE_NONSQLITE"; callers can still distinguish 522 from 5/13 by the
// primary code in the formatted message.
func annotateErr(path, op string, err error) error {
	if err == nil {
		return nil
	}
	code := -1
	name := "SQLITE_NONSQLITE"
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		code = sqliteErr.Code()
		name = sqliteCodeName(code)
	}
	return &annotatedError{
		Path:     path,
		Op:       op,
		Code:     code,
		CodeName: name,
		Err:      err,
	}
}

// annotatedError carries store-call context alongside the raw error so a
// future SQLITE_IOERR_SHORT_READ (522) is unambiguous in logs and not
// confusable with SQLITE_BUSY (5) or SQLITE_FULL (13). It is intentionally
// not a sentinel: callers compare on the *sqlite.Error via errors.As, not
// on this wrapper.
type annotatedError struct {
	Path     string // database file path the call targeted
	Op       string // store method name (e.g. "record_spend", "open")
	Code     int    // SQLite extended result code, or -1 if not a sqlite error
	CodeName string // human name for Code (e.g. "SQLITE_IOERR_SHORT_READ")
	Err      error  // the underlying error
}

// Error renders the annotated form: "store: <op> on <path>: [<code>] <name>:
// <underlying message>". The leading "[<code>] <name>:" makes the SQLite
// result code a first-class prefix in every log line.
func (e *annotatedError) Error() string {
	return fmt.Sprintf("store: %s on %s: [%d] %s: %s",
		e.Op, e.Path, e.Code, e.CodeName, e.Err.Error())
}

// Unwrap returns the underlying error so errors.Is and errors.As see the
// original cause (e.g. *sqlite.Error) and existing tests / callers that
// branch on the raw error keep working.
func (e *annotatedError) Unwrap() error { return e.Err }

// IsIOErr reports whether err originates from the SQLITE_IOERR class. It
// returns false for nil and for errors that are not SQLite errors. Callers
// use this to decide whether to run the slow-path Diagnose (quick_check +
// reopen) that disambiguates transient IOERR races from on-disk corruption.
func IsIOErr(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return isIOErr(sqliteErr.Code())
}

// IsIOErrShortRead reports whether err is exactly SQLITE_IOERR_SHORT_READ
// (code 522). This is the specific symptom of a WAL checkpoint-vs-read race
// on a pure-Go SQLite driver; if true, the caller should run Diagnose to
// classify the failure as transient (race) or persistent (corruption).
func IsIOErrShortRead(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return isIOErrShortRead(sqliteErr.Code())
}

// SQLiteCode extracts the extended result code from err. Returns 0 when err
// is nil and -1 when err is not a *sqlite.Error. Useful in tests and in
// log lines where the code is preferred over a string match.
func SQLiteCode(err error) int {
	if err == nil {
		return 0
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return -1
	}
	return sqliteErr.Code()
}
