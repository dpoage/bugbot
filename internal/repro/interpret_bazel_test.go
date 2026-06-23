package repro

// Tests for the bazel classification contract interpret() must satisfy: a bazel
// `test` run is classified by its authoritative exit code (3 = build OK, tests
// ran, >=1 FAILED = demonstrated; 1/2/4 never; 125/126/127 = env), and the
// benign read-only disk-cache warnings bazel prints on every run must NOT be
// misread as an environment failure.
//
// Kept in a dedicated file so the bazel regression coverage stays isolated from
// the per-ecosystem table tests in interpret_test.go / interpret_cpp_test.go.
//
// Only demonstrated and reason are asserted; summary is left to the
// implementation.

import (
	"testing"

	"github.com/dpoage/bugbot/internal/sandbox"
)

func TestInterpret_Bazel_Contract(t *testing.T) {
	// Single cmd reused across cases: it routes detectEcosystem() to the "bazel"
	// ecosystem via the standard targeted `bazel test //pkg:target` shape.
	cmd := []string{"bazel", "test", "//common/tests:unique_id_test", "--test_output=errors"}

	cases := []struct {
		name string
		res  sandbox.Result
		// want values; demonstrated==true implies reason is the zero value.
		wantDemonstrated bool
		wantReason       VerdictReason
	}{
		{
			// A) Real failure: exit 3 with gtest output AND the
			// read-only-cache warning. The ROCache warning is
			// present so we prove interpret() does not mistake
			// this for an environment failure.
			name: "exit_3_with_gtest_failure_demonstrated",
			res: sandbox.Result{
				ExitCode: 3,
				Stdout: "[ RUN      ] UniqueID.Construction\n" +
					"common/tests/unique_id_test.cc:16: Failure\n" +
					"[  FAILED  ] UniqueID.Construction (0 ms)\n" +
					"[  FAILED  ] 1 test, listed below:\n" +
					" 1 FAILED TEST\n" +
					"//common/tests:unique_id_test                                            FAILED in 0.0s\n" +
					"Executed 1 out of 1 test: 1 fails locally.\n",
				Stderr: "WARNING: Remote Cache: /bazel-cache/ac/2f/... (Read-only file system)\n" +
					"java.io.IOException: /bazel-cache/cas/24/... (Read-only file system)\n" +
					"INFO: Build completed, 1 test FAILED, 4 total actions\n",
			},
			wantDemonstrated: true,
		},
		{
			// B) Exit 0, passing: NOT a demonstration, even with the
			// read-only-cache warning on stderr (the cache noise is
			// benign on passing runs too).
			name: "exit_0_passing_not_demonstrated",
			res: sandbox.Result{
				ExitCode: 0,
				Stdout: "//common/tests:unique_id_test                                            PASSED in 0.0s\n" +
					"Executed 1 out of 1 test: 1 test passes.\n",
				Stderr: "WARNING: Remote Cache: /bazel-cache/ac/.. (Read-only file system)\n" +
					"INFO: Build completed successfully, 53 total actions\n",
			},
			wantDemonstrated: false,
			wantReason:       VerdictReasonExitZero,
		},
		{
			// C) Exit 1, no such target: a build_error, NOT a
			// demonstration. The test never ran.
			name: "exit_1_no_such_target_build_error",
			res: sandbox.Result{
				ExitCode: 1,
				Stderr: "ERROR: no such target '//common/tests:does_not_exist': target 'does_not_exist' not declared in package 'common/tests'\n" +
					"ERROR: Build did NOT complete successfully\n" +
					"ERROR: Couldn't start the build. Unable to run tests\n",
			},
			wantDemonstrated: false,
			wantReason:       VerdictReasonBuildError,
		},
		{
			// D) Exit 1, analysis/lockfile abort: also a build_error,
			// NOT a demonstration. The lockfile is out of date; the
			// test never ran.
			name: "exit_1_analysis_abort_build_error",
			res: sandbox.Result{
				ExitCode: 1,
				Stderr: "ERROR: Analysis of target '//common/tests:unique_id_test' failed; build aborted: MODULE.bazel.lock is no longer up-to-date\n" +
					"ERROR: Build did NOT complete successfully\n" +
					"ERROR: No test targets were found, yet testing was requested\n",
			},
			wantDemonstrated: false,
			wantReason:       VerdictReasonBuildError,
		},
		{
			// E) Exit 127: environment_error. bazel binary missing
			// from the image; not a demonstration.
			name: "exit_127_command_not_found_environment_error",
			res: sandbox.Result{
				ExitCode: 127,
				Stderr:   "sh: bazel: not found\n",
			},
			wantDemonstrated: false,
			wantReason:       VerdictReasonEnvironmentError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := interpret(tc.res, cmd)
			if v.demonstrated != tc.wantDemonstrated {
				t.Errorf("demonstrated = %v, want %v (reason=%q)",
					v.demonstrated, tc.wantDemonstrated, v.reason)
			}
			// When the run is a genuine demonstration, reason is the
			// zero value of VerdictReason (""). For non-demonstrating
			// runs, reason must match the contractually-expected
			// category.
			if tc.wantDemonstrated {
				if v.reason != "" {
					t.Errorf("demonstrated run must have zero reason, got %q", v.reason)
				}
				return
			}
			if v.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", v.reason, tc.wantReason)
			}
		})
	}
}
