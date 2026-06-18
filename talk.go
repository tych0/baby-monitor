package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// opusFrameTS is one 20ms Opus frame at 48kHz, used as the nominal timestamp
// step inserted when the talker switches so the rewritten stream keeps moving
// forward across the boundary.
const opusFrameTS = 960

// talkbackWriter feeds one phone's talkback track. The server relays whichever
// phone currently holds the mic onto it, but those phones' RTP streams have
// unrelated sequence numbers and timestamps. pion only rewrites the SSRC/payload
// type, so handing it raw foreign packets makes a talker switch look like a huge
// discontinuity on a single SSRC, which glitches/stalls the receiver's jitter
// buffer. talkbackWriter rewrites sequence numbers (always +1) and timestamps
// (by the source's own delta, or a nominal frame across a switch) so the phone
// sees one continuous stream no matter who is talking.
type talkbackWriter struct {
	track *webrtc.TrackLocalStaticRTP

	mu      sync.Mutex
	started bool
	src     string // id of the talker whose stream we're currently rewriting
	inTS    uint32 // last input timestamp from src
	outTS   uint32 // last timestamp we emitted
	outSeq  uint16 // last sequence number we emitted
}

// rewrite advances the outgoing stream's sequence/timestamp for one packet from
// talker src and returns the packet to emit. Sequence numbers always step by one
// (so the receiver never sees a gap), and timestamps advance by the source's own
// delta, except across a talker switch where a nominal frame is used instead of
// jumping to the new source's clock.
func (w *talkbackWriter) rewrite(src string, pkt *rtp.Packet) rtp.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case !w.started:
		// Anchor the outgoing stream to this first packet.
		w.started = true
		w.outSeq = pkt.SequenceNumber
		w.outTS = pkt.Timestamp
	case src != w.src:
		// Talker switched: step forward one frame rather than jumping to the new
		// source's clock, so the receiver hears a continuous stream.
		w.outSeq++
		w.outTS += opusFrameTS
	default:
		// Same talker: advance by however far its own timestamp moved (preserves
		// gaps from dropped packets, which is the right signal for Opus PLC).
		w.outSeq++
		w.outTS += pkt.Timestamp - w.inTS
	}
	w.src = src
	w.inTS = pkt.Timestamp
	out := *pkt
	out.SequenceNumber = w.outSeq
	out.Timestamp = w.outTS
	return out
}

// write rewrites one packet from talker src onto the track as a continuation of
// the outgoing stream.
func (w *talkbackWriter) write(src string, pkt *rtp.Packet) error {
	out := w.rewrite(src, pkt)
	return w.track.WriteRTP(&out)
}

// client is one connected phone. talkback is the server->phone track onto which
// the active talker's relayed audio is written; it is silent unless some other
// phone is holding the talk button. ip is the phone's LAN address, used so other
// phones can show the talker's generated name.
type client struct {
	id       string
	ip       string
	pc       *webrtc.PeerConnection
	talkback *talkbackWriter
}

// talkerInfo is what the /events stream reports: the id of the phone holding the
// mic (so a phone can tell whether it's itself) and that phone's LAN IP (from
// which every page derives the same human-friendly name). Both are empty when no
// one holds the mic.
type talkerInfo struct {
	ID string `json:"id"`
	IP string `json:"ip"`
}

// currentTalkerLocked returns the active talker's id and IP. Must hold s.mu.
func (s *server) currentTalkerLocked() talkerInfo {
	if s.talker == "" {
		return talkerInfo{}
	}
	info := talkerInfo{ID: s.talker}
	if c, ok := s.clients[s.talker]; ok {
		info.IP = c.ip
	}
	return info
}

// register adds (or replaces) the client for id, closing any prior connection
// for the same id so a reconnecting phone never leaks a peer connection.
func (s *server) register(c *client) {
	s.mu.Lock()
	if old, ok := s.clients[c.id]; ok && old.pc != c.pc {
		old.pc.Close()
	}
	s.clients[c.id] = c
	s.mu.Unlock()
}

// unregister drops the client for this exact pc (a stale pc from an earlier
// connection must not evict the current one) and releases the talk lock if it
// held it, so a talker that disconnects mid-sentence frees the mic.
func (s *server) unregister(c *client) {
	s.mu.Lock()
	if cur, ok := s.clients[c.id]; ok && cur.pc == c.pc {
		delete(s.clients, c.id)
		if s.talker == c.id {
			s.talker = ""
			s.broadcastTalkerLocked()
		}
	}
	s.mu.Unlock()
}

// relayTalk forwards one RTP packet from a talker to every other client's
// talkback track. It is a no-op unless the sender currently holds the talk
// lock, which enforces the single-talker rule even if a phone sends without
// acquiring it. Writes to a track with no subscribers are harmless no-ops.
func (s *server) relayTalk(from *client, pkt *rtp.Packet) {
	s.mu.Lock()
	if s.talker != from.id {
		s.mu.Unlock()
		return
	}
	targets := make([]*talkbackWriter, 0, len(s.clients))
	for id, c := range s.clients {
		if id != from.id {
			targets = append(targets, c.talkback)
		}
	}
	s.mu.Unlock()

	for _, t := range targets {
		_ = t.write(from.id, pkt)
	}
}

// handleTalk is the push-to-talk lock. A phone POSTs ?id=<id>&op=acquire when it
// presses and holds, and ?op=release when it lets go. acquire returns 409 if a
// different phone already holds the mic, so only one phone talks at a time.
func (s *server) handleTalk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.URL.Query().Get("op") {
	case "acquire":
		if _, ok := s.clients[id]; !ok {
			http.Error(w, "not connected", http.StatusNotFound)
			return
		}
		if s.talker != "" && s.talker != id {
			http.Error(w, "busy", http.StatusConflict)
			return
		}
		s.talker = id
		s.broadcastTalkerLocked()
		log.Printf("talk: %s acquired mic", id)
		w.WriteHeader(http.StatusNoContent)
	case "release":
		if s.talker == id {
			s.talker = ""
			s.broadcastTalkerLocked()
			log.Printf("talk: %s released mic", id)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "bad op", http.StatusBadRequest)
	}
}

// subscribeTalker registers a channel that receives the current talker (and
// every later change). It returns the channel, primed with the current value,
// plus an unsubscribe func. Callers must drain promptly; sends are coalesced so
// a slow reader only ever misses intermediate states, never the latest one.
func (s *server) subscribeTalker() (<-chan talkerInfo, func()) {
	ch := make(chan talkerInfo, 1)
	s.mu.Lock()
	s.talkerSubs[ch] = struct{}{}
	ch <- s.currentTalkerLocked()
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.talkerSubs, ch)
		s.mu.Unlock()
	}
}

// broadcastTalkerLocked pushes the current talker to every subscriber. It must be
// called with s.mu held. Sends never block: a subscriber whose buffer is full has
// its pending (now-stale) value replaced with the latest one.
func (s *server) broadcastTalkerLocked() {
	info := s.currentTalkerLocked()
	for ch := range s.talkerSubs {
		select {
		case ch <- info:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- info:
			default:
			}
		}
	}
}

// handleEvents is a Server-Sent Events stream that tells each phone who currently
// holds the mic, so receivers can show who is talking and reflect the talk button
// as busy. Each message is a JSON talkerInfo, with empty fields when no one holds
// the mic.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	updates, cancel := s.subscribeTalker()
	defer cancel()

	for {
		select {
		case <-r.Context().Done():
			return
		case talker := <-updates:
			payload, err := json.Marshal(talker)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}
