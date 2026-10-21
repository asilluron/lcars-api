package natsapi

import (
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/calendar"
)

// calendarGet answers lcars.calendar.get: the same data as GET /calendar.
func calendarGet(store *calendar.Store) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		if store == nil {
			return nil, &Error{Code: 503, Msg: "calendar is not configured: set calendar.dir in the config file"}
		}
		var in lcarsv1.GetCalendarRequest
		if err := proto.Unmarshal(req.Data(), &in); err != nil {
			return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.GetCalendarRequest"}
		}
		if in.GetDays() > calendar.MaxDays {
			return nil, &Error{Code: 400, Msg: calendar.ErrInvalidDays.Msg}
		}
		agenda, err := store.Agenda(in.GetFrom(), int(in.GetDays()), time.Now())
		if re, ok := errors.AsType[*calendar.RequestError](err); ok {
			return nil, &Error{Code: 400, Msg: re.Msg}
		} else if err != nil {
			return nil, err
		}
		return newCalendarResponse(agenda), nil
	}
}

func newCalendarResponse(a calendar.Agenda) *lcarsv1.GetCalendarResponse {
	resp := &lcarsv1.GetCalendarResponse{
		Timezone:      a.Location.String(),
		From:          a.From().Format(time.DateOnly),
		To:            a.To().Format(time.DateOnly),
		UpdatedAtUnix: a.UpdatedAt.Unix(),
		Days:          make([]*lcarsv1.CalendarDay, 0, len(a.Days)),
	}
	for _, d := range a.Days {
		day := &lcarsv1.CalendarDay{
			Date:    d.Date.Format(time.DateOnly),
			Weekday: d.Date.Weekday().String(),
			Today:   d.Today,
			Events:  make([]*lcarsv1.CalendarEvent, 0, len(d.Events)),
		}
		for _, ev := range d.Events {
			day.Events = append(day.Events, &lcarsv1.CalendarEvent{
				Id:        ev.ID,
				Title:     ev.Title,
				Calendar:  ev.Calendar,
				Location:  ev.Location,
				AllDay:    ev.AllDay,
				StartUnix: ev.Start.Unix(),
				EndUnix:   ev.End.Unix(),
				StartTime: nonEmpty(ev.StartTime),
				EndTime:   nonEmpty(ev.EndTime),
			})
		}
		resp.Days = append(resp.Days, day)
	}
	return resp
}

// nonEmpty returns a pointer to s, or nil (unset) if s is empty.
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CalendarChanged returns a function for calendar.Store.Run that publishes a
// CalendarUpdated event on lcars.calendar.updated.
func CalendarChanged(nc *nats.Conn, logger *slog.Logger) func(loadedAt time.Time) {
	return func(loadedAt time.Time) {
		data, err := proto.Marshal(&lcarsv1.CalendarUpdated{UpdatedAtUnix: loadedAt.Unix()})
		if err == nil {
			err = nc.Publish(SubjectCalendarUpdated, data)
		}
		if err != nil {
			logger.Error("publish calendar update", "subject", SubjectCalendarUpdated, "err", err)
			return
		}
		logger.Info("calendar update published", "subject", SubjectCalendarUpdated)
	}
}
