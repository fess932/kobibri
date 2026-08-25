package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/fess932/kobibri/internal/store"
)

func statsEnv(t *testing.T) (context.Context, *store.Store, int64) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "kobibri.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	userID, err := store.CreateUser(ctx, st.Writer(), "reader", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, st, userID
}

// report is one progress report as a device sends it: a position and the
// device's own cumulative count of minutes actually spent reading.
type report struct {
	at     time.Time
	source string
	block  int
	spent  int
}

func record(t *testing.T, ctx context.Context, st *store.Store, userID int64, bookID string, reports ...report) {
	t.Helper()
	for _, r := range reports {
		r := r
		err := st.Tx(ctx, func(tx *sql.Tx) error {
			spent := r.spent
			block := r.block
			_, err := store.AppendReadingEvent(ctx, tx, store.ReadingEvent{
				UserID: userID, BookID: bookID, DeviceID: 1,
				At: r.at, DeviceAt: r.at, Status: store.ReadReading,
				Source: r.source, Block: &block, Span: "kobo." + itoa(block) + ".1",
				Spent: &spent,
			})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf []byte
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	return string(buf)
}

func addBook(t *testing.T, ctx context.Context, st *store.Store, bookID string) {
	t.Helper()
	now := store.Now()
	if _, err := st.Writer().ExecContext(ctx,
		`INSERT INTO books (id, title, created_at, updated_at) VALUES (?,?,?,?)`,
		bookID, "A Book", now, now); err != nil {
		t.Fatal(err)
	}
}

// index gives the book a word offset for each block, evenly spaced.
func index(t *testing.T, ctx context.Context, st *store.Store, bookID string, blocks, wordsPerBlock int) {
	t.Helper()
	addBook(t, ctx, st, bookID)
	ix := &store.TextIndex{Fingerprint: "fp", Words: blocks * wordsPerBlock, Spanned: true}
	ix.Docs = append(ix.Docs, store.TextDoc{Seq: 0, Source: "ch1.xhtml",
		Title: "One", Words: blocks * wordsPerBlock})
	for b := 1; b <= blocks; b++ {
		ix.Blocks = append(ix.Blocks, store.TextBlock{
			Source: "ch1.xhtml", Block: b, Before: (b - 1) * wordsPerBlock,
		})
	}
	if err := st.SaveTextIndex(ctx, bookID, ix); err != nil {
		t.Fatal(err)
	}
}

// Speed is words over the minutes the device counted, not over the clock. The
// device reports every fifteen to thirty minutes and counts only real reading,
// so the two are never the same number.
func TestSpeedComesFromTheDeviceCounterNotTheClock(t *testing.T) {
	ctx, st, userID := statsEnv(t)
	const bookID = "book-1"
	index(t, ctx, st, bookID, 200, 100)

	start := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	record(t, ctx, st, userID, bookID,
		report{at: start, source: "ch1.xhtml", block: 1, spent: 200},
		// Half an hour later by the clock, but the device counted ten minutes,
		// over twenty blocks: two thousand words in ten minutes.
		report{at: start.Add(30 * time.Minute), source: "ch1.xhtml", block: 21, spent: 210},
	)

	stats, err := store.ReadingStatsForBook(ctx, st.Reader(), userID, bookID)
	if err != nil {
		t.Fatal(err)
	}

	if stats.Minutes != 10 {
		t.Errorf("minutes = %d, want 10 — the device counter, not the clock", stats.Minutes)
	}
	if stats.WordsRead != 2000 {
		t.Errorf("words read = %d, want 2000", stats.WordsRead)
	}
	if got := stats.WPM(); got != 200 {
		t.Errorf("wpm = %v, want 200", got)
	}
}

// The percentage the device sends is a whole number and does not move inside a
// long book, which is the whole reason positions are resolved to words.
func TestPositionsAreResolvedToWordsNotPercent(t *testing.T) {
	ctx, st, userID := statsEnv(t)
	const bookID = "book-2"
	index(t, ctx, st, bookID, 400, 250)

	start := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	record(t, ctx, st, userID, bookID,
		report{at: start, source: "ch1.xhtml", block: 1, spent: 0},
		report{at: start.Add(20 * time.Minute), source: "ch1.xhtml", block: 41, spent: 20},
	)

	stats, err := store.ReadingStatsForBook(ctx, st.Reader(), userID, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.WordsRead != 10000 {
		t.Fatalf("words read = %d, want 10000", stats.WordsRead)
	}
	if !stats.Indexed || stats.TotalWords != 100000 {
		t.Fatalf("book length = %d, want 100000", stats.TotalWords)
	}
	// 90,000 words left at 500 a minute.
	if got := stats.RemainingMinutes(); got != 180 {
		t.Errorf("remaining = %d minutes, want 180", got)
	}
}

// A pause longer than the session gap starts a new sitting; reports inside one
// evening are one.
func TestSittingsAreSplitByThePauseBetweenThem(t *testing.T) {
	ctx, st, userID := statsEnv(t)
	const bookID = "book-3"
	index(t, ctx, st, bookID, 100, 100)

	evening := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	record(t, ctx, st, userID, bookID,
		report{at: evening, source: "ch1.xhtml", block: 1, spent: 0},
		report{at: evening.Add(20 * time.Minute), source: "ch1.xhtml", block: 5, spent: 18},
		report{at: evening.Add(45 * time.Minute), source: "ch1.xhtml", block: 9, spent: 40},
		report{at: evening.Add(26 * time.Hour), source: "ch1.xhtml", block: 20, spent: 70},
	)

	stats, err := store.ReadingStatsForBook(ctx, st.Reader(), userID, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Sessions) != 2 {
		t.Fatalf("sittings = %d, want 2", len(stats.Sessions))
	}
	if stats.Sessions[0].Minutes != 40 {
		t.Errorf("first sitting = %d minutes, want 40", stats.Sessions[0].Minutes)
	}
	if stats.Sessions[1].Minutes != 30 {
		t.Errorf("second sitting = %d minutes, want 30", stats.Sessions[1].Minutes)
	}
}

// A counter that goes backwards is a book re-added or a device reset, not a
// reader who unread an hour.
func TestACounterGoingBackwardsDoesNotSubtract(t *testing.T) {
	ctx, st, userID := statsEnv(t)
	const bookID = "book-4"
	index(t, ctx, st, bookID, 100, 100)

	start := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	record(t, ctx, st, userID, bookID,
		report{at: start, source: "ch1.xhtml", block: 1, spent: 300},
		report{at: start.Add(time.Hour), source: "ch1.xhtml", block: 10, spent: 5},
		report{at: start.Add(2 * time.Hour), source: "ch1.xhtml", block: 20, spent: 15},
	)

	stats, err := store.ReadingStatsForBook(ctx, st.Reader(), userID, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Minutes != 10 {
		t.Errorf("minutes = %d, want 10: the reset must not count as -295", stats.Minutes)
	}
}

// A report that repeats the last one is a device being woken, not reading.
func TestARepeatedReportIsNotHistory(t *testing.T) {
	ctx, st, userID := statsEnv(t)
	const bookID = "book-5"

	start := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	record(t, ctx, st, userID, bookID,
		report{at: start, source: "ch1.xhtml", block: 3, spent: 40},
		report{at: start.Add(time.Hour), source: "ch1.xhtml", block: 3, spent: 40},
	)

	var n int
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM reading_events WHERE book_id = ?`, bookID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stored %d events, want 1", n)
	}
}

// The reader's own page counts days in the server's time zone, and a streak
// survives today not having been read yet.
func TestTheStreakCountsDaysNotReports(t *testing.T) {
	ctx, st, userID := statsEnv(t)
	const bookID = "book-6"
	index(t, ctx, st, bookID, 500, 100)

	now := time.Now().UTC()
	spent := 0
	block := 1
	for d := 3; d >= 1; d-- {
		day := now.AddDate(0, 0, -d)
		for i := 0; i < 2; i++ {
			spent += 15
			block += 10
			record(t, ctx, st, userID, bookID, report{
				at:     day.Add(time.Duration(i) * 30 * time.Minute),
				source: "ch1.xhtml", block: block, spent: spent,
			})
		}
	}

	stats, err := store.ReadingStatsForReader(ctx, st.Reader(), userID, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ActiveDays != 3 {
		t.Errorf("active days = %d, want 3", stats.ActiveDays)
	}
	if stats.Streak != 3 {
		t.Errorf("streak = %d, want 3 — yesterday's reading still counts today", stats.Streak)
	}
	// Five deltas of fifteen, not six: the first report of a book has nothing
	// to be a delta from, and counting the whole counter at that moment would
	// credit a reader with every minute they had already spent elsewhere.
	if stats.Minutes != 75 {
		t.Errorf("minutes = %d, want 75", stats.Minutes)
	}
}

// Minutes with no measurable distance still count as time read, but they must
// not land in the denominator of a speed: the first report of a book has
// nothing before it, and a status-only report has no position at all.
func TestUnmeasurableMinutesDoNotDragTheSpeedDown(t *testing.T) {
	ctx, st, userID := statsEnv(t)
	const bookID = "book-7"
	index(t, ctx, st, bookID, 200, 100)

	start := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	record(t, ctx, st, userID, bookID,
		report{at: start, source: "ch1.xhtml", block: 1, spent: 0},
		report{at: start.Add(20 * time.Minute), source: "ch1.xhtml", block: 21, spent: 10},
	)
	// A report from a second device, its own counter already at an hour: real
	// time read, but nothing before it to measure a distance against.
	if err := st.Tx(ctx, func(tx *sql.Tx) error {
		spent, block := 60, 30
		_, err := store.AppendReadingEvent(ctx, tx, store.ReadingEvent{
			UserID: userID, BookID: bookID, DeviceID: 2,
			At: start.Add(time.Hour), Status: store.ReadReading,
			Source: "ch1.xhtml", Block: &block, Span: "kobo.30.1", Spent: &spent,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := store.ReadingStatsForBook(ctx, st.Reader(), userID, bookID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats.WPM(); got != 200 {
		t.Errorf("wpm = %v, want 200", got)
	}
	if stats.Measured != 10 {
		t.Errorf("measured = %d minutes, want 10", stats.Measured)
	}
}
