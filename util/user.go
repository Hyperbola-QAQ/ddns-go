package util

import (
	"os"
	"path/filepath"
	"runtime"
)

const ConfigFilePathENV = "DDNS_CONFIG_FILE_PATH"

func GetConfigFilePath() string {
	if p := os.Getenv(ConfigFilePathENV); p != "" {
		return p
	}
	return GetConfigFilePathDefault()
}

func GetConfigFilePathDefault() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS != "linux" {
		if home == "" {
			return "../.ddns_go_config.yaml"
		}
		return filepath.Join(home, ".ddns_go_config.yaml")
	}
	return configPath(home, "/etc/ddns-go", os.Geteuid() == 0)
}
func configPath(home, system string, root bool) string {
	user := filepath.Join(home, ".config", "ddns-go", "config.yaml")
	systemFile := filepath.Join(system, "config.yaml")
	// Keep installed configurations and explicit -c / environment overrides working.
	for _, p := range []string{user, systemFile, filepath.Join(home, ".ddns_go_config.yaml")} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if root || home == "" {
		return systemFile
	}
	return user
}
