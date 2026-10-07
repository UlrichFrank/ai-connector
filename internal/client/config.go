package client

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// Config is ~/.config/mlxlab/client.json: the server the TUI talks to and its device token there.
type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// ConfigPath is where the config lives.
func ConfigPath() string { return filepath.Join(engine.ConfigDir(), "client.json") }

// LoadConfig reads the config (a zero one if there is none yet).
func LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// SaveConfig writes the config, readable only by us: the token signs in as this device.
func SaveConfig(c Config) error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	_ = os.Remove(tmp) // WriteFile keeps the mode of a file that is there
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
