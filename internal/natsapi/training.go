package natsapi

import (
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/training"
)

// trainingPut answers lcars.training.put by appending the chunk to the
// hourly WAV file in the configured directory.
func trainingPut(w *training.Writer, logger *slog.Logger) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		if w == nil {
			return nil, &Error{Code: 503, Msg: "training is not configured: set training.dir in the config file"}
		}
		var in lcarsv1.PutTrainingAudioRequest
		if err := proto.Unmarshal(req.Data(), &in); err != nil {
			return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.PutTrainingAudioRequest"}
		}
		pcm := in.GetPcm()
		switch {
		case len(pcm) == 0:
			return nil, &Error{Code: 400, Msg: "invalid request: pcm is empty"}
		case len(pcm)%2 != 0:
			return nil, &Error{Code: 400, Msg: "invalid request: pcm must be a whole number of 16-bit samples"}
		case len(pcm) > training.MaxPCM:
			return nil, &Error{Code: 400, Msg: "invalid request: pcm is larger than 256 KiB"}
		}
		n, err := w.Write(in.GetScreenId(), pcm, time.Now())
		if err != nil {
			logger.Error("store training audio", "err", err)
			return nil, &Error{Code: 500, Msg: "Internal Server Error"}
		}
		return &lcarsv1.PutTrainingAudioResponse{AcceptedBytes: uint32(n)}, nil
	}
}
