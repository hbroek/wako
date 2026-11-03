// Command wakod serves the wako wiki.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hbroek/wako/internal/ai"
	"github.com/hbroek/wako/internal/config"
	"github.com/hbroek/wako/internal/img"
	"github.com/hbroek/wako/internal/server"
	"github.com/hbroek/wako/internal/store"
	"github.com/hbroek/wako/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "gc-assets":
			gcAssets(os.Args[2:])
			return
		case "reset-admin-password":
			resetAdminPassword(os.Args[2:])
			return
		case "spaces":
			listSpaces(os.Args[2:])
			return
		case "backup":
			backup(os.Args[2:])
			return
		case "check":
			os.Exit(check(os.Args[2:]))
		case "forget-space":
			forgetSpace(os.Args[2:])
			return
		case "healthcheck":
			os.Exit(healthcheck(os.Args[2:]))
		case "version", "-version", "--version":
			fmt.Println("wakod " + version.String())
			return
		}
	}
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(2)
	}
	os.Exit(serve(cfg))
}

func newLogger(cfg *config.Config) *slog.Logger {
	level, _ := config.ParseLogLevel(cfg.LogLevel)
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// serve runs the wiki until it is told to stop, and returns the exit code.
func serve(cfg *config.Config) int {
	log := newLogger(cfg)
	fatal := func(msg string, args ...any) int {
		log.Error(msg, args...)
		return 1
	}
	log.Info("starting", "version", version.String(), "data_dir", cfg.DataDir)

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fatal("cannot open the data directory", "err", err)
	}
	defer st.Close()
	if fi, err := os.Stat(cfg.DataDir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Warn("the data directory can be read by other users; it holds password hashes and the signing key",
			"mode", fi.Mode().Perm().String(), "fix", "chmod 700 "+cfg.DataDir)
	}
	// A space whose file is gone would otherwise show up as an empty space.
	fp, err := st.CheckFiles()
	if err != nil {
		return fatal("checking the data directory", "err", err)
	}
	for _, o := range fp.Orphans {
		log.Warn("a file in spaces/ belongs to no space; it is kept", "file", o)
	}
	if len(fp.Missing) > 0 {
		return fatal("a space's database file is missing, so the space would appear empty. Restore it from a backup, "+
			"or remove the space with: wakod forget-space -data-dir "+cfg.DataDir+" SLUG", "spaces", strings.Join(fp.Missing, ", "))
	}

	st.RevisionWindow, st.RevisionKeep = cfg.RevisionWindow, cfg.RevisionKeep
	st.Quota = cfg.Quota << 20
	if cfg.Quota < 0 {
		st.Quota = -1
	}
	if spaces, err := st.ListSpaces(); err == nil && len(spaces) == 0 {
		if _, err := st.CreateSpace("main", "Main", true); err != nil {
			return fatal("creating the first space", "err", err)
		}
		log.Info("created the first space", "space", "main")
	}
	if cfg.Logo != "" {
		if _, err := os.Stat(cfg.Logo); err != nil {
			return fatal("logo", "err", err)
		}
	}

	wiki := server.New(st, cfg.BasePath)
	wiki.Logger, wiki.AccessLog = log, cfg.AccessLog
	wiki.SiteName, wiki.LogoFile, wiki.MaxUpload, wiki.PublicURL = cfg.SiteName, cfg.Logo, cfg.MaxUpload<<20, cfg.PublicURL
	wiki.MaxImport = cfg.MaxImport << 20
	wiki.AllowedHosts = cfg.AllowedHosts
	if cfg.TrustedProxies != "" {
		if wiki.TrustedProxies, err = server.ParseProxies(cfg.TrustedProxies); err != nil {
			return fatal("trusted-proxies", "err", err)
		}
	}
	if provider, err := ai.New(ai.Config{Kind: cfg.AIProvider, Key: cfg.AIKey, Model: cfg.AIModel, URL: cfg.AIURL,
		MaxTokens: cfg.AIMaxTokens, Timeout: cfg.AITimeout}); err != nil {
		return fatal("AI settings", "err", err)
	} else if provider != nil {
		wiki.EnableAI(provider, cfg.AIPerHour)
		log.Info("AI features are available", "provider", cfg.AIProvider, "model", provider.Model(),
			"note", "off for every space until a server administrator switches them on in the space's settings")
	}
	wiki.ImageLimits = img.Limits{
		MaxPixels: int(cfg.MaxMpix) * 1_000_000, MaxSide: cfg.MaxSide, MaxStored: int(cfg.MaxStored) << 20, MaxPDF: int(cfg.MaxPDF) << 20,
		MaxFrames: img.DefaultLimits.MaxFrames, Mode: img.Compact,
	}
	wiki.ExternalResize = cfg.ExternalResize
	if cfg.ImageMode == "original" {
		wiki.ImageLimits.Mode = img.Original
	}
	if cfg.DisableAuth {
		log.Warn("sign-in is OFF (--disable-auth): everyone who can reach this server is an administrator. " +
			"Only use this on a machine and network you fully trust, never on the internet. " +
			"It answers only to local names and IP addresses; add --allowed-host NAME for others.")
	} else {
		if err := wiki.EnableAuth(server.AuthOptions{AccessTTL: cfg.AccessTTL, RefreshTTL: cfg.RefreshTTL, AllowInsecure: cfg.AllowInsecure}); err != nil {
			return fatal("starting sign-in", "err", err)
		}
		created, generated, err := server.Bootstrap(st, cfg.AdminUser, cfg.AdminPassword)
		if err != nil {
			return fatal("creating the administrator", "err", err)
		}
		if created {
			log.Info("created the administrator", "user", cfg.AdminUser)
			if generated != "" {
				// Printed once, to the console only: it is not a log record.
				fmt.Fprintf(os.Stderr, "\n  administrator %q, password %s\n  (shown once; change it from the Account page)\n\n", cfg.AdminUser, generated)
			}
		}
		if cfg.AllowInsecure {
			log.Warn("--allow-insecure lets login cookies travel over plain HTTP; use HTTPS when others can reach this server")
		}
	}

	ln, scheme, err := listen(cfg)
	if err != nil {
		return fatal("cannot listen", "addr", cfg.Addr, "err", err)
	}
	srv := wiki.HTTPServer(cfg.Addr)
	if cfg.TLSCert != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	// On a signal, stop accepting and let requests in flight finish before the
	// databases are closed.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		s := <-sig
		log.Info("stopping", "signal", s.String())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Warn("some requests did not finish in time", "err", err)
			srv.Close()
		}
	}()
	log.Info("listening", "url", scheme+"://"+cfg.Addr+cfg.BasePath+"/")
	if cfg.TLSCert != "" {
		err = srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
	} else {
		err = srv.Serve(ln)
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return fatal("server stopped", "err", err)
	}
	<-done
	log.Info("stopped")
	return 0
}

// listen opens the listening socket: TCP, or a Unix socket for "unix:/path".
func listen(cfg *config.Config) (net.Listener, string, error) {
	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	if path, ok := strings.CutPrefix(cfg.Addr, "unix:"); ok {
		os.Remove(path) // a stale socket from a previous run
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, "", err
		}
		os.Chmod(path, 0o660) // the proxy's group may connect, nobody else
		return ln, scheme, nil
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	return ln, scheme, err
}
