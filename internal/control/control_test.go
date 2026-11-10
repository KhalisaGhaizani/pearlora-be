package control

import (
	"errors"
	"strings"
	"testing"
	"time"

	"pearlora/backend/internal/settings"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// okTel is telemetry with every sensor valid and in range for Defaults(),
// with the batten at the default base target.
func okTel() Telemetry {
	return Telemetry{
		TempC: 27, DepthM: 0.40, SoilRaw: 2000, TiltDeg: 2,
		OK: SensorOK{Temp: true, Depth: true, IMU: true, Soil: true},
	}
}

func newAuto(t *testing.T, s settings.Settings, tel Telemetry) *Controller {
	t.Helper()
	if err := s.Validate(); err != nil {
		t.Fatalf("test settings invalid: %v", err)
	}
	c := New(s)
	c.SetConnected(true)
	c.OnTelemetry(tel, t0)
	if err := c.SetMode(ModeAuto); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAutoRules(t *testing.T) {
	// Depth disabled so each rule's request drives the motor directly.
	base := settings.Defaults()
	base.SensorsEnabled.Depth = false

	cases := []struct {
		name   string
		mutS   func(*settings.Settings)
		mutT   func(*Telemetry)
		want   Dir
		reason string
	}{
		{"all in range", nil, nil, DirStop, "in range"},
		{"temp above", nil, func(t *Telemetry) { t.TempC = 31 }, DirDown, "temperature"},
		{"temp below", nil, func(t *Telemetry) { t.TempC = 20 }, DirUp, "temperature"},
		{"soil above", nil, func(t *Telemetry) { t.SoilRaw = 3000 }, DirDown, "soil"},
		{"soil below", nil, func(t *Telemetry) { t.SoilRaw = 900 }, DirUp, "soil"},
		{"tilt default stop", nil, func(t *Telemetry) { t.TiltDeg = 20 }, DirStop, "tilt"},
		{"tilt user action down",
			func(s *settings.Settings) { s.Tilt.Action = settings.ActionDown },
			func(t *Telemetry) { t.TiltDeg = 20 }, DirDown, "tilt"},
		{"disabled rule ignored",
			func(s *settings.Settings) { s.SensorsEnabled.Temperature = false },
			func(t *Telemetry) { t.TempC = 31 }, DirStop, "in range"},
		{"invalid sensor ignored", nil,
			func(t *Telemetry) { t.TempC = 31; t.OK.Temp = false }, DirStop, "in range"},
		{"action none ignored",
			func(s *settings.Settings) { s.Temperature.AboveAction = settings.ActionNone },
			func(t *Telemetry) { t.TempC = 31 }, DirStop, "in range"},
		{"priority: temp beats soil", nil,
			func(t *Telemetry) { t.TempC = 31; t.SoilRaw = 900 }, DirDown, "temperature"},
		{"priority: user puts soil first",
			func(s *settings.Settings) { s.Priority = []string{"soil", "temperature", "tilt"} },
			func(t *Telemetry) { t.TempC = 31; t.SoilRaw = 900 }, DirUp, "soil"},
		{"tilt stop beats temp move", nil,
			func(t *Telemetry) { t.TempC = 31; t.TiltDeg = 20 }, DirStop, "tilt"},
		{"lower-priority stop loses to move",
			func(s *settings.Settings) { s.Priority = []string{"temperature", "tilt", "soil"} },
			func(t *Telemetry) { t.TempC = 31; t.TiltDeg = 20 }, DirDown, "temperature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			s.Priority = append([]string(nil), base.Priority...)
			if tc.mutS != nil {
				tc.mutS(&s)
			}
			tel := okTel()
			if tc.mutT != nil {
				tc.mutT(&tel)
			}
			c := newAuto(t, s, tel)
			cmd := c.Tick(t0)
			if cmd.Dir != tc.want {
				t.Fatalf("dir = %s, want %s (reason %q)", cmd.Dir, tc.want, c.reason)
			}
			if !strings.Contains(c.reason, tc.reason) {
				t.Fatalf("reason %q does not mention %q", c.reason, tc.reason)
			}
		})
	}
}

func TestAutoDriveToTarget(t *testing.T) {
	cases := []struct {
		depth float64
		want  Dir
	}{
		{0.40, DirStop},
		{0.42, DirStop}, // within tolerance
		{0.38, DirStop},
		{0.50, DirUp},   // too deep → shallower
		{0.30, DirDown}, // too shallow → deeper
	}
	for _, tc := range cases {
		tel := okTel()
		tel.DepthM = tc.depth
		c := newAuto(t, settings.Defaults(), tel)
		if got := c.Tick(t0).Dir; got != tc.want {
			t.Errorf("depth %.2f: dir = %s, want %s", tc.depth, got, tc.want)
		}
	}
}

func TestAutoGradualStep(t *testing.T) {
	s := settings.Defaults() // step 0.05 m every 10 s, band 0.10..0.80
	tel := okTel()
	tel.TempC = 31 // too hot → down
	c := newAuto(t, s, tel)

	// First trigger steps immediately: 0.40 → 0.45, batten at 0.40 so go down.
	if cmd := c.Tick(t0); cmd.Dir != DirDown || !near(c.target, 0.45) {
		t.Fatalf("first step: dir %s target %.3f", cmd.Dir, c.target)
	}

	// Batten reaches 0.45 but the interval hasn't elapsed: hold.
	tel.DepthM = 0.45
	c.OnTelemetry(tel, t0.Add(5*time.Second))
	if cmd := c.Tick(t0.Add(5 * time.Second)); cmd.Dir != DirStop || !near(c.target, 0.45) {
		t.Fatalf("before interval: dir %s target %.3f", cmd.Dir, c.target)
	}

	// After the interval: step again.
	c.OnTelemetry(tel, t0.Add(10*time.Second))
	if cmd := c.Tick(t0.Add(10 * time.Second)); cmd.Dir != DirDown || !near(c.target, 0.50) {
		t.Fatalf("second step: dir %s target %.3f", cmd.Dir, c.target)
	}

	// No step while still travelling, even after the interval.
	tel.DepthM = 0.46
	c.OnTelemetry(tel, t0.Add(25*time.Second))
	if c.Tick(t0.Add(25 * time.Second)); !near(c.target, 0.50) {
		t.Fatalf("stepped while travelling: target %.3f", c.target)
	}

	// Condition clears: target stays where it is.
	tel.TempC, tel.DepthM = 27, 0.50
	c.OnTelemetry(tel, t0.Add(40*time.Second))
	if cmd := c.Tick(t0.Add(40 * time.Second)); cmd.Dir != DirStop || !near(c.target, 0.50) {
		t.Fatalf("after clear: dir %s target %.3f", cmd.Dir, c.target)
	}

	c.ResetTarget()
	if !near(c.target, 0.40) {
		t.Fatalf("reset target = %.3f", c.target)
	}
}

func TestAutoStepClamped(t *testing.T) {
	s := settings.Defaults()
	tel := okTel()
	tel.TempC = 20 // too cold → up
	tel.DepthM = 0.10
	c := newAuto(t, s, tel)
	c.target = 0.12
	c.Tick(t0)
	if !near(c.target, 0.10) {
		t.Fatalf("target = %.3f, want clamp to 0.10", c.target)
	}
	c.OnTelemetry(tel, t0.Add(time.Minute))
	c.Tick(t0.Add(time.Minute))
	if !near(c.target, 0.10) {
		t.Fatalf("target = %.3f, want to stay at min", c.target)
	}
}

func TestAutoDepthSensorMissing(t *testing.T) {
	tel := okTel()
	tel.OK.Depth = false
	tel.DepthM = 0.70
	c := newAuto(t, settings.Defaults(), tel)
	if cmd := c.Tick(t0); cmd.Dir != DirStop || !strings.Contains(c.reason, "depth") {
		t.Fatalf("dir %s reason %q", cmd.Dir, c.reason)
	}
}

func TestStaleAndDisconnected(t *testing.T) {
	tel := okTel()
	tel.DepthM = 0.70
	c := newAuto(t, settings.Defaults(), tel)
	if c.Tick(t0).Dir != DirUp {
		t.Fatal("expected up while fresh")
	}
	if c.Tick(t0.Add(StaleAfter+time.Millisecond)).Dir != DirStop {
		t.Fatal("expected stop on stale telemetry")
	}
	c.OnTelemetry(tel, t0.Add(3*time.Second))
	c.SetConnected(false)
	if c.Tick(t0.Add(3*time.Second)).Dir != DirStop {
		t.Fatal("expected stop when disconnected")
	}
}

func TestLimitSwitches(t *testing.T) {
	tel := okTel()
	tel.DepthM = 0.70
	tel.LimitTop = true
	c := newAuto(t, settings.Defaults(), tel)
	if cmd := c.Tick(t0); cmd.Dir != DirStop || !strings.Contains(c.reason, "top limit") {
		t.Fatalf("auto at top limit: dir %s reason %q", cmd.Dir, c.reason)
	}

	c.SetMode(ModeManual)
	if err := c.Manual(DirUp, false, t0); !errors.Is(err, ErrLimit) {
		t.Fatalf("manual up at top limit: err = %v", err)
	}
	if err := c.Manual(DirDown, false, t0); err != nil {
		t.Fatalf("manual down at top limit: %v", err)
	}

	// Bottom limit hit mid-move stops a latched move.
	tel.LimitTop, tel.LimitBottom = false, true
	c.OnTelemetry(tel, t0.Add(time.Second))
	if c.Tick(t0.Add(time.Second)).Dir != DirStop || c.manualDir != DirStop {
		t.Fatal("latched move should stop at the bottom limit")
	}
}

func TestManualHold(t *testing.T) {
	c := New(settings.Defaults()) // hold timeout 600 ms
	c.SetConnected(true)
	c.OnTelemetry(okTel(), t0)

	if err := c.Manual(DirUp, true, t0); err != nil {
		t.Fatal(err)
	}
	cmd := c.Tick(t0.Add(300 * time.Millisecond))
	if cmd.Dir != DirUp || cmd.Duty != 102 {
		t.Fatalf("held: %+v", cmd)
	}
	// Repeat extends the deadline.
	c.Manual(DirUp, true, t0.Add(500*time.Millisecond))
	if c.Tick(t0.Add(1000*time.Millisecond)).Dir != DirUp {
		t.Fatal("repeat should keep it moving")
	}
	// No repeat: stops after the timeout.
	if c.Tick(t0.Add(1200*time.Millisecond)).Dir != DirStop {
		t.Fatal("expected stop after hold timeout")
	}
	if !strings.Contains(c.reason, "released") {
		t.Fatalf("reason %q", c.reason)
	}
}

func TestManualLatch(t *testing.T) {
	s := settings.Defaults()
	s.Manual.LatchMaxRunS = 5
	c := New(s)
	c.SetConnected(true)

	if err := c.Manual(DirDown, false, t0); !errors.Is(err, ErrNoTelemetry) {
		t.Fatalf("without telemetry: err = %v", err)
	}
	c.OnTelemetry(okTel(), t0)
	if err := c.Manual(DirDown, false, t0); err != nil {
		t.Fatal(err)
	}
	c.OnTelemetry(okTel(), t0.Add(4*time.Second))
	if c.Tick(t0.Add(4*time.Second)).Dir != DirDown {
		t.Fatal("latched move should continue")
	}
	c.Manual(DirStop, false, t0.Add(4*time.Second))
	if c.Tick(t0.Add(4*time.Second)).Dir != DirStop {
		t.Fatal("stop should stop")
	}

	c.Manual(DirUp, false, t0.Add(4*time.Second))
	c.OnTelemetry(okTel(), t0.Add(9500*time.Millisecond))
	if c.Tick(t0.Add(9500*time.Millisecond)).Dir != DirStop {
		t.Fatal("expected stop after latch max run")
	}
}

func TestManualRejectedInAuto(t *testing.T) {
	c := newAuto(t, settings.Defaults(), okTel())
	if err := c.Manual(DirUp, true, t0); !errors.Is(err, ErrNotManual) {
		t.Fatalf("err = %v", err)
	}
}

func TestModeSwitchStopsAndEStop(t *testing.T) {
	c := New(settings.Defaults())
	c.SetConnected(true)
	c.OnTelemetry(okTel(), t0)
	c.Manual(DirUp, false, t0)
	c.Tick(t0)
	c.SetMode(ModeAuto)
	if c.motor != DirStop || c.manualDir != DirStop {
		t.Fatal("mode switch should stop the motor")
	}

	tel := okTel()
	tel.EStop = true
	c.OnTelemetry(tel, t0)
	if c.Mode() != ModeManual || c.Tick(t0).Dir != DirStop {
		t.Fatal("estop should force manual + stop")
	}
	if err := c.Manual(DirUp, false, t0); !errors.Is(err, ErrEStop) {
		t.Fatalf("manual during estop: err = %v", err)
	}
	if err := c.SetMode(ModeAuto); !errors.Is(err, ErrEStop) {
		t.Fatalf("auto during estop: err = %v", err)
	}
}

func TestSetSettingsTarget(t *testing.T) {
	c := newAuto(t, settings.Defaults(), okTel())
	c.target = 0.70

	s := settings.Defaults()
	s.Depth.MaxM = 0.60
	c.SetSettings(s)
	if !near(c.target, 0.60) {
		t.Fatalf("target should clamp to new max, got %.3f", c.target)
	}

	s.Depth.BaseTargetM = 0.30
	c.SetSettings(s)
	if !near(c.target, 0.30) {
		t.Fatalf("target should jump to new base, got %.3f", c.target)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
