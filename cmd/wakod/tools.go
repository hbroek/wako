package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/config"
	"github.com/hbroek/wako/internal/server"
	"github.com/hbroek/wako/internal/store"
)

// gcAssets implements "wakod gc-assets": delete images no page refers to.
func gcAssets(args []string) {
	fs := flag.NewFlagSet("gc-assets", flag.ExitOnError)
	dir := fs.String("data-dir", "./data", "directory holding wako.db")
	age := fs.Duration("older-than", 24*time.Hour, "only delete orphans at least this old")
	fs.Parse(args)
	st, err := store.Open(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	n, b, err := server.GCAssets(st, *age)
	fmt.Printf("removed %d unreferenced asset(s), %d bytes\n", n, b)
	if err != nil {
		log.Fatal(err)
	}
}

// resetAdminPassword implements "wakod reset-admin-password": set a new password
// for a user (creating an administrator if the name is new) and end their sessions.
func resetAdminPassword(args []string) {
	fs := flag.NewFlagSet("reset-admin-password", flag.ExitOnError)
	dir := fs.String("data-dir", "./data", "directory holding wako.db")
	user := fs.String("user", "admin", "user name")
	password := fs.String("password", os.Getenv("WAKO_ADMIN_PASSWORD"), "new password (generated if empty)")
	fs.Parse(args)
	st, err := store.Open(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	generated, err := server.ResetPassword(st, *user, *password)
	if err != nil {
		log.Fatal(err)
	}
	if generated != "" {
		fmt.Printf("new password for %s: %s\n", *user, generated)
	} else {
		fmt.Printf("password for %s changed\n", *user)
	}
}

// backup implements "wakod backup": a consistent copy of every database, made
// while the server may be running.
func backup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dir := fs.String("data-dir", "./data", "directory holding wako.db")
	to := fs.String("to", "", "directory to write the copy to (must not exist, or be empty)")
	fs.Parse(args)
	if *to == "" {
		fmt.Fprintln(os.Stderr, "usage: wakod backup -data-dir DIR -to BACKUPDIR")
		os.Exit(2)
	}
	st, err := store.Open(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if err := st.Backup(*to); err != nil {
		log.Fatalf("backup: %v", err)
	}
	fmt.Printf("backup written to %s (it holds password hashes and the signing key: keep it private)\n", *to)
}

// check implements "wakod check": look for damage in the databases.
func check(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	dir := fs.String("data-dir", "./data", "directory holding wako.db")
	deep := fs.Bool("deep", false, "run SQLite's complete integrity check (slow on big databases)")
	repair := fs.Bool("repair", false, "rebuild the search index and stored paths if they are wrong")
	fs.Parse(args)
	st, err := store.Open(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	rep, err := st.Check(*deep, *repair)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("checked %d space(s), %d page(s)\n", rep.Spaces, rep.Pages)
	for _, f := range rep.Fixed {
		fmt.Println("fixed:", f)
	}
	for _, p := range rep.Problems {
		fmt.Println("problem:", p)
	}
	if len(rep.Problems) > len(rep.Fixed) {
		return 1
	}
	if len(rep.Problems) == 0 {
		fmt.Println("no problems found")
	}
	return 0
}

// forgetSpace implements "wakod forget-space": drop a space from the catalog,
// for when its database file is gone for good.
func forgetSpace(args []string) {
	fs := flag.NewFlagSet("forget-space", flag.ExitOnError)
	dir := fs.String("data-dir", "./data", "directory holding wako.db")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: wakod forget-space -data-dir DIR SLUG")
		os.Exit(2)
	}
	st, err := store.Open(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	if err := st.DeleteSpace(fs.Arg(0)); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("space %s removed from the catalog\n", fs.Arg(0))
}

// healthcheck implements "wakod healthcheck", for container health probes in
// images that have no curl: it asks the running server on this machine.
func healthcheck(args []string) int {
	cfg, err := config.Parse(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	scheme, client := "http", &http.Client{Timeout: 3 * time.Second}
	target := cfg.Addr
	if path, ok := strings.CutPrefix(cfg.Addr, "unix:"); ok {
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}
		target = "localhost"
	} else if host, port, err := net.SplitHostPort(cfg.Addr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		target = net.JoinHostPort("127.0.0.1", port)
	}
	if cfg.TLSCert != "" {
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // our own certificate, on this machine
	}
	resp, err := client.Get(scheme + "://" + target + cfg.BasePath + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}
