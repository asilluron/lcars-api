package natsapi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
	"github.com/asilluron/lcars-api/internal/weather"
)

// weatherGet answers lcars.weather.get with the same report as GET /weather.
func weatherGet(ctx context.Context, logger *slog.Logger, src *weather.Client) handlerFunc {
	return func(req micro.Request) (proto.Message, error) {
		var in lcarsv1.GetWeatherRequest
		if err := proto.Unmarshal(req.Data(), &in); err != nil {
			return nil, &Error{Code: 400, Msg: "invalid request: want a protobuf-encoded lcars.v1.GetWeatherRequest"}
		}
		if src == nil {
			return nil, &Error{Code: 503, Msg: weather.NotConfigured}
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		report, err := src.Current(ctx)
		if err != nil {
			return nil, weatherNATSError(logger, err)
		}
		return &lcarsv1.GetWeatherResponse{
			Summary:        report.Summary,
			TemperatureF:   report.TemperatureF,
			FeelsLikeF:     report.FeelsLikeF,
			HighF:          report.HighF,
			LowF:           report.LowF,
			WindMph:        report.WindMPH,
			ObservedAtUnix: report.ObservedAt.Unix(),
		}, nil
	}
}

func weatherNATSError(logger *slog.Logger, err error) error {
	se, ok := errors.AsType[*weather.StatusError](err)
	if !ok {
		return err
	}
	if se.Err != nil {
		logger.Warn("weather forecast", "err", se.Err)
	}
	return &Error{Code: se.Code, Msg: se.Msg}
}
