package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCurrent(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/forecast" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("latitude") != "0" || q.Get("longitude") != "0" ||
			q.Get("temperature_unit") != "fahrenheit" || q.Get("wind_speed_unit") != "mph" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		if r.Header.Get("User-Agent") != "lcars-api" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		_, _ = w.Write([]byte(`{
			"current": {"temperature_2m": 54.4, "apparent_temperature": 51.2, "weather_code": 2, "wind_speed_10m": 6.4},
			"daily": {"temperature_2m_max": [61.6], "temperature_2m_min": [37.5]}
		}`))
	}))
	t.Cleanup(upstream.Close)

	now := time.Date(2030, 3, 4, 12, 0, 0, 0, time.UTC)
	client := New(upstream.URL, 0, 0)
	client.now = func() time.Time { return now }

	got, err := client.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "Partly cloudy" || got.TemperatureF != 54 || got.FeelsLikeF != 51 ||
		got.HighF != 62 || got.LowF != 38 || got.WindMPH != 6 || !got.ObservedAt.Equal(now) {
		t.Fatalf("report = %+v", got)
	}
	if _, err := client.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 while the report is fresh", hits.Load())
	}
	now = now.Add(cacheFor + time.Second)
	if _, err := client.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 after the report expires", hits.Load())
	}
}

func TestCurrentUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	_, err := New(upstream.URL, 0, 0).Current(context.Background())
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusServiceUnavailable || se.Msg != Unavailable {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(se.Error(), "latitude") || strings.Contains(se.Error(), upstream.URL) {
		t.Fatalf("error includes the location or the forecast URL: %v", err)
	}
}

func TestSummary(t *testing.T) {
	tests := map[int]string{
		0: "Clear", 1: "Mainly clear", 3: "Overcast", 45: "Fog",
		51: "Drizzle", 63: "Rain", 73: "Snow", 95: "Thunderstorm", 999: "Outside",
	}
	for code, want := range tests {
		if got := summary(code); got != want {
			t.Errorf("summary(%d) = %q, want %q", code, got, want)
		}
	}
}
