package daytona

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	toolbox "github.com/daytona/clients/toolbox-api-client-go"
)

// DesktopStatus reports whether the computer-use processes are up.
type DesktopStatus struct {
	Running bool
	Raw     string
}

// Screenshot is a captured frame.
type Screenshot struct {
	// Data is the decoded image.
	Data []byte
	// Format is "jpeg", "png", or whatever was requested.
	Format string
	Width  int
	Height int
}

// Display describes the virtual screen geometry.
type Display struct {
	Width  int
	Height int
	Count  int
}

// DesktopStatus checks the computer-use processes (Xvfb, xfce4, x11vnc, noVNC).
func (s *Sandbox) DesktopStatus(ctx context.Context) (DesktopStatus, error) {
	raw, err := s.sb.ComputerUse.GetStatus(ctx)
	if err != nil {
		return DesktopStatus{}, fmt.Errorf("daytona: computer-use status: %w", err)
	}

	// The SDK returns an untyped map. Treat anything that is not clearly
	// running as not running: a false negative costs one idempotent start
	// call, while a false positive means every later operation fails.
	text := strings.ToLower(fmt.Sprint(raw))
	running := strings.Contains(text, "running") && !strings.Contains(text, "not running")

	return DesktopStatus{Running: running, Raw: fmt.Sprint(raw)}, nil
}

// EnsureDesktop starts the computer-use processes if they are not already up.
//
// This must be called before any computer-use operation. The processes run
// inside the sandbox and do not survive a stop, so "it was running an hour ago"
// is never a safe assumption — and after a wake it is actively wrong.
func (s *Sandbox) EnsureDesktop(ctx context.Context) error {
	status, err := s.DesktopStatus(ctx)
	if err == nil && status.Running {
		return nil
	}
	if err != nil {
		slog.Debug("could not read computer-use status; starting anyway", "err", err)
	}

	slog.Info("starting computer-use processes", "sandbox", s.sb.ID)
	if err := s.sb.ComputerUse.Start(ctx); err != nil {
		return fmt.Errorf("daytona: start computer-use: %w", err)
	}
	return nil
}

// StopDesktop stops the computer-use processes.
func (s *Sandbox) StopDesktop(ctx context.Context) error {
	if err := s.sb.ComputerUse.Stop(ctx); err != nil {
		return fmt.Errorf("daytona: stop computer-use: %w", err)
	}
	return nil
}

// Display returns the real framebuffer geometry.
//
// Worth calling rather than trusting the configured resolution: an agent that
// emits normalised coordinates scales them by an assumed screen size, so any
// mismatch between the real framebuffer and the assumption displaces every
// single click.
func (s *Sandbox) Display(ctx context.Context) (Display, error) {
	// Via the generated client rather than the SDK helper, which returns an
	// untyped map and would mean string-keyed guesswork over the geometry that
	// every click depends on.
	info, _, err := s.sb.ToolboxClient.ComputerUseAPI.GetDisplayInfo(ctx).Execute()
	if err != nil {
		return Display{}, fmt.Errorf("daytona: display info: %w", err)
	}
	if info == nil || len(info.Displays) == 0 {
		return Display{}, fmt.Errorf("daytona: display info reported no displays")
	}

	// Prefer the active display; fall back to the first reported one.
	chosen := info.Displays[0]
	for _, d := range info.Displays {
		if d.IsActive != nil && *d.IsActive {
			chosen = d
			break
		}
	}

	d := Display{Count: len(info.Displays)}
	if chosen.Width != nil {
		d.Width = int(*chosen.Width)
	}
	if chosen.Height != nil {
		d.Height = int(*chosen.Height)
	}
	if d.Width == 0 || d.Height == 0 {
		return d, fmt.Errorf("daytona: display reported no geometry")
	}
	return d, nil
}

// CompressedScreenshot captures the screen, compressed.
//
// The high-level Go SDK only offers full-resolution captures, but the generated
// Toolbox client underneath it does expose the compressed endpoint, so this goes
// straight there rather than hand-rolling HTTP.
//
// Compression is not an optimisation here. A full-resolution PNG at 1280x800 is
// expensive on every single step, and E2E verification is already the costliest
// part of the loop.
func (s *Sandbox) CompressedScreenshot(ctx context.Context, showCursor bool) (Screenshot, error) {
	cfg := s.client.cfg

	req := s.sb.ToolboxClient.ComputerUseAPI.
		TakeCompressedScreenshot(ctx).
		Format(cfg.ScreenshotFormat).
		Quality(int32(cfg.ScreenshotQuality)).
		ShowCursor(showCursor)

	if cfg.ScreenshotScale > 0 && cfg.ScreenshotScale != 1 {
		req = req.Scale(float32(cfg.ScreenshotScale))
	}

	resp, _, err := req.Execute()
	if err != nil {
		return Screenshot{}, fmt.Errorf("daytona: compressed screenshot: %w", err)
	}

	return decodeScreenshot(resp, cfg.ScreenshotFormat)
}

// FullScreenshot captures the screen uncompressed. Present for the rare case
// where detail matters more than tokens; prefer CompressedScreenshot.
func (s *Sandbox) FullScreenshot(ctx context.Context, showCursor bool) (Screenshot, error) {
	resp, err := s.sb.ComputerUse.Screenshot().TakeFullScreen(ctx, &showCursor)
	if err != nil {
		return Screenshot{}, fmt.Errorf("daytona: screenshot: %w", err)
	}
	if resp == nil {
		return Screenshot{}, fmt.Errorf("daytona: screenshot response was empty")
	}

	data, err := decodeImage(resp.Image)
	if err != nil {
		return Screenshot{}, err
	}
	return Screenshot{Data: data, Format: "png", Width: resp.Width, Height: resp.Height}, nil
}

func decodeScreenshot(resp *toolbox.ScreenshotResponse, format string) (Screenshot, error) {
	if resp == nil || resp.Screenshot == nil {
		return Screenshot{}, fmt.Errorf("daytona: screenshot response was empty")
	}

	data, err := decodeImage(*resp.Screenshot)
	if err != nil {
		return Screenshot{}, err
	}
	return Screenshot{Data: data, Format: format}, nil
}

// decodeImage handles both bare base64 and data: URLs, since the two endpoints
// are not consistent about which they return.
func decodeImage(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, fmt.Errorf("daytona: screenshot payload was empty")
	}
	if _, after, ok := strings.Cut(encoded, "base64,"); ok {
		encoded = after
	}

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("daytona: decode screenshot: %w", err)
	}
	return data, nil
}

// ---------------------------------------------------------------- input

// The E2E verifier drives the desktop through these. Each one assumes the
// computer-use processes are already up; call EnsureDesktop first.

// Click clicks the left mouse button at the given coordinates.
//
// Coordinates are in the framebuffer's own pixel space, which is why callers
// should read geometry from Display rather than the configured resolution.
func (s *Sandbox) Click(ctx context.Context, x, y int) error {
	if _, err := s.sb.ComputerUse.Mouse().Click(ctx, x, y, nil, nil); err != nil {
		return fmt.Errorf("daytona: click at %d,%d: %w", x, y, err)
	}
	return nil
}

// DoubleClick double-clicks at the given coordinates.
func (s *Sandbox) DoubleClick(ctx context.Context, x, y int) error {
	double := true
	if _, err := s.sb.ComputerUse.Mouse().Click(ctx, x, y, nil, &double); err != nil {
		return fmt.Errorf("daytona: double click at %d,%d: %w", x, y, err)
	}
	return nil
}

// MoveMouse moves the cursor without clicking.
func (s *Sandbox) MoveMouse(ctx context.Context, x, y int) error {
	if _, err := s.sb.ComputerUse.Mouse().Move(ctx, x, y); err != nil {
		return fmt.Errorf("daytona: move mouse to %d,%d: %w", x, y, err)
	}
	return nil
}

// Scroll scrolls at the given coordinates. direction is "up" or "down".
func (s *Sandbox) Scroll(ctx context.Context, x, y int, direction string, amount int) error {
	if amount <= 0 {
		amount = 1
	}
	if direction != "up" && direction != "down" {
		return fmt.Errorf("daytona: scroll direction must be up or down, got %q", direction)
	}
	if _, err := s.sb.ComputerUse.Mouse().Scroll(ctx, x, y, direction, &amount); err != nil {
		return fmt.Errorf("daytona: scroll %s at %d,%d: %w", direction, x, y, err)
	}
	return nil
}

// TypeText types literal text. Newlines become Enter presses.
func (s *Sandbox) TypeText(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	if err := s.sb.ComputerUse.Keyboard().Type(ctx, text, nil); err != nil {
		return fmt.Errorf("daytona: type text: %w", err)
	}
	return nil
}

// PressKey presses a named key, optionally with modifiers.
//
// Accepts either a bare key ("enter") or a hotkey combination ("ctrl+c"), since
// a model will produce both and rejecting one form is a pointless failure.
func (s *Sandbox) PressKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("daytona: key is required")
	}

	if strings.Contains(key, "+") {
		if err := s.sb.ComputerUse.Keyboard().Hotkey(ctx, strings.ToLower(key)); err != nil {
			return fmt.Errorf("daytona: hotkey %q: %w", key, err)
		}
		return nil
	}

	if err := s.sb.ComputerUse.Keyboard().Press(ctx, strings.ToLower(key), nil); err != nil {
		return fmt.Errorf("daytona: press %q: %w", key, err)
	}
	return nil
}
