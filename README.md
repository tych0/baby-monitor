# baby-monitor

Stream this laptop's microphone to phones on your home WiFi, over WebRTC (low
latency, Opus audio). Open the page on a phone, tap **START**, and listen. The
page keeps playing with the screen off until you tap **STOP**, and it
auto-reconnects if the connection drops.

## How it works

```
mic ──► ffmpeg (Opus/RTP) ──UDP──► baby-monitor ──WebRTC──► phone browser
        (PulseAudio/PipeWire)       (pion/webrtc, one shared track → all phones)
```

A single `ffmpeg` process captures the default audio source and sends Opus RTP
to a local UDP port. The Go server forwards those packets into one shared WebRTC
track that every connected phone subscribes to, so the mic is captured once no
matter how many phones are listening. Signaling is a single `POST /offer`
(non-trickle ICE); the page is served from `/`.

## Requirements

- Go (build) and `ffmpeg` (runtime) — both already present on this machine.
- Audio: PulseAudio/PipeWire (the default source is used unless `-source` is set).

## Run

```sh
make run          # or: go build -o baby-monitor . && ./baby-monitor
```

On startup it prints a **scannable QR code** plus the LAN URL — point the phone's
camera at the terminal to open it:

```
  Scan to open the baby monitor (phone on the same WiFi):

  █▀▀▀▀▀█ ... QR ... █▀▀▀▀▀█

  http://192.168.1.42:8000
```

Only the real LAN address is shown; loopback, Docker, and Tailscale interfaces
are skipped. (Pick a specific source interface address via `-addr` if needed.)

### Flags

| Flag         | Default  | Purpose                                            |
|--------------|----------|----------------------------------------------------|
| `-addr`      | `:8000`  | HTTP listen address (all interfaces)               |
| `-source`    | `default`| PulseAudio/PipeWire capture source                 |
| `-bitrate`   | `64k`    | Opus bitrate                                       |
| `-rtp-port`  | `5004`   | local UDP port for the ffmpeg→server RTP feed      |
| `-ice-min`   | `50000`  | min UDP port for WebRTC ICE host candidates        |
| `-ice-max`   | `50010`  | max UDP port for WebRTC ICE host candidates        |

List capture sources with `pactl list short sources`; pass one to `-source`.

## Firewall (Fedora / firewalld)

If the page loads but you hear nothing on the phone, the media (UDP) is being
blocked. Open the signaling port and the ICE range:

```sh
sudo firewall-cmd --add-port=8000/tcp
sudo firewall-cmd --add-port=50000-50010/udp
# make permanent by re-running each with --permanent, or keep them session-only
```

## Notes & limitations

- **Open on the LAN** — anyone on your WiFi who knows the URL can listen. No auth.
- **Plain HTTP is fine** — the phone only *receives* audio, which doesn't require
  HTTPS. (If a browser ever refuses, the fallback is self-signed HTTPS.)
- **Autoplay** — mobile browsers won't play audio until you tap; the big button
  is that first tap. The page tries to start on load and falls back to tap-to-start.
- **Background playback** — Android Chrome keeps the audio alive with the screen
  off. iOS Safari may suspend it on lock; the page auto-reconnects when you wake it.
