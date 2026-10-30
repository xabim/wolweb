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
	"strconv"
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
	checkTimeout time.Duration
	checkEvery   time.Duration

	broadcast   string // default for servers that do not set their own
	wolCooldown time.Duration

	title   string
	servers []serverCfg
}

// serverCfg describes one machine that can be woken.
type serverCfg struct {
	Name      string `toml:"name"`
	MAC       string `toml:"mac"`
	CheckAddr string `toml:"checkAddr"`
	Broadcast string `toml:"broadcast"`
	Links     []link `toml:"links"`
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
	Links   []link      `toml:"links"`
	Servers []serverCfg `toml:"servers"`
}

type state struct {
	mu        sync.RWMutex
	online    bool
	lastCheck time.Time
	lastError string
	lastWOL   time.Time
}

type pageData struct {
	Title       string
	Multi       bool
	Now         string
	RefreshSecs int
	Servers     []serverView
}

type serverView struct {
	ID           int
	Name         string
	SubStatus    string
	Status       string
	Online       bool
	CheckAddr    string
	LastCheckStr string
	LastWOLStr   string
	LastError    string
	CoolingDown  bool
	CooldownMsg  string
	Links        []linkView
}

// target is a configured server together with its runtime state.
type target struct {
	cfg   serverCfg
	st    *state
	links []linkView
}

type server struct {
	cfg     config
	tmpl    *template.Template
	targets []*target
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
	err = run(ctx, srv)
	stop()
	if err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, s *server) error {
	// First check immediately, then periodic.
	s.refreshAll()
	go func() {
		t := time.NewTicker(s.cfg.checkEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshAll()
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
	log.Printf("wolweb: listening on %s (%d server(s), check every %s)", s.cfg.listenAddr, len(s.targets), s.cfg.checkEvery)

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

	targets := make([]*target, len(cfg.servers))
	for i, sc := range cfg.servers {
		links := make([]linkView, len(sc.Links))
		for j, l := range sc.Links {
			// Links come from the operator's config file, which is trusted, so
			// they may use any scheme (e.g. ssh://). Validated in parseConfig.
			links[j] = linkView{Name: l.Name, URL: template.URL(l.URL), Icon: l.Icon} //nolint:gosec
		}
		targets[i] = &target{cfg: sc, st: &state{}, links: links}
	}

	return &server{cfg: cfg, tmpl: tmpl, targets: targets}, nil
}

func (s *server) routes(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", s.handleHome)
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		s.handleWake(ctx, w, r)
	})
	return mux
}

// refreshAll checks every server concurrently.
func (s *server) refreshAll() {
	var wg sync.WaitGroup
	for _, t := range s.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.refresh(t)
		}()
	}
	wg.Wait()
}

// refresh runs a TCP check against t and records the result.
func (s *server) refresh(t *target) {
	ok, err := tcpCheck(t.cfg.CheckAddr, s.cfg.checkTimeout)
	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	t.st.online = ok
	t.st.lastCheck = time.Now()
	t.st.lastError = ""
	if err != nil {
		t.st.lastError = t.st.lastCheck.Format(timeFormat) + " " + err.Error()
	}
}

// lookup resolves the "server" form value to a target. It may be omitted
// when only one server is configured.
func (s *server) lookup(id string) (*target, bool) {
	if id == "" && len(s.targets) == 1 {
		return s.targets[0], true
	}
	n, err := strconv.Atoi(id)
	if err != nil || n < 0 || n >= len(s.targets) {
		return nil, false
	}
	return s.targets[n], true
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

	t, ok := s.lookup(r.FormValue("server"))
	if !ok {
		http.Error(w, "unknown server", http.StatusBadRequest)
		return
	}

	// Check online status and cooldown, and reserve the slot, under a single
	// lock so concurrent requests cannot both send a packet.
	t.st.mu.Lock()
	if t.st.online || time.Since(t.st.lastWOL) < s.cfg.wolCooldown {
		t.st.mu.Unlock()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	prev := t.st.lastWOL
	t.st.lastWOL = time.Now()
	t.st.mu.Unlock()

	if err := sendWOL(t.cfg.MAC, t.cfg.Broadcast); err != nil {
		t.st.mu.Lock()
		t.st.lastWOL = prev
		t.st.mu.Unlock()
		log.Printf("wol failed: %v", err)
		http.Error(w, "failed to send wake-on-lan packet", http.StatusInternalServerError)
		return
	}

	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(postWOLCheckDelay):
			s.refresh(t)
		}
	}()

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleHome(w http.ResponseWriter, _ *http.Request) {
	data := pageData{
		Title:       s.cfg.title,
		Multi:       len(s.targets) > 1,
		Now:         time.Now().Format(timeFormat),
		RefreshSecs: 30,
	}
	for i, t := range s.targets {
		v := s.view(i, t)
		// Reload right when a button becomes available again.
		if v.CoolingDown {
			if secs := int(v.cooldownLeft.Seconds()) + 1; secs < data.RefreshSecs {
				data.RefreshSecs = secs
			}
		}
		data.Servers = append(data.Servers, v.serverView)
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

type targetView struct {
	serverView
	cooldownLeft time.Duration
}

func (s *server) view(id int, t *target) targetView {
	t.st.mu.RLock()
	online := t.st.online
	lastCheck := t.st.lastCheck
	lastError := t.st.lastError
	lastWOL := t.st.lastWOL
	t.st.mu.RUnlock()

	name := t.cfg.Name
	if name == "" {
		name = s.cfg.title
	}
	v := targetView{serverView: serverView{
		ID:           id,
		Name:         name,
		SubStatus:    "Offline",
		Status:       "OFF",
		Online:       online,
		CheckAddr:    t.cfg.CheckAddr,
		LastCheckStr: formatTime(lastCheck),
		LastWOLStr:   formatTime(lastWOL),
		LastError:    emptyIf(lastError, "-"),
		Links:        t.links,
	}}
	if online {
		v.SubStatus = "Online"
		v.Status = "ON"
	}
	if !lastWOL.IsZero() {
		if left := s.cfg.wolCooldown - time.Since(lastWOL); left > 0 && !online {
			v.CoolingDown = true
			v.cooldownLeft = left
			v.CooldownMsg = fmt.Sprintf("Packet sent %s ago, wait %s before sending another",
				time.Since(lastWOL).Round(time.Second), left.Round(time.Second))
		}
	}
	return v
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
	var single serverCfg // from -mac/-checkAddr and the [server] table

	fs := flag.NewFlagSet("wolweb", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&configFile, "config", "", "path to TOML config file")
	fs.StringVar(&cfg.listenAddr, "listen", ":8080", "listen address, e.g. :80 or 0.0.0.0:8080")
	fs.StringVar(&single.CheckAddr, "checkAddr", "", "TCP address to check, e.g. 192.168.1.101:22 (required)")
	fs.DurationVar(&cfg.checkTimeout, "checkTimeout", 1*time.Second, "TCP check timeout")
	fs.DurationVar(&cfg.checkEvery, "checkEvery", 5*time.Minute, "how often to refresh status in background")
	fs.StringVar(&single.MAC, "mac", "", "target MAC address (required)")
	fs.StringVar(&cfg.broadcast, "broadcast", "255.255.255.255:9", "broadcast UDP address, e.g. 192.168.1.255:9")
	fs.DurationVar(&cfg.wolCooldown, "cooldown", 30*time.Second, "minimum time between WoL sends")
	fs.StringVar(&cfg.title, "title", "Server Power", "UI page title")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if configFile == "" && fs.NFlag() == 0 {
		return config{}, errors.New("no configuration: pass -config <file> and/or flags (see -h)")
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
		if err := applyTOML(&cfg, &single, tc, explicit); err != nil {
			return config{}, err
		}
		if len(tc.Servers) > 0 {
			if single.MAC != "" || single.CheckAddr != "" {
				return config{}, errors.New("use either [[servers]] or mac/checkAddr, not both")
			}
			if len(tc.Links) > 0 {
				return config{}, errors.New("top-level [[links]] cannot be combined with [[servers]]; put links under each server")
			}
			cfg.servers = tc.Servers
		}
	}
	if cfg.servers == nil {
		cfg.servers = []serverCfg{single}
	}
	for i := range cfg.servers {
		if cfg.servers[i].Broadcast == "" {
			cfg.servers[i].Broadcast = cfg.broadcast
		}
	}

	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// applyTOML overlays TOML values onto cfg, skipping anything set by an
// explicit command-line flag.
func applyTOML(cfg *config, single *serverCfg, tc tomlConfig, explicit map[string]bool) error {
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
	str("mac", s.Mac, &single.MAC)
	str("checkAddr", s.CheckAddr, &single.CheckAddr)
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
	single.Links = tc.Links
	return nil
}

func (c config) validate() error {
	if c.listenAddr == "" {
		return errors.New("listen must not be empty")
	}
	if c.checkTimeout <= 0 || c.checkEvery <= 0 {
		return errors.New("checkTimeout and checkEvery must be positive")
	}
	if c.wolCooldown < 0 {
		return errors.New("cooldown must not be negative")
	}
	if _, _, err := net.SplitHostPort(c.broadcast); err != nil {
		return fmt.Errorf("invalid broadcast %q: %w", c.broadcast, err)
	}

	names := map[string]bool{}
	for i, sc := range c.servers {
		if len(c.servers) > 1 {
			if sc.Name == "" {
				return fmt.Errorf("server #%d: name is required when several servers are configured", i+1)
			}
			if names[sc.Name] {
				return fmt.Errorf("duplicate server name %q", sc.Name)
			}
			names[sc.Name] = true
		}
		if err := sc.validate(); err != nil {
			if sc.Name != "" {
				return fmt.Errorf("server %q: %w", sc.Name, err)
			}
			return err
		}
	}
	return nil
}

func (sc serverCfg) validate() error {
	if sc.MAC == "" {
		return errors.New("mac is required")
	}
	if _, err := net.ParseMAC(sc.MAC); err != nil {
		return fmt.Errorf("invalid mac %q: %w", sc.MAC, err)
	}
	if sc.CheckAddr == "" {
		return errors.New("checkAddr is required")
	}
	if _, _, err := net.SplitHostPort(sc.CheckAddr); err != nil {
		return fmt.Errorf("invalid checkAddr %q: %w", sc.CheckAddr, err)
	}
	if _, _, err := net.SplitHostPort(sc.Broadcast); err != nil {
		return fmt.Errorf("invalid broadcast %q: %w", sc.Broadcast, err)
	}
	for _, l := range sc.Links {
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

	_, err = conn.Write(buildMagicPacket(mac))
	if cerr := conn.Close(); err == nil {
		err = cerr
	}
	return err
}

func emptyIf(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
