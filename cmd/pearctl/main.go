// Command pearctl drives and monitors the Pearlora backend from a terminal,
// so it can be tested without the frontend.
//
//	pearctl                      interactive console (live dashboard + keyboard control)
//	pearctl monitor [-plain]     live sensor/telemetry monitor (read-only)
//	pearctl status               print the dashboard once
//	pearctl mode manual|auto
//	pearctl up|down [-hold 3s]   latch, or hold for a duration (repeats like a held button)
//	pearctl stop
//	pearctl settings             print settings JSON
//	pearctl set key=value ...    e.g. temperature.max=29 tilt.action=down priority=soil,tilt,temperature
//	pearctl reset-settings | reset-target | zero-depth
//
// The server defaults to http://localhost:8080; override with -server or
// the PEARLORA_SERVER environment variable.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"pearlora/backend/internal/control"
	"pearlora/backend/internal/settings"
)

func usage() {
	fmt.Fprint(os.Stderr, `Usage: pearctl [-server URL] [command] [args]

Commands:
  (none) | console        interactive console: live dashboard + keyboard control
  monitor [-plain]        live telemetry / sensor-health monitor (read-only);
                          -plain prints one line per reading (good for logging)
  status                  print the dashboard once
  mode manual|auto        switch mode (the motor always stops first)
  up | down [-hold DUR]   manual move; latched until stop, or held for DUR (e.g. 3s)
  stop                    stop the motor
  settings                print current settings (JSON)
  set key=value ...       change settings, e.g.
                            pearctl set temperature.max=29 tilt.action=down
                            pearctl set sensors_enabled.soil=false motor_duty_pct=60
                            pearctl set priority=soil,tilt,temperature
  reset-settings          restore the dummy defaults
  reset-target            move the auto target back to the base target
  zero-depth              use the current pressure as the surface (depth 0)
`)
}

func main() {
	def := os.Getenv("PEARLORA_SERVER")
	if def == "" {
		def = "http://localhost:8080"
	}
	server := flag.String("server", def, "backend base URL")
	flag.Usage = usage
	flag.Parse()

	cl := &client{base: strings.TrimRight(*server, "/")}
	args := flag.Args()
	cmd := "console"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}

	var err error
	switch cmd {
	case "console":
		err = runConsole(cl, true, false)
	case "monitor":
		fs := flag.NewFlagSet("monitor", flag.ExitOnError)
		plain := fs.Bool("plain", false, "one line per reading instead of a dashboard")
		fs.Parse(args)
		err = runConsole(cl, false, *plain)
	case "status":
		err = cmdStatus(cl)
	case "mode":
		if len(args) != 1 {
			err = fmt.Errorf("usage: pearctl mode manual|auto")
			break
		}
		err = cl.printResult(cl.post("/api/mode", map[string]string{"mode": args[0]}))
	case "up", "down":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		hold := fs.Duration("hold", 0, "hold the button for this long (0 = latch until stop)")
		fs.Parse(args)
		err = cmdMove(cl, cmd, *hold)
	case "stop":
		err = cl.printResult(cl.post("/api/manual", map[string]any{"action": "stop"}))
	case "settings":
		var b []byte
		if b, err = cl.get("/api/settings"); err == nil {
			err = printJSON(b)
		}
	case "set":
		err = cmdSet(cl, args)
	case "reset-settings":
		var b []byte
		if b, err = cl.post("/api/settings/reset", nil); err == nil {
			err = printJSON(b)
		}
	case "reset-target":
		err = cl.printResult(cl.post("/api/auto/target/reset", nil))
	case "zero-depth":
		_, err = cl.post("/api/device/zero-depth", nil)
		if err == nil {
			fmt.Println("zero_depth sent to device")
		}
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---------------- HTTP client ----------------

type client struct{ base string }

var httpc = &http.Client{Timeout: 3 * time.Second}

func (c *client) get(path string) ([]byte, error) { return c.do("GET", path, nil) }

func (c *client) post(path string, body any) ([]byte, error) { return c.do("POST", path, body) }

func (c *client) do(method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backend unreachable at %s (is the server running?)", c.base)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return b, fmt.Errorf("%d: %s", resp.StatusCode, e.Error)
		}
		return b, fmt.Errorf("%s", resp.Status)
	}
	return b, nil
}

func (c *client) status() (control.Status, error) {
	var st control.Status
	b, err := c.get("/api/status")
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	return st, err
}

func (c *client) settings() (settings.Settings, error) {
	var s settings.Settings
	b, err := c.get("/api/settings")
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	return s, err
}

// printResult prints a one-line summary of a status response.
func (c *client) printResult(b []byte, err error) error {
	if err != nil {
		return err
	}
	var st control.Status
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	fmt.Printf("OK  mode=%s motor=%s target=%.2fm  reason: %s\n", st.Mode, st.Motor, st.TargetDepthM, st.Reason)
	return nil
}

func printJSON(b []byte) error {
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return err
	}
	fmt.Println(out.String())
	return nil
}

// ---------------- One-shot commands ----------------

func cmdStatus(cl *client) error {
	st, err := cl.status()
	if err != nil {
		return err
	}
	s, err := cl.settings()
	if err != nil {
		return err
	}
	fmt.Print(dashboard(view{st: &st, set: &s, server: cl.base, linked: true}))
	return nil
}

func cmdMove(cl *client, dir string, hold time.Duration) error {
	if hold <= 0 {
		return cl.printResult(cl.post("/api/manual", map[string]any{"action": dir}))
	}
	fmt.Printf("holding %s for %s ...\n", dir, hold)
	deadline := time.Now().Add(hold)
	for time.Now().Before(deadline) {
		b, err := cl.post("/api/manual", map[string]any{"action": dir, "hold": true})
		if err != nil {
			return err
		}
		var st control.Status
		json.Unmarshal(b, &st)
		if t := st.Telemetry; t != nil {
			fmt.Printf("  motor=%-4s depth=%.3fm\n", st.Motor, t.DepthM)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("released (backend stops the motor after the hold timeout)")
	time.Sleep(800 * time.Millisecond)
	return cl.printResult(cl.get("/api/status"))
}

// cmdSet turns key=value pairs (dotted keys) into a partial settings update.
func cmdSet(cl *client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: pearctl set key=value ... (see pearctl help)")
	}
	body := map[string]any{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("bad argument %q, want key=value", a)
		}
		parts := strings.Split(k, ".")
		m := body
		for _, p := range parts[:len(parts)-1] {
			sub, ok := m[p].(map[string]any)
			if !ok {
				sub = map[string]any{}
				m[p] = sub
			}
			m = sub
		}
		m[parts[len(parts)-1]] = parseValue(k, v)
	}
	b, err := cl.do("PUT", "/api/settings", body)
	if err != nil {
		return err
	}
	fmt.Println("settings updated:")
	return printJSON(b)
}

func parseValue(key, v string) any {
	if key == "priority" {
		return strings.Split(v, ",")
	}
	if v == "true" || v == "false" {
		return v == "true"
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return v
}
