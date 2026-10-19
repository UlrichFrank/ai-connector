package main

// The discovery file of the local apps: `ai-connector vboard` writes the address of its loopback hub and a client
// token to <config>/local-ai.json (mode 0600, so only this user's apps read it), and removes it when it ends. An app
// on this machine (EDDy) reads it and uses the OpenAI API (docs/api.md, "Local apps") — the models of this computer,
// loaded once for all of them.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const localAIFile = "local-ai.json"

type localAI struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// writeLocalAI writes the file in one piece (a reader never sees half of it).
func writeLocalAI(root string, info localAI) error {
	b, _ := json.MarshalIndent(info, "", "  ")
	tmp, err := os.CreateTemp(root, localAIFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // after the rename: nothing there any more
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(root, localAIFile))
}

// removeLocalAI removes the file when it is still this process's (another one may have taken over).
func removeLocalAI(root string, pid int) {
	file := filepath.Join(root, localAIFile)
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var info localAI
	if json.Unmarshal(b, &info) == nil && info.PID == pid {
		_ = os.Remove(file)
	}
}
