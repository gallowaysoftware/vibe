package cli

import (
	"reflect"
	"testing"
)

func TestValidateRunArgs(t *testing.T) {
	cases := []struct {
		name    string
		dashAt  int
		args    []string
		wantErr bool
	}{
		{"plain profile name", -1, []string{"omp"}, false},
		{"no args", -1, []string{}, true},
		{"two args, no dash", -1, []string{"omp", "extra"}, true},
		{"profile then dash-args", 1, []string{"omp", "-c"}, false},
		{"profile then multiple dash-args", 1, []string{"omp", "-r", "abc123"}, false},
		{"dash with zero profile args", 0, []string{"-c"}, true},
		{"dash with two profile args", 2, []string{"omp", "extra", "-c"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRunArgs(tc.dashAt, tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateRunArgs(%d, %v) error = %v, wantErr %v", tc.dashAt, tc.args, err, tc.wantErr)
			}
		})
	}
}

func TestSplitRunArgs(t *testing.T) {
	cases := []struct {
		name         string
		dashAt       int
		args         []string
		wantProfile  string
		wantPassthru []string
	}{
		{"no dash", -1, []string{"omp"}, "omp", nil},
		{"dash with one passthrough", 1, []string{"omp", "-c"}, "omp", []string{"-c"}},
		{"dash with two passthrough", 1, []string{"omp", "-r", "abc123"}, "omp", []string{"-r", "abc123"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profile, passthru := splitRunArgs(tc.dashAt, tc.args)
			if profile != tc.wantProfile {
				t.Errorf("profile = %q, want %q", profile, tc.wantProfile)
			}
			if !reflect.DeepEqual(passthru, tc.wantPassthru) {
				t.Errorf("passthru = %v, want %v", passthru, tc.wantPassthru)
			}
		})
	}
}
