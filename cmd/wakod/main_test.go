package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var wakod string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wakod-test")
	if err != nil {
		panic(err)
	}
	wakod = filepath.Join(dir, "wakod")
	if out, err := exec.Command("go", "build", "-o", wakod, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building wakod: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type proc struct {
	t    *testing.T
	cmd  *exec.Cmd
	mu   sync.Mutex
	logs bytes.Buffer
	done chan struct{}
	code int
}

func (p *proc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logs.String()
}

func (p *proc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logs.Write(b)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// start runs wakod with the given arguments and waits until it listens (or exits).
func start(t *testing.T, args ...string) *proc {
	t.Helper()
	p := &proc{t: t, done: make(chan struct{})}
	p.cmd = exec.Command(wakod, args...)
	p.cmd.Env = []string{} // nothing from the environment: it must not need any
	p.cmd.Stderr = p
	p.cmd.Stdout = p
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := p.cmd.Wait()
		p.code = 0
		if ee, ok := err.(*exec.ExitError); ok {
			p.code = ee.ExitCode()
		}
		close(p.done)
	}()
	t.Cleanup(func() {
		p.cmd.Process.Kill()
		<-p.done
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return p
		default:
		}
		if strings.Contains(p.output(), "listening") {
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("wakod did not start:\n%s", p.output())
	return nil
}

func (p *proc) stop() int {
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		p.t.Fatalf("wakod did not stop:\n%s", p.output())
	}
	return p.code
}

func args(addr, dir string, more ...string) []string {
	return append([]string{"-addr", addr, "-data-dir", dir, "-disable-auth"}, more...)
}

func put(t *testing.T, base, path, body string) int {
	t.Helper()
	req, _ := http.NewRequest("PUT", base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "text/markdown")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestServesAndStopsCleanly(t *testing.T) {
	dir, addr := filepath.Join(t.TempDir(), "data"), freeAddr(t)
	p := start(t, args(addr, dir)...)
	base := "http://" + addr
	if put(t, base, "/main/hello", "# Hello") != 201 {
		t.Fatal("cannot save a page")
	}
	// The data directory is private, and the log says what is going on.
	if fi, _ := os.Stat(dir); fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("data directory mode %v", fi.Mode().Perm())
	}
	for _, want := range []string{"starting", "created the first space", "sign-in is OFF", "listening"} {
		if !strings.Contains(p.output(), want) {
			t.Errorf("log lacks %q:\n%s", want, p.output())
		}
	}
	if code := p.stop(); code != 0 {
		t.Errorf("exit code %d:\n%s", code, p.output())
	}
	if !strings.Contains(p.output(), "stopped") {
		t.Errorf("no 'stopped' line:\n%s", p.output())
	}
	// The data survived the restart.
	p = start(t, args(addr, dir)...)
	resp, err := http.Get(base + "/main/hello.md")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != "# Hello" {
		t.Errorf("after a restart: %q", b)
	}
	p.stop()
}

// A request that is half-way through when the signal comes is finished, not dropped.
func TestShutdownLetsRequestsFinish(t *testing.T) {
	dir, addr := filepath.Join(t.TempDir(), "data"), freeAddr(t)
	p := start(t, args(addr, dir)...)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "PUT /main/slow HTTP/1.1\r\nHost: localhost\r\nContent-Type: text/markdown\r\nContent-Length: 20\r\n\r\n0123456789")
	time.Sleep(200 * time.Millisecond)

	p.cmd.Process.Signal(syscall.SIGTERM)
	time.Sleep(400 * time.Millisecond)
	select {
	case <-p.done:
		t.Fatalf("the server exited with a request in flight:\n%s", p.output())
	default:
	}
	fmt.Fprint(conn, "0123456789") // the rest of the body
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("the request in flight: %v %v", resp, err)
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("did not exit after the request finished")
	}
	if p.code != 0 || !strings.Contains(p.output(), "stopped") {
		t.Errorf("exit %d:\n%s", p.code, p.output())
	}
	// And the page it was saving is there.
	p2 := start(t, args(addr, dir)...)
	resp, _ = http.Get("http://" + addr + "/main/slow.md")
	if b, _ := io.ReadAll(resp.Body); string(b) != "0123456789"+"0123456789" {
		t.Errorf("saved text %q", b)
	}
	p2.stop()
}

func TestRefusesToServeAMissingSpace(t *testing.T) {
	dir, addr := filepath.Join(t.TempDir(), "data"), freeAddr(t)
	p := start(t, args(addr, dir)...)
	put(t, "http://"+addr, "/main/page", "# Page")
	p.stop()
	files, _ := filepath.Glob(filepath.Join(dir, "spaces", "*.db"))
	for _, f := range files {
		os.Remove(f)
		os.Remove(f + "-wal")
		os.Remove(f + "-shm")
	}
	os.WriteFile(filepath.Join(dir, "spaces", "77.db"), []byte("stray"), 0o600)

	p = start(t, args(addr, dir)...)
	<-p.done
	out := p.output()
	if p.code != 1 || !strings.Contains(out, "database file is missing") || !strings.Contains(out, "wakod forget-space") || !strings.Contains(out, "main (1.db)") {
		t.Errorf("exit %d:\n%s", p.code, out)
	}
	if !strings.Contains(out, "77.db") {
		t.Errorf("the stray file is not mentioned:\n%s", out)
	}
	// The tool the message names does what it says, and then the server starts.
	if out, err := exec.Command(wakod, "check", "-data-dir", dir).CombinedOutput(); err == nil || !strings.Contains(string(out), "problem:") {
		t.Errorf("check should fail: %v\n%s", err, out)
	}
	if out, err := exec.Command(wakod, "forget-space", "-data-dir", dir, "main").CombinedOutput(); err != nil {
		t.Fatalf("forget-space: %v\n%s", err, out)
	}
	p = start(t, args(addr, dir)...)
	if strings.Contains(p.output(), "missing") {
		t.Errorf("still complaining:\n%s", p.output())
	}
	p.stop()
}

func TestCommandLineMistakes(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"-tls-cert", "c.pem"}, 2, "go together"},
		{[]string{"-log-format", "xml"}, 2, "log-format"},
		{[]string{"-addr", "unix:relative.sock"}, 2, "absolute path"},
		{[]string{"-no-such-flag"}, 2, "not defined"},
		{[]string{"version"}, 0, "wakod "},
	} {
		out, err := exec.Command(wakod, c.args...).CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if code != c.code || !strings.Contains(string(out), c.msg) {
			t.Errorf("%v: exit %d (want %d)\n%s", c.args, code, c.code, out)
		}
	}
	// A bad trusted-proxies list is a startup error, not a silent default.
	dir := t.TempDir()
	out, err := exec.Command(wakod, "-addr", freeAddr(t), "-data-dir", dir, "-disable-auth", "-trusted-proxies", "nonsense").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "trusted-proxies") {
		t.Errorf("bad proxies: %v\n%s", err, out)
	}
}

func TestStructuredLogs(t *testing.T) {
	dir, addr := filepath.Join(t.TempDir(), "data"), freeAddr(t)
	p := start(t, args(addr, dir, "-log-format", "json")...)
	put(t, "http://"+addr, "/main/p", "# P")
	http.Get("http://" + addr + "/healthz")
	p.stop()
	var requests int
	for _, line := range strings.Split(strings.TrimSpace(p.output()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON log line: %q", line)
		}
		if m["msg"] == "request" {
			requests++
			if m["method"] != "PUT" || m["path"] != "/main/p" || m["status"] != float64(201) || m["ip"] != "127.0.0.1" || m["request_id"] == "" {
				t.Errorf("request line: %v", m)
			}
		}
	}
	if requests != 1 { // /healthz is below the info level
		t.Errorf("%d request lines", requests)
	}
	// Quiet when asked: wait for the port, not for a log line that is hidden.
	quietAddr := freeAddr(t)
	cmd := exec.Command(wakod, args(quietAddr, filepath.Join(t.TempDir(), "d"), "-access-log=false", "-log-level", "warn")...)
	var quiet bytes.Buffer
	cmd.Stderr = &quiet
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if c, err := net.Dial("tcp", quietAddr); err == nil {
			c.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	http.Get("http://" + quietAddr + "/main/")
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()
	if strings.Contains(quiet.String(), "listening") || strings.Contains(quiet.String(), "msg=request") || !strings.Contains(quiet.String(), "level=WARN") {
		t.Errorf("warn level should hide info lines but keep warnings:\n%s", quiet.String())
	}
}

func selfSigned(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	kd, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600)
	pool = x509.NewCertPool()
	cert, _ := x509.ParseCertificate(der)
	pool.AddCert(cert)
	return
}

func TestServesHTTPSAndHealthcheck(t *testing.T) {
	tmp := t.TempDir()
	cert, key, pool := selfSigned(t, tmp)
	addr := freeAddr(t)
	p := start(t, args(addr, filepath.Join(tmp, "data"), "-tls-cert", cert, "-tls-key", key)...)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Get("https://" + addr + "/main/")
	if err != nil {
		t.Fatalf("https: %v\n%s", err, p.output())
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Strict-Transport-Security"), "max-age=") || resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("https response: %d HSTS=%q tls=%x", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"), resp.TLS.Version)
	}
	if !strings.Contains(p.output(), "https://") {
		t.Errorf("log should say https:\n%s", p.output())
	}
	// Plain HTTP to the same port is not served.
	if r, err := http.Get("http://" + addr + "/healthz"); err == nil && r.StatusCode == 200 {
		t.Error("plain HTTP answered on a TLS port")
	}
	// The probe for containers uses the same flags and trusts the server's own certificate.
	if out, err := exec.Command(wakod, "healthcheck", "-addr", addr, "-tls-cert", cert, "-tls-key", key).CombinedOutput(); err != nil {
		t.Errorf("healthcheck: %v %s", err, out)
	}
	p.stop()
	if out, err := exec.Command(wakod, "healthcheck", "-addr", addr, "-tls-cert", cert, "-tls-key", key).CombinedOutput(); err == nil || !strings.Contains(string(out), "unhealthy") {
		t.Errorf("healthcheck of a stopped server: %v %s", err, out)
	}
}

func TestServesOnAUnixSocket(t *testing.T) {
	tmp, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	sock := filepath.Join(tmp, "wako.sock")
	os.WriteFile(sock, []byte("stale"), 0o600) // left over from a crash
	p := start(t, args("unix:"+sock, filepath.Join(tmp, "data"))...)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequest("GET", "http://localhost/main/", nil)
	// The proxy in front is local, so what it forwards is believed.
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%v\n%s", err, p.output())
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Errorf("%d HSTS=%q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o660 {
		t.Errorf("socket mode %v", fi.Mode().Perm())
	}
	if out, err := exec.Command(wakod, "healthcheck", "-addr", "unix:"+sock).CombinedOutput(); err != nil {
		t.Errorf("healthcheck over the socket: %v %s", err, out)
	}
	p.stop()
}

func TestSignInOnByDefaultAndEnvironmentOptional(t *testing.T) {
	dir, addr := filepath.Join(t.TempDir(), "data"), freeAddr(t)
	// No environment at all (start() clears it), a password on the command line.
	p := start(t, "-addr", addr, "-data-dir", dir, "-admin-password", "correct horse battery")
	resp, _ := http.Post("http://"+addr+"/_api/login", "application/json", strings.NewReader(`{"name":"admin","password":"correct horse battery"}`))
	if resp.StatusCode != 200 {
		t.Errorf("login: %d\n%s", resp.StatusCode, p.output())
	}
	if strings.Contains(p.output(), "correct horse battery") {
		t.Errorf("the password reached the log:\n%s", p.output())
	}
	if code, out := func() (int, string) {
		c := exec.Command(wakod, "check", "-data-dir", dir, "-deep")
		b, err := c.CombinedOutput()
		if err != nil {
			return 1, string(b)
		}
		return 0, string(b)
	}(); code != 0 || !strings.Contains(out, "no problems found") {
		t.Errorf("check while running: %d %s", code, out)
	}
	p.stop()
	// An unreadable data directory is said so, and the generated password is shown once, on the console only.
	loose := filepath.Join(t.TempDir(), "loose")
	os.Mkdir(loose, 0o755)
	os.Chmod(loose, 0o755)
	p = start(t, "-addr", freeAddr(t), "-data-dir", loose)
	if !strings.Contains(p.output(), "can be read by other users") || !strings.Contains(p.output(), "chmod 700") {
		t.Errorf("no warning about permissions:\n%s", p.output())
	}
	if !strings.Contains(p.output(), `administrator "admin", password `) {
		t.Errorf("no generated password:\n%s", p.output())
	}
	p.stop()
}
