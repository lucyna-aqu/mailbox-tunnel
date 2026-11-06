package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// githubWeb is GitHub's sign-in site; tests point it at a local server.
var githubWeb = "https://github.com"

// relayScope lets the app create, start and configure the user's codespaces, and nothing else.
const relayScope = "codespace"

// GitHub asks for at least five seconds between polls, and five more each time it says to slow down; tests shorten both.
var loginPollFloor, loginSlowDown = 5 * time.Second, 5 * time.Second

// DeviceLogin is a pending sign-in: the user types UserCode at VerificationURI while the app polls.
type DeviceLogin struct {
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	DeviceCode      string `json:"device_code"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

// ErrLoginDenied means the user refused the sign-in on GitHub's page.
var ErrLoginDenied = errors.New("sign-in refused")

// ErrLoginExpired means the code was not entered in time.
var ErrLoginExpired = errors.New("sign-in code expired")

// StartDeviceLogin asks GitHub for a code the user enters on its site, for the OAuth app with clientID.
func StartDeviceLogin(ctx context.Context, clientID string) (DeviceLogin, error) {
	var login DeviceLogin
	err := postForm(ctx, githubWeb+"/login/device/code", url.Values{"client_id": {clientID}, "scope": {relayScope}}, &login)
	if err == nil && (login.DeviceCode == "" || login.UserCode == "") {
		err = errors.New("GitHub did not give a sign-in code")
	}
	return login, err
}

// FinishDeviceLogin polls until the user has entered the code and returns the token.
func FinishDeviceLogin(ctx context.Context, clientID string, login DeviceLogin) (string, error) {
	interval := max(time.Duration(login.Interval)*time.Second, loginPollFloor)
	if login.ExpiresIn > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(login.ExpiresIn)*time.Second)
		defer cancel()
	}
	for {
		select {
		case <-ctx.Done():
			return "", ErrLoginExpired
		case <-time.After(interval):
		}
		var answer struct {
			Token string `json:"access_token"`
			Scope string `json:"scope"`
			Error string `json:"error"`
		}
		form := url.Values{"client_id": {clientID}, "device_code": {login.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
		if err := postForm(ctx, githubWeb+"/login/oauth/access_token", form, &answer); err != nil {
			if ctx.Err() != nil {
				return "", ErrLoginExpired
			}
			continue
		}
		switch answer.Error {
		case "":
			if answer.Token == "" {
				return "", errors.New("GitHub answered without a token")
			}
			if !strings.Contains(answer.Scope, relayScope) {
				return "", fmt.Errorf("GitHub granted %q, not the codespace scope", answer.Scope)
			}
			return answer.Token, nil
		case "authorization_pending":
		case "slow_down":
			interval += loginSlowDown
		case "access_denied":
			return "", ErrLoginDenied
		case "expired_token":
			return "", ErrLoginExpired
		default:
			return "", fmt.Errorf("sign-in: %s", answer.Error)
		}
	}
}

func postForm(ctx context.Context, address string, form url.Values, result any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	return doJSON(request, result)
}
