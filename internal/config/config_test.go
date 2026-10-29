package config

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"15m": 15 * time.Minute, "7d": 7 * 24 * time.Hour, "1d": 24 * time.Hour, "36h": 36 * time.Hour, "0.5d": 12 * time.Hour, "90s": 90 * time.Second,
	} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "d", "-1d", "0", "0d", "abc", "-5m", "7 days"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) accepted", bad)
		}
	}
}

func TestAuthFlags(t *testing.T) {
	c, err := Parse(nil)
	if err != nil || c.AdminUser != "admin" || c.AccessTTL != 15*time.Minute || c.RefreshTTL != 7*24*time.Hour || c.DisableAuth || c.AllowInsecure {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = Parse([]string{"-admin-user", "boss", "-admin-password", "pw-pw-pw-1", "-disable-auth", "-allow-insecure",
		"-access-token-timeout", "5m", "-refresh-token-timeout", "30d"})
	if err != nil || c.AdminUser != "boss" || c.AdminPassword != "pw-pw-pw-1" || !c.DisableAuth || !c.AllowInsecure ||
		c.AccessTTL != 5*time.Minute || c.RefreshTTL != 30*24*time.Hour {
		t.Fatalf("flags: %+v %v", c, err)
	}
	t.Setenv("WAKO_ACCESS_TOKEN_TIMEOUT", "1h")
	t.Setenv("WAKO_DISABLE_AUTH", "true")
	if c, err = Parse(nil); err != nil || c.AccessTTL != time.Hour || !c.DisableAuth {
		t.Errorf("environment: %+v %v", c, err)
	}
	if _, err := Parse([]string{"-access-token-timeout", "soon"}); err == nil {
		t.Error("bad duration accepted")
	}
}

func TestHistoryAndFeedFlags(t *testing.T) {
	c, err := Parse(nil)
	if err != nil || c.RevisionWindow != 10*time.Minute || c.RevisionKeep != 100 || c.PublicURL != "" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = Parse([]string{"-revision-window", "0", "-keep-revisions", "0", "-public-url", "https://wiki.example.com/"})
	if err != nil || c.RevisionWindow != 0 || c.RevisionKeep != 0 || c.PublicURL != "https://wiki.example.com" {
		t.Fatalf("flags: %+v %v", c, err)
	}
	if c, err = Parse([]string{"-revision-window", "90s", "-keep-revisions", "7"}); err != nil || c.RevisionWindow != 90*time.Second || c.RevisionKeep != 7 {
		t.Fatalf("values: %+v %v", c, err)
	}
	t.Setenv("WAKO_REVISION_WINDOW", "1h")
	t.Setenv("WAKO_KEEP_REVISIONS", "25")
	t.Setenv("WAKO_PUBLIC_URL", "http://localhost:8888")
	if c, err = Parse(nil); err != nil || c.RevisionWindow != time.Hour || c.RevisionKeep != 25 || c.PublicURL != "http://localhost:8888" {
		t.Errorf("environment: %+v %v", c, err)
	}
	for _, bad := range [][]string{{"-revision-window", "soon"}, {"-keep-revisions", "-1"}, {"-public-url", "wiki.example.com"}, {"-public-url", "ftp://x"}} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestOperationFlags(t *testing.T) {
	c, err := Parse(nil)
	if err != nil || c.LogFormat != "text" || c.LogLevel != "info" || !c.AccessLog || c.TrustedProxies != "" || len(c.AllowedHosts) != 0 || c.TLSCert != "" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = Parse([]string{"-log-format", "json", "-log-level", "debug", "-access-log=false", "-trusted-proxies", "10.0.0.0/8",
		"-allowed-host", "wiki.lan, notes.lan:8080", "-tls-cert", "c.pem", "-tls-key", "k.pem", "-addr", "unix:/run/wako.sock"})
	if err != nil || c.LogFormat != "json" || c.LogLevel != "debug" || c.AccessLog || c.TrustedProxies != "10.0.0.0/8" ||
		len(c.AllowedHosts) != 2 || c.AllowedHosts[1] != "notes.lan:8080" || c.TLSCert != "c.pem" || c.Addr != "unix:/run/wako.sock" {
		t.Fatalf("flags: %+v %v", c, err)
	}
	t.Setenv("WAKO_LOG_FORMAT", "json")
	t.Setenv("WAKO_ACCESS_LOG", "false")
	t.Setenv("WAKO_ALLOWED_HOSTS", "a.lan,b.lan")
	if c, err = Parse(nil); err != nil || c.LogFormat != "json" || c.AccessLog || len(c.AllowedHosts) != 2 {
		t.Errorf("environment: %+v %v", c, err)
	}
	for _, bad := range [][]string{{"-log-format", "xml"}, {"-log-level", "loud"}, {"-tls-cert", "c.pem"}, {"-tls-key", "k.pem"}, {"-addr", "unix:relative.sock"}} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
