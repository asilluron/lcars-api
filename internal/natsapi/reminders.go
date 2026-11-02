package natsapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/reminders"
)

const remindersNotConfigured = reminders.NotConfigured

// remindersList answers lcars.reminders.list: the same data as GET /reminders.
func remindersList(ctx context.Context, store *reminders.Store) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		if store == nil {
			return nil, &Error{Code: 503, Msg: remindersNotConfigured}
		}
		if len(req.Data()) > 0 {
			var in lcarsv1.ListRemindersRequest
			if err := proto.Unmarshal(req.Data(), &in); err != nil {
				return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.ListRemindersRequest"}
			}
		}
		items, updated, err := store.List(ctx)
		if err != nil {
			return nil, err
		}
		return newRemindersResponse(items, updated), nil
	}
}

// remindersSet answers lcars.reminders.set. The id selects a reminder; it
// is never used as a path.
func remindersSet(ctx context.Context, store *reminders.Store) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		if store == nil {
			return nil, &Error{Code: 503, Msg: remindersNotConfigured}
		}
		var in lcarsv1.SetReminderRequest
		if err := proto.Unmarshal(req.Data(), &in); err != nil {
			return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.SetReminderRequest"}
		}
		if err := store.SetDone(ctx, in.GetId(), in.GetDone()); err != nil {
			return nil, reminderError(err)
		}
		items, updated, err := store.List(ctx)
		if err != nil {
			return nil, err
		}
		return newRemindersResponse(items, updated), nil
	}
}

func reminderError(err error) error {
	var re *reminders.RequestError
	switch {
	case errors.As(err, &re):
		return &Error{Code: 400, Msg: re.Msg}
	case errors.Is(err, reminders.ErrNotFound):
		return &Error{Code: 404, Msg: reminders.ErrNotFound.Error()}
	default:
		return err
	}
}

func newRemindersResponse(items []reminders.Reminder, updated time.Time) *lcarsv1.ListRemindersResponse {
	resp := &lcarsv1.ListRemindersResponse{
		UpdatedAtUnix: updated.Unix(),
		Reminders:     make([]*lcarsv1.Reminder, 0, len(items)),
	}
	for _, item := range items {
		resp.Reminders = append(resp.Reminders, &lcarsv1.Reminder{
			Id:     item.ID,
			Title:  item.Title,
			Detail: item.Detail,
			Accent: item.Accent,
			Done:   item.Done,
		})
	}
	return resp
}

// RemindersChanged returns a function for Store.SetOnChange that publishes
// a RemindersUpdated event on lcars.reminders.updated. It waits until NATS
// has accepted the publish, so a handler that then replies does not lose
// the event.
func RemindersChanged(nc *nats.Conn, logger *slog.Logger) func(time.Time) {
	return func(updated time.Time) {
		data, err := proto.Marshal(&lcarsv1.RemindersUpdated{UpdatedAtUnix: updated.Unix()})
		if err == nil {
			err = nc.Publish(SubjectRemindersUpdated, data)
		}
		if err == nil {
			err = nc.Flush()
		}
		if err != nil {
			logger.Error("publish reminders update", "subject", SubjectRemindersUpdated, "err", err)
			return
		}
		logger.Info("reminders update published", "subject", SubjectRemindersUpdated)
	}
}
