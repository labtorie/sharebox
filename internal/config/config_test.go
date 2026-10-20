package config

import "testing"

func TestDefaultServer(t *testing.T) {
	t.Setenv("SHAREBOX_SERVER", "")
	old := DefaultServerURL
	defer func() { DefaultServerURL = old }()
	DefaultServerURL = "https://share.example.org"

	s := &Store{Dir: t.TempDir()}
	if got := s.LoadSettings().ServerURL; got != "https://share.example.org" {
		t.Fatalf("fresh install: %q", got)
	}
	// An explicitly chosen server is kept.
	st := s.LoadSettings()
	st.ServerURL = "https://own.example.net"
	s.SaveSettings(st)
	if got := s.LoadSettings().ServerURL; got != "https://own.example.net" {
		t.Fatalf("custom server lost: %q", got)
	}
	// Settings saved by an older version without a server get the default.
	st.ServerURL = ""
	s.SaveSettings(st)
	if got := s.LoadSettings().ServerURL; got != "https://share.example.org" {
		t.Fatalf("empty setting not defaulted: %q", got)
	}
	// The development override wins.
	t.Setenv("SHAREBOX_SERVER", "http://127.0.0.1:8080")
	if got := DefaultServer(); got != "http://127.0.0.1:8080" {
		t.Fatalf("env override: %q", got)
	}
}

func TestFormerDefaultServerMigrates(t *testing.T) {
	t.Setenv("SHAREBOX_SERVER", "")
	s := &Store{Dir: t.TempDir()}
	for _, saved := range []string{formerDefaults[0], formerDefaults[0] + "/"} {
		st := Defaults()
		st.ServerURL = saved
		if err := s.SaveSettings(st); err != nil {
			t.Fatal(err)
		}
		if got := s.LoadSettings().ServerURL; got != DefaultServerURL {
			t.Fatalf("saved %q loaded as %q", saved, got)
		}
	}
	st := Defaults()
	st.ServerURL = "https://own.example.org"
	s.SaveSettings(st)
	if got := s.LoadSettings().ServerURL; got != "https://own.example.org" {
		t.Fatalf("custom server replaced: %q", got)
	}
}
