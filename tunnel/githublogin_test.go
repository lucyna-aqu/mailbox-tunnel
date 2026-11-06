package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeGitHub answers the device sign-in with the given replies to each poll, in order.
func fakeGitHub(t *testing.T, polls ...string) {
	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_id") != "client" {
			http.Error(w, "unknown client", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/login/device/code":
			if r.Form.Get("scope") != relayScope {
				t.Errorf("asked for scope %q", r.Form.Get("scope"))
			}
			fmt.Fprint(w, `{"device_code":"device","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device","interval":0,"expires_in":60}`)
		case "/login/oauth/access_token":
			if r.Form.Get("device_code") != "device" {
				t.Errorf("polled with device code %q", r.Form.Get("device_code"))
			}
			fmt.Fprint(w, polls[min(served, len(polls)-1)])
			served++
		}
	}))
	t.Cleanup(server.Close)
	previous, floor, slow := githubWeb, loginPollFloor, loginSlowDown
	githubWeb, loginPollFloor, loginSlowDown = server.URL, time.Millisecond, time.Millisecond
	t.Cleanup(func() { githubWeb, loginPollFloor, loginSlowDown = previous, floor, slow })
}

func TestDeviceLoginWaitsForTheUser(t *testing.T) {
	fakeGitHub(t,
		`{"error":"authorization_pending"}`,
		`{"error":"slow_down"}`,
		`{"access_token":"token","scope":"codespace","token_type":"bearer"}`)
	login, err := StartDeviceLogin(context.Background(), "client")
	if err != nil || login.UserCode != "ABCD-1234" {
		t.Fatalf("start: %+v, %v", login, err)
	}
	token, err := FinishDeviceLogin(context.Background(), "client", login)
	if err != nil || token != "token" {
		t.Fatalf("finish: %q, %v", token, err)
	}
}

func TestDeviceLoginReportsRefusalAndExpiry(t *testing.T) {
	for reply, want := range map[string]error{
		`{"error":"access_denied"}`: ErrLoginDenied,
		`{"error":"expired_token"}`: ErrLoginExpired,
	} {
		fakeGitHub(t, reply)
		login, _ := StartDeviceLogin(context.Background(), "client")
		if _, err := FinishDeviceLogin(context.Background(), "client", login); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", reply, err, want)
		}
	}
}

func TestDeviceLoginRefusesATokenWithoutTheCodespaceScope(t *testing.T) {
	fakeGitHub(t, `{"access_token":"token","scope":"repo"}`)
	login, _ := StartDeviceLogin(context.Background(), "client")
	if _, err := FinishDeviceLogin(context.Background(), "client", login); err == nil {
		t.Fatal("a token without the codespace scope was accepted")
	}
}
