// Command baby-monitor streams the laptop microphone to phones over WebRTC.
//
// One ffmpeg process captures the default PulseAudio/PipeWire source, encodes
// Opus, and emits RTP to a local UDP port. A single shared WebRTC track is fed
// from that socket and added to every connected phone, so one capture serves
// all clients. The browser page (served at "/") is a receive-only peer with a
// big start/stop toggle and automatic reconnect.
package main

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/mdp/qrterminal/v3"
	"github.com/pion/webrtc/v4"
)

//go:embed static/index.html
var staticFS embed.FS

// server holds the shared WebRTC API, the single mic track that every phone
// subscribes to, and the registry used to relay push-to-talk audio. host is the
// machine name shown to phones as the audio source.
//
// Talkback is the reverse direction: a phone holds its talk button, captures
// its mic, and the server forwards that audio to every *other* connected phone
// via that phone's per-client talkback track. Only one phone may hold the talk
// lock (mu/talker) at a time.
type server struct {
	api   *webrtc.API
	track *webrtc.TrackLocalStaticRTP
	host  string // hostname shown to phones as the audio source

	mu         sync.Mutex
	clients    map[string]*client       // by client id; the active talkback subscribers
	talker     string                   // id of the phone currently holding the mic, or ""
	talkerSubs map[chan talkerInfo]struct{} // SSE listeners notified when talker changes
}

// newServer builds the shared WebRTC API (using the given SettingEngine, which
// carries the ICE UDP port range) and the single Opus mic track every phone
// subscribes to. Splitting this out of main keeps the WebRTC wiring testable.
func newServer(se webrtc.SettingEngine) (*server, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register codecs: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "baby-monitor",
	)
	if err != nil {
		return nil, fmt.Errorf("create track: %w", err)
	}

	// The hostname identifies this machine to phones as the audio source.
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "baby-monitor"
	}
	return &server{
		api:        api,
		track:      track,
		host:       host,
		clients:    make(map[string]*client),
		talkerSubs: make(map[chan talkerInfo]struct{}),
	}, nil
}

func main() {
	addr := flag.String("addr", ":8000", "HTTP listen address (binds all interfaces so phones can reach it)")
	source := flag.String("source", "default", "PulseAudio/PipeWire capture source")
	bitrate := flag.String("bitrate", "64k", "Opus bitrate")
	rtpPort := flag.Int("rtp-port", 5004, "local UDP port for the ffmpeg->server RTP feed")
	iceMin := flag.Int("ice-min", 50000, "min UDP port for WebRTC ICE host candidates")
	iceMax := flag.Int("ice-max", 50010, "max UDP port for WebRTC ICE host candidates")
	useTLS := flag.Bool("tls", false, "serve HTTPS with an in-memory self-signed cert (required for mic/talkback on iOS; accept the browser warning once)")
	flag.Parse()

	// Pin the ICE host-candidate UDP range so it can be opened in the firewall.
	se := webrtc.SettingEngine{}
	if err := se.SetEphemeralUDPPortRange(uint16(*iceMin), uint16(*iceMax)); err != nil {
		log.Fatalf("ice udp port range %d-%d: %v", *iceMin, *iceMax, err)
	}

	srv, err := newServer(se)
	if err != nil {
		log.Fatalf("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// With -tls, mint a self-signed cert up front (before printing the URL) so a
	// failure is loud and the QR code advertises the right https scheme. The cert
	// must cover the LAN IP the QR points at, since browsers validate the SAN.
	scheme := "http"
	var tlsConfig *tls.Config
	if *useTLS {
		hosts := []string{"127.0.0.1", "::1", "localhost"}
		if ip := lanIP(); ip != "" {
			hosts = append(hosts, ip)
		}
		cert, err := selfSignedCert(hosts)
		if err != nil {
			log.Fatalf("self-signed cert: %v", err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		scheme = "https"
	}

	// Print the QR/URL first so it isn't interleaved with ffmpeg's log output.
	printLANURLs(scheme, *addr)

	// Supervise ffmpeg and pump its RTP into the shared track for the whole run.
	go startCapture(ctx, *source, *bitrate, *rtpPort, srv.track)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/info", srv.handleInfo)
	mux.HandleFunc("/offer", srv.handleOffer)
	mux.HandleFunc("/talk", srv.handleTalk)
	mux.HandleFunc("/events", srv.handleEvents)

	httpServer := &http.Server{Addr: *addr, Handler: mux, TLSConfig: tlsConfig}
	go func() {
		<-ctx.Done()
		httpServer.Close()
	}()

	serve := httpServer.ListenAndServe
	if tlsConfig != nil {
		// Cert/key are supplied via TLSConfig.Certificates, so pass empty paths.
		serve = func() error { return httpServer.ListenAndServeTLS("", "") }
	}
	if err := serve(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
}

// handleIndex serves the single embedded client page.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// handleInfo reports this server's identity (so the page can show which mic it's
// hearing) and the phone's own LAN IP (which the browser hides), from which the
// page derives a stable per-device name.
func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"host": s.host, "ip": ip})
}

// printLANURLs prints the LAN URL a phone on the same WiFi should open, as both
// text and a scannable QR code. Loopback, Docker, and Tailscale addresses are
// skipped so only the real LAN address is shown. scheme is "http" or (with -tls)
// "https".
func printLANURLs(scheme, addr string) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8000"
	}
	ip := lanIP()
	if ip == "" {
		log.Printf("could not find a LAN IPv4 address — is WiFi/ethernet up?")
		return
	}
	url := fmt.Sprintf("%s://%s:%s", scheme, ip, port)

	fmt.Printf("\n  Scan to open the baby monitor (phone on the same WiFi):\n\n")
	qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)
	fmt.Printf("\n  %s\n\n", url)
	if scheme == "https" {
		fmt.Printf("  Self-signed cert: accept the browser's security warning the first time.\n")
		fmt.Printf("  (Required so the page can use the phone's mic for talkback.)\n\n")
	}
}

// lanIP returns the first private IPv4 address on a physical interface, skipping
// loopback and virtual interfaces (Docker, Tailscale, VMs, containers). IsPrivate
// also excludes Tailscale's 100.64/10 CGNAT range.
func lanIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || isVirtualIface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil && ip4.IsPrivate() {
				return ip4.String()
			}
		}
	}
	return ""
}

// isVirtualIface reports whether an interface name belongs to a virtual network
// (Docker bridges, Tailscale, VPNs, VMs) rather than real LAN hardware.
func isVirtualIface(name string) bool {
	prefixes := []string{"docker", "br-", "veth", "tailscale", "tun", "tap", "virbr", "vmnet", "vboxnet", "zt", "wg"}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
