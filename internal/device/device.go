// Package device manages the WebSocket connection to the ESP32. Only one
// device is supported; a new connection replaces the previous one.
package device

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"pearlora/backend/internal/control"
)

const (
	readTimeout  = 5 * time.Second // telemetry arrives every 500 ms
	writeTimeout = 2 * time.Second
	pingEvery    = 2 * time.Second
)

// Handler receives device events. Calls may come from several goroutines.
type Handler interface {
	DeviceConnected()
	DeviceDisconnected()
	DeviceTelemetry(control.Telemetry)
}

// MotorMsg tells the device to run the motor. The device stops on its own if
// it doesn't receive a fresh command within TTLMs.
type MotorMsg struct {
	Type  string      `json:"type"` // "motor"
	Dir   control.Dir `json:"dir"`
	Duty  int         `json:"duty"`
	TTLMs int         `json:"ttl_ms"`
}

type conn struct {
	ws   *websocket.Conn
	send chan []byte
}

type Hub struct {
	h        Handler
	upgrader websocket.Upgrader

	mu  sync.Mutex
	cur *conn
}

func NewHub(h Handler) *Hub {
	return &Hub{
		h:        h,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
}

// Connected reports whether a device is currently connected.
func (hub *Hub) Connected() bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.cur != nil
}

// Send queues a JSON message for the device. It drops the message if no
// device is connected or its queue is full (commands are re-sent anyway).
func (hub *Hub) Send(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		log.Printf("device: marshal: %v", err)
		return
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.cur == nil {
		return
	}
	select {
	case hub.cur.send <- b:
	default:
	}
}

func (hub *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := hub.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{ws: ws, send: make(chan []byte, 16)}

	hub.mu.Lock()
	old := hub.cur
	hub.cur = c
	hub.mu.Unlock()
	if old != nil {
		log.Printf("device: new connection from %s replaces the old one", r.RemoteAddr)
		old.ws.Close()
	} else {
		log.Printf("device: connected from %s", r.RemoteAddr)
	}
	hub.h.DeviceConnected()

	done := make(chan struct{})
	go hub.writeLoop(c, done)
	hub.readLoop(c)
	close(done)
	ws.Close()

	hub.mu.Lock()
	wasCurrent := hub.cur == c
	if wasCurrent {
		hub.cur = nil
	}
	hub.mu.Unlock()
	if wasCurrent {
		log.Printf("device: disconnected")
		hub.h.DeviceDisconnected()
	}
}

func (hub *Hub) readLoop(c *conn) {
	c.ws.SetReadDeadline(time.Now().Add(readTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(readTimeout))
	})
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		c.ws.SetReadDeadline(time.Now().Add(readTimeout))

		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			log.Printf("device: bad message: %v", err)
			continue
		}
		switch head.Type {
		case "telemetry":
			var t control.Telemetry
			if err := json.Unmarshal(data, &t); err != nil {
				log.Printf("device: bad telemetry: %v", err)
				continue
			}
			hub.h.DeviceTelemetry(t)
		case "log":
			log.Printf("device log: %s", data)
		default:
			log.Printf("device: unknown message type %q", head.Type)
		}
	}
}

func (hub *Hub) writeLoop(c *conn, done <-chan struct{}) {
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-done:
			return
		case b := <-c.send:
			c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
				c.ws.Close()
				return
			}
		case <-ping.C:
			c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.ws.Close()
				return
			}
		}
	}
}
