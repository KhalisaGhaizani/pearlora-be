package settings

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultsValid(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Settings){
		"duty zero":          func(s *Settings) { s.MotorDutyPct = 0 },
		"depth band flipped": func(s *Settings) { s.Depth.MinM, s.Depth.MaxM = 1, 0.5 },
		"base outside band":  func(s *Settings) { s.Depth.BaseTargetM = 5 },
		"temp min >= max":    func(s *Settings) { s.Temperature.Min = 31 },
		"bad action":         func(s *Settings) { s.Soil.AboveAction = "sideways" },
		"bad tilt action":    func(s *Settings) { s.Tilt.Action = "" },
		"priority dup":       func(s *Settings) { s.Priority = []string{"tilt", "tilt", "soil"} },
		"priority short":     func(s *Settings) { s.Priority = []string{"tilt"} },
		"priority unknown":   func(s *Settings) { s.Priority = []string{"tilt", "soil", "depth"} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s := Defaults()
			mut(&s)
			if s.Validate() == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	got, err := Load(path)
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if !reflect.DeepEqual(got, Defaults()) {
		t.Fatal("missing file should give defaults")
	}

	s := Defaults()
	s.MotorDutyPct = 55
	s.Priority = []string{RuleSoil, RuleTilt, RuleTemperature}
	s.Tilt.Action = ActionDown
	if err := Save(path, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err = Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, s)
	}
}
