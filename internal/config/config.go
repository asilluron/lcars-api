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
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	Training Training
	NATS     NATS
	Supabase Supabase
	Eufy     Eufy
	Weather  Weather
}

// Weather is the forecast shown on a device. Latitude and longitude are
// decimal degrees. Both are required to enable it. The values are sent to
// the forecast service and are not logged.
type Weather struct {
	Latitude  *float64
	Longitude *float64
	// URL is the forecast service base URL, or "" for the default.
	URL string
}

// Enabled reports whether a location is configured.
func (w Weather) Enabled() bool { return w.Latitude != nil && w.Longitude != nil }

// Eufy is the camera proxy (the eufy/ process). When URL is set, devices can
// list cameras and open their live video through this server.
type Eufy struct {
	// URL is the proxy's base URL, such as "http://127.0.0.1:3000", or ""
	// to disable cameras.
	URL string
}

// Supabase configures the database that stores device tokens and the admin
// users who manage them. When URL is set, devices must authenticate with a
// token and the admin page is enabled.
type Supabase struct {
	// URL is the project's API URL, such as "http://127.0.0.1:54321", or ""
	// to disable authentication and the admin page.
	URL string
	// SecretKey is the project's secret key (or legacy service_role key).
	SecretKey string
	// RefreshInterval is how often device tokens are re-read, to pick up
	// changes made directly in the database.
	RefreshInterval time.Duration
}

// Enabled reports whether Supabase is configured.
func (s Supabase) Enabled() bool { return s.URL != "" }

// Training configures where wake-word training audio is stored.
type Training struct {
	// Dir is the absolute path of the directory that receives WAV files, or
	// "" if training capture is disabled.
	Dir string
	// MaxBytes is the most the directory may hold. After each write the
	// oldest hourly files are deleted until the tree is within this size.
	MaxBytes int64
}

// NATS configures the NATS connection the API is served on.
type NATS struct {
	// URL is an existing NATS server to connect to. If it is "", the server
	// runs its own embedded NATS server on Addr instead.
	URL string
	// Addr is the address the embedded NATS server listens on.
	Addr string
	// TLSCert and TLSKey are PEM files for the embedded server. When both are
	// set, network clients connect with tls://. Ignored when URL is set.
	TLSCert string
	TLSKey  string
	// TLSHosts are DNS names and IPs that must be in the certificate SAN.
	// When TLSCert is empty, a CA and server certificate are generated in
	// TLSDir on first start and reused after that.
	TLSHosts []string
	// TLSDir is where a generated CA and server certificate are stored.
	TLSDir string
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
	{"training.dir", "LCARS_TRAINING_DIR", func(c *Config, v, base string) error {
		dir, err := resolvePath(v, base)
		if err != nil {
			return err
		}
		c.Training.Dir = dir
		return nil
	}},
	{"training.max_bytes", "LCARS_TRAINING_MAX_BYTES", func(c *Config, v, _ string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("want a positive integer number of bytes, got %q", v)
		}
		if n < 1 {
			return fmt.Errorf("must be at least 1, got %d", n)
		}
		c.Training.MaxBytes = n
		return nil
	}},
	{"nats.addr", "LCARS_NATS_ADDR", func(c *Config, v, _ string) error {
		_, port, err := net.SplitHostPort(v)
		if err != nil {
			return fmt.Errorf("want host:port or :port, got %q", v)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("invalid port %q", port)
		}
		c.NATS.Addr = v
		return nil
	}},
	{"nats.url", "LCARS_NATS_URL", func(c *Config, v, _ string) error {
		c.NATS.URL = v
		return nil
	}},
	{"nats.tls_cert", "LCARS_NATS_TLS_CERT", func(c *Config, v, base string) error {
		path, err := resolvePath(v, base)
		if err != nil {
			return err
		}
		c.NATS.TLSCert = path
		return nil
	}},
	{"nats.tls_key", "LCARS_NATS_TLS_KEY", func(c *Config, v, base string) error {
		path, err := resolvePath(v, base)
		if err != nil {
			return err
		}
		c.NATS.TLSKey = path
		return nil
	}},
	{"nats.tls_hosts", "LCARS_NATS_TLS_HOSTS", func(c *Config, v, _ string) error {
		hosts, err := parseTLSHosts(v)
		if err != nil {
			return err
		}
		c.NATS.TLSHosts = hosts
		return nil
	}},
	{"nats.tls_dir", "LCARS_NATS_TLS_DIR", func(c *Config, v, base string) error {
		path, err := resolvePath(v, base)
		if err != nil {
			return err
		}
		c.NATS.TLSDir = path
		return nil
	}},
	{"supabase.url", "LCARS_SUPABASE_URL", func(c *Config, v, _ string) error {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("want an http(s) URL like http://127.0.0.1:54321, got %q", v)
		}
		if u.User != nil {
			return errors.New("must not contain credentials; set supabase.secret_key instead")
		}
		c.Supabase.URL = strings.TrimSuffix(v, "/")
		return nil
	}},
	{"supabase.secret_key", "LCARS_SUPABASE_SECRET_KEY", func(c *Config, v, _ string) error {
		c.Supabase.SecretKey = v
		return nil
	}},
	{"supabase.refresh_interval", "LCARS_SUPABASE_REFRESH_INTERVAL", func(c *Config, v, _ string) error {
		return parseDuration(&c.Supabase.RefreshInterval, v, time.Second)
	}},
	{"eufy.url", "LCARS_EUFY_URL", func(c *Config, v, _ string) error {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("want an http(s) URL like http://127.0.0.1:3000, got %q", v)
		}
		if u.User != nil {
			return errors.New("must not contain credentials")
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("want a base URL with no path, such as http://127.0.0.1:3000")
		}
		c.Eufy.URL = strings.TrimSuffix(v, "/")
		return nil
	}},
	{"weather.latitude", "LCARS_WEATHER_LATITUDE", func(c *Config, v, _ string) error {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < -90 || n > 90 {
			return fmt.Errorf("want a latitude from -90 to 90, got %q", v)
		}
		c.Weather.Latitude = &n
		return nil
	}},
	{"weather.longitude", "LCARS_WEATHER_LONGITUDE", func(c *Config, v, _ string) error {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < -180 || n > 180 {
			return fmt.Errorf("want a longitude from -180 to 180, got %q", v)
		}
		c.Weather.Longitude = &n
		return nil
	}},
	{"weather.url", "LCARS_WEATHER_URL", func(c *Config, v, _ string) error {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("want an http(s) URL like https://api.open-meteo.com, got %q", v)
		}
		if u.User != nil {
			return errors.New("must not contain credentials")
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("want a base URL with no path, such as https://api.open-meteo.com")
		}
		c.Weather.URL = strings.TrimSuffix(v, "/")
		return nil
	}},
}

// EnvVars returns the environment variable of every setting.
func EnvVars() []string {
	out := make([]string, len(settings))
	for i, s := range settings {
		out[i] = s.env
	}
	return out
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
		Training: Training{
			MaxBytes: 32 << 30, // 32 GiB, about a week and a half of 16 kHz mono
		},
		NATS: NATS{
			Addr: ":4222",
		},
		Supabase: Supabase{
			RefreshInterval: time.Minute,
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
	if err := cfg.NATS.validate(); err != nil {
		return Config{}, fmt.Errorf("nats: %w", err)
	}
	if (cfg.Supabase.URL == "") != (cfg.Supabase.SecretKey == "") {
		return Config{}, errors.New("supabase.url and supabase.secret_key (LCARS_SUPABASE_URL, " +
			"LCARS_SUPABASE_SECRET_KEY) must be set together")
	}
	if (cfg.Weather.Latitude == nil) != (cfg.Weather.Longitude == nil) {
		return Config{}, errors.New("weather.latitude and weather.longitude (LCARS_WEATHER_LATITUDE, " +
			"LCARS_WEATHER_LONGITUDE) must be set together")
	}
	if cfg.Weather.Enabled() && cfg.Weather.URL == "" {
		cfg.Weather.URL = "https://api.open-meteo.com"
	}
	return cfg, nil
}

// validate checks combinations of TLS settings. The files themselves are
// read when the server starts.
func (n NATS) validate() error {
	switch {
	case (n.TLSCert == "") != (n.TLSKey == ""):
		return errors.New("tls_cert and tls_key must both be set")
	case len(n.TLSHosts) > 0 && n.TLSCert == "" && n.TLSDir == "":
		return errors.New("tls_hosts needs tls_dir, or tls_cert and tls_key")
	case n.TLSDir != "" && n.TLSCert == "" && len(n.TLSHosts) == 0:
		return errors.New("tls_dir needs tls_hosts")
	default:
		return nil
	}
}

// parseTLSHosts splits a comma-separated list of DNS names and IP addresses.
func parseTLSHosts(value string) ([]string, error) {
	var hosts []string
	for part := range strings.SplitSeq(value, ",") {
		host := strings.TrimSpace(part)
		if host == "" {
			continue
		}
		if net.ParseIP(host) == nil && !validDNS(host) {
			return nil, fmt.Errorf("want a DNS name or IP, got %q", host)
		}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return nil, errors.New("list at least one DNS name or IP")
	}
	return hosts, nil
}

func validDNS(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '-' && i > 0 && i < len(label)-1:
			default:
				return false
			}
		}
	}
	return true
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
