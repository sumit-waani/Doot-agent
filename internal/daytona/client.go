// Package daytona wraps the Daytona SDK behind the narrow surface Doot needs.
//
// Two things this package exists to encapsulate:
//
//  1. The high-level Go SDK does not expose every documented operation. Two
//     matter here — compressed screenshots and the activity heartbeat — and both
//     are reachable through the generated clients underneath it. Those detours
//     live here so callers never see them.
//
//  2. Lifecycle defaults. Three of Daytona's four inactivity defaults would
//     eventually stop or destroy the sandbox out from under a running agent, so
//     every interval is set explicitly on create.
package daytona

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sdk "github.com/daytona/clients/sdk-go/pkg/daytona"
	"github.com/daytona/clients/sdk-go/pkg/types"

	apiclient "github.com/daytona/clients/api-client-go"
)

// DefaultAPIURL is Daytona's hosted control plane.
const DefaultAPIURL = "https://app.daytona.io/api"

// RequiredSnapshot is the only snapshot family Doot supports.
//
// This is not a preference. Daytona ships VNC and computer-use support *only in
// its default image*, and Doot requires computer-use and the project to share
// one sandbox. A custom image would look tidier and would silently break the
// E2E verifier, so project dependencies are installed at runtime instead.
//
// daytona-medium is 2 vCPU / 4 GiB / 8 GiB, which is the configured target
// size, so the requirement is met by a stock snapshot with no custom resource
// request.
const RequiredSnapshot = "daytona-medium"

// knownDefaultSnapshots are the stock snapshots that carry the default image.
var knownDefaultSnapshots = map[string]bool{
	"daytona-small":  true,
	"daytona-medium": true,
	"daytona-large":  true,
}

var (
	// ErrNoAPIKey is returned when the Daytona credential has not been set.
	ErrNoAPIKey = errors.New("daytona: API key is not set (add it in Settings)")

	// ErrNoSandbox is returned when the project has no sandbox yet.
	ErrNoSandbox = errors.New("daytona: project has no sandbox")
)

// Config configures a Client.
type Config struct {
	APIKey string
	APIURL string
	Target string

	// Snapshot to create sandboxes from. Must be a default-image snapshot.
	Snapshot string

	// VNCResolution is applied at creation only. Daytona allocates the X
	// framebuffer when the server starts, so this cannot be changed afterwards
	// without creating a new sandbox.
	VNCResolution string

	// Lifecycle intervals, in minutes.
	AutoStopMinutes    int
	AutoArchiveMinutes int
	AutoDeleteMinutes  int
	TTLMinutes         int

	// Public makes preview URLs reachable without a token.
	Public bool

	// Screenshot defaults for computer-use.
	ScreenshotFormat  string
	ScreenshotQuality int
	ScreenshotScale   float64

	// PreviewTTL is how long a signed preview link stays valid.
	PreviewTTL time.Duration
}

// withDefaults fills in anything the caller left blank.
func (c Config) withDefaults() Config {
	if c.APIURL == "" {
		c.APIURL = DefaultAPIURL
	}
	if c.Snapshot == "" {
		c.Snapshot = RequiredSnapshot
	}
	if c.VNCResolution == "" {
		c.VNCResolution = "1280x800"
	}
	if c.AutoStopMinutes == 0 {
		c.AutoStopMinutes = 30
	}
	if c.AutoDeleteMinutes == 0 {
		// 0 would mean "delete as soon as it stops", which is the one value that
		// must never be used here. Anything unset becomes "never".
		c.AutoDeleteMinutes = -1
	}
	if c.ScreenshotFormat == "" {
		c.ScreenshotFormat = "jpeg"
	}
	if c.ScreenshotQuality <= 0 {
		c.ScreenshotQuality = 80
	}
	if c.ScreenshotScale <= 0 {
		c.ScreenshotScale = 1
	}
	if c.PreviewTTL <= 0 {
		c.PreviewTTL = time.Hour
	}
	return c
}

// Validate reports configuration that would produce a broken sandbox.
func (c Config) Validate() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return ErrNoAPIKey
	}
	if !knownDefaultSnapshots[c.Snapshot] {
		// Not fatal: Daytona may add snapshots, and a custom default-image
		// snapshot is legitimate. But it is worth being loud about, because the
		// failure it causes shows up much later as computer-use not working.
		return fmt.Errorf(
			"daytona: snapshot %q is not a known default-image snapshot; "+
				"computer-use requires the default image (expected one of daytona-small, daytona-medium, daytona-large)",
			c.Snapshot)
	}
	if _, _, err := parseResolution(c.VNCResolution); err != nil {
		return err
	}
	return nil
}

// Client talks to Daytona.
type Client struct {
	cfg Config

	sdk *sdk.Client

	// control is the generated control-plane client. The high-level SDK keeps
	// its own copy unexported, and one operation Doot depends on — the activity
	// heartbeat — is not surfaced by the SDK, so this exists to reach it.
	control *apiclient.APIClient
}

// New builds a Client.
func New(cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	sdkClient, err := sdk.NewClientWithConfig(&types.DaytonaConfig{
		APIKey: cfg.APIKey,
		APIUrl: cfg.APIURL,
		Target: cfg.Target,
	})
	if err != nil {
		return nil, fmt.Errorf("daytona: build client: %w", err)
	}

	return &Client{
		cfg:     cfg,
		sdk:     sdkClient,
		control: newControlClient(cfg),
	}, nil
}

// newControlClient builds the generated control-plane client, mirroring how the
// SDK configures its own.
func newControlClient(cfg Config) *apiclient.APIClient {
	apiCfg := apiclient.NewConfiguration()
	apiCfg.Servers = apiclient.ServerConfigurations{{URL: strings.TrimRight(cfg.APIURL, "/")}}
	apiCfg.UserAgent = "doot"
	return apiclient.NewAPIClient(apiCfg)
}

// authContext attaches the API key the generated client expects.
func (c *Client) authContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, apiclient.ContextAccessToken, c.cfg.APIKey)
}

// Config returns the effective configuration.
func (c *Client) Config() Config { return c.cfg }

// Close releases the SDK's connections, including its state-event WebSocket.
func (c *Client) Close(ctx context.Context) error {
	if c.sdk == nil {
		return nil
	}
	return c.sdk.Close(ctx)
}

// IsDefaultSnapshot reports whether a snapshot name is one of Daytona's stock
// default-image snapshots, which are the only ones that carry computer-use.
func IsDefaultSnapshot(name string) bool {
	return knownDefaultSnapshots[strings.TrimSpace(name)]
}

// ValidateResolution checks a "WIDTHxHEIGHT" string against what Daytona
// accepts. An invalid value is silently replaced with the default at sandbox
// creation, which then mismatches whatever the agent assumes.
func ValidateResolution(s string) error {
	_, _, err := parseResolution(s)
	return err
}

// parseResolution splits a "WIDTHxHEIGHT" string and range-checks it against
// what Daytona accepts.
func parseResolution(s string) (width, height int, err error) {
	w, h, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "x")
	if !ok {
		return 0, 0, fmt.Errorf("daytona: resolution %q must look like 1280x800", s)
	}
	if _, err := fmt.Sscanf(w, "%d", &width); err != nil {
		return 0, 0, fmt.Errorf("daytona: resolution %q has a bad width", s)
	}
	if _, err := fmt.Sscanf(h, "%d", &height); err != nil {
		return 0, 0, fmt.Errorf("daytona: resolution %q has a bad height", s)
	}
	if width < 640 || width > 7680 || height < 480 || height > 4320 {
		return 0, 0, fmt.Errorf(
			"daytona: resolution %dx%d is outside the supported range (640-7680 x 480-4320)",
			width, height)
	}
	return width, height, nil
}
