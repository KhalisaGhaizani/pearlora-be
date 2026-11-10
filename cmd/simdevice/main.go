// Command simdevice pretends to be the ESP32 so the backend can be tried
// without hardware. It connects to /ws/device, sends telemetry every 500 ms
// and moves a simulated batten when told to run the motor.
//
// Type a key + Enter to change conditions:
//
//	t / T  water temperature +2 / -2 °C
//	s / S  soil raw +500 / -500
//	w      toggle a 25° tilt (waves)
//	e      toggle the emergency-stop button
//	z      re-zero depth (same as the backend's zero-depth command)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"math"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	speedMps   = 0.05 // batten speed at 40% duty
	bottomM    = 1.00 // bottom limit switch depth
	refDuty    = 102  // 40% of 255
	sendEvery  = 500 * time.Millisecond
	physicsDt  = 50 * time.Millisecond
	mbarPerM   = 97.8 // fresh water, ≈ 1 mbar per cm
	surfaceAir = 1013.25
)

type sim struct {
	mu        sync.Mutex
	depth     float64
	tempOff   float64
	soil      float64
	tilt      bool
	estop     bool
	zero      float64 // depth offset from re-zeroing
	dir       string
	duty      int
	expiresAt time.Time
}

func main() {
	url := flag.String("url", "ws://localhost:8080/ws/device", "backend device WebSocket URL")
	start := flag.Float64("depth", 0.70, "starting depth in metres")
	flag.Parse()

	s := &sim{depth: *start, soil: 2000, dir: "stop"}
	go s.physics()
	go s.keys()

	for {
		if err := s.run(*url); err != nil {
			log.Printf("connection: %v (retrying in 2 s)", err)
		}
		s.mu.Lock()
		s.dir = "stop" // like the firmware: stop on disconnect
		s.mu.Unlock()
		time.Sleep(2 * time.Second)
	}
}

func (s *sim) run(url string) error {
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return err
	}
	defer ws.Close()
	log.Printf("connected to %s", url)

	errc := make(chan error, 1)
	go func() {
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				errc <- err
				return
			}
			s.handle(data)
		}
	}()

	t := time.NewTicker(sendEvery)
	defer t.Stop()
	for {
		select {
		case err := <-errc:
			return err
		case <-t.C:
			if err := ws.WriteJSON(s.telemetry()); err != nil {
				return err
			}
		}
	}
}

func (s *sim) handle(data []byte) {
	var m struct {
		Type  string `json:"type"`
		Dir   string `json:"dir"`
		Duty  int    `json:"duty"`
		TTLMs int    `json:"ttl_ms"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch m.Type {
	case "motor":
		if m.Dir != s.dir {
			log.Printf("motor %s (duty %d)", m.Dir, m.Duty)
		}
		s.dir, s.duty = m.Dir, m.Duty
		s.expiresAt = time.Now().Add(time.Duration(m.TTLMs) * time.Millisecond)
	case "zero_depth":
		s.zero = s.depth
		log.Printf("depth re-zeroed")
	}
}

func (s *sim) physics() {
	t := time.NewTicker(physicsDt)
	last := time.Now()
	for now := range t.C {
		s.mu.Lock()
		if s.dir != "stop" && now.After(s.expiresAt) {
			log.Printf("motor command expired - stopping")
			s.dir = "stop"
		}
		v := speedMps * float64(s.duty) / refDuty * now.Sub(last).Seconds()
		switch {
		case s.estop:
			s.dir = "stop"
		case s.dir == "up":
			s.depth = math.Max(0, s.depth-v)
		case s.dir == "down":
			s.depth = math.Min(bottomM, s.depth+v)
		}
		s.mu.Unlock()
		last = now
	}
}

func (s *sim) telemetry() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	tilt := 2.0
	if s.tilt {
		tilt = 25
	}
	// Warmer near the surface: 32 °C at 0 m, 1 °C cooler every 10 cm.
	temp := 32 - 10*s.depth + s.tempOff
	return map[string]any{
		"type":          "telemetry",
		"temp_c":        round(temp, 2),
		"pressure_mbar": round(surfaceAir+s.depth*mbarPerM, 1),
		"depth_m":       round(s.depth-s.zero, 3),
		"soil_raw":      s.soil,
		"tilt_deg":      tilt,
		"limit_top":     s.depth <= 0,
		"limit_bottom":  s.depth >= bottomM,
		"motor":         s.dir,
		"ok":            map[string]bool{"temp": true, "depth": true, "imu": true, "soil": true},
		"estop":         s.estop,
	}
}

func (s *sim) keys() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		s.mu.Lock()
		for _, k := range sc.Text() {
			switch k {
			case 't':
				s.tempOff += 2
			case 'T':
				s.tempOff -= 2
			case 's':
				s.soil += 500
			case 'S':
				s.soil -= 500
			case 'w':
				s.tilt = !s.tilt
			case 'e':
				s.estop = !s.estop
			case 'z':
				s.zero = s.depth
			}
		}
		log.Printf("depth %.3f m  temp %.1f °C  soil %.0f  tilt %v  estop %v",
			s.depth, 32-10*s.depth+s.tempOff, s.soil, s.tilt, s.estop)
		s.mu.Unlock()
	}
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
