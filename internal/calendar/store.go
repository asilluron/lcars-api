// Package calendar reads iCalendar (.ics) files from a directory and lists the
// events in a time range, expanding recurring events.
package calendar

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one occurrence of a calendar event.
type Event struct {
	// ID identifies this occurrence: the event's UID plus its start time, so
	// each occurrence of a recurring event has its own ID.
	ID       string
	Title    string
	Location string
	// Calendar is the name of the calendar the event belongs to.
	Calendar string
	AllDay   bool
	// Start and End are in the Store's location. End is exclusive: an all-day
	// event on 1 May ends at midnight starting 2 May.
	Start time.Time
	End   time.Time
}

// Store holds the events parsed from a directory of .ics files. It is safe for
// concurrent use.
type Store struct {
	dir    string
	loc    *time.Location
	logger *slog.Logger
	snap   atomic.Pointer[snapshot]
}

type snapshot struct {
	loadedAt time.Time
	// mu serializes expansion: rrule.Set sorts its date lists in place on every
	// query, so two queries can't run on the same set at once.
	mu     sync.Mutex
	series []*series
}

// NewStore returns a Store for the .ics files under dir. Times without a
// timezone, including all-day dates, are interpreted in loc, and events are
// returned in loc. Call Load before using it.
func NewStore(dir string, loc *time.Location, logger *slog.Logger) *Store {
	s := &Store{dir: dir, loc: loc, logger: logger}
	s.snap.Store(&snapshot{})
	return s
}

// Location returns the timezone events are reported in.
func (s *Store) Location() *time.Location { return s.loc }

// Load re-reads every .ics file under the directory, including in
// subdirectories, replacing the events served. Hidden files and directories
// are skipped, as are symlinks to directories.
//
// A file that can't be parsed is logged and skipped, so one bad file doesn't
// hide the rest. Load fails only if the directory itself can't be read; the
// previously loaded events are then kept.
func (s *Store) Load() error {
	began := time.Now()
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return fmt.Errorf("open calendar directory: %w", err)
	}
	defer root.Close()

	var (
		all      []*series
		replaced = map[string][]time.Time{}
		files    int
		skipped  int
	)
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			s.logger.Warn("calendar: skipping unreadable path", "path", p, "err", err)
			return nil
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.EqualFold(path.Ext(p), ".ics") {
			return nil
		}
		files++
		parsed, err := s.loadFile(root, p)
		if err != nil {
			skipped++
			s.logger.Warn("calendar: skipping file", "path", p, "err", err)
			return nil
		}
		all = append(all, parsed.series...)
		for uid, ts := range parsed.replaced {
			replaced[uid] = append(replaced[uid], ts...)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read calendar directory: %w", err)
	}

	// Drop the occurrences of recurring events that a RECURRENCE-ID override
	// replaces; the override itself is a series of its own.
	for _, sr := range all {
		if sr.recur != nil {
			for _, t := range replaced[sr.uid] {
				sr.recur.ExDate(t)
			}
		}
	}

	s.snap.Store(&snapshot{loadedAt: time.Now(), series: all})
	s.logger.Info("calendar loaded",
		"dir", s.dir, "files", files, "skipped", skipped, "events", len(all),
		"duration", time.Since(began))
	return nil
}

// loadFile parses one .ics file. Its events belong to the calendar named by
// X-WR-CALNAME if set, otherwise by the top-level subdirectory the file is in,
// otherwise by the file name without its extension.
func (s *Store) loadFile(root *os.Root, p string) (*parsed, error) {
	f, err := root.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	name, _, nested := strings.Cut(p, "/")
	if !nested {
		name = strings.TrimSuffix(name, path.Ext(name))
	}
	return parse(f, name, s.loc)
}

// Run reloads the directory every interval until ctx is done. A failed reload
// is logged, and the previous events are kept.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Load(); err != nil {
				s.logger.ErrorContext(ctx, "calendar refresh failed; serving previous events", "err", err)
			}
		}
	}
}

// Events returns the occurrences that overlap [from, to), sorted by start
// time with all-day events first, and when the events were last loaded.
func (s *Store) Events(from, to time.Time) (events []Event, loadedAt time.Time) {
	snap := s.snap.Load()
	snap.mu.Lock()
	defer snap.mu.Unlock()

	for _, sr := range snap.series {
		if sr.recur == nil {
			if ev := s.occurrence(sr, sr.start); ev.Overlaps(from, to) {
				events = append(events, ev)
			}
			continue
		}
		for _, start := range sr.recur.Between(from.Add(-sr.maxLen()), to, true) {
			if ev := s.occurrence(sr, start); ev.Overlaps(from, to) {
				events = append(events, ev)
			}
		}
	}

	slices.SortFunc(events, func(a, b Event) int {
		return cmp.Or(
			a.Start.Compare(b.Start),
			compareBool(b.AllDay, a.AllDay), // all-day first
			strings.Compare(a.Title, b.Title),
			strings.Compare(a.ID, b.ID),
		)
	})
	return events, snap.loadedAt
}

func (s *Store) occurrence(sr *series, start time.Time) Event {
	id := sr.uid
	if id == "" {
		id = sr.calendar + "/" + sr.title
	}
	return Event{
		ID:       id + "/" + start.UTC().Format("20060102T150405Z"),
		Title:    sr.title,
		Location: sr.location,
		Calendar: sr.calendar,
		AllDay:   sr.allDay,
		Start:    start.In(s.loc),
		End:      sr.end(start).In(s.loc),
	}
}

// Overlaps reports whether ev overlaps [from, to). An event with no duration
// overlaps if it starts in the range.
func (ev Event) Overlaps(from, to time.Time) bool {
	if !ev.Start.Before(to) {
		return false
	}
	if ev.Start.Equal(ev.End) {
		return !ev.Start.Before(from)
	}
	return ev.End.After(from)
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}
