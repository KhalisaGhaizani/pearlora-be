// Package app wires the controller to the device connection, the settings
// file and the frontend status stream.
package app

import (
	"context"
	"log"
	"sync"
	"time"

	"pearlora/backend/internal/control"
	"pearlora/backend/internal/settings"
)

const (
	tickEvery   = 100 * time.Millisecond
	resendEvery = 500 * time.Millisecond // re-send a running command before its TTL
	commandTTL  = 1500 * time.Millisecond
)

// DeviceSender sends a message to the device (implemented by device.Hub).
type DeviceSender interface {
	Send(msg any)
}

// StatusSink receives status updates for the frontend.
type StatusSink interface {
	Broadcast(control.Status)
}

type Engine struct {
	mu           sync.Mutex
	c            *control.Controller
	settingsPath string
	dev          DeviceSender
	ui           StatusSink

	lastCmd    control.Command
	lastSent   time.Time
	telSeq     int // bumps on every telemetry, so the UI gets every reading
	lastPushed statusKey
}

// statusKey is the part of the status whose change triggers a UI push.
type statusKey struct {
	telSeq    int
	mode      control.Mode
	connected bool
	motor     control.Dir
	reason    string
	target    float64
}

func NewEngine(s settings.Settings, settingsPath string) *Engine {
	return &Engine{c: control.New(s), settingsPath: settingsPath, lastCmd: control.Command{Dir: control.DirStop}}
}

// Attach sets the device and UI outputs; call before Run.
func (e *Engine) Attach(dev DeviceSender, ui StatusSink) { e.dev, e.ui = dev, ui }

func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			e.mu.Lock()
			e.tickLocked(now)
			e.mu.Unlock()
		}
	}
}

// tickLocked runs the controller and pushes the command and status out.
func (e *Engine) tickLocked(now time.Time) {
	cmd := e.c.Tick(now)
	if cmd != e.lastCmd || (cmd.Dir != control.DirStop && now.Sub(e.lastSent) >= resendEvery) {
		e.sendMotor(cmd, now)
	}

	st := e.c.Status(now)
	key := statusKey{e.telSeq, st.Mode, st.DeviceConnected, st.Motor, st.Reason, st.TargetDepthM}
	if key != e.lastPushed {
		if key.motor != e.lastPushed.motor || key.mode != e.lastPushed.mode {
			log.Printf("mode=%s motor=%s reason=%q", st.Mode, st.Motor, st.Reason)
		}
		e.lastPushed = key
		e.ui.Broadcast(st)
	}
}

func (e *Engine) sendMotor(cmd control.Command, now time.Time) {
	e.dev.Send(map[string]any{
		"type": "motor", "dir": cmd.Dir, "duty": cmd.Duty, "ttl_ms": commandTTL.Milliseconds(),
	})
	e.lastCmd, e.lastSent = cmd, now
}

// ---- device.Handler ----

func (e *Engine) DeviceConnected() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.c.SetConnected(true)
	e.sendMotor(control.Command{Dir: control.DirStop}, time.Now())
}

func (e *Engine) DeviceDisconnected() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.c.SetConnected(false)
	e.tickLocked(time.Now())
}

func (e *Engine) DeviceTelemetry(t control.Telemetry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	e.c.OnTelemetry(t, now)
	e.telSeq++
	e.tickLocked(now)
}

// ---- API operations ----

func (e *Engine) Status() control.Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.c.Status(time.Now())
}

func (e *Engine) SetMode(m control.Mode) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.c.SetMode(m); err != nil {
		return err
	}
	e.tickLocked(time.Now())
	return nil
}

func (e *Engine) Manual(action control.Dir, hold bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if err := e.c.Manual(action, hold, now); err != nil {
		return err
	}
	e.tickLocked(now)
	return nil
}

func (e *Engine) ResetTarget() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.c.ResetTarget()
	e.tickLocked(time.Now())
}

func (e *Engine) ZeroDepth() {
	e.dev.Send(map[string]string{"type": "zero_depth"})
}

func (e *Engine) Settings() settings.Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.c.Settings()
}

// UpdateSettings validates, saves and applies new settings.
func (e *Engine) UpdateSettings(s settings.Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := settings.Save(e.settingsPath, s); err != nil {
		return err
	}
	e.c.SetSettings(s)
	e.tickLocked(time.Now())
	return nil
}
