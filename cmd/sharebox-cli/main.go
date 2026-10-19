// Command sharebox-cli shares a folder without the desktop UI (servers,
// NAS boxes, automation and testing).
//
//	sharebox-cli -server https://share.example.org -folder ~/Photos
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sharebox/internal/config"
	"sharebox/internal/host"
)

func main() {
	store, err := config.DefaultStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st := store.LoadSettings()
	folder := flag.String("folder", st.Folder, "folder to share")
	server := flag.String("server", st.ServerURL, "Sharebox server URL (or SHAREBOX_SERVER)")
	password := flag.String("password", os.Getenv("SHAREBOX_PASSWORD"), "optional share password")
	noMap := flag.Bool("no-portmap", !st.PortMapping, "disable automatic router port mapping")
	upload := flag.Bool("upload", st.Perms.Upload, "allow uploads")
	mkdir := flag.Bool("mkdir", st.Perms.Mkdir, "allow creating folders")
	del := flag.Bool("delete", false, "allow deleting (off by default)")
	download := flag.Bool("download", st.Perms.Download, "allow downloads")
	expiry := flag.Int("expiry-hours", st.ExpiryHours, "link lifetime in hours (0 = until stopped)")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	st.Folder, st.ServerURL, st.PortMapping, st.ExpiryHours = *folder, *server, !*noMap, *expiry
	st.Perms.Upload, st.Perms.Mkdir, st.Perms.Delete, st.Perms.Download = *upload, *mkdir, *del, *download

	e := host.New(store, log)
	if err := e.Start(st, host.StartOptions{Password: *password}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	tick := time.NewTicker(time.Second)
	lastLine, printed := "", false
	for {
		select {
		case <-sig:
			fmt.Println("\nStopping…")
			e.Stop()
			return
		case <-tick.C:
		}
		s := e.Status()
		line := fmt.Sprintf("%s | server:%s direct:%s p2p:%s relay:%s mapping:%s | guests:%d ↑%s/s ↓%s/s",
			s.Headline, s.Server.State, s.Direct.State, s.P2P.State, s.Relay.State, s.Mapping.State,
			len(s.Stats.Guests), human(s.Stats.UpSpeed), human(s.Stats.DownSpeed))
		if line != lastLine {
			fmt.Println(line)
			lastLine = line
		}
		if s.LinkReady && !printed {
			printed = true
			fmt.Printf("\nShare link:\n  %s\n\nLocal preview: %s\n\n", s.Link, s.PreviewURL)
			for _, c := range []struct {
				n string
				c host.Check
			}{{"Direct", s.Direct}, {"P2P", s.P2P}, {"Relay", s.Relay}, {"Mapping", s.Mapping}} {
				if c.c.Detail != "" {
					fmt.Printf("  %-8s %s\n", c.n+":", c.c.Detail)
				}
			}
		}
	}
}

func human(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", b/(1<<10))
	}
	return fmt.Sprintf("%.0fB", b)
}
