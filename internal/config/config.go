// Package config persists host settings and device identity.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"sharebox/internal/fileserver"
	"sharebox/internal/security"
)

// Settings are user preferences. The share password is intentionally never
// persisted.
type Settings struct {
	Folder             string                 `json:"folder"`
	ServerURL          string                 `json:"serverUrl"`
	Perms              fileserver.Permissions `json:"permissions"`
	MaxUploadMB        int64                  `json:"maxUploadMB"`   // 0 = unlimited
	MinFreeMB          int64                  `json:"minFreeMB"`     // reserve
	MaxConcurrent      int                    `json:"maxConcurrent"` // transfers
	ExpiryHours        int                    `json:"expiryHours"`   // 0 = never
	PortMapping        bool                   `json:"portMapping"`
	KeepRunningOnClose bool                   `json:"keepRunningOnClose"`
	Theme              string                 `json:"theme"` // system | light | dark
	ShowQR             bool                   `json:"showQr"`
}

// Defaults returns safe defaults (deletion disabled).
func Defaults() Settings {
	return Settings{
		Perms: fileserver.DefaultPermissions(), MinFreeMB: 1024, MaxConcurrent: 8,
		PortMapping: true, KeepRunningOnClose: true, Theme: "system", ShowQR: true,
		ServerURL: os.Getenv("SHAREBOX_SERVER"),
	}
}

// Device is this installation's identity towards the coordination server.
type Device struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	UDPPort int    `json:"udpPort"`
	TCPPort int    `json:"tcpPort"`
}

// Store reads and writes files in the app's config directory.
type Store struct {
	Dir string
	mu  sync.Mutex
}

// DefaultStore uses the OS config dir (e.g. ~/Library/Application Support/Sharebox).
func DefaultStore() (*Store, error) {
	dir := os.Getenv("SHAREBOX_CONFIG_DIR")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "Sharebox")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) read(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(s.Dir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// write saves atomically (temp file + rename).
func (s *Store) write(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, name))
}

// LoadSettings returns saved settings or defaults.
func (s *Store) LoadSettings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Defaults()
	if err := s.read("settings.json", &st); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Defaults()
	}
	if st.MaxConcurrent <= 0 {
		st.MaxConcurrent = 8
	}
	return st
}

// SaveSettings persists settings.
func (s *Store) SaveSettings(st Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write("settings.json", st)
}

// Device returns (creating if needed) the device identity.
func (s *Store) Device() Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	var d Device
	if err := s.read("device.json", &d); err != nil || d.ID == "" || d.Key == "" {
		d = Device{ID: "d" + security.RandomToken(12), Key: security.RandomToken(32)}
		_ = s.write("device.json", d)
	}
	return d
}

// SaveDevice persists the device identity (e.g. chosen ports).
func (s *Store) SaveDevice(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write("device.json", d)
}

// Path returns a file path inside the store.
func (s *Store) Path(name string) string { return filepath.Join(s.Dir, name) }
