package alert

import "testing"

func TestFingerprintIsOrderIndependentAndLabelSensitive(t *testing.T) {
	a := Fingerprint(map[string]string{"alertname": "error_rate_spike", "project_id": "alpha"})
	b := Fingerprint(map[string]string{"project_id": "alpha", "alertname": "error_rate_spike"})
	if a != b {
		t.Fatalf("fingerprint depends on map order: %s != %s", a, b)
	}
	if c := Fingerprint(map[string]string{"alertname": "error_rate_spike", "project_id": "beta"}); c == a {
		t.Fatal("fingerprint ignored label value change")
	}
}

func TestValidateMatchersRejectsEmptyAndInvalidRegex(t *testing.T) {
	if err := ValidateMatchers(nil); err == nil {
		t.Fatal("empty matcher set accepted")
	}
	if err := ValidateMatchers([]Matcher{{Name: "instance", Op: "=~", Value: "[unclosed"}}); err == nil {
		t.Fatal("invalid regex accepted")
	}
	if err := ValidateMatchers([]Matcher{{Name: "severity", Op: "=", Value: "critical"}}); err != nil {
		t.Fatalf("valid matcher rejected: %v", err)
	}
}
