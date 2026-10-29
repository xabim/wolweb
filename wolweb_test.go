package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildMagicPacket(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	p := buildMagicPacket(mac)
	if len(p) != 102 {
		t.Fatalf("len = %d, want 102", len(p))
	}
	if !bytes.Equal(p[:6], bytes.Repeat([]byte{0xff}, 6)) {
		t.Errorf("bad header: %x", p[:6])
	}
	for i := 0; i < 16; i++ {
		if !bytes.Equal(p[6+i*6:12+i*6], mac) {
			t.Errorf("repetition %d mismatch", i)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseConfigPrecedence(t *testing.T) {
	p := writeConfig(t, `
[server]
mac = "aa:bb:cc:dd:ee:ff"
checkAddr = "10.0.0.1:22"
title = "from toml"
cooldown = "7s"
`)
	cfg, err := parseConfig([]string{"-config", p, "-title", "from flag"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.title != "from flag" {
		t.Errorf("title = %q, want flag to override TOML", cfg.title)
	}
	if cfg.wolCooldown != 7*time.Second {
		t.Errorf("cooldown = %v, want 7s from TOML", cfg.wolCooldown)
	}
	if cfg.checkTimeout != time.Second {
		t.Errorf("checkTimeout = %v, want default", cfg.checkTimeout)
	}
}

func TestParseConfigErrors(t *testing.T) {
	good := `mac = "aa:bb:cc:dd:ee:ff"
checkAddr = "10.0.0.1:22"
`
	tests := map[string]string{
		"missing mac":      "[server]\ncheckAddr = \"10.0.0.1:22\"\n",
		"bad mac":          "[server]\nmac = \"nope\"\ncheckAddr = \"10.0.0.1:22\"\n",
		"missing check":    "[server]\nmac = \"aa:bb:cc:dd:ee:ff\"\n",
		"bad duration":     "[server]\n" + good + "checkEvery = \"soon\"\n",
		"unknown key":      "[server]\n" + good + "chekEvery = \"1s\"\n",
		"javascript link":  "[server]\n" + good + "[[links]]\nname=\"x\"\nurl=\"javascript:alert(1)\"\n",
		"schemeless link":  "[server]\n" + good + "[[links]]\nname=\"x\"\nurl=\"example.com\"\n",
		"zero checkEvery":  "[server]\n" + good + "checkEvery = \"0s\"\n",
		"bad broadcastadr": "[server]\n" + good + "broadcast = \"255.255.255.255\"\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]string{"-config", writeConfig(t, body)}, &bytes.Buffer{}); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func testServer(t *testing.T, cfg config) *server {
	t.Helper()
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHomeEscapesAndLinks(t *testing.T) {
	s := testServer(t, config{
		title:     `<script>alert(1)</script>`,
		checkAddr: "127.0.0.1:1",
		links:     []link{{Name: "SSH", URL: "ssh://user@host", Icon: "x"}},
	})
	s.st.online = true
	s.st.lastError = `<img src=x onerror=alert(1)>`

	rec := httptest.NewRecorder()
	s.routes(context.Background()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, bad := range []string{"<script>alert", "<img src=x", "ZgotmplZ"} {
		if strings.Contains(body, bad) {
			t.Errorf("body contains %q", bad)
		}
	}
	if !strings.Contains(body, `href="ssh://user@host"`) {
		t.Error("ssh link was not rendered")
	}
	if !strings.Contains(body, `class="dot on"`) {
		t.Error("online dot class missing")
	}
}

func TestLinksHiddenWhenOffline(t *testing.T) {
	s := testServer(t, config{links: []link{{Name: "A", URL: "https://a.example", Icon: "x"}}})
	rec := httptest.NewRecorder()
	s.routes(context.Background()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "a.example") {
		t.Error("links shown while offline")
	}
}

func TestWakeHandler(t *testing.T) {
	// Listen on a UDP port to receive the magic packet.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("udp unavailable: %v", err)
	}
	defer pc.Close()

	s := testServer(t, config{
		macAddr:      "aa:bb:cc:dd:ee:ff",
		broadcast:    pc.LocalAddr().String(),
		checkAddr:    "127.0.0.1:1",
		checkTimeout: 50 * time.Millisecond,
		wolCooldown:  time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := s.routes(ctx)

	do := func(method string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/wake", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if c := do(http.MethodGet, nil).Code; c != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", c)
	}
	if c := do(http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}).Code; c != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", c)
	}
	if c := do(http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}).Code; c != http.StatusSeeOther {
		t.Errorf("POST = %d, want 303", c)
	}

	buf := make([]byte, 256)
	_ = pc.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil || n != 102 {
		t.Fatalf("expected 102-byte magic packet, got n=%d err=%v", n, err)
	}

	// Second POST is within cooldown: no packet.
	do(http.MethodPost, nil)
	_ = pc.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := pc.ReadFrom(buf); err == nil {
		t.Error("packet sent during cooldown")
	}
}

func TestTCPCheck(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	addr := l.Addr().String()
	if ok, _ := tcpCheck(addr, time.Second); !ok {
		t.Error("expected online")
	}
	l.Close()
	if ok, err := tcpCheck(addr, time.Second); ok || err == nil {
		t.Error("expected offline with error")
	}
}

func TestParseConfigRequiresConfigOrFlags(t *testing.T) {
	if _, err := parseConfig(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no configuration") {
		t.Fatalf("expected no-configuration error, got %v", err)
	}
}

func TestHomeShowsCooldown(t *testing.T) {
	s := testServer(t, config{checkAddr: "127.0.0.1:1", wolCooldown: time.Hour})
	s.st.lastWOL = time.Now().Add(-time.Minute)

	rec := httptest.NewRecorder()
	s.handleHome(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Packet sent") || !strings.Contains(body, "disabled") {
		t.Errorf("expected cooldown notice and disabled button, got:\n%s", body)
	}
}
