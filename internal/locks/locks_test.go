package locks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testID = "11111111-1111-1111-1111-111111111111"

func TestList(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/locks/list" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{
			"ok": true,
			"devices": [{
				"device_id": "`+testID+`",
				"display_name": "Front door",
				"can_remotely_lock": true,
				"can_remotely_unlock": true,
				"properties": {"locked": true, "online": true}
			}]
		}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "seam_test_key")
	list, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer seam_test_key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if len(list) != 1 || list[0].ID != testID || list[0].Name != "Front door" ||
		!list[0].Locked || !list[0].Online || !list[0].CanLock || !list[0].CanUnlock {
		t.Fatalf("list = %+v", list)
	}
}

func TestLockDoorWaitsForSuccess(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/locks/lock_door":
			if !strings.Contains(string(body), testID) {
				t.Errorf("body = %s", body)
			}
			_, _ = io.WriteString(w, `{
				"ok": true,
				"action_attempt": {
					"action_attempt_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"action_type": "LOCK_DOOR",
					"status": "pending"
				}
			}`)
		case "/action_attempts/get":
			n := polls.Add(1)
			status := "pending"
			if n >= 2 {
				status = "success"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"action_attempt": map[string]any{
					"action_attempt_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"action_type":       "LOCK_DOOR",
					"status":            status,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	c.sleep = func(context.Context, time.Duration) error { return nil }
	if err := c.LockDoor(context.Background(), testID); err != nil {
		t.Fatal(err)
	}
	if polls.Load() < 2 {
		t.Fatalf("polls = %d, want at least 2", polls.Load())
	}
}

func TestUnlockDoorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/locks/unlock_door":
			_, _ = io.WriteString(w, `{
				"ok": true,
				"action_attempt": {
					"action_attempt_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"status": "error",
					"error": {"type": "device_offline", "message": "offline"}
				}
			}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	err := c.UnlockDoor(context.Background(), testID)
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadGateway || se.Msg != "lock action failed" {
		t.Fatalf("err = %v", err)
	}
	if se.Err == nil || !strings.Contains(se.Err.Error(), "offline") {
		t.Errorf("cause = %v", se.Err)
	}
}

func TestInvalidID(t *testing.T) {
	c := New("http://127.0.0.1:9", "key")
	err := c.LockDoor(context.Background(), "not-a-uuid")
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Fatalf("err = %v", err)
	}
}

func TestSeamNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"type":"not_found","message":"missing"}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	err := c.UnlockDoor(context.Background(), testID)
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotFound {
		t.Fatalf("err = %v", err)
	}
}
