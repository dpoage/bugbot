package funnel

import "github.com/dpoage/bugbot/internal/ingest"

// lens_manifestations.go is DATA, not plumbing: the per-language manifestation
// rows below are finder-prompt prose, and the lensYields table drives which
// lenses survive budget degradation per language. Adding a language column or
// a lens row here requires no change to prompt composition or to the
// degradation logic — composition iterates whatever rows exist (see
// finderSystemPrompt), and yields resolve through effectiveYield.
//
// Authoring bar for rows: every row names a CONCRETE, investigable pattern in
// the imperative register of the lens Cores — the exact construct to grep for
// and the exact way it goes wrong. No "be careful with X". Rows must reflect
// the language's REAL failure modes for that lens, not a translation of the Go
// prose: concurrency on Python is forgotten awaits and check-then-act under
// threading, not goroutines.

// rowsJSTS marks row slices shared verbatim between LangJavaScript and
// LangTypeScript: the two share one runtime and one failure-mode surface, and
// TypeScript's types do not remove the runtime hazards (undefined at runtime,
// coercion through any, un-awaited promises). Prompt composition merges
// languages whose row slices are equal into a single block, so a mixed JS/TS
// chunk gets one "JavaScript/TypeScript" block, not two copies.
//
// Similarly, C and C++ share the injection rows (cFamilyInjection): the sinks
// are libc in both languages. Their other lenses diverge (RAII, iterators, and
// move semantics are C++-only), so those rows are per-language.

// --- nil-safety/error-handling ---------------------------------------------

var goNilSafety = []string{
	"Dereferencing a pointer, map, slice, channel, or interface that a reachable code path can leave nil.",
	"Using a value returned alongside an error WITHOUT checking that error first; ignored errors that hide a failed operation whose result is then used.",
	"Type assertions without the comma-ok form on a value that may not hold that type.",
	"Returning a nil error while also returning a zero/invalid value the caller will use.",
}

var pyNilSafety = []string{
	"Attribute access or method calls on a value a reachable path leaves None: functions that return None on their not-found/error branch, dict.get without a default, re.match/re.search results used without a None check.",
	"except clauses that swallow the failure (bare except, or except Exception: pass) and let execution continue with a missing or half-initialized result.",
	"Direct dict indexing (d[k]) where the key is not guaranteed present, and getattr chains assuming an attribute exists on every type that can flow there.",
	"Truthiness guards (if not x) that conflate None with legitimate falsy values like 0, '' or an empty collection, taking the error branch on valid input.",
	"Mutable default arguments (def f(x, acc=[])) silently accumulating state across calls.",
}

var cppNilSafety = []string{
	"Dereferencing a raw or smart pointer a reachable path leaves null: an unchecked dynamic_cast<T*> result, ->/* on an empty unique_ptr/shared_ptr, weak_ptr::lock() used without checking, map::find() dereferenced without comparing to end().",
	"Use-after-move: reading from an object after std::move handed it away; the moved-from object is in a valid but unspecified state.",
	"Discarding a status/error return (or std::optional/std::expected) and using the output parameter or partial result anyway.",
	"std::optional dereferenced with * or -> without has_value(); calling .value() on a path where it throws.",
	"Out-parameters left uninitialized on the error branch and then read by the caller.",
}

var jstsNilSafety = []string{
	"Property access on a value a reachable path leaves undefined or null: Array.prototype.find returning undefined, optional fields, map lookups; optional chaining (?.) that quietly yields undefined which then flows into arithmetic, an index, or a template string.",
	"Un-awaited promises: a Promise used as if it were the resolved value, and async calls whose rejection has no handler (a floating rejection that vanishes).",
	"Loose equality (==) coercion accepting values strict equality would reject (null == undefined, '' == 0, '0' == 0) on a guard that is supposed to filter them.",
	"NaN propagation: parseInt/parseFloat/Number on unvalidated input flowing into arithmetic or comparisons (every comparison with NaN is false, so the guard never fires).",
	"catch blocks that swallow the error and let the caller proceed with an undefined result.",
}

var rustNilSafety = []string{
	"unwrap()/expect() on a Result or Option that a reachable path can leave Err/None — a guaranteed panic on that path.",
	"Errors discarded with let _ = or .ok() where later code assumes the failed operation's effect actually happened.",
	"unsafe blocks dereferencing raw pointers that can be null or dangling.",
}

var cNilSafety = []string{
	"Dereferencing a pointer a reachable path leaves NULL: unchecked malloc/calloc/realloc returns, fopen/getenv/strchr/strstr results used without a NULL check.",
	"Discarding the return of functions that signal failure (fclose, fwrite, snprintf truncation) and using the output buffer or file state anyway.",
	"Out-parameters left uninitialized on the error branch and then read by the caller.",
}

// --- concurrency ------------------------------------------------------------

var goConcurrency = []string{
	"Data races on shared state accessed from multiple goroutines without synchronization.",
	"A mutex that is taken and not released on every return path.",
	"Deadlocks from lock ordering or from sending/receiving on a channel no one services.",
	"Closing or writing to a channel from multiple goroutines; loop-variable capture in goroutines; and WaitGroup Add/Done imbalances.",
	"Read the goroutine launch sites (go statements) to confirm which code actually runs concurrently.",
}

var pyConcurrency = []string{
	"Forgotten awaits: a coroutine called without await (it never runs, and the call site gets a coroutine object); asyncio tasks fired without create_task or never awaited, so their exceptions vanish.",
	"Check-then-act races in threaded code (if key not in d: d[key] = ...) — the GIL serializes bytecodes, not compound operations, so threads still interleave between the check and the act.",
	"Blocking calls (requests, time.sleep, blocking file/DB I/O) inside async def, stalling the entire event loop.",
	"Shared state mutated across an await boundary while an invariant is mid-update — another task observes or clobbers the half-updated state.",
	"threading.Lock acquired without with or try/finally, so an exception on the critical path leaks the lock.",
}

var cppConcurrency = []string{
	"Data races: shared variables read and written from multiple threads without a mutex or std::atomic — undefined behavior even when each access looks atomic.",
	"A mutex locked without lock_guard/unique_lock so an early return or thrown exception skips the unlock.",
	"Deadlocks from inconsistent lock acquisition order across threads, and condition_variable waits without a predicate (lost or spurious wakeups) or whose notify can never fire.",
	"Detached threads or callbacks capturing this or references to objects that can be destroyed before the thread finishes — a use-after-free race.",
	"Hand-rolled double-checked locking or 'benign race' lazy initialization without atomics.",
}

var jstsConcurrency = []string{
	"Check-then-act across an await: state validated before an await boundary and used after it, while another callback or handler mutates that state in between.",
	"Concurrent async writers to a shared structure (cache, map, counter) without coordination — interleaved read-modify-write across awaits loses updates.",
	"Promise.all results assumed complete when one rejection short-circuits the rest; re-entrancy where an event handler fires during an in-flight async operation and mutates the state it depends on.",
}

var rustConcurrency = []string{
	"Blocking inside async tasks: std::thread::sleep, blocking I/O, or a std::sync::Mutex guard held across an .await — stalls or deadlocks the executor.",
	"Deadlocks from lock ordering across threads or tasks.",
	"unsafe impl Send/Sync or raw-pointer sharing that reintroduces the data races the compiler otherwise rules out.",
}

var cConcurrency = []string{
	"Data races: shared variables accessed from multiple threads (pthreads) without a mutex or C11 atomics.",
	"pthread_mutex_lock without a matching unlock on every path, including error returns.",
	"Deadlocks from inconsistent lock ordering; condition-variable waits without a predicate loop (spurious wakeups) or whose signal can be missed.",
	"Signal handlers calling non-async-signal-safe functions or touching shared state that is not a volatile sig_atomic_t.",
}

// --- resource-leaks ---------------------------------------------------------

var goResourceLeaks = []string{
	"An opened file, network connection, HTTP response body, database rows/statement, ticker, or context-cancel func that is not closed/stopped on every return path (especially early-return error paths).",
	"Goroutines that can never exit because their stop signal is unreachable.",
	"Defers placed inside loops that accumulate until the function returns.",
}

var pyResourceLeaks = []string{
	"Files, sockets, or DB connections opened without a with block or try/finally close — leaked when an exception fires before close().",
	"requests/aiohttp sessions and subprocess pipes never closed; asyncio tasks created and abandoned without being awaited or cancelled.",
	"Generators holding open resources that are abandoned mid-iteration and never closed.",
	"Registrations that grow without an unregister path: signal handlers, observers, caches keyed by request data.",
}

var cppResourceLeaks = []string{
	"Manual new/delete or malloc/free pairs where an early return or thrown exception skips the release — anything not owned by a RAII type (unique_ptr, lock_guard, fstream) leaks on the exception path.",
	"Mismatched acquisition/release: new[] freed with delete (not delete[]), fopen without fclose on every branch, double delete on a path that releases twice.",
	"Raw owning pointers stored in containers, where erase/clear drops the pointers without deleting the pointees.",
	"std::thread objects that are never joined or detached — their destructor terminates the process.",
	"A base class without a virtual destructor deleted through a base pointer — derived members are never destroyed.",
}

var jstsResourceLeaks = []string{
	"setInterval/setTimeout handles and event listeners registered without a matching clearInterval/clearTimeout/removeEventListener — the live closure pins everything it captures.",
	"File handles, streams, and DB connections opened without try/finally (or await using); stream readers/writers acquired and never released (getReader without releaseLock).",
	"Subscriptions (observables, sockets, watchers) accumulated per call or per render with no teardown path.",
}

var rustResourceLeaks = []string{
	"Cleanup detached from Drop: mem::forget, Box::leak, or ManuallyDrop with no matching explicit drop on every path.",
	"Spawned tasks or threads that can never exit because their stop signal (channel close, cancellation token) is unreachable; JoinHandles dropped without join or abort.",
}

var cResourceLeaks = []string{
	"malloc/calloc/strdup without a free on every path — especially error branches that free some-but-not-all of the allocations made so far in the function.",
	"File descriptors and FILE* from open/fopen/socket not closed on every return path, including when a later setup step fails.",
	"Mismatched pairs: memory from one allocator released by another, or a double free on a shared error path.",
}

// --- boundary-conditions ----------------------------------------------------

var goBoundary = []string{
	"Off-by-one in slice or array indexing; indexing or slicing with an unchecked attacker- or caller-controlled length (panic on out-of-range).",
	"Assuming a slice, map, or string is non-empty before indexing [0] or [len-1].",
	"Conversions between integer sizes or signedness in index/size arithmetic that can truncate or wrap (e.g. int(n) of an int64 on a 32-bit platform, int(u) of a large uint).",
}

var pyBoundary = []string{
	"Off-by-one in manual range()/len() index arithmetic; code assuming slice semantics for index access — a slice silently clamps out-of-range bounds, but s[i] raises.",
	"A computed index that can go negative: Python reads from the END of the sequence instead of raising, so an off-by-one returns wrong data rather than an exception.",
	"Assuming a list/string/dict is non-empty before s[0], s[-1], or next(iter(...)).",
	"Integer vs float division (/ vs //) in size arithmetic producing a float where an index is needed, or off-by-one rounding in paging/chunking math.",
}

var cppBoundary = []string{
	"operator[] (no bounds check) with an index that can equal or exceed size(); off-by-one in loop bounds (<= vs <) reading or writing one past the end — silent corruption, not an exception.",
	"Signed/unsigned comparison in a bounds check: a negative signed value converts to a huge unsigned so i < v.size() passes; size() - 1 on an empty container wraps to SIZE_MAX.",
	"Iterator invalidation: erasing or inserting into a container while iterating it, or holding iterators/references/pointers across a reallocating operation (vector push_back).",
	"front()/back() on a container a reachable path leaves empty — undefined behavior.",
	"Integer overflow or truncation in size arithmetic: an int holding a size_t, or n * sizeof(T) overflowing before it reaches the allocator.",
}

var jstsBoundary = []string{
	"Off-by-one on .length: arr[arr.length] (or a <= bound in a loop) yields undefined, not an error, and propagates silently into later expressions.",
	"indexOf/findIndex returning -1 used directly as an index or as a slice argument (where -1 means 'from the end').",
	"Assuming non-empty before arr[0] or .at(-1) — both yield undefined that flows onward.",
	"Precision loss in size/index math past Number.MAX_SAFE_INTEGER; parseInt without a radix; naive string indexing splitting surrogate pairs (an index is a UTF-16 unit, not a code point).",
}

var rustBoundary = []string{
	"as casts that truncate or wrap in size/index arithmetic (u64 as u32, i64 as usize of a negative value).",
	"Indexing with [] where the index can be out of bounds (guaranteed panic), and slice[0]/.last().unwrap() on possibly-empty slices.",
	"Arithmetic that can overflow: release builds wrap silently where debug builds would panic, so the bug ships.",
}

var cBoundary = []string{
	"Off-by-one in buffer indexing or loop bounds (<= vs <) writing one past the end; fixed-size buffers written with unchecked lengths (strcpy/sprintf/memcpy with an attacker-influenced size) — memory corruption, not an exception.",
	"Missing space for the NUL terminator: malloc(strlen(s)) instead of strlen(s)+1, or strncpy filling the buffer exactly and leaving it unterminated.",
	"Signed/unsigned comparison in bounds checks, and size_t underflow (len - 1 when len is 0 wraps to SIZE_MAX).",
	"Integer overflow in allocation-size arithmetic (n * sizeof(T) wrapping before malloc).",
}

// --- api-contract-misuse ----------------------------------------------------

var goAPIContract = []string{
	"Misusing the standard library: time.After in a hot loop, sql.Rows not iterated to completion, json/encoding round-trip assumptions.",
	"Passing a value where the API requires a pointer or vice versa in a way the compiler allows but the contract forbids.",
}

var pyAPIContract = []string{
	"Mutating a list or dict while iterating it; using an object after close() (file, connection, executor).",
	"Contracts the runtime accepts but the docs forbid: mixing naive and aware datetimes, json.dumps on non-serializable types that only fails at runtime, copy vs deepcopy aliasing shared mutable members.",
	"Subclasses skipping a required super().__init__() or super().method() call that the base class contract demands.",
	"Calling async APIs from the wrong context: asyncio.run inside a running event loop, or loop-bound objects shared across loops.",
}

var cppAPIContract = []string{
	"Required ordering violated: calling methods on a moved-from or already-destroyed object, or using a resource after close/free.",
	"reserve vs resize confusion: writing through operator[] after reserve() — the capacity exists but the elements do not.",
	"Contracts the compiler accepts but the standard forbids: a std::sort comparator that is not a strict weak ordering, mutating a container inside a range-for over it.",
	"Dangling views: string_view/span/c_str() pointers kept past the owning object's modification or lifetime.",
}

var jstsAPIContract = []string{
	"await inside Array.prototype.forEach — forEach ignores the returned promises, so nothing actually waits; ordering-sensitive async calls made without await.",
	"this binding: a prototype method extracted and passed as a bare callback, so this is undefined at call time.",
	"sort() comparators returning a boolean instead of negative/zero/positive; mutating arguments an API documents as immutable.",
	"JSON.parse/stringify round-trip assumptions: Dates become strings, undefined and functions are dropped, key order is not a contract.",
}

var rustAPIContract = []string{
	"Contracts in docs, not types: assuming HashMap iteration order, mem::take/swap leaving a value later code assumes still initialized, lock().unwrap() panicking on a poisoned mutex.",
	"unsafe APIs whose invariants are not actually upheld at the call site: from_utf8_unchecked, get_unchecked, transmute.",
}

var cAPIContract = []string{
	"Required ordering violated: use after free or after fclose; a realloc result discarded so the (possibly freed) original pointer is still used; strtok's hidden global state shared across callers.",
	"Misuse the compiler accepts: printf-family format strings that do not match the argument types; functions requiring NUL-terminated input handed an unterminated buffer; memcpy on overlapping regions where the contract demands memmove.",
}

// --- injection/input-validation ---------------------------------------------

var goInjection = []string{
	"SQL built with fmt.Sprintf or string concatenation instead of parameterized queries; exec.Command run through a shell (sh -c) with interpolated input.",
	"text/template used where html/template's contextual escaping is required; filepath.Join/Clean on user paths without confining the result to the intended root.",
}

var pyInjection = []string{
	"SQL built with f-strings, %-formatting, or .format instead of parameterized queries; os.system or subprocess with shell=True and interpolated input.",
	"eval/exec, pickle.loads, or yaml.load (without SafeLoader) on untrusted data — direct code execution.",
	"Template injection: user input used AS the template (str.format or Jinja2 rendering of attacker-influenced template strings); os.path.join silently discarding the base directory when the user path is absolute.",
}

var jstsInjection = []string{
	"SQL or shell commands built with template strings (child_process.exec with interpolated input) instead of parameterized queries / execFile argument arrays.",
	"innerHTML/document.write/dangerouslySetInnerHTML with untrusted input (XSS); eval, new Function, or setTimeout(string) on user data.",
	"Prototype pollution: deep-merging untrusted objects into targets without filtering __proto__/constructor/prototype keys.",
	"path.join/res.sendFile with user-supplied paths lacking a confinement check after normalization.",
}

// cFamilyInjection is shared verbatim between C and C++: the injection sinks
// (libc process spawning, printf format strings, raw buffers) are the same in
// both languages.
var cFamilyInjection = []string{
	"system()/popen() with concatenated untrusted input instead of an argv-style exec; SQL built by string concatenation instead of bound parameters.",
	"Format-string vulnerabilities: printf-family functions called with untrusted data AS the format argument.",
	"Unbounded reads into fixed buffers sized or indexed by untrusted lengths — here the missing bounds check IS the injection surface.",
	"User-supplied paths used without canonicalization (realpath) and a confinement check against the intended root.",
}

var rustInjection = []string{
	"std::process::Command run through a shell (sh -c) with interpolated input; SQL built with format! instead of parameterized queries.",
	"Path::join with an untrusted path — an absolute user path REPLACES the base, escaping the intended root.",
}

// manifestations maps lens name -> language -> manifestation rows. It is the
// per-language half of every lens (the universal half is Lens.Core): prompt
// composition appends one "How this manifests in <Language>" block per chunk
// language that has rows here. Languages without rows are simply omitted, and
// a lens absent from this map entirely composes Core alone — both are
// first-class, so adding a language column or a language-free lens requires NO
// composition-code change.
var manifestations = map[string]map[ingest.Language][]string{
	"nil-safety/error-handling": {
		ingest.LangGo:         goNilSafety,
		ingest.LangPython:     pyNilSafety,
		ingest.LangCPP:        cppNilSafety,
		ingest.LangJavaScript: jstsNilSafety,
		ingest.LangTypeScript: jstsNilSafety,
		ingest.LangRust:       rustNilSafety,
		ingest.LangC:          cNilSafety,
	},
	"concurrency": {
		ingest.LangGo:         goConcurrency,
		ingest.LangPython:     pyConcurrency,
		ingest.LangCPP:        cppConcurrency,
		ingest.LangJavaScript: jstsConcurrency,
		ingest.LangTypeScript: jstsConcurrency,
		ingest.LangRust:       rustConcurrency,
		ingest.LangC:          cConcurrency,
	},
	"resource-leaks": {
		ingest.LangGo:         goResourceLeaks,
		ingest.LangPython:     pyResourceLeaks,
		ingest.LangCPP:        cppResourceLeaks,
		ingest.LangJavaScript: jstsResourceLeaks,
		ingest.LangTypeScript: jstsResourceLeaks,
		ingest.LangRust:       rustResourceLeaks,
		ingest.LangC:          cResourceLeaks,
	},
	"boundary-conditions": {
		ingest.LangGo:         goBoundary,
		ingest.LangPython:     pyBoundary,
		ingest.LangCPP:        cppBoundary,
		ingest.LangJavaScript: jstsBoundary,
		ingest.LangTypeScript: jstsBoundary,
		ingest.LangRust:       rustBoundary,
		ingest.LangC:          cBoundary,
	},
	"api-contract-misuse": {
		ingest.LangGo:         goAPIContract,
		ingest.LangPython:     pyAPIContract,
		ingest.LangCPP:        cppAPIContract,
		ingest.LangJavaScript: jstsAPIContract,
		ingest.LangTypeScript: jstsAPIContract,
		ingest.LangRust:       rustAPIContract,
		ingest.LangC:          cAPIContract,
	},
	"injection/input-validation": {
		ingest.LangGo:         goInjection,
		ingest.LangPython:     pyInjection,
		ingest.LangCPP:        cFamilyInjection,
		ingest.LangJavaScript: jstsInjection,
		ingest.LangTypeScript: jstsInjection,
		ingest.LangRust:       rustInjection,
		ingest.LangC:          cFamilyInjection,
	},
}

// anyLanguage is the default column in lensYields: it stands in for any
// dominant language without an explicit column, and for runs where no dominant
// language could be detected at all. A language-free lens needs only this
// column.
const anyLanguage = ingest.Language("*")

// lensYields ranks lenses for budget degradation, per language: when the run
// is over its soft-budget threshold, only the highest-effective-yield lenses
// keep running (see effectiveYield for the max-over-dominant-languages
// resolution). Higher is kept longer.
//
// The Go column preserves the empirically-grounded historical rankings
// (nil/concurrency/resource bugs are both more common and more severe than the
// others in typical Go code). EVERY OTHER COLUMN IS A REASONED PRIOR pending
// learned yields (the bugbot-mi5 follow-on): the rationale per language is
//
//   - Python: injection rises sharply (string-built SQL/templates and
//     eval/pickle are idiomatic hazards) and concurrency falls (the GIL
//     serializes most memory races; what remains is asyncio misuse).
//   - C/C++: boundary-conditions and resource-leaks dominate — manual memory
//     and no GC turn off-by-ones into corruption and missed frees into real
//     leaks; concurrency stays high (data races are UB). Injection sits midtable
//     via libc sinks (format strings, system()).
//   - JavaScript/TypeScript: undefined/NaN propagation (nil-safety) and
//     injection (XSS, template-built commands) lead; the single-threaded event
//     loop demotes concurrency to await-interleaving bugs.
//   - Rust: the compiler eliminates most nil/race/leak classes; what remains is
//     panics on unwrap (kept top), contract misuse around unsafe, and
//     truncating casts.
//   - anyLanguage: conservative mid-table priors so an unprofiled repo keeps
//     the historically strongest lenses.
var lensYields = map[string]map[ingest.Language]int{
	// diff-intent is language-free (Core-only, no manifestation rows): its
	// value comes from the commit context, not the language. 95 under
	// anyLanguage ranks it above concurrency's Go column and below nil-safety
	// for every language mix — the unique-advantage lens on commit runs.
	"diff-intent": {
		anyLanguage: 95,
	},
	"nil-safety/error-handling": {
		ingest.LangGo:         100,
		ingest.LangPython:     90,
		ingest.LangCPP:        90,
		ingest.LangJavaScript: 95,
		ingest.LangTypeScript: 95,
		ingest.LangRust:       70,
		ingest.LangC:          90,
		anyLanguage:           90,
	},
	"concurrency": {
		ingest.LangGo:         90,
		ingest.LangPython:     45,
		ingest.LangCPP:        85,
		ingest.LangJavaScript: 45,
		ingest.LangTypeScript: 45,
		ingest.LangRust:       55,
		ingest.LangC:          75,
		anyLanguage:           70,
	},
	"resource-leaks": {
		ingest.LangGo:         80,
		ingest.LangPython:     55,
		ingest.LangCPP:        90,
		ingest.LangJavaScript: 50,
		ingest.LangTypeScript: 50,
		ingest.LangRust:       45,
		ingest.LangC:          95,
		anyLanguage:           65,
	},
	"boundary-conditions": {
		ingest.LangGo:         60,
		ingest.LangPython:     50,
		ingest.LangCPP:        95,
		ingest.LangJavaScript: 50,
		ingest.LangTypeScript: 50,
		ingest.LangRust:       55,
		ingest.LangC:          100,
		anyLanguage:           60,
	},
	"api-contract-misuse": {
		ingest.LangGo:         50,
		ingest.LangPython:     55,
		ingest.LangCPP:        60,
		ingest.LangJavaScript: 55,
		ingest.LangTypeScript: 55,
		ingest.LangRust:       60,
		ingest.LangC:          50,
		anyLanguage:           50,
	},
	"injection/input-validation": {
		ingest.LangGo:         40,
		ingest.LangPython:     70,
		ingest.LangCPP:        50,
		ingest.LangJavaScript: 70,
		ingest.LangTypeScript: 70,
		ingest.LangRust:       35,
		ingest.LangC:          55,
		anyLanguage:           50,
	},
	// cross-language-boundary: the cross-language differentiator. Its value
	// comes from polyglot repos, so the yield is per-language-independent:
	// the precondition is "the repo is polyglot", which the seam discovery
	// upstream enforces. Ranked below the dominant single-language lenses
	// (nil/concurrency/resource) so budget pressure on a polyglot repo
	// still runs the high-yield per-language lenses first; the boundary
	// lens keeps running when there is room. 70 (mid-table) is a reasoned
	// prior — no empirical corpus exists yet for cross-language contract
	// bugs, so the bar matches the empirically-grounded "language has
	// bugs" baseline rather than the strongest per-language column.
	"cross-language-boundary": {
		anyLanguage: 70,
	},
}
