package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateLegacyEnvCopiesOnceAndKeepsOriginal(t *testing.T) {
	home := t.TempDir()
	old := legacyEnvPath(home)
	writeEnv(t, old, "DISCORD_TOKEN=abc\n", 0644)

	migrated, err := migrateLegacyEnv(home)
	if err != nil || !migrated {
		t.Fatalf("migrate = %v, %v", migrated, err)
	}
	current := filepath.Join(home, ".config", configDirName, ".env")
	data, err := os.ReadFile(current)
	if err != nil || string(data) != "DISCORD_TOKEN=abc\n" {
		t.Fatalf("copied file = %q, %v", data, err)
	}
	if info, _ := os.Stat(current); info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("original must be left in place: %v", err)
	}

	// A second run, and a run after the user edited the new file, never overwrite.
	writeEnv(t, current, "DISCORD_TOKEN=edited\n", 0600)
	if migrated, err := migrateLegacyEnv(home); migrated || err != nil {
		t.Fatalf("second migrate = %v, %v", migrated, err)
	}
	if data, _ := os.ReadFile(current); string(data) != "DISCORD_TOKEN=edited\n" {
		t.Fatalf("existing file was overwritten: %q", data)
	}
}

func TestMigrateLegacyEnvNoSource(t *testing.T) {
	home := t.TempDir()
	if migrated, err := migrateLegacyEnv(home); migrated || err != nil {
		t.Fatalf("migrate = %v, %v", migrated, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", configDirName)); err == nil {
		t.Fatal("no config dir should be created without a source")
	}
}

func TestLoadEnvOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	explicit := filepath.Join(t.TempDir(), "explicit.env")
	writeEnv(t, explicit, "RELAY_TEST_ORDER=explicit\n", 0600)
	writeEnv(t, filepath.Join(home, ".config", configDirName, ".env"), "RELAY_TEST_ORDER=config\n", 0600)

	t.Setenv("RELAY_TEST_ORDER", "")
	t.Setenv("LLM_RELAY_ENV", explicit)
	if err := loadEnv(); err != nil || os.Getenv("RELAY_TEST_ORDER") != "explicit" {
		t.Fatalf("explicit: %v %q", err, os.Getenv("RELAY_TEST_ORDER"))
	}
	t.Setenv("RELAY_TEST_ORDER", "")
	t.Setenv("LLM_RELAY_ENV", "")
	if err := loadEnv(); err != nil || os.Getenv("RELAY_TEST_ORDER") != "config" {
		t.Fatalf("config dir: %v %q", err, os.Getenv("RELAY_TEST_ORDER"))
	}
}

func TestLoadEnvMigratesLegacyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LLM_RELAY_ENV", "")
	t.Setenv("RELAY_TEST_MIGRATED", "")
	writeEnv(t, legacyEnvPath(home), "RELAY_TEST_MIGRATED=yes\n", 0600)
	if err := loadEnv(); err != nil || os.Getenv("RELAY_TEST_MIGRATED") != "yes" {
		t.Fatalf("loadEnv: %v %q", err, os.Getenv("RELAY_TEST_MIGRATED"))
	}
	if _, err := os.Stat(filepath.Join(home, ".config", configDirName, ".env")); err != nil {
		t.Fatalf("migrated file missing: %v", err)
	}
}
