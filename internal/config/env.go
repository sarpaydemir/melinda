package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // IANA zone names resolve without zone files on the host
)

// Env is the process configuration read once at startup from MELINDA_* environment variables.
type Env struct {
	AgentID            string
	SettingsFile       string
	SettingsJSON       string
	SettingsTTL        time.Duration
	PromptsDir         string
	DatabaseURL        string
	DBHost             string
	DBPort             int
	DBUser             string
	DBPassword         string
	DBName             string
	Timezone           *time.Location
	LogLevel           slog.Level
	LogJSON            bool // MELINDA_LOG_FORMAT=json
	SearXNGURL         string
	HTTPAddr           string   // empty disables the HTTP server
	Components         []string // empty means all
	ShutdownTimeout    time.Duration
	PipelineWorkers    int
	PipelineRunTimeout time.Duration
	DryRun             bool
	DryRunLog          string   // file the shadow-mode records are appended to; empty means the log output
	Unknown            []string // set MELINDA_* variables that are not recognized
}

// LoadEnv reads the MELINDA_* variables from environ ("NAME=value" entries). A variable that is set is used as
// given, even when empty; an unset one takes its default. Every invalid value is reported in one error.
func LoadEnv(environ []string) (Env, error) {
	r := envReader{vars: map[string]string{}, read: map[string]bool{}}
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if _, dup := r.vars[name]; ok && !dup && strings.HasPrefix(name, "MELINDA_") {
			r.vars[name] = value
		}
	}

	env := Env{
		AgentID:            parse(&r, "MELINDA_AGENT_ID", "melinda", agentID),
		SettingsFile:       r.get("MELINDA_SETTINGS_FILE", ""),
		SettingsJSON:       r.get("MELINDA_SETTINGS_JSON", ""),
		SettingsTTL:        parse(&r, "MELINDA_SETTINGS_TTL", "5s", duration),
		PromptsDir:         r.get("MELINDA_PROMPTS_DIR", ""),
		DatabaseURL:        r.get("MELINDA_DATABASE_URL", ""),
		DBHost:             r.get("MELINDA_DB_HOST", "localhost"),
		DBPort:             parse(&r, "MELINDA_DB_PORT", "5432", port),
		DBUser:             r.get("MELINDA_DB_USER", ""),
		DBPassword:         r.get("MELINDA_DB_PASSWORD", ""),
		DBName:             r.get("MELINDA_DB_NAME", ""),
		Timezone:           parse(&r, "MELINDA_TIMEZONE", "UTC", location),
		LogLevel:           parse(&r, "MELINDA_LOG_LEVEL", "INFO", logLevel),
		LogJSON:            parse(&r, "MELINDA_LOG_FORMAT", "text", logJSON),
		SearXNGURL:         parse(&r, "MELINDA_SEARXNG_URL", "http://searxng:8080", httpURL),
		HTTPAddr:           parse(&r, "MELINDA_HTTP_ADDR", ":8080", listenAddr),
		Components:         parse(&r, "MELINDA_COMPONENTS", "", list),
		ShutdownTimeout:    parse(&r, "MELINDA_SHUTDOWN_TIMEOUT", "30s", positiveDuration),
		PipelineWorkers:    parse(&r, "MELINDA_PIPELINE_WORKERS", "8", positiveInt),
		PipelineRunTimeout: parse(&r, "MELINDA_PIPELINE_RUN_TIMEOUT", "30m", positiveDuration),
		DryRun:             parse(&r, "MELINDA_DRY_RUN", "false", boolean),
		DryRunLog:          r.get("MELINDA_DRY_RUN_LOG", ""),
	}
	if len(r.errs) > 0 {
		return Env{}, errors.New("invalid environment: " + strings.Join(r.errs, "; "))
	}
	for name := range r.vars {
		if !r.read[name] {
			env.Unknown = append(env.Unknown, name)
		}
	}
	slices.Sort(env.Unknown)
	return env, nil
}

// SettingsSource names the source the settings document is read from: the first of MELINDA_SETTINGS_FILE and
// MELINDA_SETTINGS_JSON that is set. It is empty when neither is.
func (e Env) SettingsSource() string {
	switch {
	case e.SettingsFile != "":
		return "MELINDA_SETTINGS_FILE"
	case e.SettingsJSON != "":
		return "MELINDA_SETTINGS_JSON"
	}
	return ""
}

// Secrets returns the secret values in the environment, for masking in logs.
func (e Env) Secrets() []string {
	out := []string{e.DBPassword}
	if u, err := url.Parse(e.DatabaseURL); err == nil {
		if pass, ok := u.User.Password(); ok {
			out = append(out, pass)
		}
	}
	return slices.DeleteFunc(out, func(s string) bool { return s == "" })
}

type envReader struct {
	vars map[string]string
	read map[string]bool
	errs []string
}

func (r *envReader) get(name, def string) string {
	r.read[name] = true
	if v, ok := r.vars[name]; ok {
		return v
	}
	return def
}

func parse[T any](r *envReader, name, def string, conv func(string) (T, error)) T {
	v, err := conv(r.get(name, def))
	if err != nil {
		r.errs = append(r.errs, name+": "+err.Error())
	}
	return v
}

var agentIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func agentID(s string) (string, error) {
	if !agentIDPattern.MatchString(s) {
		return "", fmt.Errorf("%q must match %s", s, agentIDPattern)
	}
	return s, nil
}

// duration accepts Go duration syntax or a bare number of seconds.
func duration(s string) (time.Duration, error) {
	goSyntax := s
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		goSyntax += "s"
	}
	d, err := time.ParseDuration(goSyntax)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a duration such as 30s, 5m, or a number of seconds", s)
	}
	return d, nil
}

func positiveDuration(s string) (time.Duration, error) {
	d, err := duration(s)
	if err == nil && d == 0 {
		err = errors.New("must be greater than zero")
	}
	return d, err
}

func positiveInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a positive integer", s)
	}
	return n, nil
}

func port(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port number from 1 to 65535", s)
	}
	return n, nil
}

func boolean(s string) (bool, error) {
	switch s {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q is not one of true, false, 1, 0", s)
}

func logLevel(s string) (slog.Level, error) {
	switch strings.ToUpper(s) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARNING", "WARN":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%q is not one of DEBUG, INFO, WARNING, WARN, ERROR", s)
}

func logJSON(s string) (bool, error) {
	switch s {
	case "text":
		return false, nil
	case "json":
		return true, nil
	}
	return false, fmt.Errorf("%q is not one of text, json", s)
}

var offsetPattern = regexp.MustCompile(`^[+-](\d{2}):([0-5]\d)$`)

// location accepts an IANA zone name or a fixed ±HH:MM offset without daylight saving.
func location(s string) (*time.Location, error) {
	if m := offsetPattern.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mins, _ := strconv.Atoi(m[2])
		secs := (h*60 + mins) * 60
		if s[0] == '-' {
			secs = -secs
		}
		if secs < -12*3600 || secs > 14*3600 {
			return nil, fmt.Errorf("offset %q is outside -12:00 to +14:00", s)
		}
		return time.FixedZone(s, secs), nil
	}
	if s == "" || s == "Local" {
		return nil, fmt.Errorf("%q is not an IANA zone name or a ±HH:MM offset", s)
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not an IANA zone name or a ±HH:MM offset", s)
	}
	return loc, nil
}

// httpURL does not echo the value in its error because a URL can carry credentials.
func httpURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("not an absolute http or https URL")
	}
	return s, nil
}

func listenAddr(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		return "", fmt.Errorf("%q is not a host:port listen address", s)
	}
	return s, nil
}

func list(s string) ([]string, error) {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out, nil
}
