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

func TestBuildRunArgsRendersReadOnlyMounts(t *testing.T) {
	args := buildRunArgs(runParams{
		containerName: "bugbot-ro",
		workspace:     "/tmp/ws",
		image:         "img",
		network:       "none",
		cmd:           []string{"true"},
		roMounts: []ROMount{
			// Shared=false (default): bugbot-owned dir gets :ro,Z for SELinux isolation.
			{HostPath: "/host/modcache", ContainerPath: "/modcache"},
			{HostPath: "/host/other", ContainerPath: "/other"},
		},
	})

	// Non-shared RO mounts are rendered :ro,Z and never rw.
	mustContainSeq(t, args, "-v", "/host/modcache:/modcache:ro,Z")
	mustContainSeq(t, args, "-v", "/host/other:/other:ro,Z")

	// The workspace must still be the only rw mount.
	mustContainSeq(t, args, "-v", "/tmp/ws:/workspace:rw,Z")
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			val := args[i+1]
			if strings.Contains(val, "modcache") && strings.Contains(val, "rw,") {
				t.Errorf("modcache mount must be read-only, got %q", val)
			}
		}
	}

	// Ordering: RO mounts render immediately after the workspace mount.
	wsIdx := slices.Index(args, "/tmp/ws:/workspace:rw,Z")
	roIdx := slices.Index(args, "/host/modcache:/modcache:ro,Z")
	if wsIdx < 0 || roIdx < 0 || roIdx < wsIdx {
		t.Errorf("RO mount must follow workspace mount; ws=%d ro=%d args=%q", wsIdx, roIdx, args)
	}
	// And RO mounts must come before the image/command.
	imgIdx := slices.Index(args, "img")
	if imgIdx < 0 || roIdx > imgIdx {
		t.Errorf("RO mount must precede image; ro=%d img=%d", roIdx, imgIdx)
	}
}

func TestBuildRunArgsSharedMountNoRelabel(t *testing.T) {
	// Shared=true (host Go module cache): must render :ro with NO :Z suffix.
	// :Z on a shared host dir relabels it to a container-private MCS label,
	// which is slow on large caches and can break the host go toolchain.
	args := buildRunArgs(runParams{
		containerName: "bugbot-shared",
		workspace:     "/tmp/ws",
		image:         "img",
		network:       "none",
		cmd:           []string{"true"},
		roMounts: []ROMount{
			{HostPath: "/home/user/go/pkg/mod", ContainerPath: "/modcache", Shared: true},
		},
	})

	// Shared mount must render :ro with NO ,Z suffix.
	mustContainSeq(t, args, "-v", "/home/user/go/pkg/mod:/modcache:ro")

	// Verify :Z is absent from the shared mount entry.
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			val := args[i+1]
			if strings.Contains(val, "/modcache") && strings.Contains(val, ",Z") {
				t.Errorf("shared mount must not have ,Z relabel suffix, got %q", val)
			}
		}
	}
}

func TestBuildRunArgsNonSharedMountRelabel(t *testing.T) {
	// Shared=false (default, bugbot-owned dir): must render :ro,Z.
	args := buildRunArgs(runParams{
		containerName: "bugbot-owned",
		workspace:     "/tmp/ws",
		image:         "img",
		network:       "none",
		cmd:           []string{"true"},
		roMounts: []ROMount{
			{HostPath: "/home/user/.cache/bugbot/modcache/abc123", ContainerPath: "/modcache", Shared: false},
		},
	})
	mustContainSeq(t, args, "-v", "/home/user/.cache/bugbot/modcache/abc123:/modcache:ro,Z")
}

func TestBuildRunArgsRendersWritableMounts(t *testing.T) {
	args := buildRunArgs(runParams{
		containerName: "bugbot-rw",
		workspace:     "/tmp/ws",
		image:         "img",
		network:       "bridge",
		cmd:           []string{"go", "mod", "download"},
		rwMounts:      []ROMount{{HostPath: "/host/cache", ContainerPath: "/modcache"}},
	})
	mustContainSeq(t, args, "-v", "/host/cache:/modcache:rw,Z")
	mustContainSeq(t, args, "--network=bridge")
}

func TestValidateMounts(t *testing.T) {
	tests := []struct {
		name    string
		ro, rw  []ROMount
		wantErr bool
	}{
		{"empty", nil, nil, false},
		{"valid ro", []ROMount{{HostPath: "/h", ContainerPath: "/c"}}, nil, false},
		{"valid rw", nil, []ROMount{{HostPath: "/h", ContainerPath: "/c"}}, false},
		{"empty host", []ROMount{{HostPath: "", ContainerPath: "/c"}}, nil, true},
		{"empty ctr", []ROMount{{HostPath: "/h", ContainerPath: ""}}, nil, true},
		{"relative host", []ROMount{{HostPath: "rel", ContainerPath: "/c"}}, nil, true},
		{"relative ctr", []ROMount{{HostPath: "/h", ContainerPath: "rel"}}, nil, true},
		{"dup within ro", []ROMount{{HostPath: "/a", ContainerPath: "/c"}, {HostPath: "/b", ContainerPath: "/c"}}, nil, true},
		{"dup across ro/rw", []ROMount{{HostPath: "/a", ContainerPath: "/c"}}, []ROMount{{HostPath: "/b", ContainerPath: "/c"}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMounts(tc.ro, tc.rw)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateMounts(%v,%v) err=%v wantErr=%v", tc.ro, tc.rw, err, tc.wantErr)
			}
		})
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
