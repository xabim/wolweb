package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
)

//go:embed template.html
var templateFS embed.FS

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

type tomlConfig struct {
	Server struct {
		Mac         string        `toml:"mac"`
		CheckAddr   string        `toml:"checkAddr"`
		CheckTimeout string       `toml:"checkTimeout"`
		CheckEvery  string        `toml:"checkEvery"`
		Broadcast   string        `toml:"broadcast"`
		Cooldown    string        `toml:"cooldown"`
		Title       string        `toml:"title"`
		Listen      string        `toml:"listen"`
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
	Badge        string
	CheckAddr    string
	LastCheckStr string
	LastWOLStr   string
	LastError    string
	DisabledAttr string
	Now          string
	Links        []link
}

func main() {
	cfg := parseFlags()

	st := &state{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// First check immediately, then periodic.
	doCheck := func() {
		ok, err := tcpCheck(cfg.checkAddr, cfg.checkTimeout)
		st.mu.Lock()
		st.online = ok
		st.lastCheck = time.Now()
		if err != nil {
			st.lastError = err.Error()
		} else {
			st.lastError = ""
		}
		st.mu.Unlock()
	}
	doCheck()

	go func() {
		t := time.NewTicker(cfg.checkEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				doCheck()
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		renderHome(w, cfg, st)
	})
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// If already online, no need to send WOL.
		st.mu.RLock()
		online := st.online
		lastWOL := st.lastWOL
		st.mu.RUnlock()
		if online {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		// Cooldown to avoid spamming WOL.
		if time.Since(lastWOL) < cfg.wolCooldown {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}

		if err := sendWOL(cfg.macAddr, cfg.broadcast); err != nil {
			http.Error(w, "wol failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		st.mu.Lock()
		st.lastWOL = time.Now()
		st.mu.Unlock()

		// Optional: trigger an immediate re-check shortly after sending WOL
		// so the UI flips faster than 5 minutes.
		go func() {
			time.Sleep(8 * time.Second)
			ok, err := tcpCheck(cfg.checkAddr, cfg.checkTimeout)
			st.mu.Lock()
			st.online = ok
			st.lastCheck = time.Now()
			if err != nil {
				st.lastError = err.Error()
			} else {
				st.lastError = ""
			}
			st.mu.Unlock()
		}()

		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("wolweb: listening on %s (check=%s every=%s)\n", cfg.listenAddr, cfg.checkAddr, cfg.checkEvery)
	log.Fatal(srv.ListenAndServe())
}

func parseFlags() config {
	var cfg config
	var configFile string

	flag.StringVar(&configFile, "config", "", "path to TOML config file")
	flag.StringVar(&cfg.listenAddr, "listen", ":8080", "listen address, e.g. :80 or 0.0.0.0:8080")
	flag.StringVar(&cfg.checkAddr, "checkAddr", "192.168.1.101:22", "TCP address to check, e.g. 192.168.1.101:22")
	flag.DurationVar(&cfg.checkTimeout, "checkTimeout", 1*time.Second, "TCP check timeout")
	flag.DurationVar(&cfg.checkEvery, "checkEvery", 5*time.Minute, "how often to refresh status in background")

	flag.StringVar(&cfg.macAddr, "mac", "34:5a:60:d4:31:34", "target MAC address")
	flag.StringVar(&cfg.broadcast, "broadcast", "192.168.1.255:9", "broadcast UDP address, e.g. 192.168.1.255:9")
	flag.DurationVar(&cfg.wolCooldown, "cooldown", 30*time.Second, "minimum time between WoL sends")

	flag.StringVar(&cfg.title, "title", "Server Power", "UI page title")

	flag.Parse()

	// If config file is provided, load it
	if configFile != "" {
		data, err := os.ReadFile(configFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading config file: %v\n", err)
			os.Exit(1)
		}

		var tomlCfg tomlConfig
		if err := toml.Unmarshal(data, &tomlCfg); err != nil {
			fmt.Fprintf(os.Stderr, "error parsing config file: %v\n", err)
			os.Exit(1)
		}

		// Load from TOML, CLI flags override TOML values
		if tomlCfg.Server.Listen != "" {
			cfg.listenAddr = tomlCfg.Server.Listen
		}
		if tomlCfg.Server.Mac != "" {
			cfg.macAddr = tomlCfg.Server.Mac
		}
		if tomlCfg.Server.CheckAddr != "" {
			cfg.checkAddr = tomlCfg.Server.CheckAddr
		}
		if tomlCfg.Server.CheckTimeout != "" {
			d, err := time.ParseDuration(tomlCfg.Server.CheckTimeout)
			if err == nil {
				cfg.checkTimeout = d
			}
		}
		if tomlCfg.Server.CheckEvery != "" {
			d, err := time.ParseDuration(tomlCfg.Server.CheckEvery)
			if err == nil {
				cfg.checkEvery = d
			}
		}
		if tomlCfg.Server.Broadcast != "" {
			cfg.broadcast = tomlCfg.Server.Broadcast
		}
		if tomlCfg.Server.Cooldown != "" {
			d, err := time.ParseDuration(tomlCfg.Server.Cooldown)
			if err == nil {
				cfg.wolCooldown = d
			}
		}
		if tomlCfg.Server.Title != "" {
			cfg.title = tomlCfg.Server.Title
		}
		cfg.links = tomlCfg.Links
	}

	// Validation
	if cfg.listenAddr == "" || cfg.checkAddr == "" || cfg.macAddr == "" || cfg.broadcast == "" {
		fmt.Fprintln(os.Stderr, "error: missing required flags")
		os.Exit(2)
	}

	return cfg
}

func tcpCheck(addr string, timeout time.Duration) (bool, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	return true, nil
}

func sendWOL(macStr, bcast string) error {
	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return err
	}

	// magic packet
	packet := make([]byte, 6+16*len(mac))
	for i := 0; i < 6; i++ {
		packet[i] = 0xff
	}
	for i := 6; i < len(packet); i += len(mac) {
		copy(packet[i:], mac)
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

	_, err = conn.Write(packet)
	return err
}

func renderHome(w http.ResponseWriter, cfg config, st *state) {
	st.mu.RLock()
	online := st.online
	lastCheck := st.lastCheck
	lastError := st.lastError
	lastWOL := st.lastWOL
	st.mu.RUnlock()

	status := "OFF"
	badge := "#dc2626" // red
	sub := "Offline"
	if online {
		status = "ON"
		badge = "#16a34a" // green
		sub = "Online"
	}

	now := time.Now()
	lastCheckStr := "never"
	if !lastCheck.IsZero() {
		lastCheckStr = lastCheck.Format("2006-01-02 15:04:05")
	}
	lastWOLStr := "never"
	if !lastWOL.IsZero() {
		lastWOLStr = lastWOL.Format("2006-01-02 15:04:05")
	}

	disabledAttr := ""
	if online {
		disabledAttr = "disabled"
	}

	data := pageData{
		Title:        cfg.title,
		SubStatus:    sub,
		Status:       status,
		Badge:        badge,
		CheckAddr:    cfg.checkAddr,
		LastCheckStr: lastCheckStr,
		LastWOLStr:   lastWOLStr,
		LastError:    emptyIf(lastError, "-"),
		DisabledAttr: disabledAttr,
		Now:          now.Format("2006-01-02 15:04:05"),
		Links:        cfg.links,
	}

	tmpl, err := template.ParseFS(templateFS, "template.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("template error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("template render error: %v", err)
	}
}

func emptyIf(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
