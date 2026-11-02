// Package weather reads current conditions from a forecast service.
//
// The location comes from the operator's config. It is sent to the forecast
// service and is not logged or returned to devices.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// NotConfigured is the message a device gets when no location is set.
	// HTTP and NATS both use it.
	NotConfigured = "weather is not configured: set weather.latitude and weather.longitude in the config file"
	// Unavailable is the message a device gets when the forecast cannot be read.
	Unavailable = "weather is unavailable"
	// DefaultURL is the forecast service used when weather.url is unset.
	DefaultURL = "https://api.open-meteo.com"

	cacheFor     = 10 * time.Minute
	maxBodyBytes = 64 << 10
)

// StatusError is a failure with a status and a message safe to send to a device.
// Err, when set, is for the server log only and must not include the location.
type StatusError struct {
	Code int
	Msg  string
	Err  error
}

func (e *StatusError) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *StatusError) Unwrap() error { return e.Err }

// Report is the current conditions, in Fahrenheit and miles per hour.
type Report struct {
	Summary      string
	TemperatureF int32
	FeelsLikeF   int32
	HighF        int32
	LowF         int32
	WindMPH      int32
	ObservedAt   time.Time
}

// Client reads one forecast service and keeps the last report for cacheFor.
type Client struct {
	base string
	lat  string
	lon  string
	http *http.Client
	now  func() time.Time

	mu         sync.Mutex
	report     Report
	freshUntil time.Time
}

// New returns a client for the forecast service at base, such as
// "https://api.open-meteo.com". lat and lon are decimal degrees.
func New(base string, lat, lon float64) *Client {
	return &Client{
		base: strings.TrimSuffix(base, "/"),
		lat:  strconv.FormatFloat(lat, 'f', -1, 64),
		lon:  strconv.FormatFloat(lon, 'f', -1, 64),
		http: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
}

// Current returns the cached report, or reads a new one when the cache is empty
// or older than ten minutes. A failed read is not cached.
func (c *Client) Current(ctx context.Context) (Report, error) {
	c.mu.Lock()
	if !c.freshUntil.IsZero() && c.now().Before(c.freshUntil) {
		report := c.report
		c.mu.Unlock()
		return report, nil
	}
	c.mu.Unlock()

	report, err := c.fetch(ctx)
	if err != nil {
		return Report{}, err
	}
	c.mu.Lock()
	c.report = report
	c.freshUntil = c.now().Add(cacheFor)
	c.mu.Unlock()
	return report, nil
}

func (c *Client) fetch(ctx context.Context) (Report, error) {
	endpoint, err := url.Parse(c.base + "/v1/forecast")
	if err != nil {
		return Report{}, &StatusError{Code: http.StatusServiceUnavailable, Msg: Unavailable, Err: errors.New("forecast address is invalid")}
	}
	q := endpoint.Query()
	q.Set("latitude", c.lat)
	q.Set("longitude", c.lon)
	q.Set("current", "temperature_2m,apparent_temperature,weather_code,wind_speed_10m")
	q.Set("daily", "temperature_2m_max,temperature_2m_min")
	q.Set("temperature_unit", "fahrenheit")
	q.Set("wind_speed_unit", "mph")
	q.Set("forecast_days", "1")
	q.Set("timezone", "auto")
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Report{}, &StatusError{Code: http.StatusServiceUnavailable, Msg: Unavailable, Err: errors.New("forecast request could not be built")}
	}
	req.Header.Set("User-Agent", "lcars-api")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// The client error includes the request URL, and that URL contains
		// the location, so it stays out of the log.
		return Report{}, &StatusError{Code: http.StatusServiceUnavailable, Msg: Unavailable, Err: errors.New("forecast request failed")}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return Report{}, &StatusError{Code: http.StatusServiceUnavailable, Msg: Unavailable, Err: errors.New("forecast response could not be read")}
	}
	if resp.StatusCode != http.StatusOK {
		return Report{}, &StatusError{Code: http.StatusServiceUnavailable, Msg: Unavailable, Err: fmt.Errorf("forecast status %d", resp.StatusCode)}
	}
	return decodeForecast(body, c.now())
}

type forecastBody struct {
	Current struct {
		Temperature *float64 `json:"temperature_2m"`
		Apparent    *float64 `json:"apparent_temperature"`
		Code        *float64 `json:"weather_code"`
		Wind        *float64 `json:"wind_speed_10m"`
	} `json:"current"`
	Daily struct {
		Max []float64 `json:"temperature_2m_max"`
		Min []float64 `json:"temperature_2m_min"`
	} `json:"daily"`
}

func decodeForecast(body []byte, observed time.Time) (Report, error) {
	var raw forecastBody
	if err := json.Unmarshal(body, &raw); err != nil {
		return Report{}, &StatusError{Code: http.StatusServiceUnavailable, Msg: Unavailable, Err: errors.New("forecast response was not valid JSON")}
	}
	if raw.Current.Temperature == nil || raw.Current.Apparent == nil || raw.Current.Wind == nil ||
		len(raw.Daily.Max) == 0 || len(raw.Daily.Min) == 0 {
		return Report{}, &StatusError{Code: http.StatusServiceUnavailable, Msg: Unavailable, Err: errors.New("forecast response was missing conditions")}
	}
	code := 0
	if raw.Current.Code != nil {
		code = int(*raw.Current.Code)
	}
	return Report{
		Summary:      summary(code),
		TemperatureF: round(*raw.Current.Temperature),
		FeelsLikeF:   round(*raw.Current.Apparent),
		HighF:        round(raw.Daily.Max[0]),
		LowF:         round(raw.Daily.Min[0]),
		WindMPH:      round(*raw.Current.Wind),
		ObservedAt:   observed,
	}, nil
}

func round(v float64) int32 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int32(math.Round(v))
}

// summary is a short label for a WMO weather interpretation code.
func summary(code int) string {
	switch code {
	case 0:
		return "Clear"
	case 1:
		return "Mainly clear"
	case 2:
		return "Partly cloudy"
	case 3:
		return "Overcast"
	case 45, 48:
		return "Fog"
	case 51, 53, 55, 56, 57:
		return "Drizzle"
	case 61, 63, 65, 66, 67, 80, 81, 82:
		return "Rain"
	case 71, 73, 75, 77, 85, 86:
		return "Snow"
	case 95, 96, 99:
		return "Thunderstorm"
	default:
		return "Outside"
	}
}
