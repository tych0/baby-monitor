package main

import (
	"bufio"
	"context"
	"log"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
)

// startCapture runs the UDP reader and an ffmpeg supervisor for the lifetime of
// ctx. ffmpeg captures the mic, encodes Opus, and sends RTP to 127.0.0.1:rtpPort;
// the reader forwards those packets into the shared track. ffmpeg is restarted
// with backoff if it ever exits.
func startCapture(ctx context.Context, source, bitrate string, rtpPort int, track *webrtc.TrackLocalStaticRTP) {
	go readRTP(ctx, rtpPort, track)

	backoff := time.Second
	for ctx.Err() == nil {
		err := runFFmpeg(ctx, source, bitrate, rtpPort)
		if ctx.Err() != nil {
			return
		}
		log.Printf("ffmpeg exited (%v); restarting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

// runFFmpeg starts ffmpeg and blocks until it exits.
func runFFmpeg(ctx context.Context, source, bitrate string, rtpPort int) error {
	url := "rtp://127.0.0.1:" + strconv.Itoa(rtpPort) + "?pkt_size=1200"
	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-f", "pulse", "-i", source,
		// Resample against a steady output clock to absorb capture-clock drift,
		// which otherwise causes periodic dropouts/choppiness in a live stream.
		"-af", "aresample=async=1",
		"-ac", "2", "-ar", "48000",
		"-c:a", "libopus", "-b:a", bitrate,
		"-application", "audio", "-frame_duration", "20",
		// In-band FEC + an expected-loss hint so Opus carries enough redundancy
		// to reconstruct packets dropped over WiFi (browsers decode FEC by default).
		// Scoped with :a so they hit the libopus encoder, not the RTP muxer's
		// own (incompatible) -fec option.
		"-fec:a", "1", "-packet_loss:a", "10",
		"-payload_type", "111",
		"-f", "rtp", url,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	// Have the kernel SIGKILL ffmpeg if this server dies abruptly (e.g. kill -9),
	// so a capture process can never be orphaned and keep flooding the RTP port.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	// Pdeathsig fires when the thread that started the child exits, so pin this
	// goroutine to its OS thread for the child's whole lifetime (it blocks in
	// Wait below anyway).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("ffmpeg capturing pulse source %q -> rtp 127.0.0.1:%d (opus %s)", source, rtpPort, bitrate)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.Printf("ffmpeg: %s", sc.Text())
		}
	}()
	return cmd.Wait()
}

// readRTP listens on the local UDP port and writes each datagram into the shared
// track. pion rewrites SSRC/payload-type per subscriber, and a write with zero
// subscribers is a harmless no-op, so this loop runs continuously regardless of
// how many phones are connected.
func readRTP(ctx context.Context, rtpPort int, track *webrtc.TrackLocalStaticRTP) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: rtpPort}

	var conn *net.UDPConn
	for {
		var err error
		conn, err = net.ListenUDP("udp", addr)
		if err == nil {
			break
		}
		log.Printf("listen udp %d: %v (retrying)", rtpPort, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 1600)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("udp read: %v", err)
			return
		}
		// Per-subscriber write errors (a phone dropping) are transient; pion
		// removes the binding on close, so just keep streaming.
		_, _ = track.Write(buf[:n])
	}
}
