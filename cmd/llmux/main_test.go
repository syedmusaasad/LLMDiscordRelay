package main

import (
	"encoding/json"

	"github.com/syedmusaasad/LLMDiscordRelay/internal/transcript"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateSourceUsesInstallMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "checkout")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, ".config", "llm-discord-relay", "install-source")
	if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(root+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := updateSource()
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("source = %q, want %q", got, root)
	}
}

// updateRepo is a temp remote plus a clone registered as HOME's install source.
type updateRepo struct {
	home, seed, checkout string
	latest               string // commit the latest tag initially points at
}

func newUpdateRepo(t *testing.T) *updateRepo {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bare := filepath.Join(t.TempDir(), "remote.git")
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, "init", "--bare", bare)
	runGit(t, "init", seed)
	runGitAt(t, seed, "config", "user.email", "test@example.invalid")
	runGitAt(t, seed, "config", "user.name", "Test")
	writeUpdateFixture(t, seed, "latest")
	runGitAt(t, seed, "add", ".")
	runGitAt(t, seed, "commit", "-m", "release")
	latest := strings.TrimSpace(runGitAt(t, seed, "rev-parse", "HEAD"))
	runGitAt(t, seed, "tag", "latest")
	writeUpdateFixture(t, seed, "branch-tip")
	runGitAt(t, seed, "add", ".")
	runGitAt(t, seed, "commit", "-m", "newer branch tip")
	runGitAt(t, seed, "remote", "add", "origin", bare)
	runGitAt(t, seed, "push", "origin", "HEAD:master", "--tags")

	checkout := filepath.Join(t.TempDir(), "checkout")
	runGit(t, "clone", bare, checkout)
	marker := filepath.Join(home, ".config", "llm-discord-relay", "install-source")
	if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte(checkout+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", updateTestPath(t))
	return &updateRepo{home: home, seed: seed, checkout: checkout, latest: latest}
}

func TestLlmuxUpdate_ChecksOutLatestTag(t *testing.T) {
	r := newUpdateRepo(t)
	if err := runUpdate(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitAt(t, r.checkout, "rev-parse", "HEAD")); got != r.latest {
		t.Fatalf("HEAD after update = %s, want latest tag commit %s", got, r.latest)
	}
}

// TestLlmuxUpdate_FollowsMovedLatestTag moves the latest tag in the remote after
// the clone already holds the old tag; a plain `git fetch --tags` would keep it.
func TestLlmuxUpdate_FollowsMovedLatestTag(t *testing.T) {
	r := newUpdateRepo(t)
	if err := runUpdate(); err != nil {
		t.Fatal(err)
	}
	writeUpdateFixture(t, r.seed, "moved")
	runGitAt(t, r.seed, "add", ".")
	runGitAt(t, r.seed, "commit", "-m", "moved release")
	moved := strings.TrimSpace(runGitAt(t, r.seed, "rev-parse", "HEAD"))
	if moved == r.latest {
		t.Fatal("test setup: moved commit equals original")
	}
	runGitAt(t, r.seed, "tag", "-f", "latest")
	runGitAt(t, r.seed, "push", "--force", "origin", "HEAD:master", "refs/tags/latest")

	if err := runUpdate(); err != nil {
		t.Fatalf("update after tag moved: %v", err)
	}
	if got := strings.TrimSpace(runGitAt(t, r.checkout, "rev-parse", "HEAD")); got != moved {
		t.Fatalf("HEAD after update = %s, want moved latest tag commit %s", got, moved)
	}
	got, err := os.ReadFile(filepath.Join(r.home, ".local", "bin", "relay"))
	if err != nil || string(got) != "moved" {
		t.Fatalf("installed relay = %q, %v; want rebuilt %q", got, err, "moved")
	}
}

// TestLlmuxUpdate_ReplacesRunningBinary keeps copies of real executables running
// from the install paths while the update runs. Truncating a running executable
// fails with ETXTBSY; temp-file-plus-rename must succeed.
func TestLlmuxUpdate_ReplacesRunningBinary(t *testing.T) {
	// Resolve a real, non-multicall executable before newUpdateRepo narrows PATH.
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("sh not found: %v", err)
	}
	if shPath, err = filepath.EvalSymlinks(shPath); err != nil {
		t.Fatal(err)
	}
	r := newUpdateRepo(t)
	src, err := os.ReadFile(shPath)
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(r.home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relay", "llmux"} {
		dest := filepath.Join(binDir, name)
		if err := os.WriteFile(dest, src, 0755); err != nil {
			t.Fatal(err)
		}
		// The copy blocks reading stdin, so it stays running for the whole update.
		cmd := exec.Command(dest, "-c", "read _")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start old %s: %v", name, err)
		}
		t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
		// Precondition: the running binary really cannot be opened for writing.
		if f, err := os.OpenFile(dest, os.O_WRONLY|os.O_TRUNC, 0); err == nil {
			f.Close()
			t.Fatalf("precondition failed: running %s was writable, test would not prove ETXTBSY safety", name)
		}
	}
	if err := runUpdate(); err != nil {
		t.Fatalf("update with running binaries: %v", err)
	}
	for _, name := range []string{"relay", "llmux"} {
		got, err := os.ReadFile(filepath.Join(binDir, name))
		if err != nil || string(got) != "latest" {
			t.Fatalf("installed %s = %q, %v; want rebuilt %q", name, got, err, "latest")
		}
		info, _ := os.Stat(filepath.Join(binDir, name))
		if info.Mode().Perm() != 0755 {
			t.Fatalf("%s mode = %v, want 0755", name, info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(binDir)
	if len(entries) != 2 {
		t.Fatalf("leftover temp files in %s: %v", binDir, entries)
	}
}

func updateTestPath(t *testing.T) string {
	t.Helper()
	tools := t.TempDir()
	for _, name := range []string{"git", "make", "mkdir"} {
		bin, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("locate %s: %v", name, err)
		}
		if err := os.Symlink(bin, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	return tools
}

func writeUpdateFixture(t *testing.T, root, version string) {
	t.Helper()
	makefile := "build:\n\tmkdir -p bin\n\tprintf '" + version + "' > bin/relay\n\tprintf '" + version + "' > bin/llmux\n"
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(makefile), 0644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	return runGitAt(t, "", args...)
}

func runGitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestPlanLaunchNewSessionUsesFreshUUID(t *testing.T) {
	plan, err := planLaunch([]string{"--model", "haiku"}, newSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Resume || !transcript.ValidSessionID(plan.SessionID) {
		t.Fatalf("plan = %+v", plan)
	}
	want := []string{"--session-id", plan.SessionID, "--model", "haiku"}
	if strings.Join(plan.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %v, want %v", plan.Args, want)
	}
	other, _ := planLaunch(nil, newSessionID)
	if other.SessionID == plan.SessionID {
		t.Fatal("session ids must be unique per launch")
	}
	// RFC 4122 v4 shape.
	if id := plan.SessionID; id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
		t.Fatalf("not a v4 uuid: %s", id)
	}
}

func TestPlanLaunchResume(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	for _, args := range [][]string{{"--resume", id}, {"--resume=" + id}} {
		plan, err := planLaunch(args, newSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Resume || plan.SessionID != id || strings.Join(plan.Args, " ") != "--resume "+id {
			t.Fatalf("plan = %+v", plan)
		}
	}
	for _, bad := range [][]string{{"--resume"}, {"--resume", "not-a-uuid"}, {"--resume", "*"}, {"--resume", "../x"}} {
		if _, err := planLaunch(bad, newSessionID); err == nil {
			t.Errorf("planLaunch(%v) succeeded, want refusal", bad)
		}
	}
}

func TestBuildAgentCmd(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", "/tmp/it's state")
	t.Setenv("RELAY_CLAUDE_PROJECTS", "")
	t.Setenv("FAKE_CLAUDE_UNTRUSTED", "1")
	t.Setenv("PATH", "")
	got := buildAgentCmd([]string{"/usr/bin/claude", "--model", "haiku"}, []string{"--session-id", "abc"})
	want := `env RELAY_STATE_DIR='/tmp/it'\''s state' FAKE_CLAUDE_UNTRUSTED='1' '/usr/bin/claude' '--model' 'haiku' '--session-id' 'abc'`
	if got != want {
		t.Fatalf("cmd = %s\nwant  %s", got, want)
	}
	if strings.Contains(got, "2>") || strings.Contains(got, "--print-logs") || strings.Contains(got, "--auto") {
		t.Fatal("no stderr redirect or legacy flags")
	}
}

func TestBuildAgentCmdForwardsPATH(t *testing.T) {
	t.Setenv("RELAY_STATE_DIR", "")
	t.Setenv("RELAY_CLAUDE_PROJECTS", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", "/root/.system/bin:/opt/my tools/bin:/usr/bin")
	got := buildAgentCmd([]string{"/usr/bin/claude"}, nil)
	want := `env PATH='/root/.system/bin:/opt/my tools/bin:/usr/bin' '/usr/bin/claude'`
	if got != want {
		t.Fatalf("cmd = %s\nwant  %s", got, want)
	}

	t.Setenv("PATH", `/a/it's$HOME;b`)
	got = buildAgentCmd([]string{"/usr/bin/claude"}, nil)
	want = `env PATH='/a/it'\''s$HOME;b' '/usr/bin/claude'`
	if got != want {
		t.Fatalf("cmd = %s\nwant  %s", got, want)
	}

	t.Setenv("PATH", "")
	got = buildAgentCmd([]string{"/usr/bin/claude"}, nil)
	if strings.Contains(got, "PATH") || strings.Contains(got, "env ") {
		t.Fatalf("empty PATH must add nothing: %s", got)
	}
}

func TestResolveAgentCmd(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "myagent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("RELAY_AGENT_CMD", "myagent --model haiku  --effort low")
	got, err := resolveAgentCmd()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != bin+" --model haiku --effort low" {
		t.Fatalf("agent = %v", got)
	}
	t.Setenv("RELAY_AGENT_CMD", "")
	if _, err := resolveAgentCmd(); err == nil {
		t.Fatal("default claude must not resolve in an empty PATH")
	}
	t.Setenv("RELAY_AGENT_CMD", "nosuchagent")
	if _, err := resolveAgentCmd(); err == nil {
		t.Fatal("unknown agent must fail")
	}
}

func TestWritePendingMarkerIsAtomicJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-llm-1-2")
	m := pendingMarker{Target: "llm-1-2:0.0", SpawnInstant: 5, SessionID: "11111111-2222-4333-8444-555555555555", Resume: true}
	if err := writePendingMarker(path, m); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back pendingMarker
	if err := json.Unmarshal(data, &back); err != nil || back != m {
		t.Fatalf("marker = %+v err=%v", back, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}
