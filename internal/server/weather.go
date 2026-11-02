package server

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/asilluron/lcars-api/internal/weather"
)

type weatherResponse struct {
	Summary      string    `json:"summary"`
	TemperatureF int32     `json:"temperature_f"`
	FeelsLikeF   int32     `json:"feels_like_f"`
	HighF        int32     `json:"high_f"`
	LowF         int32     `json:"low_f"`
	WindMPH      int32     `json:"wind_mph"`
	ObservedAt   time.Time `json:"observed_at"`
}

// weatherHandler serves the current conditions. The same report is served
// over NATS on lcars.weather.get.
func weatherHandler(logger *slog.Logger, src *weather.Client) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if src == nil {
			return &HTTPError{Code: http.StatusServiceUnavailable, Msg: weather.NotConfigured}
		}
		report, err := src.Current(r.Context())
		if err != nil {
			return weatherHTTPError(logger, err)
		}
		return writeJSON(w, http.StatusOK, weatherResponse{
			Summary:      report.Summary,
			TemperatureF: report.TemperatureF,
			FeelsLikeF:   report.FeelsLikeF,
			HighF:        report.HighF,
			LowF:         report.LowF,
			WindMPH:      report.WindMPH,
			ObservedAt:   report.ObservedAt,
		})
	}
}

func weatherHTTPError(logger *slog.Logger, err error) error {
	se, ok := errors.AsType[*weather.StatusError](err)
	if !ok {
		return err
	}
	if se.Err != nil {
		logger.Warn("weather forecast", "err", se.Err)
	}
	return &HTTPError{Code: se.Code, Msg: se.Msg}
}
