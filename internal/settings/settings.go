// Package settings holds the user-configurable control settings, their dummy
// defaults, validation, and JSON persistence.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Action is what a rule asks the motor to do when it triggers.
type Action string

const (
	ActionNone Action = "none"
	ActionUp   Action = "up" // shallower
	ActionDown Action = "down"
	ActionStop Action = "stop"
)

func (a Action) valid() bool {
	switch a {
	case ActionNone, ActionUp, ActionDown, ActionStop:
		return true
	}
	return false
}

// Rule names used in Priority and SensorsEnabled.
const (
	RuleTilt        = "tilt"
	RuleTemperature = "temperature"
	RuleSoil        = "soil"
)

var ruleNames = []string{RuleTilt, RuleTemperature, RuleSoil}

type SensorsEnabled struct {
	Depth       bool `json:"depth"`
	Temperature bool `json:"temperature"`
	Soil        bool `json:"soil"`
	Tilt        bool `json:"tilt"`
}

// Depth controls the target-depth loop. Depths are metres below the surface.
type Depth struct {
	BaseTargetM   float64 `json:"base_target_m"`
	ToleranceM    float64 `json:"tolerance_m"`
	MinM          float64 `json:"min_m"`
	MaxM          float64 `json:"max_m"`
	StepM         float64 `json:"step_m"`
	StepIntervalS float64 `json:"step_interval_s"`
}

// RangeRule triggers AboveAction when the value is above Max and
// BelowAction when it is below Min.
type RangeRule struct {
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
	AboveAction Action  `json:"above_action"`
	BelowAction Action  `json:"below_action"`
}

// Tilt triggers Action when the tilt from vertical exceeds MaxDeg.
type Tilt struct {
	MaxDeg float64 `json:"max_deg"`
	Action Action  `json:"action"`
}

type Manual struct {
	HoldTimeoutMs int     `json:"hold_timeout_ms"`
	LatchMaxRunS  float64 `json:"latch_max_run_s"`
}

type Settings struct {
	MotorDutyPct   float64        `json:"motor_duty_pct"`
	SensorsEnabled SensorsEnabled `json:"sensors_enabled"`
	Priority       []string       `json:"priority"`
	Depth          Depth          `json:"depth"`
	Temperature    RangeRule      `json:"temperature"`
	Soil           RangeRule      `json:"soil"`
	Tilt           Tilt           `json:"tilt"`
	Manual         Manual         `json:"manual"`
}

// Defaults returns dummy values sized for the tank prototype.
func Defaults() Settings {
	return Settings{
		MotorDutyPct:   40,
		SensorsEnabled: SensorsEnabled{Depth: true, Temperature: true, Soil: true, Tilt: true},
		Priority:       []string{RuleTilt, RuleTemperature, RuleSoil},
		Depth: Depth{
			BaseTargetM: 0.40, ToleranceM: 0.03, MinM: 0.10, MaxM: 0.80,
			StepM: 0.05, StepIntervalS: 10,
		},
		Temperature: RangeRule{Min: 24, Max: 30, AboveAction: ActionDown, BelowAction: ActionUp},
		// Soil sensor raw ADC (0..4095, lower = wetter); stand-in for salinity.
		Soil:   RangeRule{Min: 1200, Max: 2800, AboveAction: ActionDown, BelowAction: ActionUp},
		Tilt:   Tilt{MaxDeg: 15, Action: ActionStop},
		Manual: Manual{HoldTimeoutMs: 600, LatchMaxRunS: 30},
	}
}

func (s Settings) Validate() error {
	var errs []error
	if s.MotorDutyPct <= 0 || s.MotorDutyPct > 100 {
		errs = append(errs, errors.New("motor_duty_pct must be in (0, 100]"))
	}

	d := s.Depth
	if d.MinM < 0 || d.MinM >= d.MaxM {
		errs = append(errs, errors.New("depth: need 0 <= min_m < max_m"))
	}
	if d.BaseTargetM < d.MinM || d.BaseTargetM > d.MaxM {
		errs = append(errs, errors.New("depth: base_target_m must be within [min_m, max_m]"))
	}
	if d.ToleranceM <= 0 {
		errs = append(errs, errors.New("depth: tolerance_m must be > 0"))
	}
	if d.StepM <= 0 || d.StepIntervalS <= 0 {
		errs = append(errs, errors.New("depth: step_m and step_interval_s must be > 0"))
	}

	for name, r := range map[string]RangeRule{RuleTemperature: s.Temperature, RuleSoil: s.Soil} {
		if r.Min >= r.Max {
			errs = append(errs, fmt.Errorf("%s: min must be < max", name))
		}
		if !r.AboveAction.valid() || !r.BelowAction.valid() {
			errs = append(errs, fmt.Errorf("%s: actions must be one of none/up/down/stop", name))
		}
	}

	if s.Tilt.MaxDeg <= 0 || s.Tilt.MaxDeg >= 90 {
		errs = append(errs, errors.New("tilt: max_deg must be in (0, 90)"))
	}
	if !s.Tilt.Action.valid() {
		errs = append(errs, errors.New("tilt: action must be one of none/up/down/stop"))
	}

	if s.Manual.HoldTimeoutMs <= 0 || s.Manual.LatchMaxRunS <= 0 {
		errs = append(errs, errors.New("manual: hold_timeout_ms and latch_max_run_s must be > 0"))
	}

	if !isPermutation(s.Priority, ruleNames) {
		errs = append(errs, fmt.Errorf("priority must list each of %v exactly once", ruleNames))
	}
	return errors.Join(errs...)
}

func isPermutation(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

// Load reads settings from path. A missing file yields the defaults.
func Load(path string) (Settings, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	s := Defaults() // fields missing from the file keep their defaults
	if err := json.Unmarshal(b, &s); err != nil {
		return Settings{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return Settings{}, fmt.Errorf("invalid %s: %w", path, err)
	}
	return s, nil
}

// Save writes settings atomically (temp file + rename).
func Save(path string, s Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
