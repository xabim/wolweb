package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed template.html
var templateFS embed.FS

const timeFormat = "2006-01-02 15:04:05"

// postWOLCheckDelay is how long to wait after sending a magic packet before
// re-checking status, so the UI flips faster than the periodic check.
const postWOLCheckDelay = 8 * time.Second

type config struct {
	listenAddr   string
	checkAddr    string
	checkTimeout time.Duration
	checkEvery   time.Duration

	macAddr     string
	broadcast   string
	wolCooldown time.Duration

	title string
	links []link
}

type link struct {
	Name string `toml:"name"`
	URL  string `toml:"url"`
	Icon string `toml:"icon"`
}

// linkView is a link whose URL has been validated and may be emitted as-is
// (including non-http schemes such as ssh://).
type linkView struct {
	Name string
	URL  template.URL
	Icon string
}

type tomlConfig struct {
	Server struct {
		Mac          string `toml:"mac"`
		CheckAddr    string `toml:"checkAddr"`
		CheckTimeout string `toml:"checkTimeout"`
		CheckEvery   string `toml:"checkEvery"`
		Broadcast    string `toml:"broadcast"`
		Cooldown     string `toml:"cooldown"`
		Title        string `toml:"title"`
		Listen       string `toml:"listen"`
	} `toml:"server"`
	Links []link `toml:"links"`
}

type state struct {
	mu        sync.RWMutex
	online    bool
	lastCheck time.Time
	lastError string
	lastWOL   time.Time
}

type pageData struct {
	Title        string
	SubStatus    string
	Status       string
	Online       bool
	CheckAddr    string
	LastCheckStr string
	LastWOLStr   string
	LastError    string
	Now          string
	Links        []linkView
}

type server struct {
	cfg   config
	st    *state
	tmpl  *template.Template
	links []linkView
}

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatalf("error: %v", err)
	}

	srv, err := newServer(cfg)
	if err != nil {
		log.Fatalf("error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, srv); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, s *server) error {
	// First check immediately, then periodic.
	s.refresh()
	go func() {
		t := time.NewTicker(s.cfg.checkEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refresh()
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              s.cfg.listenAddr,
		Handler:           s.routes(ctx),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	log.Printf("wolweb: listening on %s (check=%s every=%s)", s.cfg.listenAddr, s.cfg.checkAddr, s.cfg.checkEvery)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func newServer(cfg config) (*server, error) {
	tmpl, err := template.ParseFS(templateFS, "template.html")
	if err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	links := make([]linkView, len(cfg.links))
	for i, l := range cfg.links {
		// Links come from the operator's config file, which is trusted, so
		// they may use any scheme (e.g. ssh://). Validated in parseConfig.
		links[i] = linkView{Name: l.Name, URL: template.URL(l.URL), Icon: l.Icon} //nolint:gosec
	}

	return &server{cfg: cfg, st: &state{}, tmpl: tmpl, links: links}, nil
}

func (s *server) routes(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", s.handleHome)
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		s.handleWake(ctx, w, r)
	})
	return mux
}

// refresh runs a TCP check and records the result.
func (s *server) refresh() {
	ok, err := tcpCheck(s.cfg.checkAddr, s.cfg.checkTimeout)
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	s.st.online = ok
	s.st.lastCheck = time.Now()
	s.st.lastError = ""
	if err != nil {
		s.st.lastError = s.st.lastCheck.Format(timeFormat) + " " + err.Error()
	}
}

func (s *server) handleWake(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Reject cross-site form posts (CSRF). Browsers always send
	// Sec-Fetch-Site; non-browser clients (curl) omit it and are allowed.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}

	// Check online status and cooldown, and reserve the slot, under a single
	// lock so concurrent requests cannot both send a packet.
	s.st.mu.Lock()
	if s.st.online || time.Since(s.st.lastWOL) < s.cfg.wolCooldown {
		s.st.mu.Unlock()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	prev := s.st.lastWOL
	s.st.lastWOL = time.Now()
	s.st.mu.Unlock()

	if err := sendWOL(s.cfg.macAddr, s.cfg.broadcast); err != nil {
		s.st.mu.Lock()
		s.st.lastWOL = prev
		s.st.mu.Unlock()
		log.Printf("wol failed: %v", err)
		http.Error(w, "failed to send wake-on-lan packet", http.StatusInternalServerError)
		return
	}

	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(postWOLCheckDelay):
			s.refresh()
		}
	}()

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleHome(w http.ResponseWriter, _ *http.Request) {
	s.st.mu.RLock()
	online := s.st.online
	lastCheck := s.st.lastCheck
	lastError := s.st.lastError
	lastWOL := s.st.lastWOL
	s.st.mu.RUnlock()

	data := pageData{
		Title:        s.cfg.title,
		SubStatus:    "Offline",
		Status:       "OFF",
		Online:       online,
		CheckAddr:    s.cfg.checkAddr,
		LastCheckStr: formatTime(lastCheck),
		LastWOLStr:   formatTime(lastWOL),
		LastError:    emptyIf(lastError, "-"),
		Now:          time.Now().Format(timeFormat),
		Links:        s.links,
	}
	if online {
		data.SubStatus = "Online"
		data.Status = "ON"
	}

	// Render to a buffer so a template failure yields a clean 500 instead of
	// a half-written page.
	var buf strings.Builder
	if err := s.tmpl.Execute(&buf, data); err != nil {
		log.Printf("template render error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = io.WriteString(w, buf.String())
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(timeFormat)
}

// parseConfig builds the configuration with precedence
// defaults < TOML file < explicitly set command-line flags.
func parseConfig(args []string, output io.Writer) (config, error) {
	var cfg config
	var configFile string

	fs := flag.NewFlagSet("wolweb", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&configFile, "config", "", "path to TOML config file")
	fs.StringVar(&cfg.listenAddr, "listen", ":8080", "listen address, e.g. :80 or 0.0.0.0:8080")
	fs.StringVar(&cfg.checkAddr, "checkAddr", "", "TCP address to check, e.g. 192.168.1.101:22 (required)")
	fs.DurationVar(&cfg.checkTimeout, "checkTimeout", 1*time.Second, "TCP check timeout")
	fs.DurationVar(&cfg.checkEvery, "checkEvery", 5*time.Minute, "how often to refresh status in background")
	fs.StringVar(&cfg.macAddr, "mac", "", "target MAC address (required)")
	fs.StringVar(&cfg.broadcast, "broadcast", "255.255.255.255:9", "broadcast UDP address, e.g. 192.168.1.255:9")
	fs.DurationVar(&cfg.wolCooldown, "cooldown", 30*time.Second, "minimum time between WoL sends")
	fs.StringVar(&cfg.title, "title", "Server Power", "UI page title")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if configFile != "" {
		explicit := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

		data, err := os.ReadFile(configFile)
		if err != nil {
			return config{}, fmt.Errorf("reading config file: %w", err)
		}
		var tc tomlConfig
		md, err := toml.Decode(string(data), &tc)
		if err != nil {
			return config{}, fmt.Errorf("parsing config file: %w", err)
		}
		if u := md.Undecoded(); len(u) > 0 {
			return config{}, fmt.Errorf("config file: unknown keys: %v", u)
		}
		if err := applyTOML(&cfg, tc, explicit); err != nil {
			return config{}, err
		}
	}

	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// applyTOML overlays TOML values onto cfg, skipping anything set by an
// explicit command-line flag.
func applyTOML(cfg *config, tc tomlConfig, explicit map[string]bool) error {
	str := func(flagName, v string, dst *string) {
		if v != "" && !explicit[flagName] {
			*dst = v
		}
	}
	dur := func(flagName, v string, dst *time.Duration) error {
		if v == "" || explicit[flagName] {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("config file: invalid %s %q: %w", flagName, v, err)
		}
		*dst = d
		return nil
	}

	s := tc.Server
	str("listen", s.Listen, &cfg.listenAddr)
	str("mac", s.Mac, &cfg.macAddr)
	str("checkAddr", s.CheckAddr, &cfg.checkAddr)
	str("broadcast", s.Broadcast, &cfg.broadcast)
	str("title", s.Title, &cfg.title)
	for _, d := range []struct {
		name string
		v    string
		dst  *time.Duration
	}{
		{"checkTimeout", s.CheckTimeout, &cfg.checkTimeout},
		{"checkEvery", s.CheckEvery, &cfg.checkEvery},
		{"cooldown", s.Cooldown, &cfg.wolCooldown},
	} {
		if err := dur(d.name, d.v, d.dst); err != nil {
			return err
		}
	}
	cfg.links = tc.Links
	return nil
}

func (c config) validate() error {
	if c.macAddr == "" {
		return errors.New("mac is required")
	}
	if _, err := net.ParseMAC(c.macAddr); err != nil {
		return fmt.Errorf("invalid mac %q: %w", c.macAddr, err)
	}
	if c.checkAddr == "" {
		return errors.New("checkAddr is required")
	}
	if _, _, err := net.SplitHostPort(c.checkAddr); err != nil {
		return fmt.Errorf("invalid checkAddr %q: %w", c.checkAddr, err)
	}
	if _, _, err := net.SplitHostPort(c.broadcast); err != nil {
		return fmt.Errorf("invalid broadcast %q: %w", c.broadcast, err)
	}
	if c.listenAddr == "" {
		return errors.New("listen must not be empty")
	}
	if c.checkTimeout <= 0 || c.checkEvery <= 0 {
		return errors.New("checkTimeout and checkEvery must be positive")
	}
	if c.wolCooldown < 0 {
		return errors.New("cooldown must not be negative")
	}
	for _, l := range c.links {
		u, err := url.Parse(l.URL)
		if err != nil || u.Scheme == "" {
			return fmt.Errorf("link %q: invalid url %q (a scheme such as https:// is required)", l.Name, l.URL)
		}
		switch strings.ToLower(u.Scheme) {
		case "javascript", "data", "vbscript":
			return fmt.Errorf("link %q: url scheme %q is not allowed", l.Name, u.Scheme)
		}
	}
	return nil
}

func tcpCheck(addr string, timeout time.Duration) (bool, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	return true, nil
}

// buildMagicPacket returns 6 bytes of 0xff followed by the MAC repeated 16 times.
func buildMagicPacket(mac net.HardwareAddr) []byte {
	packet := make([]byte, 6+16*len(mac))
	for i := 0; i < 6; i++ {
		packet[i] = 0xff
	}
	for i := 6; i < len(packet); i += len(mac) {
		copy(packet[i:], mac)
	}
	return packet
}

func sendWOL(macStr, bcast string) error {
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return err
	}

	addr, err := net.ResolveUDPAddr("udp", bcast)
	if err != nil {
		return err
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.Write(buildMagicPacket(mac))
	return err
}

func emptyIf(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
