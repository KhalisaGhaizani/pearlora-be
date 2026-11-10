package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"pearlora/backend/internal/control"
)

// UIHub pushes status updates to every connected frontend over /ws.
type UIHub struct {
	upgrader websocket.Upgrader
	current  func() control.Status

	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

// NewUIHub takes a function that returns the current status, sent to each
// client when it connects.
func NewUIHub(current func() control.Status) *UIHub {
	return &UIHub{
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		current:  current,
		clients:  map[chan []byte]struct{}{},
	}
}

// Broadcast sends a status to every client; slow clients miss updates.
func (h *UIHub) Broadcast(st control.Status) {
	b, err := json.Marshal(map[string]any{"type": "status", "status": st})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- b:
		default:
		}
	}
}

func (h *UIHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}()

	// Discard anything the client sends; notice when it goes away.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	if b, err := json.Marshal(map[string]any{"type": "status", "status": h.current()}); err == nil {
		ws.WriteMessage(websocket.TextMessage, b)
	}
	for {
		select {
		case <-gone:
			return
		case b := <-ch:
			ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
			if err := ws.WriteMessage(websocket.TextMessage, b); err != nil {
				return
			}
		}
	}
}
