package repro

import (
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
)

// TestInterpret_CppBuildFailuresNeverDemonstrate is the regression test for
// the services-runtime standalone-repro round, where 5 of 6 "successful"
// Tier-1 promotions were in fact pure build / configure / network / link
// failures. Root cause: the C/C++ build+compile launchers (cmake, g++,
// clang++, ...) were not recognized by detectEcosystem, so they fell through
// to EcosystemUnknown, whose ran-evidence markers included the bare substrings
// "failed" / "fail " / "failure". CMake/compiler/linker output is saturated
// with those words ("ABI info - failed", "Build step ... failed", "linker
// command failed", and even a test target named *Failure*), so a non-zero exit
// from a broken build was minted as a demonstrated bug.
//
// The contract: a build/configure/link failure NEVER demonstrates, regardless
// of how the command was launched (direct, bash -c, or bash -lc).
func TestInterpret_CppBuildFailuresNeverDemonstrate(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		res  sandbox.Result
	}{
		{
			// #1 PeepEvents: GCC rejects clang-only -fsanitize=memory; the
			// CMake C-compiler probe fails before any test compiles.
			name: "gcc rejects -fsanitize=memory at cmake configure",
			cmd:  []string{"bash", "-c", "cmake -B build -S . -DCMAKE_C_FLAGS='-fsanitize=memory -g' && cmake --build build --target run_tests && ./build/bin/run_tests --gtest_filter=PeepEventsUninitializedTest.UninitializedTailIsRead"},
			res: sandbox.Result{
				ExitCode: 1,
				Stderr: "CMake Error at /usr/share/cmake-3.28/Modules/CMakeTestCCompiler.cmake:67 (message):\n" +
					"  The C compiler \"/usr/bin/cc\" is not able to compile a simple test program.\n" +
					"    cc: error: unrecognized argument to '-fsanitize=' option: 'memory'\n" +
					"-- Detecting C compiler ABI info - failed\n" +
					"-- Check for working C compiler: /usr/bin/cc - broken\n" +
					"-- Configuring incomplete, errors occurred!\n",
			},
		},
		{
			// #2 Tileset: googletest FetchContent cannot reach github under
			// network=none. bash -lc (login shell) — must still be unwrapped.
			name: "googletest fetch fails under network none (bash -lc)",
			cmd:  []string{"bash", "-lc", "cmake -B build -S . -DBUILD_TESTS=ON && cmake --build build --target run_tests -j && ctest --test-dir build -R TilesetLeakTest --output-on-failure"},
			res: sandbox.Result{
				ExitCode: 1,
				Stderr: "fatal: unable to access 'https://github.com/google/googletest.git/': Could not resolve host: github.com\n" +
					"CMake Error at /usr/share/cmake-3.28/Modules/FetchContent.cmake:1679 (message):\n" +
					"  Build step for googletest failed: 2\n" +
					"-- Configuring incomplete, errors occurred!\n",
			},
		},
		{
			// #4 JSON: the plan's argv carried raw shell operators (&&, cd)
			// that were passed literally to cmake, so cmake rejected them. The
			// test target name "JSONSilentFailureRepro" contains "Failure".
			name: "cmake arg-parse error, target name contains Failure",
			cmd:  []string{"cmake", "-B", "build", "-S", ".", "&&", "cmake", "--build", "build", "&&", "cd", "build", "&&", "./tests/JSONSilentFailureRepro", "--gtest_filter=JSONWriteSilentFailure.X", "--gtest_color=no"},
			res: sandbox.Result{
				ExitCode: 1,
				Stderr: "CMake Warning:\n  Ignoring extra path from command line:\n   \"./tests/JSONSilentFailureRepro\"\n" +
					"CMake Error: Unknown argument --gtest_color=no\n" +
					"CMake Error: Run 'cmake --help' for all supported options.\n",
			},
		},
		{
			// #5 add_effect: same googletest fetch failure, bash -c.
			name: "googletest fetch fails under network none (bash -c)",
			cmd:  []string{"bash", "-c", "cmake -B build -S . -DBUILD_TESTS=True && cmake --build build -j$(nproc) && ./build/artifacts/bin/run_tests --gtest_filter='PostProcessingEffectBugTest.*'"},
			res: sandbox.Result{
				ExitCode: 1,
				Stderr: "Cloning into 'googletest-src'...\n" +
					"fatal: unable to access 'https://github.com/google/googletest.git/': Could not resolve host: github.com\n" +
					"CMake Error at /usr/share/cmake-3.28/Modules/FetchContent.cmake:1679 (message):\n" +
					"  Build step for googletest failed: 2\n" +
					"-- Configuring incomplete, errors occurred!\n",
			},
		},
		{
			// #6 max_fd: the MSan runtime archive is missing, so the link step
			// fails. The launcher is buried behind `set -e; CXX=...; $CXX ...`
			// so detection cannot see clang++ — it lands on unknown, and must
			// still refuse to promote ("linker command failed" is not evidence
			// a test ran).
			name: "clang msan link failure (launcher hidden behind $CXX)",
			cmd:  []string{"bash", "-lc", "set -e; CXX=${CXX:-clang++}; $CXX -std=c++17 -fsanitize=memory -g repro_test.cpp -o /tmp/repro_maxfd && /tmp/repro_maxfd"},
			res: sandbox.Result{
				ExitCode: 1,
				Stderr: "/usr/bin/ld: cannot find /usr/lib/llvm-18/lib/clang/18/lib/linux/libclang_rt.msan-x86_64.a: No such file or directory\n" +
					"clang++: error: linker command failed with exit code 1 (use -v to see invocation)\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res, tc.cmd)
			if v.demonstrated {
				t.Fatalf("build/env failure must NOT demonstrate; got demonstrated=true (ecosystem=%q)\noutput:\n%s",
					v.ecosystem, v.summary)
			}
		})
	}
}

// TestInterpret_CppGenuineLeak_Demonstrated is the one true positive from the
// services-runtime round (#3): a direct g++ build of a standalone repro that
// ASan/LSan flagged a real leak in. It demonstrates via the dispositive
// runtimeFailureMarkers path and must keep working after the ecosystem/marker
// fix.
func TestInterpret_CppGenuineLeak_Demonstrated(t *testing.T) {
	res := sandbox.Result{
		ExitCode: 1,
		Stderr: "=================================================================\n" +
			"==1==ERROR: LeakSanitizer: detected memory leaks\n" +
			"Direct leak of 4096 byte(s) in 1 object(s) allocated from:\n" +
			"    #1 0x55bfe13503e8 in FakeRenderSystem::FakeRenderSystem() /workspace/test_renderer_leak.cpp:37\n" +
			"SUMMARY: AddressSanitizer: 4096 byte(s) leaked in 1 allocation(s).\n",
	}
	cmd := []string{"bash", "-c", "g++ -fsanitize=address -g test_renderer_leak.cpp -o test_renderer_leak && ASAN_OPTIONS=detect_leaks=1 ./test_renderer_leak"}
	v := interpret(res, cmd)
	if !v.demonstrated {
		t.Fatalf("genuine ASan/LSan leak must demonstrate; got reason=%q ecosystem=%q", v.reason, v.ecosystem)
	}
}

// TestInterpret_CppGenuineTestFailure_Demonstrated guards against
// over-tightening the C++ ran-evidence: a real gtest/ctest failure (the build
// succeeded and a test actually ran and FAILED) must still demonstrate. Covers
// the ctest launcher, a bare ./binary launcher (unknown ecosystem), and a
// bash -c cmake+run wrapper.
func TestInterpret_CppGenuineTestFailure_Demonstrated(t *testing.T) {
	gtestFail := "[==========] Running 1 test from 1 test suite.\n" +
		"[ RUN      ] TilesetLeakTest.LeaksTexture\n" +
		"/workspace/test/Tileset.cpp:40: Failure\n" +
		"Expected equality of these values:\n  leaked\n    Which is: 1\n  0\n" +
		"[  FAILED  ] TilesetLeakTest.LeaksTexture (0 ms)\n" +
		"[==========] 1 test from 1 test suite ran. (5 ms total)\n" +
		"[  PASSED  ] 0 tests.\n" +
		"[  FAILED  ] 1 test, listed below:\n" +
		"[  FAILED  ] TilesetLeakTest.LeaksTexture\n\n 1 FAILED TEST\n"
	ctestFail := "Test project /workspace/build\n" +
		"    Start 1: TilesetLeakTest\n" +
		"1/1 Test #1: TilesetLeakTest ..................***Failed    0.01 sec\n" +
		"0% tests passed, 1 tests failed out of 1\n" +
		"The following tests FAILED:\n	  1 - TilesetLeakTest (Failed)\n"

	cases := []struct {
		name string
		cmd  []string
		res  sandbox.Result
	}{
		{
			name: "ctest launcher",
			cmd:  []string{"ctest", "--test-dir", "build", "--output-on-failure"},
			res:  sandbox.Result{ExitCode: 8, Stdout: ctestFail + gtestFail},
		},
		{
			name: "bare gtest binary (unknown ecosystem)",
			cmd:  []string{"./build/bin/run_tests", "--gtest_filter=TilesetLeakTest.LeaksTexture"},
			res:  sandbox.Result{ExitCode: 1, Stdout: gtestFail},
		},
		{
			name: "bash -c cmake build then run",
			cmd:  []string{"bash", "-c", "cmake -B build -S . && cmake --build build --target run_tests && ./build/bin/run_tests"},
			res:  sandbox.Result{ExitCode: 1, Stdout: gtestFail},
		},
		{
			// glibc assert() abort: "Assertion `expr' failed." — the expr
			// splits the two words, so the bare "assertion failed" marker does
			// NOT match; the dedicated backtick marker must. Bare ./binary =>
			// unknown ecosystem (exercises the unknown ran-marker).
			name: "glibc assert abort (bare binary, unknown ecosystem)",
			cmd:  []string{"./repro_test"},
			res:  sandbox.Result{ExitCode: 134, Stderr: "repro_test: repro_test.cpp:12: int main(): Assertion `max_fd != 0' failed.\nAborted (core dumped)\n"},
		},
		{
			// BSD/macOS assert() form: "Assertion failed: (expr), ..." run via
			// a g++ compile+run wrapper (cpp ecosystem).
			name: "bsd assert abort (g++ wrapper, cpp ecosystem)",
			cmd:  []string{"bash", "-c", "g++ -std=c++17 repro_test.cpp -o t && ./t"},
			res:  sandbox.Result{ExitCode: 134, Stderr: "Assertion failed: (max_fd != 0), function main, file repro_test.cpp, line 12.\n"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res, tc.cmd)
			if !v.demonstrated {
				t.Fatalf("genuine C++ test failure must demonstrate; got reason=%q ecosystem=%q", v.reason, v.ecosystem)
			}
		})
	}
}

// TestInterpret_MaskedExitCode_SanitizerStillDemonstrates is the regression for
// the services-runtime UAF round (#3): the reproducer built a deterministic
// heap-use-after-free with -fsanitize=address and ran it, but appended a
// trailing `; echo "EXIT_CODE=$?"` (and/or piped through `| tail`), which resets
// the script's exit status to 0. The ASan report was right there in the output,
// yet interpret()'s exit-0 short-circuit fired first and classified it
// exit_zero — discarding a genuine reproduction. A sanitizer/valgrind report
// must demonstrate INDEPENDENTLY of the (masked) exit code.
func TestInterpret_MaskedExitCode_SanitizerStillDemonstrates(t *testing.T) {
	// The exact masking the agent emitted: test piped to tail, trailing echo.
	maskedCmd := []string{"bash", "-c",
		"g++ -fsanitize=address -g repro_uaf.cpp -o /tmp/r && /tmp/r 2>&1 | tail -40; echo \"EXIT_CODE=$?\""}

	t.Run("asan_uaf_exit0_masked_demonstrates", func(t *testing.T) {
		res := sandbox.Result{
			ExitCode: 0, // masked by the trailing echo
			Stdout: "Calling publish() on freed subscription...\n" +
				"=================================================================\n" +
				"==1==ERROR: AddressSanitizer: heap-use-after-free on address 0x602000000010\n" +
				"READ of size 8 at 0x602000000010 thread T0\n" +
				"EXIT_CODE=0\n",
		}
		v := interpret(res, maskedCmd)
		if !v.demonstrated {
			t.Fatalf("masked-exit-0 ASan UAF report must demonstrate; got reason=%q", v.reason)
		}
	})

	t.Run("valgrind_leak_exit0_demonstrates", func(t *testing.T) {
		// valgrind run without --error-exitcode=1 exits with the program's own
		// status (0), but the "definitely lost" report still proves the leak.
		res := sandbox.Result{
			ExitCode: 0,
			Stderr:   "==12== LEAK SUMMARY:\n==12==    definitely lost: 4,096 bytes in 1 blocks\n",
		}
		v := interpret(res, []string{"bash", "-c", "valgrind ./repro | tail -5"})
		if !v.demonstrated {
			t.Fatalf("masked-exit-0 valgrind leak must demonstrate; got reason=%q", v.reason)
		}
	})

	// SAFETY of the high-confidence/loose split: a genuinely PASSING test
	// (exit 0) whose prose merely mentions a loose phrase ("data race",
	// "runtime error:") must NOT be promoted. Those phrases are trusted only
	// behind a non-zero exit, so a clean run stays exit_zero.
	t.Run("exit0_loose_phrase_not_demonstrated", func(t *testing.T) {
		res := sandbox.Result{
			ExitCode: 0,
			Stdout:   "Checking for a data race... none found.\nNo runtime error: all good.\nPASS\n",
		}
		v := interpret(res, []string{"./build/bin/race_probe"})
		if v.demonstrated {
			t.Fatalf("clean exit-0 run mentioning loose phrases must NOT demonstrate")
		}
		if v.reason != VerdictReasonExitZero {
			t.Errorf("reason = %q, want exit_zero", v.reason)
		}
	})

	// A non-zero exit WITH a loose phrase still demonstrates via the full set,
	// preserving the pre-split behavior for non-sanitizer race tools.
	t.Run("nonzero_loose_data_race_demonstrates", func(t *testing.T) {
		res := sandbox.Result{
			ExitCode: 1,
			Stderr:   "==99== Possible data race during write of size 4\n",
		}
		v := interpret(res, []string{"bash", "-c", "valgrind --tool=helgrind ./t"})
		if !v.demonstrated {
			t.Fatalf("non-zero exit with a data-race report must demonstrate; got reason=%q", v.reason)
		}
	})
}

// TestDetectEcosystem_CmakeSuiteCmd asserts that the compound cmake+ctest
// command produced by detectSuiteCmd is classified as the cpp ecosystem by
// detectEcosystem. The command is wrapped in bash -c; unwrapShell must peel
// the wrapper and recognise "cmake" as the first real token.
func TestDetectEcosystem_CmakeSuiteCmd(t *testing.T) {
	cmd := []string{"bash", "-c", "cmake -S . -B build -DCMAKE_BUILD_TYPE=Debug && cmake --build build --parallel 4 && ctest --test-dir build --output-on-failure --no-tests=ignore"}
	got := detectEcosystem(cmd)
	if got.name != sandbox.EcosystemCpp {
		t.Errorf("detectEcosystem(cmake suite cmd) = %q, want %q", got.name, sandbox.EcosystemCpp)
	}
}

// TestDetectEcosystem_MesonSuiteCmd asserts that the compound meson command
// produced by detectSuiteCmd is classified as the cpp ecosystem. The command
// is wrapped in bash -c; unwrapShell peels it and "meson" routes to cpp.
func TestDetectEcosystem_MesonSuiteCmd(t *testing.T) {
	cmd := []string{"bash", "-c", "meson setup build && meson test -C build --print-errorlogs"}
	got := detectEcosystem(cmd)
	if got.name != sandbox.EcosystemCpp {
		t.Errorf("detectEcosystem(meson suite cmd) = %q, want %q", got.name, sandbox.EcosystemCpp)
	}
}
