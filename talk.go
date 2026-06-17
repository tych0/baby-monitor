package main

import (
	"log"
	"net/http"

	"github.com/pion/webrtc/v4"
)

// client is one connected phone. talkback is the server->phone track onto which
// the active talker's relayed audio is written; it is silent unless some other
// phone is holding the talk button.
type client struct {
	id       string
	pc       *webrtc.PeerConnection
	talkback *webrtc.TrackLocalStaticRTP
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
		}
	}
	s.mu.Unlock()
}

// relayTalk forwards one RTP packet from a talker to every other client's
// talkback track. It is a no-op unless the sender currently holds the talk
// lock, which enforces the single-talker rule even if a phone sends without
// acquiring it. Writes to a track with no subscribers are harmless no-ops.
func (s *server) relayTalk(from *client, pkt []byte) {
	s.mu.Lock()
	if s.talker != from.id {
		s.mu.Unlock()
		return
	}
	targets := make([]*webrtc.TrackLocalStaticRTP, 0, len(s.clients))
	for id, c := range s.clients {
		if id != from.id {
			targets = append(targets, c.talkback)
		}
	}
	s.mu.Unlock()

	for _, t := range targets {
		_, _ = t.Write(pkt)
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
		log.Printf("talk: %s acquired mic", id)
		w.WriteHeader(http.StatusNoContent)
	case "release":
		if s.talker == id {
			s.talker = ""
			log.Printf("talk: %s released mic", id)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "bad op", http.StatusBadRequest)
	}
}
