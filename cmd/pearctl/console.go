package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"pearlora/backend/internal/control"
	"pearlora/backend/internal/settings"
)

// Telemetry older than this is shown as stale (device sends every 500 ms).
const staleAfter = 1500 * time.Millisecond

// holdGrace bridges the terminal's key-repeat delay: a held w/s key keeps
// the hold alive as long as presses keep coming within this window.
const holdGrace = 650 * time.Millisecond

var noColor = os.Getenv("NO_COLOR") != "" || !isTerminal(os.Stdout)

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func color(code, s string) string {
	if noColor {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func green(s string) string  { return color("32", s) }
func red(s string) string    { return color("31", s) }
func yellow(s string) string { return color("33", s) }
func bold(s string) string   { return color("1", s) }
func dim(s string) string    { return color("2", s) }

// view is everything the dashboard shows.
type view struct {
	st          *control.Status
	set         *settings.Settings
	server      string
	linked      bool          // connected to the backend
	extraAge    time.Duration // time since the status was received
	interactive bool
	last        string // result of the last key command
}

type console struct {
	cl          *client
	interactive bool
	plain       bool

	mu       sync.Mutex
	st       *control.Status
	gotAt    time.Time
	set      *settings.Settings
	linked   bool
	last     string
	holdDir  string
	holdSeen time.Time
}

func runConsole(cl *client, interactive, plain bool) error {
	c := &console{cl: cl, interactive: interactive, plain: plain}

	if interactive {
		restore, err := rawMode()
		if err != nil {
			return fmt.Errorf("can't put terminal in raw mode (run in a real terminal): %w", err)
		}
		fmt.Print("\033[?25l") // hide cursor
		defer func() { restore(); fmt.Print("\033[?25h\n") }()
		c.last = "ready"
	}

	quit := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; close(quit) }()

	go c.settingsLoop()
	go c.streamLoop()
	if interactive {
		go c.keyLoop(quit)
		go c.holdLoop()
	}
	if !plain {
		go c.redrawLoop()
	}
	<-quit
	return nil
}

// streamLoop follows /ws, reconnecting as needed.
func (c *console) streamLoop() {
	wsURL := "ws" + strings.TrimPrefix(c.cl.base, "http") + "/ws"
	for {
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			c.setLinked(false)
			if c.plain {
				fmt.Printf("%s backend unreachable at %s, retrying...\n", stamp(), c.cl.base)
			}
			time.Sleep(time.Second)
			continue
		}
		c.setLinked(true)
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				break
			}
			var msg struct {
				Type   string         `json:"type"`
				Status control.Status `json:"status"`
			}
			if json.Unmarshal(data, &msg) != nil || msg.Type != "status" {
				continue
			}
			c.mu.Lock()
			c.st, c.gotAt = &msg.Status, time.Now()
			c.mu.Unlock()
			if c.plain {
				printLine(&msg.Status)
			} else {
				c.draw()
			}
		}
		ws.Close()
		c.setLinked(false)
	}
}

func (c *console) setLinked(ok bool) {
	c.mu.Lock()
	changed := c.linked != ok
	c.linked = ok
	c.mu.Unlock()
	if changed && !c.plain {
		c.draw()
	}
}

// settingsLoop refreshes settings so the rule columns stay current.
func (c *console) settingsLoop() {
	for {
		if s, err := c.cl.settings(); err == nil {
			c.mu.Lock()
			c.set = &s
			c.mu.Unlock()
		}
		time.Sleep(3 * time.Second)
	}
}

// redrawLoop keeps the telemetry age ticking even without new messages.
func (c *console) redrawLoop() {
	for range time.Tick(500 * time.Millisecond) {
		c.draw()
	}
}

func (c *console) draw() {
	c.mu.Lock()
	v := view{
		st: c.st, set: c.set, server: c.cl.base, linked: c.linked,
		extraAge: time.Since(c.gotAt), interactive: c.interactive, last: c.last,
	}
	c.mu.Unlock()
	out := dashboard(v)
	// Raw mode needs explicit carriage returns.
	out = strings.ReplaceAll(out, "\n", "\033[K\r\n")
	fmt.Print("\033[H" + out + "\033[J")
}

// ---------------- Keyboard ----------------

func (c *console) keyLoop(quit chan struct{}) {
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			close(quit)
			return
		}
		var res string
		switch k := buf[0]; k {
		case 'q', 3: // q or Ctrl-C
			c.post("/api/manual", map[string]any{"action": "stop"})
			close(quit)
			return
		case 'm':
			res = c.post("/api/mode", map[string]string{"mode": "manual"})
		case 'a':
			res = c.post("/api/mode", map[string]string{"mode": "auto"})
		case 'u':
			res = c.post("/api/manual", map[string]any{"action": "up"})
		case 'd':
			res = c.post("/api/manual", map[string]any{"action": "down"})
		case 'w', 's':
			dir := map[byte]string{'w': "up", 's': "down"}[k]
			c.mu.Lock()
			fresh := c.holdDir != dir
			c.holdDir, c.holdSeen = dir, time.Now()
			c.mu.Unlock()
			if !fresh {
				continue // the hold loop keeps it going
			}
			res = c.post("/api/manual", map[string]any{"action": dir, "hold": true})
			res = "hold " + dir + " (keep the key held down): " + res
		case 'x', ' ':
			c.mu.Lock()
			c.holdDir = ""
			c.mu.Unlock()
			res = c.post("/api/manual", map[string]any{"action": "stop"})
		case 'r':
			res = c.post("/api/auto/target/reset", nil)
		case 'z':
			res = c.post("/api/device/zero-depth", nil)
		default:
			continue
		}
		c.mu.Lock()
		c.last = fmt.Sprintf("%s  %s", time.Now().Format("15:04:05"), res)
		c.mu.Unlock()
		c.draw()
	}
}

// holdLoop repeats the hold request every 200 ms while w/s is held,
// like a frontend button would.
func (c *console) holdLoop() {
	for range time.Tick(200 * time.Millisecond) {
		c.mu.Lock()
		dir := c.holdDir
		if dir != "" && time.Since(c.holdSeen) > holdGrace {
			c.holdDir, dir = "", ""
			c.last = time.Now().Format("15:04:05") + "  hold released"
		}
		c.mu.Unlock()
		if dir != "" {
			c.cl.post("/api/manual", map[string]any{"action": dir, "hold": true})
		}
	}
}

func (c *console) post(path string, body any) string {
	b, err := c.cl.post(path, body)
	if err != nil {
		return red("✗ " + err.Error())
	}
	var st control.Status
	if json.Unmarshal(b, &st) == nil && st.Mode != "" {
		return green("✓ ") + fmt.Sprintf("mode=%s motor=%s", st.Mode, st.Motor)
	}
	return green("✓ ok")
}

func rawMode() (func(), error) {
	saved, err := stty("-g")
	if err != nil {
		return nil, err
	}
	if _, err := stty("-icanon", "-echo", "min", "1"); err != nil {
		return nil, err
	}
	return func() { stty(strings.TrimSpace(saved)) }, nil
}

func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	return string(out), err
}

// ---------------- Rendering ----------------

func dashboard(v view) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	p("%s  %s", bold("PEARLORA"), dim(v.server))
	if !v.linked {
		p("%s", red("● backend unreachable - is `go run ./cmd/server` running?"))
	}
	st := v.st
	if st == nil {
		p("waiting for status...")
		return b.String()
	}

	dev := red("● DISCONNECTED")
	if st.DeviceConnected {
		dev = green("● connected")
	}
	age := "no telemetry yet"
	fresh := false
	if st.TelemetryAgeMs != nil {
		d := time.Duration(*st.TelemetryAgeMs)*time.Millisecond + v.extraAge
		fresh = d <= staleAfter
		age = fmt.Sprintf("last reading %d ms ago", d.Milliseconds())
		if !fresh {
			age = red(age + " (STALE)")
		}
	}
	p("Device %s   %s", dev, age)

	motor := map[control.Dir]string{
		control.DirUp: yellow("▲ UP"), control.DirDown: yellow("▼ DOWN"), control.DirStop: "■ stop",
	}[st.Motor]
	if st.Motor != control.DirStop {
		motor += fmt.Sprintf(" (%.0f%%)", st.DutyPct)
	}
	hold := ""
	if st.ManualHold {
		hold = dim(" hold")
	}
	p("Mode   %-8s  Motor %s%s   Target depth %.2f m", bold(strings.ToUpper(string(st.Mode))), motor, hold, st.TargetDepthM)
	p("Reason %s", st.Reason)
	p("")

	t := st.Telemetry
	if t == nil {
		p("No telemetry received from the device yet.")
	} else {
		s := v.set
		p("%s", bold(fmt.Sprintf("%-14s %-6s %-22s %s", "SENSOR", "HEALTH", "READING", "RULE")))
		p("%s", sensorRow("Depth MS5837", t.OK.Depth, fresh,
			fmt.Sprintf("%.3f m  %.1f mbar", t.DepthM, t.PressureMbar), depthRule(s),
			"MS5837 not responding at 0x76 - check I2C wiring / 3V3"))
		p("%s", sensorRow("Temp DS18B20", t.OK.Temp, fresh,
			fmt.Sprintf("%.2f °C", t.TempC), rangeRule(s, "temperature", t.TempC),
			"DS18B20 disconnected - check the 4.7k pull-up to 3V3 / DATA on GPIO 4"))
		p("%s", sensorRow("Soil (salin.)", t.OK.Soil, fresh,
			fmt.Sprintf("%.0f / 4095", t.SoilRaw), rangeRule(s, "soil", t.SoilRaw),
			"reads 0 - check AOUT on GPIO 34"))
		p("%s", sensorRow("Tilt MPU6050", t.OK.IMU, fresh,
			fmt.Sprintf("%.1f°", t.TiltDeg), tiltRule(s, t.TiltDeg),
			"MPU6050 not responding at 0x68 - check I2C wiring"))
		if t.OK.Soil && t.SoilRaw >= 4090 {
			p("  %s", yellow("soil reads full scale (4095): sensor dry, or AOUT floating"))
		}
		p("")
		p("Limit top %s   Limit bottom %s   E-stop %s   Device motor %s",
			pressed(t.LimitTop), pressed(t.LimitBottom), estop(t.EStop), t.Motor)
		if s != nil {
			p("Priority  %s", strings.Join(s.Priority, " > "))
		}
	}

	if v.interactive {
		p("")
		p("%s", dim("────────────────────────────────────────────────────────────────────"))
		p("[m] manual  [a] auto  │ [u]/[d] latch up/down  [w]/[s] hold up/down  [x]/space stop")
		p("[r] reset target  [z] zero depth  [q] quit (stops the motor)")
		p("%s", v.last)
	}
	return b.String()
}

func sensorRow(name string, ok, fresh bool, reading, rule, hint string) string {
	health := green("OK")
	switch {
	case !ok:
		health = red("FAIL")
		reading = red(hint)
		rule = ""
	case !fresh:
		health = yellow("STALE")
	}
	// Pad by visible width; color codes would throw %-6s off.
	return fmt.Sprintf("%-14s %s %-22s %s", name, health+strings.Repeat(" ", 6-visibleLen(health)), reading, rule)
}

func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case r == '\033':
			esc = true
		case esc && r == 'm':
			esc = false
		case !esc:
			n++
		}
	}
	return n
}

func enabled(s *settings.Settings, name string) bool {
	switch name {
	case "depth":
		return s.SensorsEnabled.Depth
	case "temperature":
		return s.SensorsEnabled.Temperature
	case "soil":
		return s.SensorsEnabled.Soil
	case "tilt":
		return s.SensorsEnabled.Tilt
	}
	return false
}

func off(s *settings.Settings, name string) string {
	if !enabled(s, name) {
		return dim(" [disabled]")
	}
	return ""
}

func rangeRule(s *settings.Settings, name string, v float64) string {
	if s == nil {
		return ""
	}
	r := s.Temperature
	if name == "soil" {
		r = s.Soil
	}
	state := green("in range")
	switch {
	case v > r.Max:
		state = yellow(fmt.Sprintf("above max → %s", r.AboveAction))
	case v < r.Min:
		state = yellow(fmt.Sprintf("below min → %s", r.BelowAction))
	}
	return fmt.Sprintf("%g–%g  %s%s", r.Min, r.Max, state, off(s, name))
}

func tiltRule(s *settings.Settings, v float64) string {
	if s == nil {
		return ""
	}
	state := green("ok")
	if v > s.Tilt.MaxDeg {
		state = yellow(fmt.Sprintf("too much → %s", s.Tilt.Action))
	}
	return fmt.Sprintf("max %g°  %s%s", s.Tilt.MaxDeg, state, off(s, "tilt"))
}

func depthRule(s *settings.Settings) string {
	if s == nil {
		return ""
	}
	d := s.Depth
	return fmt.Sprintf("band %g–%g m, ±%g%s", d.MinM, d.MaxM, d.ToleranceM, off(s, "depth"))
}

func pressed(p bool) string {
	if p {
		return yellow("PRESSED")
	}
	return "open"
}

func estop(on bool) string {
	if on {
		return red("ENGAGED")
	}
	return "off"
}

// ---------------- Plain (log) output ----------------

func stamp() string { return time.Now().Format("15:04:05.000") }

func flag01(ok bool, name string) string {
	if ok {
		return name
	}
	return strings.ToUpper(name) + "!"
}

// printLine prints one line per new reading or state change.
func printLine(st *control.Status) {
	t := st.Telemetry
	if t == nil {
		fmt.Printf("%s mode=%s device=%v motor=%s (no telemetry) | %s\n", stamp(), st.Mode, st.DeviceConnected, st.Motor, st.Reason)
		return
	}
	limits := ""
	if t.LimitTop {
		limits += " TOP"
	}
	if t.LimitBottom {
		limits += " BOTTOM"
	}
	if t.EStop {
		limits += " ESTOP"
	}
	fmt.Printf("%s %-6s %-4s depth=%6.3fm p=%7.1f temp=%6.2fC soil=%4.0f tilt=%5.1f° ok=[%s %s %s %s]%s target=%.2f | %s\n",
		stamp(), st.Mode, st.Motor, t.DepthM, t.PressureMbar, t.TempC, t.SoilRaw, t.TiltDeg,
		flag01(t.OK.Depth, "depth"), flag01(t.OK.Temp, "temp"), flag01(t.OK.Soil, "soil"), flag01(t.OK.IMU, "imu"),
		limits, st.TargetDepthM, st.Reason)
}
