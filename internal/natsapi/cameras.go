package natsapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/cameras"
)

// camerasList answers lcars.cameras.list with the same catalog as GET /cameras.
// Video is not sent on NATS; each camera carries the HTTP paths to open.
func camerasList(ctx context.Context, logger *slog.Logger, src *cameras.Client) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		var in lcarsv1.ListCamerasRequest
		if err := proto.Unmarshal(req.Data(), &in); err != nil {
			return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.ListCamerasRequest"}
		}
		if src == nil {
			return nil, &Error{Code: 503, Msg: cameras.NotConfigured}
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		list, err := src.List(ctx)
		if err != nil {
			return nil, cameraNATSError(logger, err)
		}
		out := &lcarsv1.ListCamerasResponse{Cameras: make([]*lcarsv1.Camera, 0, len(list))}
		for _, cam := range list {
			out.Cameras = append(out.Cameras, &lcarsv1.Camera{
				Sn:           cam.SN,
				Name:         cam.Name,
				HasCamera:    cam.HasCamera,
				StreamPath:   cam.StreamPath,
				SnapshotPath: cam.SnapshotPath,
			})
		}
		return out, nil
	}
}

func cameraNATSError(logger *slog.Logger, err error) error {
	se, ok := errors.AsType[*cameras.StatusError](err)
	if !ok {
		return err
	}
	if se.Err != nil {
		logger.Warn("camera proxy", "err", se.Err)
	}
	return &Error{Code: se.Code, Msg: se.Msg}
}
