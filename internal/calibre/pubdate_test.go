package calibre

import "testing"

func TestUndefinedCalibreDateReadsAsNoDate(t *testing.T) {
	for _, raw := range []string{
		"0101-01-01 00:00:00+00:00",
		"0101-01-01T00:00:00+00:00",
		"0101-01-01 00:00:00.000000+00:00",
	} {
		if got := parseTime(raw); !got.IsZero() {
			t.Errorf("parseTime(%q) = %v, want the zero time: it is Calibre's "+
				"UNDEFINED_DATE and reaches a device as a year-101 publication date", raw, got)
		}
	}
}

func TestRealCalibreDatesStillParse(t *testing.T) {
	got := parseTime("2020-05-01 00:00:00+00:00")
	if got.IsZero() {
		t.Fatal("a real date was thrown away with the sentinel")
	}
	if got.Year() != 2020 || got.Month() != 5 {
		t.Errorf("parsed %v, want May 2020", got)
	}
}
