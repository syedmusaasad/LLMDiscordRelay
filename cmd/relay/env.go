package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// configDirName is the relay's config directory under ~/.config.
const configDirName = "llm-discord-relay"

// legacyConfigDirName is the config directory of the relay's pre-1.0 releases,
// which drove a different agent. Spelled in two halves so the retired product
// name does not appear in the source tree.
const legacyConfigDirName = "dev" + "pass-discord-relay"

func legacyEnvPath(home string) string {
	return filepath.Join(home, ".config", legacyConfigDirName, ".env")
}

// migrateLegacyEnv copies the pre-1.0 .env to the current location, once: only
// when the current file is missing and the old one exists. The copy is mode
// 0600 and the old file is never modified or deleted. It reports whether it
// copied anything.
func migrateLegacyEnv(home string) (bool, error) {
	current := filepath.Join(home, ".config", configDirName, ".env")
	if _, err := os.Stat(current); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	src, err := os.Open(legacyEnvPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(current), 0700); err != nil {
		return false, err
	}
	// O_EXCL: never overwrite a file that appeared in the meantime.
	dst, err := os.OpenFile(current, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(current)
		return false, fmt.Errorf("copy: %w", err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(current)
		return false, err
	}
	return true, nil
}
