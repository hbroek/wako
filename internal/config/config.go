// Package config reads settings from flags and WAKO_* environment variables.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr      string
	DataDir   string
	BasePath  string // reverse-proxy prefix such as "/wiki"
	SiteName  string // shown in the header and page titles
	Logo      string // optional logo/favicon image file
	MaxUpload int64  // largest accepted upload in MiB
	MaxStored int64  // largest stored image or PDF in MiB, after processing
	MaxSide   int    // longest side of a compacted image, in pixels
	MaxMpix   int64  // most decoded megapixels accepted
	ImageMode string // "compact" or "original"
	Quota     int64  // media quota per space in MiB; negative means unlimited

	AdminUser      string        // name of the administrator created on first start
	AdminPassword  string        // its password; generated and printed once if empty
	DisableAuth    bool          // no sign-in: everyone is an administrator
	AllowInsecure  bool          // cookies over plain HTTP on any host
	ExternalResize bool          // /_resize may fetch and resize pictures from other sites
	AccessTTL      time.Duration // lifetime of an access token
	RefreshTTL     time.Duration // how long a session lasts without use

	RevisionWindow time.Duration // saves by one author closer together than this are one revision
	RevisionKeep   int           // revisions kept per page; 0 keeps them all
	PublicURL      string        // scheme://host the site is reached at, for feed links
	MaxImport      int64         // largest import (a zip) in MiB

	LogFormat      string   // "text" or "json"
	LogLevel       string   // debug, info, warn or error
	AccessLog      bool     // one log line per request
	TrustedProxies string   // networks whose X-Forwarded-* headers are believed (default: loopback)
	AllowedHosts   []string // names served when sign-in is off, besides local ones
	TLSCert        string
	TLSKey         string

	AIProvider  string        // "anthropic", "openai" (any OpenAI-compatible service) or "" for off
	AIKey       string        // the service's API key
	AIModel     string        // model name; a sensible default per provider
	AIURL       string        // service address, for a proxy or a local server
	AIMaxTokens int           // longest answer or draft, in tokens
	AITimeout   time.Duration // how long to wait for the model
	AIPerHour   int           // requests per user per hour
}

// ParseDuration is time.ParseDuration plus a "d" suffix for days ("7d").
func ParseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err == nil && d <= 0 {
		err = fmt.Errorf("duration must be positive")
	}
	return d, err
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if n, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil && n > 0 {
		return n
	}
	return def
}

// envSigned is like envInt but accepts zero and negative values.
func envSigned(key string, def int64) int64 {
	if n, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil {
		return n
	}
	return def
}

// Parse reads flags (defaults come from the environment) from args.
func Parse(args []string) (*Config, error) {
	var c Config
	fs := flag.NewFlagSet("wakod", flag.ContinueOnError)
	fs.StringVar(&c.Addr, "addr", env("WAKO_ADDR", "127.0.0.1:8888"), "listen address")
	fs.StringVar(&c.DataDir, "data-dir", env("WAKO_DATA_DIR", "./data"), "directory holding wako.db")
	fs.StringVar(&c.BasePath, "base-path", env("WAKO_BASE_PATH", ""), "URL prefix when served behind a proxy")
	fs.StringVar(&c.SiteName, "site-name", env("WAKO_SITE_NAME", "Wako"), "site name shown in the header")
	fs.StringVar(&c.Logo, "logo", env("WAKO_LOGO", ""), "path to a logo image (png, svg, ...)")
	fs.Int64Var(&c.MaxUpload, "max-upload-mb", envInt("WAKO_MAX_UPLOAD_MB", 25), "largest accepted image upload, in MiB")
	fs.Int64Var(&c.MaxStored, "max-stored-mb", envInt("WAKO_MAX_STORED_MB", 5), "largest image or PDF kept after processing, in MiB")
	fs.IntVar(&c.MaxSide, "image-max-side", int(envInt("WAKO_IMAGE_MAX_SIDE", 1600)), "longest side in pixels after compacting a picture")
	fs.Int64Var(&c.MaxMpix, "image-max-mpix", envInt("WAKO_IMAGE_MAX_MPIX", 24), "most megapixels a picture may have when decoded")
	fs.StringVar(&c.ImageMode, "image-mode", env("WAKO_IMAGE_MODE", "compact"), `"compact" shrinks large pictures, "original" only strips metadata`)
	fs.Int64Var(&c.Quota, "space-quota-mb", envSigned("WAKO_SPACE_QUOTA_MB", 200), "media quota per space in MiB (negative: unlimited)")
	fs.StringVar(&c.AdminUser, "admin-user", env("WAKO_ADMIN_USER", "admin"), "name of the administrator created on first start")
	fs.StringVar(&c.AdminPassword, "admin-password", env("WAKO_ADMIN_PASSWORD", ""), "password of that administrator (generated and printed once if empty)")
	fs.BoolVar(&c.DisableAuth, "disable-auth", env("WAKO_DISABLE_AUTH", "") == "true", "turn sign-in off: everyone is an administrator (only for a trusted machine)")
	fs.BoolVar(&c.AllowInsecure, "allow-insecure", env("WAKO_ALLOW_INSECURE", "") == "true", "set login cookies over plain HTTP (HTTPS is strongly preferred)")
	access, refresh := env("WAKO_ACCESS_TOKEN_TIMEOUT", "15m"), env("WAKO_REFRESH_TOKEN_TIMEOUT", "7d")
	fs.StringVar(&access, "access-token-timeout", access, "how long a login token lives, e.g. 15m")
	fs.StringVar(&refresh, "refresh-token-timeout", refresh, "how long a session lasts without use, e.g. 7d")
	window := env("WAKO_REVISION_WINDOW", "10m")
	fs.StringVar(&window, "revision-window", window, `saves by one author this close together count as one revision ("0" keeps every save)`)
	fs.IntVar(&c.RevisionKeep, "keep-revisions", int(envSigned("WAKO_KEEP_REVISIONS", 100)), "revisions kept per page (0 keeps all)")
	fs.StringVar(&c.LogFormat, "log-format", env("WAKO_LOG_FORMAT", "text"), `log lines as "text" or "json"`)
	fs.StringVar(&c.LogLevel, "log-level", env("WAKO_LOG_LEVEL", "info"), "debug, info, warn or error")
	fs.BoolVar(&c.ExternalResize, "external-resize", env("WAKO_EXTERNAL_RESIZE", "true") != "false", "let pages show resized copies of pictures from other sites (/_resize); spaces can still switch it off")
	fs.BoolVar(&c.AccessLog, "access-log", env("WAKO_ACCESS_LOG", "true") != "false", "log one line per request")
	fs.StringVar(&c.TrustedProxies, "trusted-proxies", env("WAKO_TRUSTED_PROXIES", ""), "networks whose X-Forwarded-For/-Proto/-Host headers are believed, e.g. 10.0.0.0/8 (default: this machine only)")
	hosts := env("WAKO_ALLOWED_HOSTS", "")
	fs.StringVar(&hosts, "allowed-host", hosts, "with --disable-auth: extra host names to answer to (comma-separated)")
	fs.StringVar(&c.TLSCert, "tls-cert", env("WAKO_TLS_CERT", ""), "certificate file: serve HTTPS directly")
	fs.StringVar(&c.TLSKey, "tls-key", env("WAKO_TLS_KEY", ""), "private key file for --tls-cert")
	fs.Int64Var(&c.MaxImport, "max-import-mb", envInt("WAKO_MAX_IMPORT_MB", 256), "largest zip a space import accepts, in MiB")
	fs.StringVar(&c.PublicURL, "public-url", env("WAKO_PUBLIC_URL", ""), "scheme://host the site is reached at, e.g. https://wiki.example.com (for feed links behind a proxy)")
	fs.StringVar(&c.AIProvider, "ai-provider", env("WAKO_AI_PROVIDER", ""), `language model service for "ask the wiki" and page drafts: "anthropic", "openai" (any OpenAI-compatible service, such as a local Ollama) or empty for off`)
	fs.StringVar(&c.AIKey, "ai-key", env("WAKO_AI_KEY", ""), "API key of that service (prefer the WAKO_AI_KEY environment variable: a flag shows in the process list)")
	fs.StringVar(&c.AIModel, "ai-model", env("WAKO_AI_MODEL", ""), "model name (default: a current model of the provider)")
	fs.StringVar(&c.AIURL, "ai-url", env("WAKO_AI_URL", ""), "address of the service when it is not the provider's own, e.g. http://localhost:11434/v1")
	fs.IntVar(&c.AIMaxTokens, "ai-max-tokens", int(envInt("WAKO_AI_MAX_TOKENS", 32768)), "longest answer or draft, in tokens")
	aiTimeout := env("WAKO_AI_TIMEOUT", "90s")
	fs.StringVar(&aiTimeout, "ai-timeout", aiTimeout, "how long to wait for the model, e.g. 90s")
	fs.IntVar(&c.AIPerHour, "ai-requests-per-hour", int(envInt("WAKO_AI_REQUESTS_PER_HOUR", 30)), "AI requests each user may make per hour")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	var err error
	if c.AITimeout, err = ParseDuration(aiTimeout); err != nil {
		return nil, fmt.Errorf("ai-timeout: %w", err)
	}
	switch strings.ToLower(c.AIProvider) {
	case "", "off", "anthropic", "openai":
	default:
		return nil, fmt.Errorf(`ai-provider must be "anthropic", "openai" or empty, not %q`, c.AIProvider)
	}
	if c.AccessTTL, err = ParseDuration(access); err != nil {
		return nil, fmt.Errorf("access-token-timeout: %w", err)
	}
	if c.RefreshTTL, err = ParseDuration(refresh); err != nil {
		return nil, fmt.Errorf("refresh-token-timeout: %w", err)
	}
	if window == "0" {
		c.RevisionWindow = 0
	} else if c.RevisionWindow, err = ParseDuration(window); err != nil {
		return nil, fmt.Errorf("revision-window: %w", err)
	}
	if c.RevisionKeep < 0 {
		return nil, fmt.Errorf("keep-revisions must not be negative")
	}
	if c.PublicURL != "" && !(strings.HasPrefix(c.PublicURL, "http://") || strings.HasPrefix(c.PublicURL, "https://")) {
		return nil, fmt.Errorf("public-url must start with http:// or https://")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return nil, fmt.Errorf(`log-format must be "text" or "json", not %q`, c.LogFormat)
	}
	if _, err := ParseLogLevel(c.LogLevel); err != nil {
		return nil, err
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("tls-cert and tls-key go together")
	}
	if strings.HasPrefix(c.Addr, "unix:") && !strings.HasPrefix(strings.TrimPrefix(c.Addr, "unix:"), "/") {
		return nil, fmt.Errorf("a unix: address needs an absolute path, e.g. unix:/run/wako/wako.sock")
	}
	for _, h := range strings.Split(hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.AllowedHosts = append(c.AllowedHosts, h)
		}
	}
	if c.ImageMode != "compact" && c.ImageMode != "original" {
		return nil, fmt.Errorf(`image-mode must be "compact" or "original", not %q`, c.ImageMode)
	}
	c.BasePath = strings.TrimRight(c.BasePath, "/")
	if c.BasePath != "" && !strings.HasPrefix(c.BasePath, "/") {
		c.BasePath = "/" + c.BasePath
	}
	return &c, nil
}

// ParseLogLevel reads a log level name.
func ParseLogLevel(name string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(name)); err != nil {
		return 0, fmt.Errorf("log-level must be debug, info, warn or error, not %q", name)
	}
	return l, nil
}
