package calendar

import (
	"cmp"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// gmailLabel is the source name for a Google or Gmail calendar whose mailbox
// can't be read (a group calendar, or a file that only says it came from Google).
const gmailLabel = "Gmail"

// MaxLabelLen is the longest display name an admin can set, in characters.
// The panel shows the first 8 of them.
const MaxLabelLen = 40

// identity chooses the stable id, the default display name and the title for
// one parsed file. fallback is the name from the file's path (its top-level
// directory, or its file name). filePath is the path relative to the calendar
// directory, with slashes.
//
// A Google Calendar export names itself with X-WR-CALNAME, which is often a
// long title ("Ada Google Calendar") that the panel cuts down to something
// like "ADA GOOG". The link on each event carries the calendar's mailbox, so
// that mailbox (or "Gmail", when there isn't a personal one) is the name
// instead. X-WR-CALNAME is still the title, so an admin can see which calendar
// a file is.
func identity(p *parsed, fallback, filePath string) (id, label, title string) {
	title = p.calName
	if title == "" {
		title = fallback
	}
	if id, label, ok := sourceFromURLs(p.urls); ok {
		return id, label, title
	}
	if p.google {
		return "file:" + filePath, gmailLabel, title
	}
	return "file:" + filePath, title, title
}

// sourceFromURLs returns the mailbox shared by the most events, as
// "mail:" plus the lowercased address, and a short label: the part before
// "@" when it looks like a person's mailbox, or "Gmail" for a Google group
// or imported calendar.
func sourceFromURLs(urls []string) (id, label string, ok bool) {
	counts := map[string]int{}
	var order []string
	for _, raw := range urls {
		calID, found := googleCalendarID(raw)
		if !found {
			continue
		}
		if counts[calID] == 0 {
			order = append(order, calID)
		}
		counts[calID]++
	}
	slices.SortStableFunc(order, func(a, b string) int {
		return cmp.Or(cmp.Compare(counts[b], counts[a]), strings.Compare(strings.ToLower(a), strings.ToLower(b)))
	})
	for _, calID := range order {
		label, ok := labelForCalendarID(calID)
		if ok {
			return "mail:" + strings.ToLower(calID), label, true
		}
	}
	return "", "", false
}

// googleCalendarID reads the calendar id from a Google Calendar event link.
// The eid query parameter is the base64 of "<event id> <calendar id>", and the
// calendar id is usually an email address.
func googleCalendarID(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host != "google.com" && host != "calendar.google.com" {
		return "", false
	}
	if !strings.Contains(strings.ToLower(u.Path), "calendar") {
		return "", false
	}
	eid := queryParam(u.RawQuery, "eid")
	if eid == "" {
		return "", false
	}
	_, calID, ok := decodeEID(eid)
	return calID, ok
}

// queryParam reads one query parameter without turning '+' into a space.
// Query().Get does that, and '+' is a base64 character.
func queryParam(rawQuery, key string) string {
	for part := range strings.SplitSeq(rawQuery, "&") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || k != key {
			continue
		}
		decoded, err := url.QueryUnescape(strings.ReplaceAll(v, "+", "%2B"))
		if err != nil {
			return ""
		}
		return decoded
	}
	return ""
}

// decodeEID decodes a Google event id into the event id and the calendar id.
func decodeEID(eid string) (eventID, calID string, ok bool) {
	eid = strings.TrimSpace(eid)
	candidates := []string{eid}
	if pad := len(eid) % 4; pad != 0 {
		candidates = append(candidates, eid+strings.Repeat("=", 4-pad))
	}
	encodings := []*base64.Encoding{
		base64.RawStdEncoding, base64.StdEncoding,
		base64.RawURLEncoding, base64.URLEncoding,
	}
	for _, candidate := range candidates {
		for _, enc := range encodings {
			b, err := enc.DecodeString(candidate)
			if err != nil || len(b) == 0 || !utf8.Valid(b) {
				continue
			}
			eventID, calID, ok = strings.Cut(string(b), " ")
			calID = strings.TrimSpace(calID)
			if ok && eventID != "" && calID != "" {
				return eventID, calID, true
			}
		}
	}
	return "", "", false
}

// labelForCalendarID turns a Google calendar id into the short name shown
// when the admin hasn't set one.
func labelForCalendarID(id string) (string, bool) {
	local, domain, ok := strings.Cut(id, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", false
	}
	switch strings.ToLower(domain) {
	case "group.calendar.google.com", "import.calendar.google.com":
		return gmailLabel, true
	}
	if mailboxLocal(local) {
		return local, true
	}
	switch strings.ToLower(domain) {
	case "gmail.com", "googlemail.com":
		return gmailLabel, true
	}
	return "", false
}

// mailboxLocal reports whether local looks like a person's mailbox rather
// than a generated calendar id.
func mailboxLocal(local string) bool {
	if local == "" || utf8.RuneCountInString(local) > 32 {
		return false
	}
	letter := false
	for i, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letter = true
		case r >= '0' && r <= '9', r == '_', r == '-', r == '+':
		case r == '.' && i > 0 && i < len(local)-1:
		default:
			return false
		}
	}
	return letter
}

// KnownCategory reports whether category is a lane the panel can filter on.
func KnownCategory(category string) bool {
	switch category {
	case "work", "band", "personal", "house":
		return true
	default:
		return false
	}
}
