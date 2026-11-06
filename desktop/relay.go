package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/lucyna-aqu/mailbox-tunnel/devtunnel"
	"github.com/lucyna-aqu/mailbox-tunnel/tunnel"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// relayRepo is the public repository whose dev container runs the relay, and githubClientID the OAuth app users sign in to.
// Release builds set both with -ldflags; without them the app offers no GitHub relay setup.
var relayRepo, githubClientID string

// relayWakeTimeout covers a codespace waking from sleep, which measured about 25 seconds.
const relayWakeTimeout = 2 * time.Minute

// RelaySetup is the progress of setting up the GitHub relay, shown in Settings.
// Step is "code" while the user types the code on GitHub's page, then the setup's own steps, then "done".
type RelaySetup struct {
	Step  string `json:"step"`
	Code  string `json:"code"`
	URL   string `json:"url"`
	Error string `json:"error"`
}

func (a *App) setRelay(setup RelaySetup) {
	a.mutex.Lock()
	a.state.Relay = setup
	a.mutex.Unlock()
	a.publish()
}

// SetUpRelay signs the user in to GitHub with a code and then builds the relay in their codespace. It returns an error message or "".
func (a *App) SetUpRelay() string {
	if relayRepo == "" || githubClientID == "" {
		return "This build cannot set up a GitHub relay."
	}
	a.mutex.Lock()
	busy := a.relayCancel != nil
	a.mutex.Unlock()
	if busy {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	login, err := tunnel.StartDeviceLogin(ctx, githubClientID)
	if err != nil {
		cancel()
		log.Printf("relay sign-in: %v", err)
		return "Could not reach GitHub to sign in."
	}
	a.mutex.Lock()
	a.relayCancel = cancel
	a.mutex.Unlock()
	a.setRelay(RelaySetup{Step: "code", Code: login.UserCode, URL: login.VerificationURI})
	runtime.BrowserOpenURL(a.ctx, login.VerificationURI)
	go a.finishRelaySetup(ctx, login)
	return ""
}

// CancelRelaySetup stops a setup that is waiting for the code or still building.
func (a *App) CancelRelaySetup() {
	a.mutex.Lock()
	cancel := a.relayCancel
	a.mutex.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *App) finishRelaySetup(ctx context.Context, login tunnel.DeviceLogin) {
	defer func() {
		a.mutex.Lock()
		a.relayCancel()
		a.relayCancel = nil
		a.mutex.Unlock()
	}()
	fail := func(err error) {
		log.Printf("relay setup: %v", err)
		message := "The relay could not be set up. See Logs for the reason."
		switch {
		case ctx.Err() != nil && !errors.Is(err, tunnel.ErrLoginExpired):
			message = ""
		case errors.Is(err, tunnel.ErrLoginDenied):
			message = "Sign-in was refused on GitHub."
		case errors.Is(err, tunnel.ErrLoginExpired):
			message = "The code was not entered in time. Try again."
		}
		a.setRelay(RelaySetup{Error: message})
	}
	token, err := tunnel.FinishDeviceLogin(ctx, githubClientID, login)
	if err != nil {
		fail(err)
		return
	}
	if err := a.updateSettings(func(s *tunnel.Settings) { s.GitHubToken = token }); err != nil {
		fail(err)
		return
	}
	relay := &tunnel.CodespaceRelay{Token: token, Repo: relayRepo, Announce: devtunnel.Announce}
	node, err := relay.SetUp(ctx, func(step string) { a.setRelay(RelaySetup{Step: step}) })
	if relay.Name != "" {
		// The codespace is kept even when the setup fails later, so it can be woken or removed.
		a.updateSettings(func(s *tunnel.Settings) { s.RelaySpace = relay.Name })
	}
	if err != nil {
		fail(err)
		return
	}
	if err := a.updateSettings(func(s *tunnel.Settings) { s.Backup = node }); err != nil {
		fail(err)
		return
	}
	a.mutex.Lock()
	a.state.Backup, a.state.RelayManaged = true, true
	a.mutex.Unlock()
	a.setRelay(RelaySetup{Step: "done"})
}

// RemoveRelay forgets the relay and deletes its codespace, so it no longer counts against the user's hours.
func (a *App) RemoveRelay() string {
	settings, err := tunnel.LoadSettings(a.settingsPath)
	if err != nil {
		return "Enter your settings first."
	}
	if settings.GitHubToken != "" && settings.RelaySpace != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		relay := &tunnel.CodespaceRelay{Token: settings.GitHubToken, Name: settings.RelaySpace}
		if err := relay.Delete(ctx); err != nil {
			log.Printf("relay removal: %v", err)
		}
	}
	if err := a.updateSettings(func(s *tunnel.Settings) {
		s.Backup, s.GitHubToken, s.RelaySpace, s.PreferRelay = "", "", "", false
	}); err != nil {
		return err.Error()
	}
	a.mutex.Lock()
	a.state.Backup, a.state.RelayManaged, a.state.PreferRelay = false, false, false
	a.state.Relay = RelaySetup{}
	a.mutex.Unlock()
	a.publish()
	return ""
}

// wakeRelay starts the user's relay codespace when it sleeps; it returns once the relay answers or the wake fails.
func wakeRelay(ctx context.Context, settings tunnel.Settings) error {
	if settings.GitHubToken == "" || settings.RelaySpace == "" {
		return errors.New("the relay was not set up from this app")
	}
	ctx, cancel := context.WithTimeout(ctx, relayWakeTimeout)
	defer cancel()
	relay := &tunnel.CodespaceRelay{Token: settings.GitHubToken, Repo: relayRepo, Name: settings.RelaySpace, Announce: devtunnel.Announce}
	log.Printf("relay: waking the codespace")
	return relay.Start(ctx, nil)
}

// updateSettings applies a change to the saved settings, reading them fresh so other changes are kept.
func (a *App) updateSettings(change func(*tunnel.Settings)) error {
	a.settingsMutex.Lock()
	defer a.settingsMutex.Unlock()
	settings, err := tunnel.LoadSettings(a.settingsPath)
	if err != nil {
		return err
	}
	change(&settings)
	return tunnel.SaveSettings(a.settingsPath, settings)
}
