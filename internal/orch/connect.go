package orch

// Dashboard-driven integration connect. Currently covers GitHub via the
// device-flow grant: the dashboard kicks off /api/connect/github/start,
// renders the user_code + verification URL, and polls /api/connect/github/poll
// until the daemon's background goroutine has exchanged the device_code
// for an access token and written it into gh's hosts.yml. After that
// orchestrator.BotLogin auto-populates on next tick.
//
// We reuse the GitHub CLI's well-known device-flow client_id so no app
// registration is required — same UX a `gh auth login --web` user gets.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const ghDeviceClientID = "178c6fc778ccc68e1d6a"

type ghDeviceFlow struct {
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time

	mu     sync.Mutex
	done   bool
	err    string
	token  string
	login  string
	cancel func()
}

var (
	ghFlowMu   sync.Mutex
	ghFlow     *ghDeviceFlow
	ghBotLogin string // last successfully connected login; orch reads it via getConnectedLogin
)

func getConnectedLogin() string {
	ghFlowMu.Lock()
	defer ghFlowMu.Unlock()
	return ghBotLogin
}

// startGHDeviceFlow asks GitHub for a device code, stores the pending
// flow, and spins a background poll. Returns the user_code +
// verification URL the dashboard shows to the operator.
func startGHDeviceFlow(scope string) (*ghDeviceFlow, error) {
	form := url.Values{
		"client_id": {ghDeviceClientID},
		"scope":     {scope},
	}
	req, _ := http.NewRequest("POST",
		"https://github.com/login/device/code",
		strings.NewReader(form.Encode()))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Error != "" {
		return nil, fmt.Errorf("github: %s: %s", body.Error, body.ErrorDescription)
	}
	interval := body.Interval
	if interval < 5 {
		interval = 5
	}

	f := &ghDeviceFlow{
		UserCode:        body.UserCode,
		VerificationURL: body.VerificationURI,
		ExpiresAt:       time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
	}
	stopCh := make(chan struct{})
	f.cancel = func() { close(stopCh) }

	ghFlowMu.Lock()
	if ghFlow != nil && ghFlow.cancel != nil {
		ghFlow.cancel()
	}
	ghFlow = f
	ghFlowMu.Unlock()

	go pollGHDevice(f, body.DeviceCode, interval, stopCh)
	return f, nil
}

func pollGHDevice(f *ghDeviceFlow, deviceCode string, intervalSec int, stop <-chan struct{}) {
	tick := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		if time.Now().After(f.ExpiresAt) {
			finishFlow(f, "", "", "device code expired — start over")
			return
		}
		form := url.Values{
			"client_id":   {ghDeviceClientID},
			"device_code": {deviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		req, _ := http.NewRequest("POST",
			"https://github.com/login/oauth/access_token",
			strings.NewReader(form.Encode()))
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			continue
		}
		var body struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			Scope       string `json:"scope"`
			Error       string `json:"error"`
			Interval    int    `json:"interval"`
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err := json.Unmarshal(raw, &body); err != nil {
			continue
		}
		switch body.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			if body.Interval > 0 {
				tick.Reset(time.Duration(body.Interval) * time.Second)
			}
			continue
		case "expired_token", "access_denied":
			finishFlow(f, "", "", body.Error)
			return
		case "":
			if body.AccessToken == "" {
				continue
			}
			login, ferr := finalizeGHToken(body.AccessToken)
			if ferr != nil {
				finishFlow(f, "", "", ferr.Error())
				return
			}
			finishFlow(f, body.AccessToken, login, "")
			return
		default:
			finishFlow(f, "", "", body.Error)
			return
		}
	}
}

// finalizeGHToken hands the token to gh (`gh auth login --with-token`)
// so that the rest of orch's gh shell-outs pick it up, sets up the git
// credential helper, and returns the login the token belongs to.
func finalizeGHToken(token string) (string, error) {
	cmd := exec.Command("gh", "auth", "login",
		"--hostname", "github.com",
		"--git-protocol", "https",
		"--with-token")
	cmd.Stdin = strings.NewReader(token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("gh auth login: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("gh", "auth", "setup-git", "-h", "github.com").CombinedOutput(); err != nil {
		return "", fmt.Errorf("gh auth setup-git: %v: %s", err, strings.TrimSpace(string(out)))
	}
	out, _, err := run("gh", "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func finishFlow(f *ghDeviceFlow, token, login, errMsg string) {
	f.mu.Lock()
	f.done = true
	f.token = token
	f.login = login
	f.err = errMsg
	f.mu.Unlock()

	if login != "" {
		ghFlowMu.Lock()
		ghBotLogin = login
		ghFlowMu.Unlock()
	}
}

// connectStatus is the response shape for GET /api/connect/status.
type connectStatus struct {
	GitHub struct {
		Connected bool   `json:"connected"`
		Login     string `json:"login,omitempty"`
	} `json:"github"`
}

func buildConnectStatus(cfg *Config) connectStatus {
	var s connectStatus
	login := cfg.Orch.BotLogin
	if login == "" {
		login = getConnectedLogin()
	}
	if login == "" {
		// Last-chance probe: gh might have been authed out-of-band.
		if out, _, err := run("gh", "api", "user", "--jq", ".login"); err == nil {
			login = strings.TrimSpace(out)
		}
	}
	s.GitHub.Connected = login != ""
	s.GitHub.Login = login
	return s
}
