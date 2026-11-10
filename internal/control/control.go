// Package control decides what the winch motor should do. It has no I/O:
// callers feed it telemetry, user commands and the current time, and ask it
// for the motor command on every tick. It is not safe for concurrent use;
// the server drives it from a single goroutine.
package control

import (
	"errors"
	"fmt"
	"math"
	"time"

	"pearlora/backend/internal/settings"
)

type Mode string

const (
	ModeManual Mode = "manual"
	ModeAuto   Mode = "auto"
)

// Dir is the motor direction. Up = shallower, down = deeper.
type Dir string

const (
	DirStop Dir = "stop"
	DirUp   Dir = "up"
	DirDown Dir = "down"
)

// StaleAfter is how old telemetry may get before the motor is stopped.
const StaleAfter = 2 * time.Second

// SensorOK reports which sensors returned a valid reading.
type SensorOK struct {
	Temp  bool `json:"temp"`
	Depth bool `json:"depth"`
	IMU   bool `json:"imu"`
	Soil  bool `json:"soil"`
}

// Telemetry is one reading from the device.
type Telemetry struct {
	TempC        float64  `json:"temp_c"`
	PressureMbar float64  `json:"pressure_mbar"`
	DepthM       float64  `json:"depth_m"`
	SoilRaw      float64  `json:"soil_raw"`
	TiltDeg      float64  `json:"tilt_deg"`
	LimitTop     bool     `json:"limit_top"`
	LimitBottom  bool     `json:"limit_bottom"`
	Motor        Dir      `json:"motor"`
	OK           SensorOK `json:"ok"`
	EStop        bool     `json:"estop"`
}

// Command is what should be sent to the motor driver.
type Command struct {
	Dir  Dir `json:"dir"`
	Duty int `json:"duty"` // 0..255 PWM duty
}

// Status is the controller state exposed to the frontend.
type Status struct {
	Mode            Mode       `json:"mode"`
	DeviceConnected bool       `json:"device_connected"`
	TelemetryAgeMs  *int64     `json:"telemetry_age_ms"` // nil when no telemetry yet
	Telemetry       *Telemetry `json:"telemetry"`
	Motor           Dir        `json:"motor"`
	DutyPct         float64    `json:"duty_pct"`
	TargetDepthM    float64    `json:"target_depth_m"`
	Reason          string     `json:"reason"`
	ManualHold      bool       `json:"manual_hold"`
}

var (
	ErrNotManual   = errors.New("manual commands are only accepted in manual mode")
	ErrNoDevice    = errors.New("device not connected")
	ErrLimit       = errors.New("limit switch pressed in that direction")
	ErrBadAction   = errors.New("action must be up, down or stop")
	ErrBadMode     = errors.New("mode must be manual or auto")
	ErrNoTelemetry = errors.New("no recent telemetry from device")
	ErrEStop       = errors.New("emergency stop is active on the device; release it first")
)

type Controller struct {
	s Settings

	mode      Mode
	connected bool
	tel       Telemetry
	telAt     time.Time
	haveTel   bool

	motor  Dir
	reason string

	// auto
	target   float64
	lastStep time.Time

	// manual
	manualDir      Dir
	manualHold     bool
	manualDeadline time.Time
}

// Settings is an alias so callers don't need to import both packages.
type Settings = settings.Settings

func New(s Settings) *Controller {
	return &Controller{
		s:         s,
		mode:      ModeManual,
		motor:     DirStop,
		manualDir: DirStop,
		target:    s.Depth.BaseTargetM,
		reason:    "idle",
	}
}

func (c *Controller) Settings() Settings { return c.s }

// SetSettings applies new (already validated) settings. If the base target
// changed, the current target jumps to it; otherwise it is clamped into the
// new depth band.
func (c *Controller) SetSettings(s Settings) {
	if s.Depth.BaseTargetM != c.s.Depth.BaseTargetM {
		c.target = s.Depth.BaseTargetM
	}
	c.target = clamp(c.target, s.Depth.MinM, s.Depth.MaxM)
	c.s = s
}

func (c *Controller) Mode() Mode { return c.mode }

// SetMode switches mode. The motor always stops on a switch; entering auto
// resets the target to the base target.
func (c *Controller) SetMode(m Mode) error {
	if m != ModeManual && m != ModeAuto {
		return ErrBadMode
	}
	if m == ModeAuto && c.tel.EStop {
		return ErrEStop
	}
	c.mode = m
	c.motor = DirStop
	c.manualDir = DirStop
	c.reason = "mode set to " + string(m)
	if m == ModeAuto {
		c.target = c.s.Depth.BaseTargetM
		c.lastStep = time.Time{}
	}
	return nil
}

// ResetTarget puts the auto target back to the base target.
func (c *Controller) ResetTarget() {
	c.target = c.s.Depth.BaseTargetM
	c.lastStep = time.Time{}
}

// SetConnected records whether the device is connected. Disconnecting stops
// any manual move so it doesn't resume on reconnect.
func (c *Controller) SetConnected(ok bool) {
	c.connected = ok
	if !ok {
		c.manualDir = DirStop
		c.motor = DirStop
		c.reason = "device disconnected"
	}
}

// OnTelemetry stores a reading. A device emergency stop forces manual mode.
func (c *Controller) OnTelemetry(t Telemetry, now time.Time) {
	c.tel, c.telAt, c.haveTel = t, now, true
	if t.EStop {
		c.mode = ModeManual
		c.manualDir = DirStop
		c.motor = DirStop
		c.reason = "emergency stop pressed on device"
	}
}

// Manual handles an up/down/stop request. With hold=true the move lasts
// until hold_timeout_ms after the last request (the frontend repeats it while
// the button is held); otherwise it latches until stop or latch_max_run_s.
func (c *Controller) Manual(action Dir, hold bool, now time.Time) error {
	if c.mode != ModeManual {
		return ErrNotManual
	}
	switch action {
	case DirStop:
		c.manualDir = DirStop
		c.reason = "manual stop"
		return nil
	case DirUp, DirDown:
	default:
		return ErrBadAction
	}
	if !c.connected {
		return ErrNoDevice
	}
	if !c.fresh(now) {
		return ErrNoTelemetry
	}
	if c.tel.EStop {
		return ErrEStop
	}
	if c.limitHit(action) {
		return ErrLimit
	}
	if hold {
		// A repeat of the same held move only extends the deadline.
		c.manualDeadline = now.Add(time.Duration(c.s.Manual.HoldTimeoutMs) * time.Millisecond)
	} else if c.manualDir != action || c.manualHold {
		c.manualDeadline = now.Add(time.Duration(c.s.Manual.LatchMaxRunS * float64(time.Second)))
	}
	c.manualDir, c.manualHold = action, hold
	c.reason = fmt.Sprintf("manual %s (%s)", action, holdName(hold))
	return nil
}

// Tick computes the motor command for this instant.
func (c *Controller) Tick(now time.Time) Command {
	dir, reason := c.decide(now)
	if dir != DirStop && c.limitHit(dir) {
		reason = fmt.Sprintf("%s limit switch reached", map[Dir]string{DirUp: "top", DirDown: "bottom"}[dir])
		dir = DirStop
		c.manualDir = DirStop
	}
	c.motor, c.reason = dir, reason
	if dir == DirStop {
		return Command{Dir: DirStop}
	}
	return Command{Dir: dir, Duty: c.duty()}
}

func (c *Controller) decide(now time.Time) (Dir, string) {
	if !c.connected {
		return DirStop, "device disconnected"
	}
	if !c.fresh(now) {
		return DirStop, "waiting for telemetry"
	}
	if c.mode == ModeManual {
		return c.decideManual(now)
	}
	return c.decideAuto(now)
}

func (c *Controller) decideManual(now time.Time) (Dir, string) {
	if c.manualDir == DirStop {
		if c.motor != DirStop {
			return DirStop, "manual stop"
		}
		if c.reason == "device disconnected" || c.reason == "waiting for telemetry" {
			return DirStop, "manual: idle"
		}
		return DirStop, c.reason
	}
	if now.After(c.manualDeadline) {
		c.manualDir = DirStop
		if c.manualHold {
			return DirStop, "manual hold released"
		}
		return DirStop, "manual latch max run time reached"
	}
	return c.manualDir, fmt.Sprintf("manual %s (%s)", c.manualDir, holdName(c.manualHold))
}

// ruleRequest is what one sensor rule asks for.
type ruleRequest struct {
	action settings.Action
	why    string
}

func (c *Controller) evalRule(name string) (ruleRequest, bool) {
	s, t := c.s, c.tel
	switch name {
	case settings.RuleTilt:
		if !s.SensorsEnabled.Tilt || !t.OK.IMU {
			return ruleRequest{}, false
		}
		if t.TiltDeg > s.Tilt.MaxDeg {
			return ruleRequest{s.Tilt.Action, fmt.Sprintf("tilt %.1f° > max %.1f°", t.TiltDeg, s.Tilt.MaxDeg)}, true
		}
	case settings.RuleTemperature:
		if !s.SensorsEnabled.Temperature || !t.OK.Temp {
			return ruleRequest{}, false
		}
		return evalRange("temperature", t.TempC, s.Temperature, "%.1f °C")
	case settings.RuleSoil:
		if !s.SensorsEnabled.Soil || !t.OK.Soil {
			return ruleRequest{}, false
		}
		return evalRange("soil", t.SoilRaw, s.Soil, "%.0f")
	}
	return ruleRequest{}, false
}

func evalRange(name string, v float64, r settings.RangeRule, f string) (ruleRequest, bool) {
	switch {
	case v > r.Max:
		return ruleRequest{r.AboveAction, fmt.Sprintf("%s "+f+" > max "+f, name, v, r.Max)}, r.AboveAction != settings.ActionNone
	case v < r.Min:
		return ruleRequest{r.BelowAction, fmt.Sprintf("%s "+f+" < min "+f, name, v, r.Min)}, r.BelowAction != settings.ActionNone
	}
	return ruleRequest{}, false
}

// winner returns the highest-priority triggered rule.
func (c *Controller) winner() (ruleRequest, bool) {
	for _, name := range c.s.Priority {
		if req, ok := c.evalRule(name); ok {
			return req, true
		}
	}
	return ruleRequest{}, false
}

func (c *Controller) decideAuto(now time.Time) (Dir, string) {
	req, triggered := c.winner()
	if triggered && req.action == settings.ActionStop {
		return DirStop, req.why + " → stop"
	}

	if !c.s.SensorsEnabled.Depth {
		if !triggered {
			return DirStop, "all enabled sensors in range"
		}
		return Dir(req.action), fmt.Sprintf("%s → %s", req.why, req.action)
	}

	if !c.tel.OK.Depth {
		return DirStop, "depth sensor unavailable"
	}

	d := c.s.Depth
	depth := c.tel.DepthM
	atTarget := math.Abs(depth-c.target) <= d.ToleranceM
	prefix := fmt.Sprintf("moving to target %.2f m", c.target)

	// Gradual step: only once the batten has settled at the current target,
	// so the rule is re-evaluated at the new depth before stepping again.
	if triggered && atTarget && now.Sub(c.lastStep) >= time.Duration(d.StepIntervalS*float64(time.Second)) {
		next := c.target
		if req.action == settings.ActionUp {
			next -= d.StepM
		} else {
			next += d.StepM
		}
		next = clamp(next, d.MinM, d.MaxM)
		if next != c.target {
			c.target = next
			c.lastStep = now
			atTarget = math.Abs(depth-c.target) <= d.ToleranceM
		}
	}
	if triggered {
		prefix = fmt.Sprintf("%s → target %.2f m", req.why, c.target)
	}

	switch {
	case atTarget:
		if triggered {
			return DirStop, prefix
		}
		return DirStop, fmt.Sprintf("holding target %.2f m", c.target)
	case depth > c.target:
		return DirUp, fmt.Sprintf("%s (depth %.2f m)", prefix, depth)
	default:
		return DirDown, fmt.Sprintf("%s (depth %.2f m)", prefix, depth)
	}
}

// Status reports the current state. Call after Tick for an up-to-date motor.
func (c *Controller) Status(now time.Time) Status {
	st := Status{
		Mode:            c.mode,
		DeviceConnected: c.connected,
		Motor:           c.motor,
		DutyPct:         c.s.MotorDutyPct,
		TargetDepthM:    round(c.target, 3),
		Reason:          c.reason,
		ManualHold:      c.manualHold && c.manualDir != DirStop,
	}
	if c.haveTel {
		t := c.tel
		age := now.Sub(c.telAt).Milliseconds()
		st.Telemetry, st.TelemetryAgeMs = &t, &age
	}
	return st
}

func (c *Controller) fresh(now time.Time) bool {
	return c.haveTel && now.Sub(c.telAt) <= StaleAfter
}

func (c *Controller) limitHit(d Dir) bool {
	return (d == DirUp && c.tel.LimitTop) || (d == DirDown && c.tel.LimitBottom)
}

func (c *Controller) duty() int {
	return int(math.Round(c.s.MotorDutyPct * 255 / 100))
}

func holdName(hold bool) string {
	if hold {
		return "hold"
	}
	return "latch"
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
