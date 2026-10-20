// Package config loads runtime configuration from an optional TOML file and
// environment variables.
//
// Every setting can come from either source. Environment variables override the
// file, and the file overrides the defaults. See lcars.example.toml at the
// repository root for the file format.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // so timezone names resolve in minimal containers too

	"github.com/BurntSushi/toml"
)

// DefaultFile is the config file read when LCARS_CONFIG is unset. It is
// relative to the working directory, and it is fine for it not to exist.
const DefaultFile = "lcars.toml"

// Config holds the server's runtime settings.
type Config struct {
	// File is the config file that was loaded, or "" if none was.
	File string

	Addr      string
	LogLevel  slog.Level
	LogFormat string // "json" or "text"
	// Location is the timezone used to decide what "today" is and to render
	// event times. Its String() is an IANA name such as "Europe/Paris" when
	// one can be determined.
	Location          *time.Location
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	Calendar Calendar
}

// Calendar configures the calendar feed.
type Calendar struct {
	// Dir is the absolute path of the directory holding .ics files, or "" if
	// the calendar is disabled.
	Dir string
	// RefreshInterval is how often Dir is re-read.
	RefreshInterval time.Duration
}

// setting is one configuration value, settable from the config file and the
// environment.
type setting struct {
	key string // dotted config file key, e.g. "calendar.dir"
	env string // environment variable, e.g. "LCARS_CALENDAR_DIR"
	// apply parses v into cfg. base is the directory relative paths are
	// resolved against: the config file's directory, or "" for the working
	// directory.
	apply func(cfg *Config, v, base string) error
}

var settings = []setting{
	{"addr", "LCARS_ADDR", func(c *Config, v, _ string) error {
		c.Addr = v
		return nil
	}},
	{"log_level", "LCARS_LOG_LEVEL", func(c *Config, v, _ string) error {
		return c.LogLevel.UnmarshalText([]byte(v))
	}},
	{"log_format", "LCARS_LOG_FORMAT", func(c *Config, v, _ string) error {
		if v != "json" && v != "text" {
			return fmt.Errorf("must be json or text, got %q", v)
		}
		c.LogFormat = v
		return nil
	}},
	{"shutdown_timeout", "LCARS_SHUTDOWN_TIMEOUT", func(c *Config, v, _ string) error {
		return parseDuration(&c.ShutdownTimeout, v, 0)
	}},
	{"timezone", "LCARS_TIMEZONE", func(c *Config, v, _ string) error {
		loc, err := time.LoadLocation(v)
		if err != nil {
			return fmt.Errorf("unknown timezone %q (want an IANA name like Europe/Paris)", v)
		}
		c.Location = loc
		return nil
	}},
	{"calendar.dir", "LCARS_CALENDAR_DIR", func(c *Config, v, base string) error {
		dir, err := resolvePath(v, base)
		if err != nil {
			return err
		}
		c.Calendar.Dir = dir
		return nil
	}},
	{"calendar.refresh_interval", "LCARS_CALENDAR_REFRESH_INTERVAL", func(c *Config, v, _ string) error {
		return parseDuration(&c.Calendar.RefreshInterval, v, time.Second)
	}},
}

// Load builds a Config from defaults, then the config file, then getenv.
//
// The config file is LCARS_CONFIG if set, which must then exist, or else
// DefaultFile if it exists.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:              ":8080",
		LogLevel:          slog.LevelInfo,
		LogFormat:         "json",
		Location:          localZone(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ShutdownTimeout:   10 * time.Second,
		Calendar: Calendar{
			RefreshInterval: time.Hour,
		},
	}

	path, required := getenv("LCARS_CONFIG"), true
	if path == "" {
		path, required = DefaultFile, false
	}
	values, err := readFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !required:
	case err != nil:
		return Config{}, err
	default:
		cfg.File = path
		base := filepath.Dir(path)
		for _, s := range settings {
			if v, ok := values[s.key]; ok {
				if err := s.apply(&cfg, v, base); err != nil {
					return Config{}, fmt.Errorf("%s: %s: %w", path, s.key, err)
				}
			}
		}
	}

	for _, s := range settings {
		if v := getenv(s.env); v != "" {
			if err := s.apply(&cfg, v, ""); err != nil {
				return Config{}, fmt.Errorf("%s: %w", s.env, err)
			}
		}
	}
	return cfg, nil
}

// readFile parses the TOML config file at path into a map from dotted key to
// value. Unknown keys and non-string values are errors, so typos are caught at
// startup instead of being silently ignored.
func readFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the operator chooses this path
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	values := map[string]string{}
	var flatten func(prefix string, m map[string]any) error
	flatten = func(prefix string, m map[string]any) error {
		for k, v := range m {
			key := prefix + k
			switch v := v.(type) {
			case map[string]any:
				if err := flatten(key+".", v); err != nil {
					return err
				}
			case string:
				if !slices.ContainsFunc(settings, func(s setting) bool { return s.key == key }) {
					return fmt.Errorf("%s: unknown setting %q", path, key)
				}
				values[key] = v
			default:
				return fmt.Errorf("%s: %s: must be a string (in quotes), got %v", path, key, v)
			}
		}
		return nil
	}
	if err := flatten("", raw); err != nil {
		return nil, err
	}
	return values, nil
}

func parseDuration(dst *time.Duration, v string, minimum time.Duration) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	if d < minimum {
		return fmt.Errorf("must be at least %s, got %s", minimum, d)
	}
	*dst = d
	return nil
}

// resolvePath expands a leading "~/" to the home directory and makes p
// absolute, resolving relative paths against base.
func resolvePath(p, base string) (string, error) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok || p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		p = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Abs(p)
}

// localZone returns the system timezone. time.Local is named "Local", so
// when possible this returns the same zone loaded by its IANA name, which is
// what clients need to see.
func localZone() *time.Location {
	name := os.Getenv("TZ")
	if name == "" {
		if target, err := os.Readlink("/etc/localtime"); err == nil {
			if _, after, ok := strings.Cut(target, "zoneinfo/"); ok {
				name = after
			}
		}
	}
	if loc, err := time.LoadLocation(strings.TrimPrefix(name, ":")); err == nil && name != "" {
		return loc
	}
	return time.Local
}
