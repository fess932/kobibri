package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/fess932/kobibri/internal/store"
)

// heatmapWeeks is how far back the calendar goes. A year is what makes a habit
// visible; less than that and a quiet fortnight looks like a quiet life.
const heatmapWeeks = 53

type statsData struct {
	Stats   store.ReaderStats
	Weeks   []statsWeek
	Hours   []statsHour
	Books   []store.BookPace
	Busiest int
	PeakDay int
	HasData bool
	Pending int
}

type statsWeek struct {
	Days []statsDay
}

type statsDay struct {
	Date    time.Time
	Minutes int
	Words   int
	Level   int
	Future  bool
}

type statsHour struct {
	Hour    int
	Minutes int
	Share   float64
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		http.NotFound(w, r)
		return
	}

	stats, err := store.ReadingStatsForReader(r.Context(), s.store.Reader(), user.ID, time.Local)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	data := statsData{
		Stats:   stats,
		Books:   stats.TopBooks,
		HasData: stats.Reports > 0,
	}
	data.Weeks, data.PeakDay = heatmap(stats.Days, time.Now())
	data.Hours, data.Busiest = hours(stats.ByHour)

	if pending, err := store.BooksNeedingTextIndex(r.Context(), s.store.Reader(), 100); err == nil {
		data.Pending = len(pending)
	}

	s.render(w, r, "stats.gohtml",
		page{Title: T(langOf(r), "stats.title"), Nav: "stats", Data: data})
}

// heatmap lays the days out in calendar weeks, Monday first, ending on the week
// holding today. Squares after today are drawn as empty rather than left out,
// so the grid keeps its shape.
func heatmap(days []store.Day, now time.Time) ([]statsWeek, int) {
	byDay := make(map[string]store.Day, len(days))
	peak := 0
	for _, d := range days {
		byDay[d.Date.Format("2006-01-02")] = d
		if d.Minutes > peak {
			peak = d.Minutes
		}
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// Monday of the current week, then back the whole span.
	offset := (int(today.Weekday()) + 6) % 7
	start := today.AddDate(0, 0, -offset-(heatmapWeeks-1)*7)

	weeks := make([]statsWeek, 0, heatmapWeeks)
	for w := 0; w < heatmapWeeks; w++ {
		week := statsWeek{Days: make([]statsDay, 0, 7)}
		for i := 0; i < 7; i++ {
			date := start.AddDate(0, 0, w*7+i)
			day := statsDay{Date: date, Future: date.After(today)}
			if d, ok := byDay[date.Format("2006-01-02")]; ok {
				day.Minutes, day.Words = d.Minutes, d.Words
				day.Level = level(d.Minutes, peak)
			}
			week.Days = append(week.Days, day)
		}
		weeks = append(weeks, week)
	}
	return weeks, peak
}

// level buckets a day against the busiest one rather than against a fixed
// number of minutes: what counts as a lot of reading is different for everyone.
func level(minutes, peak int) int {
	if minutes <= 0 || peak <= 0 {
		return 0
	}
	switch share := float64(minutes) / float64(peak); {
	case share >= 0.75:
		return 4
	case share >= 0.5:
		return 3
	case share >= 0.25:
		return 2
	default:
		return 1
	}
}

func hours(byHour [24]int) ([]statsHour, int) {
	busiest, peak := 0, 0
	for h, m := range byHour {
		if m > peak {
			busiest, peak = h, m
		}
	}

	out := make([]statsHour, 0, 24)
	for h, m := range byHour {
		share := 0.0
		if peak > 0 {
			share = float64(m) / float64(peak)
		}
		out = append(out, statsHour{Hour: h, Minutes: m, Share: share})
	}
	return out, busiest
}

// bookStats is the reading history of one book, for its own page.
func (s *Server) bookStats(r *http.Request, bookID string) (store.BookStats, bool) {
	user := userFrom(r.Context())
	if user == nil {
		return store.BookStats{}, false
	}

	stats, err := store.ReadingStatsForBook(r.Context(), s.store.Reader(), user.ID, bookID)
	if err != nil || stats.Reports == 0 {
		return store.BookStats{}, false
	}

	// Most recent sitting first: what someone opens this page for is the one
	// they have just finished.
	sort.Slice(stats.Sessions, func(i, j int) bool {
		return stats.Sessions[i].Start.After(stats.Sessions[j].Start)
	})
	if len(stats.Sessions) > 10 {
		stats.Sessions = stats.Sessions[:10]
	}
	return stats, true
}
