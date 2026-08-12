package store_test

import (
	"testing"

	"github.com/fess932/kobibri/internal/store"
)

// Progress is stored exactly as the device sent it, so reading it back means
// digging through a bookmark. Getting that wrong shows every book as unstarted.
func TestProgressIsReadOutOfTheBookmark(t *testing.T) {
	tests := []struct {
		name     string
		progress store.Progress
		want     int
		started  bool
	}{
		{"unread", store.Progress{Status: store.ReadReady}, 0, false},
		{"a third in", store.Progress{Status: store.ReadReading, Percent: 33.7}, 33, true},
		{"finished", store.Progress{Status: store.ReadFinished, Percent: 100}, 100, true},
		{
			// A device that reports 99.6% has not finished the book, and saying
			// it has is worse than being a percent shy.
			"nearly finished",
			store.Progress{Status: store.ReadReading, Percent: 99.6}, 99, true,
		},
		{
			// Status alone is enough to say someone has started.
			"reading with no percentage",
			store.Progress{Status: store.ReadReading}, 0, true,
		},
	}

	for _, tt := range tests {
		if got := tt.progress.Rounded(); got != tt.want {
			t.Errorf("%s: Rounded() = %d, want %d", tt.name, got, tt.want)
		}
		if got := tt.progress.Started(); got != tt.started {
			t.Errorf("%s: Started() = %v, want %v", tt.name, got, tt.started)
		}
	}
}
