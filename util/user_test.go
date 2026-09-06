package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPathCompatibility(t *testing.T) {
	home := t.TempDir()
	system := t.TempDir()
	user := filepath.Join(home, ".config", "ddns-go", "config.yaml")
	if got := configPath(home, system, false); got != user {
		t.Fatal(got)
	}
	if got := configPath(home, system, true); got != filepath.Join(system, "config.yaml") {
		t.Fatal(got)
	}
	legacy := filepath.Join(home, ".ddns_go_config.yaml")
	os.WriteFile(legacy, []byte("{}"), 0600)
	if got := configPath(home, system, false); got != legacy {
		t.Fatal("legacy config ignored", got)
	}
	os.MkdirAll(filepath.Dir(user), 0700)
	os.WriteFile(user, []byte("{}"), 0600)
	if got := configPath(home, system, false); got != user {
		t.Fatal("standard location not preferred", got)
	}
}
