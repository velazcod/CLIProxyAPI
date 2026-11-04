package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

func TestConfigWatchSurvivesAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.Mkdir(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{
		Port:               8080,
		AuthDir:            authDir,
		CredentialInFlight: config.DefaultCredentialInFlightConfig(),
	}
	writeConfig := func(path, userAgent string) {
		t.Helper()
		next := *cfg
		next.ClaudeHeaderDefaults.UserAgent = userAgent
		data, err := yaml.Marshal(&next)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(configPath, "watch-test/1.0.0")
	cfg.ClaudeHeaderDefaults.UserAgent = "watch-test/1.0.0"

	reloads := make(chan string, 16)
	w, err := NewWatcher(configPath, authDir, func(updated *config.Config) {
		reloads <- updated.ClaudeHeaderDefaults.UserAgent
	})
	if err != nil {
		t.Fatal(err)
	}
	w.SetConfig(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if err := w.Stop(); err != nil {
			t.Error(err)
		}
	})
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	awaitReload := func(want string) {
		t.Helper()
		select {
		case got := <-reloads:
			if got != want {
				t.Fatalf("reload user agent = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for reload with user agent %q", want)
		}
	}
	lastHash := ""
	awaitHashChange := func() {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			w.clientsMutex.RLock()
			hash := w.lastConfigHash
			w.clientsMutex.RUnlock()
			if hash != "" && hash != lastHash {
				lastHash = hash
				return
			}
			select {
			case <-tick.C:
			case <-deadline.C:
				t.Fatal("timed out waiting for config reload to finish")
			}
		}
	}
	awaitReload("watch-test/1.0.0")

	// Linux drops file-inode watches after replacement; watch the parent instead.
	watchingParent := false
	for _, path := range w.watcher.WatchList() {
		if w.normalizeAuthPath(path) == w.normalizeAuthPath(dir) {
			watchingParent = true
			break
		}
	}
	if !watchingParent {
		t.Errorf("config parent directory is not watched: %v", w.watcher.WatchList())
	}

	// Change only fingerprint defaults, first with a normal write.
	writeConfig(configPath, "watch-test/2.0.0")
	awaitReload("watch-test/2.0.0")
	awaitHashChange()

	replacement := filepath.Join(dir, "replacement.yaml")
	writeConfig(replacement, "watch-test/3.0.0")
	if err := os.Rename(replacement, configPath); err != nil {
		t.Fatal(err)
	}
	awaitReload("watch-test/3.0.0")
	awaitHashChange()

	// The watch must still receive events for the replacement inode.
	writeConfig(configPath, "watch-test/4.0.0")
	awaitReload("watch-test/4.0.0")
	awaitHashChange()

	writeConfig(filepath.Join(dir, "unrelated.yaml"), "watch-test/5.0.0")
	select {
	case got := <-reloads:
		t.Fatalf("unrelated sibling write triggered reload with user agent %q", got)
	case <-time.After(3 * configReloadDebounce):
	}
}

func TestConfigWatchIgnoresSiblingEvents(t *testing.T) {
	for _, op := range []fsnotify.Op{fsnotify.Write, fsnotify.Create, fsnotify.Rename} {
		t.Run(op.String(), func(t *testing.T) {
			dir := t.TempDir()
			w := &Watcher{configPath: filepath.Join(dir, "config.yaml"), authDir: filepath.Join(dir, "auths")}
			defer w.stopConfigReloadTimer()
			w.handleEvent(fsnotify.Event{Name: filepath.Join(dir, "unrelated.yaml"), Op: op})
			w.configReloadMu.Lock()
			scheduled := w.configReloadTimer != nil
			w.configReloadMu.Unlock()
			if scheduled {
				t.Fatal("unrelated sibling event scheduled config reload")
			}
		})
	}
}
