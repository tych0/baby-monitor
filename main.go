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
	"embed"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mdp/qrterminal/v3"
	"github.com/pion/webrtc/v4"
)

//go:embed static/index.html
var staticFS embed.FS

// server holds the shared WebRTC API and the single mic track that every phone
// subscribes to.
type server struct {
	api   *webrtc.API
	track *webrtc.TrackLocalStaticRTP
}

func main() {
	addr := flag.String("addr", ":8000", "HTTP listen address (binds all interfaces so phones can reach it)")
	source := flag.String("source", "default", "PulseAudio/PipeWire capture source")
	bitrate := flag.String("bitrate", "64k", "Opus bitrate")
	rtpPort := flag.Int("rtp-port", 5004, "local UDP port for the ffmpeg->server RTP feed")
	iceMin := flag.Int("ice-min", 50000, "min UDP port for WebRTC ICE host candidates")
	iceMax := flag.Int("ice-max", 50010, "max UDP port for WebRTC ICE host candidates")
	flag.Parse()

	// Pin the ICE host-candidate UDP range so it can be opened in the firewall.
	se := webrtc.SettingEngine{}
	if err := se.SetEphemeralUDPPortRange(uint16(*iceMin), uint16(*iceMax)); err != nil {
		log.Fatalf("ice udp port range %d-%d: %v", *iceMin, *iceMax, err)
	}

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		log.Fatalf("register codecs: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "baby-monitor",
	)
	if err != nil {
		log.Fatalf("create track: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Print the QR/URL first so it isn't interleaved with ffmpeg's log output.
	printLANURLs(*addr)

	// Supervise ffmpeg and pump its RTP into the shared track for the whole run.
	go startCapture(ctx, *source, *bitrate, *rtpPort, track)

	srv := &server{api: api, track: track}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/offer", srv.handleOffer)

	httpServer := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		httpServer.Close()
	}()

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

// printLANURLs prints the LAN URL a phone on the same WiFi should open, as both
// text and a scannable QR code. Loopback, Docker, and Tailscale addresses are
// skipped so only the real LAN address is shown.
func printLANURLs(addr string) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8000"
	}
	ip := lanIP()
	if ip == "" {
		log.Printf("could not find a LAN IPv4 address — is WiFi/ethernet up?")
		return
	}
	url := fmt.Sprintf("http://%s:%s", ip, port)

	fmt.Printf("\n  Scan to open the baby monitor (phone on the same WiFi):\n\n")
	qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)
	fmt.Printf("\n  %s\n\n", url)
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
