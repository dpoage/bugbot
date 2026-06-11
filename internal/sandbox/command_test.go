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

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Plain arguments need no escaping beyond the outer quotes.
		{"hello", "'hello'"},
		{"hello world", "'hello world'"},
		// Embedded single quote: close, escape, reopen.
		{"it's", "'it'\\''s'"},
		// Dollar sign must not be expanded (stays inside single quotes).
		{"$HOME", "'$HOME'"},
		// Semicolon injection attempt: harmless inside single quotes.
		{"; rm -rf /", "'; rm -rf /'"},
		// Empty string: two adjacent single quotes.
		{"", "''"},
		// Multiple single quotes in a row.
		{"a'b'c", "'a'\\''b'\\''c'"},
		// Newline inside arg.
		{"foo\nbar", "'foo\nbar'"},
		// Backslash: no special meaning inside single quotes.
		{`a\b`, `'a\b'`},
		// Command substitution attempts: inert inside single quotes.
		{"$(rm -rf /)", "'$(rm -rf /)'"},
		{"`rm -rf /`", "'`rm -rf /`'"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := shellQuote(tc.in)
			if got != tc.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildSetupScript(t *testing.T) {
	cmds := [][]string{
		{"npm", "ci", "--offline"},
		{"echo", "hello world"},
	}
	script := buildSetupScript(cmds)
	if !strings.Contains(script, "'npm' 'ci' '--offline' || exit 125") {
		t.Errorf("script missing npm cmd with exit 125: %q", script)
	}
	if !strings.Contains(script, "'echo' 'hello world' || exit 125") {
		t.Errorf("script missing echo cmd with exit 125: %q", script)
	}
	if !strings.HasSuffix(script, `exec "$@"`) {
		t.Errorf("script does not end with exec \"$@\": %q", script)
	}

	// An empty argv must be skipped entirely: rendering it would produce a
	// bare "|| exit 125" line, which sh treats as a successful no-op — a
	// silently dead guard.
	withEmpty := buildSetupScript([][]string{{}, {"true"}})
	if strings.Contains(withEmpty, "\n || exit 125") || strings.HasPrefix(withEmpty, " || exit 125") {
		t.Errorf("empty argv must be skipped, got %q", withEmpty)
	}
	if !strings.Contains(withEmpty, "'true' || exit 125") {
		t.Errorf("non-empty argv after an empty one must still render, got %q", withEmpty)
	}
}

func TestBuildRunArgsSetupCmds(t *testing.T) {
	// When SetupCmds are present, the argv must use /bin/sh -c <script> sh
	// followed by the original command (as positional params to exec "$@").
	args := buildRunArgs(runParams{
		containerName: "bugbot-setup",
		workspace:     "/tmp/ws",
		image:         "node:20-alpine",
		network:       "none",
		cmd:           []string{"node", "--version"},
		setupCmds: [][]string{
			{"npm", "ci", "--offline"},
		},
	})

	// /bin/sh -c <script> sh must appear before the original command.
	shIdx := slices.Index(args, "/bin/sh")
	if shIdx < 0 {
		t.Fatalf("expected /bin/sh in args; got %q", args)
	}
	if shIdx+1 >= len(args) || args[shIdx+1] != "-c" {
		t.Fatalf("expected -c after /bin/sh; got %q", args)
	}
	// The script arg follows -c.
	script := args[shIdx+2]
	if !strings.Contains(script, "'npm' 'ci' '--offline' || exit 125") {
		t.Errorf("script missing npm cmd: %q", script)
	}
	if !strings.HasSuffix(script, `exec "$@"`) {
		t.Errorf("script missing exec trailer: %q", script)
	}
	// The sh $0 placeholder follows the script.
	if shIdx+3 >= len(args) || args[shIdx+3] != "sh" {
		t.Fatalf("expected sh ($0) after script; got %q", args)
	}
	// The original command must appear at the tail.
	tail := args[len(args)-2:]
	if !slices.Equal(tail, []string{"node", "--version"}) {
		t.Errorf("original cmd not at tail; got %q", tail)
	}
}

func TestBuildRunArgsNoSetupCmds(t *testing.T) {
	// When SetupCmds is empty, no /bin/sh wrapping occurs and the cmd
	// appears directly after the image — existing behavior is unchanged.
	args := buildRunArgs(runParams{
		containerName: "bugbot-plain",
		workspace:     "/tmp/ws",
		image:         "golang:1.23",
		network:       "none",
		cmd:           []string{"go", "test", "./..."},
	})
	if slices.Contains(args, "/bin/sh") {
		t.Errorf("/bin/sh must not appear when SetupCmds is empty; got %q", args)
	}
	tail := args[len(args)-3:]
	if !slices.Equal(tail, []string{"go", "test", "./..."}) {
		t.Errorf("cmd not at tail; got %q", tail)
	}
}
