package octopus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/hems/config"
	"github.com/evcc-io/evcc/util"
)

func init() {
	config.AddCtx("octopussmartcharge", NewFromConfig)
}

const (
	apiURL               = "https://api.oeg-kraken.energy/v1/graphql/"
	batteryRenewInterval = 30 * time.Second
)

// SmartCharge watches Octopus Intelligent planned dispatch windows and hands
// control of a loadpoint to Octopus (via SetExternalControl) while a dispatch
// is active. Octopus controls the wallbox directly via its cloud backend;
// evcc resumes its own charging logic (PV surplus, target SoC) between dispatches.
type SmartCharge struct {
	log           *util.Logger
	username      string
	password      string
	accountNumber string
	lp            loadpoint.API
	site          site.API
	refresh       time.Duration

	token       string
	tokenExpiry time.Time
}

// NewFromConfig creates a SmartCharge HEMS from generic config.
func NewFromConfig(ctx context.Context, other map[string]any, s site.API) (*SmartCharge, error) {
	cc := struct {
		Username      string
		Password      string
		AccountNumber string
		Loadpoint     int
		Refresh       time.Duration
	}{
		Loadpoint: 1,
		Refresh:   30 * time.Second,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	lps := s.Loadpoints()
	if cc.Loadpoint < 1 || cc.Loadpoint > len(lps) {
		return nil, fmt.Errorf("invalid loadpoint index %d (have %d)", cc.Loadpoint, len(lps))
	}

	return &SmartCharge{
		log:           util.NewLogger("octopus-smartcharge").Redact(cc.Password),
		username:      cc.Username,
		password:      cc.Password,
		accountNumber: cc.AccountNumber,
		lp:            lps[cc.Loadpoint-1],
		site:          s,
		refresh:       cc.Refresh,
	}, nil
}

// SetUpdated implements api.HEMS.
func (s *SmartCharge) SetUpdated(func()) {
}

// Curtailed implements hems.API. Octopus smart charge does not curtail.
func (s *SmartCharge) Curtailed() *bool {
	return nil
}

// Dimmed implements hems.API.
func (s *SmartCharge) Dimmed() *bool {
	return nil
}

// MaxConsumptionPower implements api.HEMS.
func (s *SmartCharge) MaxConsumptionPower() float64 {
	return 0
}

// MaxProductionPower implements api.HEMS.
func (s *SmartCharge) MaxProductionPower() *float64 {
	return nil
}

// CurtailedPercent implements api.HEMS. Octopus smart charge does not curtail production.
func (s *SmartCharge) CurtailedPercent() *int {
	return nil
}

// Run implements hems.API.
func (s *SmartCharge) Run() {
	ctx := context.Background()

	if s.accountNumber == "" {
		if acc, err := s.resolveAccount(ctx); err != nil {
			s.log.WARN.Printf("resolve account: %v", err)
		} else {
			s.accountNumber = acc
		}
	}

	// Lease covers the refresh interval plus a buffer so evcc doesn't
	// resume control mid-dispatch if a poll is slightly delayed.
	lease := s.refresh + min(s.refresh/2, 60*time.Second)

	dispatching := false

	pollTicker := time.NewTicker(s.refresh)
	defer pollTicker.Stop()
	batteryTicker := time.NewTicker(batteryRenewInterval)
	defer batteryTicker.Stop()

	for {
		select {
		case <-pollTicker.C:
			active, err := s.isDispatching(ctx)
			if err != nil {
				s.log.ERROR.Println(err)
				// Safe default on error: release control so evcc takes over.
				s.lp.SetExternalControl(0)
				s.setBatteryHold(false)
				dispatching = false
				continue
			}

			dispatching = active
			if dispatching {
				s.log.DEBUG.Printf("Octopus dispatch active, renewing external control (%v)", lease)
				s.lp.SetExternalControl(lease)
				s.setBatteryHold(true)
			} else {
				s.log.DEBUG.Println("no active Octopus dispatch")
				s.lp.SetExternalControl(0)
				s.setBatteryHold(false)
			}

		case <-batteryTicker.C:
			// Renew battery hold independently of the poll interval to
			// stay ahead of the site's 60s watchdog.
			if dispatching {
				s.setBatteryHold(true)
			}
		}
	}
}

// resolveAccount queries the account number for the authenticated user.
// Required when accountNumber is not set in config and the user has exactly one account.
func (s *SmartCharge) resolveAccount(ctx context.Context) (string, error) {
	data, err := s.gqlPost(ctx, `{ viewer { accounts { number } } }`)
	if err != nil {
		return "", err
	}

	var res struct {
		Viewer struct {
			Accounts []struct {
				Number string `json:"number"`
			} `json:"accounts"`
		} `json:"viewer"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return "", err
	}

	switch len(res.Viewer.Accounts) {
	case 0:
		return "", fmt.Errorf("no accounts found")
	case 1:
		acc := res.Viewer.Accounts[0].Number
		s.log.DEBUG.Printf("resolved account: %s", acc)
		return acc, nil
	default:
		return "", fmt.Errorf("multiple accounts found, please configure accountNumber")
	}
}

// isDispatching returns true if the current time falls within any planned
// Octopus Intelligent dispatch window (i.e. Octopus is actively controlling the wallbox).
func (s *SmartCharge) isDispatching(ctx context.Context) (bool, error) {
	if s.accountNumber == "" {
		return false, fmt.Errorf("no account number")
	}

	data, err := s.gqlPost(ctx, fmt.Sprintf(
		`{ flexPlannedDispatches(accountNumber: %q) { startDt endDt } }`,
		s.accountNumber,
	))
	if err != nil {
		return false, err
	}

	var res struct {
		FlexPlannedDispatches []struct {
			StartDt time.Time `json:"startDt"`
			EndDt   time.Time `json:"endDt"`
		} `json:"flexPlannedDispatches"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return false, err
	}

	now := time.Now()
	for _, d := range res.FlexPlannedDispatches {
		if !now.Before(d.StartDt) && now.Before(d.EndDt) {
			s.log.DEBUG.Printf("active dispatch window: %v – %v", d.StartDt.Local(), d.EndDt.Local())
			return true, nil
		}
	}
	return false, nil
}

// setBatteryHold sets or clears the external battery hold mode.
// Errors are silently ignored (e.g. no battery configured).
func (s *SmartCharge) setBatteryHold(hold bool) {
	mode := api.BatteryUnknown
	if hold {
		mode = api.BatteryHold
	}
	_ = s.site.SetBatteryModeExternal(mode)
}

// gqlPost sends a GraphQL query or mutation to the Octopus API.
func (s *SmartCharge) gqlPost(ctx context.Context, query string) (json.RawMessage, error) {
	token, err := s.fetchToken(ctx)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "JWT "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.Errors != nil {
		return nil, fmt.Errorf("%s", result.Errors)
	}

	return result.Data, nil
}

// fetchToken returns a cached Octopus JWT, refreshing when near expiry.
func (s *SmartCharge) fetchToken(ctx context.Context) (string, error) {
	if s.token != "" && time.Now().Before(s.tokenExpiry) {
		return s.token, nil
	}

	query := fmt.Sprintf(
		`mutation { obtainKrakenToken(input: { email: %q, password: %q }) { token } }`,
		s.username, s.password,
	)
	body, _ := json.Marshal(map[string]string{"query": query})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			ObtainKrakenToken struct {
				Token string `json:"token"`
			} `json:"obtainKrakenToken"`
		} `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.Errors != nil {
		return "", fmt.Errorf("auth: %s", result.Errors)
	}
	if result.Data.ObtainKrakenToken.Token == "" {
		return "", fmt.Errorf("octopus authentication failed")
	}

	s.token = result.Data.ObtainKrakenToken.Token
	s.tokenExpiry = time.Now().Add(55 * time.Minute)
	return s.token, nil
}
