package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// writeConfig writes a config file to a temp dir and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lcars.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
	if cfg.File != "" {
		t.Errorf("File = %q, want none", cfg.File)
	}
	if cfg.Calendar.Dir != "" || cfg.Calendar.RefreshInterval != time.Hour {
		t.Errorf("Calendar = %+v, want disabled with 1h refresh", cfg.Calendar)
	}
	if cfg.Location == nil {
		t.Error("Location is nil")
	}
}

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(env(map[string]string{
		"LCARS_ADDR":                      "127.0.0.1:9000",
		"LCARS_LOG_LEVEL":                 "debug",
		"LCARS_LOG_FORMAT":                "text",
		"LCARS_SHUTDOWN_TIMEOUT":          "3s",
		"LCARS_TIMEZONE":                  "Europe/Berlin",
		"LCARS_CALENDAR_DIR":              dir,
		"LCARS_CALENDAR_REFRESH_INTERVAL": "5m",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9000" || cfg.LogLevel != slog.LevelDebug ||
		cfg.LogFormat != "text" || cfg.ShutdownTimeout != 3*time.Second ||
		cfg.Location.String() != "Europe/Berlin" ||
		cfg.Calendar.Dir != dir || cfg.Calendar.RefreshInterval != 5*time.Minute {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadFile(t *testing.T) {
	path := writeConfig(t, `
addr = "127.0.0.1:9001"
log_level = "warn"
timezone = "Asia/Tokyo"

[calendar]
dir = "ics"
refresh_interval = "15m"
`)
	cfg, err := Load(env(map[string]string{"LCARS_CONFIG": path}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.File != path {
		t.Errorf("File = %q, want %q", cfg.File, path)
	}
	if cfg.Addr != "127.0.0.1:9001" || cfg.LogLevel != slog.LevelWarn || cfg.Location.String() != "Asia/Tokyo" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	// Relative paths are relative to the config file, not the working directory.
	if want := filepath.Join(filepath.Dir(path), "ics"); cfg.Calendar.Dir != want {
		t.Errorf("Calendar.Dir = %q, want %q", cfg.Calendar.Dir, want)
	}
	if cfg.Calendar.RefreshInterval != 15*time.Minute {
		t.Errorf("RefreshInterval = %v, want 15m", cfg.Calendar.RefreshInterval)
	}
	// Settings not in the file keep their defaults.
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want default json", cfg.LogFormat)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, "addr = \"127.0.0.1:9001\"\nlog_format = \"text\"\n")
	cfg, err := Load(env(map[string]string{
		"LCARS_CONFIG": path,
		"LCARS_ADDR":   "127.0.0.1:9002",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9002" {
		t.Errorf("Addr = %q, want env value 127.0.0.1:9002", cfg.Addr)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want file value text", cfg.LogFormat)
	}
}

func TestDefaultFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DefaultFile), []byte(`addr = ":7000"`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.File != DefaultFile || cfg.Addr != ":7000" {
		t.Errorf("File = %q, Addr = %q; want %s and :7000", cfg.File, cfg.Addr, DefaultFile)
	}
}

func TestHomeDirExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Load(env(map[string]string{"LCARS_CALENDAR_DIR": "~/Calendars"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, "Calendars"); cfg.Calendar.Dir != want {
		t.Errorf("Calendar.Dir = %q, want %q", cfg.Calendar.Dir, want)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := map[string]struct {
		file    string // config file content; "" means no file
		env     map[string]string
		wantErr string
	}{
		"bad level":            {env: map[string]string{"LCARS_LOG_LEVEL": "loud"}, wantErr: "LCARS_LOG_LEVEL"},
		"bad format":           {env: map[string]string{"LCARS_LOG_FORMAT": "xml"}, wantErr: "LCARS_LOG_FORMAT"},
		"bad duration":         {env: map[string]string{"LCARS_SHUTDOWN_TIMEOUT": "soon"}, wantErr: "LCARS_SHUTDOWN_TIMEOUT"},
		"bad timezone":         {env: map[string]string{"LCARS_TIMEZONE": "Mars/Olympus_Mons"}, wantErr: "LCARS_TIMEZONE"},
		"refresh too fast":     {env: map[string]string{"LCARS_CALENDAR_REFRESH_INTERVAL": "10ms"}, wantErr: "at least 1s"},
		"missing named file":   {env: map[string]string{"LCARS_CONFIG": "/nonexistent/lcars.toml"}, wantErr: "read config file"},
		"invalid toml":         {file: "addr = ", wantErr: "parse config file"},
		"unknown key":          {file: "listen = \":80\"", wantErr: `unknown setting "listen"`},
		"unknown nested key":   {file: "[calendar]\ndirectory = \"x\"", wantErr: `unknown setting "calendar.directory"`},
		"non-string value":     {file: "[calendar]\nrefresh_interval = 60", wantErr: "calendar.refresh_interval: must be a string"},
		"bad value in file":    {file: "log_format = \"xml\"", wantErr: "log_format: must be json or text"},
		"bad timezone in file": {file: "timezone = \"Nowhere\"", wantErr: "timezone: unknown timezone"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			vars := map[string]string{}
			for k, v := range tt.env {
				vars[k] = v
			}
			if tt.file != "" {
				vars["LCARS_CONFIG"] = writeConfig(t, tt.file)
			}
			_, err := Load(env(vars))
			if err == nil {
				t.Fatal("Load: want error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestExampleFile checks that lcars.example.toml documents every setting with
// its environment variable, and that it is valid once uncommented.
func TestExampleFile(t *testing.T) {
	data, err := os.ReadFile("../../lcars.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	example := string(data)

	for _, s := range settings {
		name := s.key[strings.LastIndex(s.key, ".")+1:]
		if !regexp.MustCompile(`(?m)^# ` + regexp.QuoteMeta(name) + ` = `).MatchString(example) {
			t.Errorf("example file has no commented-out %q setting", s.key)
		}
		if !strings.Contains(example, "Env: "+s.env) {
			t.Errorf("example file does not mention %s", s.env)
		}
	}

	t.Setenv("HOME", t.TempDir())
	uncommented := regexp.MustCompile(`(?m)^# (\w+ = )`).ReplaceAllString(example, "$1")
	if _, err := Load(env(map[string]string{"LCARS_CONFIG": writeConfig(t, uncommented)})); err != nil {
		t.Errorf("uncommented example file does not load: %v", err)
	}
}
