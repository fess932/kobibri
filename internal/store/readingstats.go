package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"
)

// sessionGap is how long a pause has to be before two reports belong to
// different sittings. A device reports every fifteen to thirty minutes while a
// book is open, so the gap has to be wider than that; an hour is well clear of
// it and still separates an evening from the next morning.
const sessionGap = time.Hour

// A reading interval only counts towards a speed when both ends are known and
// neither is absurd. A single report claiming three hundred pages in a minute
// is a device that was reset, not a reader.
const maxSaneWPM = 2000

// ReadingSession is one sitting.
type ReadingSession struct {
	Start    time.Time
	End      time.Time
	Minutes  int
	Measured int
	Words    int
	Reports  int
}

// WPM is words a minute, zero when there is nothing to divide.
func (s ReadingSession) WPM() float64 {
	if s.Measured <= 0 {
		return 0
	}
	return float64(s.Words) / float64(s.Measured)
}

// BookStats is everything the history says about one book.
type BookStats struct {
	Words     int
	WordsRead int
	Minutes   int
	// Measured is the part of those minutes whose distance is known: minutes
	// spent between two positions that both resolve. Dividing words by the
	// whole figure instead counts time nobody can say a distance for, and every
	// speed comes out low.
	Measured   int
	Reports    int
	First      time.Time
	Last       time.Time
	Sessions   []ReadingSession
	Chapters   []ChapterPace
	Position   int
	Percent    float64
	Status     string
	Indexed    bool
	TotalWords int
	// Sittings is the true number, kept apart from Sessions because a page
	// shows only the last few.
	Sittings int
	// ActiveDays is how many separate days this book has been read on, which is
	// what a finishing date has to be projected from: reading an hour on the
	// two days a week you have one is not fourteen minutes a day.
	ActiveDays int
}

// Pace is minutes of this book a day, over the days it was actually opened.
func (b BookStats) Pace() float64 {
	if b.ActiveDays == 0 {
		return 0
	}
	return float64(b.Minutes) / float64(b.ActiveDays)
}

// WPM is the reader's pace in this book.
func (b BookStats) WPM() float64 {
	if b.Measured <= 0 {
		return 0
	}
	return float64(b.WordsRead) / float64(b.Measured)
}

// RemainingMinutes is how long the rest of the book takes at the pace measured
// here, rather than at the estimate the device carries — which barely moves on
// a long book and has been seen to rise over half an hour of reading.
func (b BookStats) RemainingMinutes() int {
	wpm := b.WPM()
	if wpm <= 0 || b.TotalWords <= 0 || b.Position >= b.TotalWords {
		return 0
	}
	return int(float64(b.TotalWords-b.Position) / wpm)
}

// Finish is when the book runs out at this pace and this much reading a day.
func (b BookStats) Finish(minutesPerDay float64) time.Time {
	rest := b.RemainingMinutes()
	if rest <= 0 || minutesPerDay <= 0 {
		return time.Time{}
	}
	days := math.Ceil(float64(rest) / minutesPerDay)
	return time.Now().AddDate(0, 0, int(days))
}

// ChapterPace is how a single content document went.
type ChapterPace struct {
	Seq     int
	Source  string
	Title   string
	Words   int
	Minutes int
	Read    int
}

func (c ChapterPace) WPM() float64 {
	if c.Minutes <= 0 {
		return 0
	}
	return float64(c.Read) / float64(c.Minutes)
}

type eventRow struct {
	at       time.Time
	deviceAt time.Time
	device   int64
	status   string
	source   string
	percent  sql.NullFloat64
	before   sql.NullInt64
	docStart sql.NullInt64
	delta    int
}

// bookEvents reads one book's history with each position already resolved to a
// word offset. The block is the paragraph the reader was on; a book with no
// index, or a position inside a document that has none, falls back to the
// start of the document and then to the percentage the device sent.
const bookEventsSQL = `
	SELECT e.at, e.device_at, COALESCE(e.device_id, 0), e.status, e.source,
	       e.percent, bl.before, d.before, e.spent_delta
	FROM reading_events e
	LEFT JOIN book_text_blocks bl
	       ON bl.book_id = e.book_id AND bl.source = e.source AND bl.block = e.block
	LEFT JOIN book_text_docs d
	       ON d.book_id = e.book_id AND d.source = e.source
	WHERE e.user_id = ? AND e.book_id = ?
	ORDER BY e.id`

// ReadingStatsForBook builds one book's statistics out of its history.
func ReadingStatsForBook(ctx context.Context, q Querier, userID int64, bookID string) (BookStats, error) {
	total, err := BookWords(ctx, q, bookID)
	if err != nil {
		return BookStats{}, err
	}

	rows, err := q.QueryContext(ctx, bookEventsSQL, userID, bookID)
	if err != nil {
		return BookStats{}, err
	}
	defer rows.Close()

	stats := BookStats{Words: total, TotalWords: total, Indexed: total > 0}
	perDevice := map[int64]int{}
	seenDevice := map[int64]bool{}
	chapters := map[string]*ChapterPace{}

	var session *ReadingSession
	for rows.Next() {
		var (
			e            eventRow
			at, deviceAt string
		)
		if err := rows.Scan(&at, &deviceAt, &e.device, &e.status, &e.source,
			&e.percent, &e.before, &e.docStart, &e.delta); err != nil {
			return BookStats{}, err
		}
		e.at, e.deviceAt = ParseTime(at), ParseTime(deviceAt)

		offset, known := resolveOffset(e, total)

		stats.Reports++
		stats.Status = e.status
		if stats.First.IsZero() {
			stats.First = e.at
		}
		stats.Last = e.at
		if known {
			stats.Position = offset
		}
		if e.percent.Valid {
			stats.Percent = clampPercent(e.percent.Float64)
		}

		words, measurable := 0, known && seenDevice[e.device]
		if measurable {
			if d := offset - perDevice[e.device]; d > 0 {
				words = d
			}
		}
		if known {
			perDevice[e.device], seenDevice[e.device] = offset, true
		}

		minutes := e.delta
		if minutes > 0 && words > 0 && float64(words)/float64(minutes) > maxSaneWPM {
			words, measurable = 0, false
		}

		measured := 0
		if measurable {
			measured = minutes
		}

		stats.Minutes += minutes
		stats.Measured += measured
		stats.WordsRead += words

		if session == nil || e.at.Sub(session.End) > sessionGap {
			stats.Sessions = append(stats.Sessions, ReadingSession{Start: e.at, End: e.at})
			session = &stats.Sessions[len(stats.Sessions)-1]
		}
		session.End = e.at
		session.Minutes += minutes
		session.Measured += measured
		session.Words += words
		session.Reports++

		if e.source != "" && (minutes > 0 || words > 0) {
			c := chapters[e.source]
			if c == nil {
				c = &ChapterPace{Source: e.source}
				chapters[e.source] = c
			}
			c.Minutes += minutes
			c.Read += words
		}
	}
	if err := rows.Err(); err != nil {
		return BookStats{}, err
	}

	seenDays := map[string]bool{}
	for _, s := range stats.Sessions {
		if s.Minutes > 0 {
			seenDays[s.Start.Local().Format("2006-01-02")] = true
		}
	}
	stats.ActiveDays = len(seenDays)
	stats.Sittings = len(stats.Sessions)

	stats.Chapters, err = namedChapters(ctx, q, bookID, chapters)
	return stats, err
}

// resolveOffset turns a reported position into a word offset, best evidence
// first: the paragraph, then the document it is in, then the whole-book
// percentage the device sent.
func resolveOffset(e eventRow, total int) (int, bool) {
	if e.before.Valid {
		return int(e.before.Int64), true
	}
	if e.docStart.Valid {
		return int(e.docStart.Int64), true
	}
	if total > 0 && e.percent.Valid {
		return int(e.percent.Float64 / 100 * float64(total)), true
	}
	return 0, false
}

func namedChapters(ctx context.Context, q Querier, bookID string, paces map[string]*ChapterPace) ([]ChapterPace, error) {
	if len(paces) == 0 {
		return nil, nil
	}

	rows, err := q.QueryContext(ctx,
		`SELECT source, seq, title, words FROM book_text_docs WHERE book_id = ?`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			source, title string
			seq, words    int
		)
		if err := rows.Scan(&source, &seq, &title, &words); err != nil {
			return nil, err
		}
		if c, ok := paces[source]; ok {
			c.Seq, c.Title, c.Words = seq, title, words
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ChapterPace, 0, len(paces))
	for _, c := range paces {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// Day is one calendar day of reading, in the server's own time zone: this is
// read by the person the server belongs to, and a heatmap in UTC would put an
// evening's reading on the wrong square for most of the world.
type Day struct {
	Date    time.Time
	Minutes int
	Words   int
}

// ReaderStats is one person's reading, across every book.
type ReaderStats struct {
	Minutes     int
	Measured    int
	Words       int
	Reports     int
	Sessions    int
	Books       int
	Finished    int
	Days        []Day
	ByHour      [24]int
	Streak      int
	LongestRun  int
	ActiveDays  int
	Since       time.Time
	Books7Days  int
	Minutes7    int
	TopBooks    []BookPace
	firstReport time.Time
}

// WPM is the reader's pace over everything they have read.
func (r ReaderStats) WPM() float64 {
	if r.Measured <= 0 {
		return 0
	}
	return float64(r.Words) / float64(r.Measured)
}

// MinutesPerDay averages over the days with any reading on them rather than
// over the calendar: a week off is not a week of reading nothing.
func (r ReaderStats) MinutesPerDay() float64 {
	if r.ActiveDays == 0 {
		return 0
	}
	return float64(r.Minutes) / float64(r.ActiveDays)
}

// BookPace is one book in the reader's own table.
type BookPace struct {
	BookID   string
	Title    string
	Authors  string
	Minutes  int
	Measured int
	Words    int
	Last     time.Time
	Status   string
	Percent  float64
}

func (b BookPace) WPM() float64 {
	if b.Measured <= 0 {
		return 0
	}
	return float64(b.Words) / float64(b.Measured)
}

// ReadingStatsForReader aggregates one person's whole history.
func ReadingStatsForReader(ctx context.Context, q Querier, userID int64, loc *time.Location) (ReaderStats, error) {
	if loc == nil {
		loc = time.Local
	}

	rows, err := q.QueryContext(ctx, `
		SELECT e.book_id, e.at, e.device_at, COALESCE(e.device_id, 0), e.status, e.source,
		       e.percent, bl.before, d.before, e.spent_delta,
		       COALESCE(ti.words, 0)
		FROM reading_events e
		LEFT JOIN book_text_blocks bl
		       ON bl.book_id = e.book_id AND bl.source = e.source AND bl.block = e.block
		LEFT JOIN book_text_docs d
		       ON d.book_id = e.book_id AND d.source = e.source
		LEFT JOIN book_text_index ti ON ti.book_id = e.book_id
		WHERE e.user_id = ?
		ORDER BY e.id`, userID)
	if err != nil {
		return ReaderStats{}, err
	}
	defer rows.Close()

	var stats ReaderStats
	days := map[string]*Day{}
	perBook := map[string]*BookPace{}
	lastPos := map[string]int{}
	seenPos := map[string]bool{}
	lastAt := map[string]time.Time{}

	for rows.Next() {
		var (
			e                eventRow
			bookID           string
			at, deviceAt     string
			totalWords       int
			percentOfTheBook sql.NullFloat64
		)
		if err := rows.Scan(&bookID, &at, &deviceAt, &e.device, &e.status, &e.source,
			&percentOfTheBook, &e.before, &e.docStart, &e.delta, &totalWords); err != nil {
			return ReaderStats{}, err
		}
		e.at, e.deviceAt, e.percent = ParseTime(at), ParseTime(deviceAt), percentOfTheBook

		key := bookID + ":" + strconv.FormatInt(e.device, 10)
		offset, known := resolveOffset(e, totalWords)

		words, measurable := 0, known && seenPos[key]
		if measurable {
			if d := offset - lastPos[key]; d > 0 {
				words = d
			}
		}
		if known {
			lastPos[key], seenPos[key] = offset, true
		}
		if e.delta > 0 && words > 0 && float64(words)/float64(e.delta) > maxSaneWPM {
			words, measurable = 0, false
		}

		stats.Reports++
		stats.Minutes += e.delta
		if measurable {
			stats.Measured += e.delta
		}
		stats.Words += words
		if stats.firstReport.IsZero() {
			stats.firstReport, stats.Since = e.at, e.at
		}

		when := e.at
		if !e.deviceAt.IsZero() {
			when = e.deviceAt
		}
		local := when.In(loc)
		if e.delta > 0 {
			stats.ByHour[local.Hour()] += e.delta
		}

		key2 := local.Format("2006-01-02")
		d := days[key2]
		if d == nil {
			d = &Day{Date: time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)}
			days[key2] = d
		}
		d.Minutes += e.delta
		d.Words += words

		b := perBook[bookID]
		if b == nil {
			b = &BookPace{BookID: bookID}
			perBook[bookID] = b
		}
		b.Minutes += e.delta
		if measurable {
			b.Measured += e.delta
		}
		b.Words += words
		b.Last = e.at
		b.Status = e.status
		if percentOfTheBook.Valid {
			b.Percent = clampPercent(percentOfTheBook.Float64)
		}

		if prev, ok := lastAt[bookID]; !ok || e.at.Sub(prev) > sessionGap {
			stats.Sessions++
		}
		lastAt[bookID] = e.at
	}
	if err := rows.Err(); err != nil {
		return ReaderStats{}, err
	}

	stats.Books = len(perBook)
	for _, b := range perBook {
		if b.Status == ReadFinished {
			stats.Finished++
		}
	}

	stats.Days = make([]Day, 0, len(days))
	for _, d := range days {
		stats.Days = append(stats.Days, *d)
	}
	sort.Slice(stats.Days, func(i, j int) bool { return stats.Days[i].Date.Before(stats.Days[j].Date) })

	cutoff := time.Now().In(loc).AddDate(0, 0, -7)
	for _, d := range stats.Days {
		if d.Minutes > 0 {
			stats.ActiveDays++
		}
		if d.Date.After(cutoff) {
			stats.Minutes7 += d.Minutes
		}
	}
	stats.Streak, stats.LongestRun = runs(stats.Days, time.Now().In(loc))

	stats.TopBooks = make([]BookPace, 0, len(perBook))
	for _, b := range perBook {
		stats.TopBooks = append(stats.TopBooks, *b)
	}
	sort.Slice(stats.TopBooks, func(i, j int) bool {
		return stats.TopBooks[i].Last.After(stats.TopBooks[j].Last)
	})

	return stats, titleBooks(ctx, q, stats.TopBooks)
}

// runs measures the current and longest streak of consecutive days with any
// reading on them. Today not being read yet does not break the streak — it is
// only broken once a whole day has passed with nothing in it.
// clampPercent keeps a device's own figure inside the range it claims to be
// in. A book that gained chapters can report more than a hundred.
func clampPercent(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

func firstAuthor(authorsJSON string) string {
	var authors []string
	if err := json.Unmarshal([]byte(authorsJSON), &authors); err != nil || len(authors) == 0 {
		return ""
	}
	return authors[0]
}

func runs(days []Day, now time.Time) (current, longest int) {
	active := make(map[string]bool, len(days))
	for _, d := range days {
		if d.Minutes > 0 {
			active[d.Date.Format("2006-01-02")] = true
		}
	}
	if len(active) == 0 {
		return 0, 0
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := today
	if !active[today.Format("2006-01-02")] {
		start = today.AddDate(0, 0, -1)
	}
	for active[start.Format("2006-01-02")] {
		current++
		start = start.AddDate(0, 0, -1)
	}

	run := 0
	var prev time.Time
	for _, d := range days {
		if d.Minutes == 0 {
			continue
		}
		if !prev.IsZero() && d.Date.Sub(prev) > 36*time.Hour {
			run = 0
		}
		run++
		if run > longest {
			longest = run
		}
		prev = d.Date
	}
	return current, longest
}

func titleBooks(ctx context.Context, q Querier, books []BookPace) error {
	for i := range books {
		book, err := GetBook(ctx, q, books[i].BookID)
		if err != nil {
			continue
		}
		books[i].Title = book.Title
		books[i].Authors = firstAuthor(book.AuthorsJSON)
	}
	return nil
}
