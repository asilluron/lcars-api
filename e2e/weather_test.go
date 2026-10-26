//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/micro"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
)

func TestWeather(t *testing.T) {
	p := startServer(t)
	resp := mustDo(t, http.MethodGet, p.url("/weather"))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(resp.body), "weather.latitude") {
		t.Fatalf("unconfigured GET /weather = %d %s", resp.StatusCode, resp.body)
	}
	nc := connectNATS(t, p.natsURL())
	msg := natsRequest(t, nc, subjectWeatherGet, &lcarsv1.GetWeatherRequest{})
	if code := msg.Header.Get(micro.ErrorCodeHeader); code != "503" {
		t.Fatalf("unconfigured NATS = %s %s", code, msg.Header.Get(micro.ErrorHeader))
	}

	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("latitude") != "0" || r.URL.Query().Get("longitude") != "0" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{
			"current": {"temperature_2m": 72.2, "apparent_temperature": 70.4, "weather_code": 0, "wind_speed_10m": 4.2},
			"daily": {"temperature_2m_max": [78.6], "temperature_2m_min": [55.2]}
		}`)
	}))
	t.Cleanup(upstream.Close)
	live := startServer(t,
		"LCARS_WEATHER_LATITUDE=0",
		"LCARS_WEATHER_LONGITUDE=0",
		"LCARS_WEATHER_URL="+upstream.URL,
	)
	resp = mustDo(t, http.MethodGet, live.url("/weather"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /weather = %d %s", resp.StatusCode, resp.body)
	}
	var body struct {
		Summary      string `json:"summary"`
		TemperatureF int32  `json:"temperature_f"`
		FeelsLikeF   int32  `json:"feels_like_f"`
		HighF        int32  `json:"high_f"`
		LowF         int32  `json:"low_f"`
		WindMPH      int32  `json:"wind_mph"`
	}
	if err := json.Unmarshal(resp.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Summary != "Clear" || body.TemperatureF != 72 || body.FeelsLikeF != 70 ||
		body.HighF != 79 || body.LowF != 55 || body.WindMPH != 4 {
		t.Fatalf("report = %+v", body)
	}
	if strings.Contains(string(resp.body), `"latitude"`) || strings.Contains(live.stdout.String()+live.stderr.String(), "latitude") {
		t.Fatal("weather response or log includes the location")
	}

	nc = connectNATS(t, live.natsURL())
	msg = natsRequest(t, nc, subjectWeatherGet, &lcarsv1.GetWeatherRequest{})
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}
	var got lcarsv1.GetWeatherResponse
	if err := proto.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.GetSummary() != body.Summary || got.GetTemperatureF() != body.TemperatureF ||
		got.GetHighF() != body.HighF || got.GetWindMph() != body.WindMPH {
		t.Fatalf("NATS report = %+v", &got)
	}
	if hits.Load() != 1 {
		t.Fatalf("forecast requests = %d, want 1 shared by HTTP and NATS", hits.Load())
	}
	raw, err := nc.Request(subjectWeatherGet, []byte{0xff, 0xff, 0xff, 0xff, 0xff}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Header.Get(micro.ErrorCodeHeader) != "400" {
		t.Errorf("bad protobuf = %s, want 400", raw.Header.Get(micro.ErrorCodeHeader))
	}
}
