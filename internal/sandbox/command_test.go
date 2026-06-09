package sandbox

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildRunArgsSecurityFlags(t *testing.T) {
	args := buildRunArgs(runParams{
		containerName: "bugbot-abc",
		workspace:     "/tmp/ws",
		image:         "alpine:latest",
		network:       "none",
		cpus:          1.5,
		memoryMB:      512,
		pidsLimit:     128,
		env:           []string{"FOO=bar", "BAZ=qux"},
		cmd:           []string{"sh", "-c", "echo hi"},
	})

	joined := strings.Join(args, " ")

	// Subcommand and security-relevant flags must all be present.
	mustContainSeq(t, args, "run")
	mustContainSeq(t, args, "--rm")
	mustContainSeq(t, args, "--network=none")
	mustContainSeq(t, args, "--read-only")
	mustContainSeq(t, args, "--cap-drop", "ALL")
	mustContainSeq(t, args, "--security-opt", "no-new-privileges")
	mustContainSeq(t, args, "--name", "bugbot-abc")
	mustContainSeq(t, args, "--workdir", workspaceMount)
	mustContainSeq(t, args, "--pids-limit", "128")
	mustContainSeq(t, args, "--memory", "512m")
	mustContainSeq(t, args, "--cpus", "1.5")
	mustContainSeq(t, args, "-v", "/tmp/ws:/workspace:rw,Z")
	mustContainSeq(t, args, "--env", "FOO=bar")
	mustContainSeq(t, args, "--env", "BAZ=qux")

	// Image must appear after the flags and before the command.
	imgIdx := slices.Index(args, "alpine:latest")
	cmdIdx := slices.Index(args, "echo hi")
	if imgIdx < 0 || cmdIdx < 0 || imgIdx > cmdIdx {
		t.Fatalf("image must precede command; args=%q", joined)
	}
	// Command tail must be exactly the spec command, in order, at the end.
	tail := args[len(args)-3:]
	if !slices.Equal(tail, []string{"sh", "-c", "echo hi"}) {
		t.Errorf("command tail = %q, want sh -c 'echo hi'", tail)
	}
}

func TestBuildRunArgsOmitsZeroLimits(t *testing.T) {
	args := buildRunArgs(runParams{
		containerName: "bugbot-x",
		workspace:     "/ws",
		image:         "img",
		network:       "none",
		cpus:          0,
		memoryMB:      0,
		pidsLimit:     0,
		cmd:           []string{"true"},
	})
	for _, flag := range []string{"--cpus", "--memory", "--pids-limit"} {
		if slices.Contains(args, flag) {
			t.Errorf("expected %s to be omitted when its value is zero; args=%q", flag, args)
		}
	}
}

func TestRemoveArgs(t *testing.T) {
	if got := removeArgs("bugbot-x"); !slices.Equal(got, []string{"rm", "-f", "bugbot-x"}) {
		t.Errorf("removeArgs = %q", got)
	}
}

// mustContainSeq asserts that the contiguous subsequence seq appears in args.
func mustContainSeq(t *testing.T, args []string, seq ...string) {
	t.Helper()
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return
		}
	}
	t.Errorf("expected args to contain sequence %q; got %q", seq, args)
}
