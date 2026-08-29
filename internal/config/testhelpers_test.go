package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// repoConfigPath はリポジトリ直下の config.yaml の絶対パスを返す。
func repoConfigPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/config/testhelpers_test.go からリポジトリ直下へ。
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "config.yaml")
}
