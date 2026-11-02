package natsapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/locks"
)

// locksList answers lcars.locks.list with the same catalog as GET /locks.
func locksList(ctx context.Context, logger *slog.Logger, src *locks.Client) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		var in lcarsv1.ListLocksRequest
		if err := proto.Unmarshal(req.Data(), &in); err != nil {
			return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.ListLocksRequest"}
		}
		if src == nil {
			return nil, &Error{Code: 503, Msg: locks.NotConfigured}
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		list, err := src.List(ctx)
		if err != nil {
			return nil, lockNATSError(logger, err)
		}
		out := &lcarsv1.ListLocksResponse{Locks: make([]*lcarsv1.Lock, 0, len(list))}
		for _, lock := range list {
			out.Locks = append(out.Locks, &lcarsv1.Lock{
				Id:        lock.ID,
				Name:      lock.Name,
				Locked:    lock.Locked,
				Online:    lock.Online,
				CanLock:   lock.CanLock,
				CanUnlock: lock.CanUnlock,
			})
		}
		return out, nil
	}
}

// locksLock answers lcars.locks.lock by asking Seam to lock the door once.
func locksLock(ctx context.Context, logger *slog.Logger, src *locks.Client) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		var in lcarsv1.LockDoorRequest
		if err := proto.Unmarshal(req.Data(), &in); err != nil {
			return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.LockDoorRequest"}
		}
		if src == nil {
			return nil, &Error{Code: 503, Msg: locks.NotConfigured}
		}
		if err := src.LockDoor(ctx, in.GetId()); err != nil {
			return nil, lockNATSError(logger, err)
		}
		return &lcarsv1.LockDoorResponse{Id: in.GetId(), Locked: true}, nil
	}
}

// locksUnlock answers lcars.locks.unlock by asking Seam to unlock the door once.
func locksUnlock(ctx context.Context, logger *slog.Logger, src *locks.Client) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		var in lcarsv1.UnlockDoorRequest
		if err := proto.Unmarshal(req.Data(), &in); err != nil {
			return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.UnlockDoorRequest"}
		}
		if src == nil {
			return nil, &Error{Code: 503, Msg: locks.NotConfigured}
		}
		if err := src.UnlockDoor(ctx, in.GetId()); err != nil {
			return nil, lockNATSError(logger, err)
		}
		return &lcarsv1.UnlockDoorResponse{Id: in.GetId(), Locked: false}, nil
	}
}

func lockNATSError(logger *slog.Logger, err error) error {
	se, ok := errors.AsType[*locks.StatusError](err)
	if !ok {
		return err
	}
	if se.Err != nil {
		logger.Warn("seam", "err", se.Err)
	}
	return &Error{Code: se.Code, Msg: se.Msg}
}
