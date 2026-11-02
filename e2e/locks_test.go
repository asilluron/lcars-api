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

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	lcarsv1 "github.com/asilluron/lcars-api/gen/go/lcars/v1"
)

const (
	subjectLocksList   = "lcars.locks.list"
	subjectLocksLock   = "lcars.locks.lock"
	subjectLocksUnlock = "lcars.locks.unlock"
	testLockID         = "11111111-1111-1111-1111-111111111111"
)

func TestLocks(t *testing.T) {
	var locked atomic.Bool
	locked.Store(true)
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/locks/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"devices": []map[string]any{{
					"device_id":           testLockID,
					"display_name":        "Front door",
					"can_remotely_lock":   true,
					"can_remotely_unlock": true,
					"properties": map[string]any{
						"locked": locked.Load(),
						"online": true,
					},
				}},
			})
		case "/locks/lock_door":
			if !strings.Contains(string(body), testLockID) {
				t.Errorf("lock_door body = %s", body)
			}
			locked.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"action_attempt": map[string]any{
					"action_attempt_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"action_type":       "LOCK_DOOR",
					"status":            "success",
				},
			})
		case "/locks/unlock_door":
			if !strings.Contains(string(body), testLockID) {
				t.Errorf("unlock_door body = %s", body)
			}
			locked.Store(false)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"action_attempt": map[string]any{
					"action_attempt_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"action_type":       "UNLOCK_DOOR",
					"status":            "success",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	p := startServer(t,
		"LCARS_SEAM_API_KEY=seam_test_key",
		"LCARS_SEAM_URL="+upstream.URL,
	)
	if !p.hasLog(map[string]any{"msg": "locks enabled"}) {
		t.Errorf("missing locks enabled log\nstdout: %s", p.stdout.String())
	}

	resp := mustDo(t, http.MethodGet, p.url("/locks"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /locks = %d %s", resp.StatusCode, resp.body)
	}
	var list struct {
		Locks []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Locked    bool   `json:"locked"`
			Online    bool   `json:"online"`
			CanLock   bool   `json:"can_lock"`
			CanUnlock bool   `json:"can_unlock"`
		} `json:"locks"`
	}
	if err := json.Unmarshal(resp.body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Locks) != 1 || list.Locks[0].ID != testLockID || list.Locks[0].Name != "Front door" ||
		!list.Locks[0].Locked || !list.Locks[0].Online || !list.Locks[0].CanLock || !list.Locks[0].CanUnlock {
		t.Fatalf("catalog = %+v", list.Locks)
	}
	if gotAuth != "Bearer seam_test_key" {
		t.Errorf("Seam Authorization = %q", gotAuth)
	}

	nc := connectNATS(t, p.natsURL())
	msg := natsRequest(t, nc, subjectLocksList, &lcarsv1.ListLocksRequest{})
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}
	var got lcarsv1.ListLocksResponse
	if err := proto.Unmarshal(msg.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.GetLocks()) != 1 || got.GetLocks()[0].GetId() != list.Locks[0].ID ||
		got.GetLocks()[0].GetLocked() != list.Locks[0].Locked {
		t.Fatalf("NATS catalog = %+v", got.GetLocks())
	}
	cover(subjectCoverage, subjectLocksList)

	unlock := mustDo(t, http.MethodPost, p.url("/locks/"+testLockID+"/unlock"))
	if unlock.StatusCode != http.StatusOK || !strings.Contains(string(unlock.body), `"locked":false`) {
		t.Fatalf("POST unlock = %d %s", unlock.StatusCode, unlock.body)
	}
	cover(pathCoverage, "/locks/"+testLockID+"/unlock")

	lockMsg := natsRequest(t, nc, subjectLocksLock, &lcarsv1.LockDoorRequest{Id: testLockID})
	if err := replyError(lockMsg); err != nil {
		t.Fatal(err)
	}
	var lockedReply lcarsv1.LockDoorResponse
	if err := proto.Unmarshal(lockMsg.Data, &lockedReply); err != nil || !lockedReply.GetLocked() {
		t.Fatalf("NATS lock = %+v %v", &lockedReply, err)
	}
	cover(subjectCoverage, subjectLocksLock)

	unlockMsg := natsRequest(t, nc, subjectLocksUnlock, &lcarsv1.UnlockDoorRequest{Id: testLockID})
	if err := replyError(unlockMsg); err != nil {
		t.Fatal(err)
	}
	var unlockedReply lcarsv1.UnlockDoorResponse
	if err := proto.Unmarshal(unlockMsg.Data, &unlockedReply); err != nil || unlockedReply.GetLocked() {
		t.Fatalf("NATS unlock = %+v %v", &unlockedReply, err)
	}
	cover(subjectCoverage, subjectLocksUnlock)

	lockHTTP := mustDo(t, http.MethodPost, p.url("/locks/"+testLockID+"/lock"))
	if lockHTTP.StatusCode != http.StatusOK || !strings.Contains(string(lockHTTP.body), `"locked":true`) {
		t.Fatalf("POST lock = %d %s", lockHTTP.StatusCode, lockHTTP.body)
	}
	cover(pathCoverage, "/locks/"+testLockID+"/lock")

	bad := mustDo(t, http.MethodPost, p.url("/locks/not-a-uuid/lock"))
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", bad.StatusCode)
	}
	raw, err := nc.Request(subjectLocksLock, []byte{0xff, 0xff, 0xff, 0xff, 0xff}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Header.Get("Nats-Service-Error-Code") != "400" {
		t.Errorf("bad protobuf = %s, want 400", raw.Header.Get("Nats-Service-Error-Code"))
	}
	logs := p.stdout.String() + p.stderr.String()
	if strings.Contains(logs, "seam_test_key") {
		t.Error("logs contain Seam API key")
	}
}

func TestLocksPendingIsOneShot(t *testing.T) {
	var polls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/locks/lock_door":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"action_attempt": map[string]any{
					"action_attempt_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"action_type":       "LOCK_DOOR",
					"status":            "pending",
				},
			})
		case "/action_attempts/get":
			polls.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	p := startServer(t,
		"LCARS_SEAM_API_KEY=seam_test_key",
		"LCARS_SEAM_URL="+upstream.URL,
	)
	lock := mustDo(t, http.MethodPost, p.url("/locks/"+testLockID+"/lock"))
	if lock.StatusCode != http.StatusOK {
		t.Fatalf("POST lock pending = %d %s", lock.StatusCode, lock.body)
	}
	if polls.Load() != 0 {
		t.Fatalf("polled action_attempts %d times; want 0", polls.Load())
	}
}

func TestLocksDisabled(t *testing.T) {
	p := startServer(t)
	if !p.hasLog(map[string]any{"msg": "locks disabled: seam.api_key is not set"}) {
		t.Errorf("missing locks disabled log\nstdout: %s", p.stdout.String())
	}
	resp := mustDo(t, http.MethodGet, p.url("/locks"))
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(resp.body), "seam.api_key") {
		t.Fatalf("GET /locks = %d %s", resp.StatusCode, resp.body)
	}
	nc := connectNATS(t, p.natsURL())
	msg := natsRequest(t, nc, subjectLocksList, &lcarsv1.ListLocksRequest{})
	if err := replyError(msg); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("NATS = %v, want 503", err)
	}
}

func TestLocksNeedToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/locks/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"devices": []map[string]any{{
					"device_id":    testLockID,
					"display_name": "Front door",
					"properties":   map[string]any{"locked": true, "online": true},
				}},
			})
		case "/locks/unlock_door":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"action_attempt": map[string]any{
					"action_attempt_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					"status":            "success",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	p, admin := authServer(t,
		"LCARS_SEAM_API_KEY=seam_test_key",
		"LCARS_SEAM_URL="+upstream.URL,
	)
	base := p.url("")
	if resp := mustDo(t, http.MethodGet, base+"/locks"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /locks without a token = %d, want 401", resp.StatusCode)
	}
	if resp := mustDo(t, http.MethodPost, base+"/locks/"+testLockID+"/unlock"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST unlock without a token = %d, want 401", resp.StatusCode)
	}

	tok := createToken(t, base, admin, "Kitchen display")
	authed := basicURL(base, tok)
	resp := mustDo(t, http.MethodGet, authed+"/locks")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.body), testLockID) {
		t.Fatalf("GET /locks with a token = %d %s", resp.StatusCode, resp.body)
	}
	unlock := mustDo(t, http.MethodPost, authed+"/locks/"+testLockID+"/unlock")
	if unlock.StatusCode != http.StatusOK {
		t.Fatalf("POST unlock with a token = %d %s", unlock.StatusCode, unlock.body)
	}

	nc, err := nats.Connect(natsURLWith(p.natsURL(), tok), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	msg := natsRequest(t, nc, subjectLocksList, &lcarsv1.ListLocksRequest{})
	if err := replyError(msg); err != nil {
		t.Fatal(err)
	}
}
