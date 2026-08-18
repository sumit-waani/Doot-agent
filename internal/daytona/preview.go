package daytona

import (
	"context"
	"fmt"
	"time"
)

// Preview is a link to a port inside the sandbox.
type Preview struct {
	URL       string
	Port      int
	ExpiresAt time.Time
	// Signed reports whether the token is embedded in the URL and therefore
	// survives a sandbox restart.
	Signed bool
}

// SignedPreview returns a preview link whose token is embedded in the URL.
//
// Signed links are used rather than standard ones because the standard link's
// token is reset on every sandbox restart, which would mean re-fetching the URL
// after each wake. The whole point is handing myself a link that still works
// later from a phone, and a link that dies on restart defeats that.
//
// The default expiry is short, so it is always set explicitly.
func (s *Sandbox) SignedPreview(ctx context.Context, port int) (Preview, error) {
	if port <= 0 || port > 65535 {
		return Preview{}, fmt.Errorf("daytona: preview port %d is out of range", port)
	}

	ttl := s.client.cfg.PreviewTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	seconds := int(ttl.Seconds())

	link, err := s.sb.GetSignedPreviewLink(ctx, port, seconds)
	if err != nil {
		return Preview{}, fmt.Errorf("daytona: signed preview link for port %d: %w", port, err)
	}
	if link == nil || link.URL == "" {
		return Preview{}, fmt.Errorf("daytona: signed preview link for port %d was empty", port)
	}

	return Preview{
		URL:       link.URL,
		Port:      port,
		ExpiresAt: time.Now().UTC().Add(ttl),
		Signed:    true,
	}, nil
}

// Preview returns the standard preview link and its token.
//
// Kept for completeness. The token resets whenever the sandbox restarts, so
// prefer SignedPreview for anything shown in the UI.
func (s *Sandbox) Preview(ctx context.Context, port int) (Preview, string, error) {
	link, err := s.sb.GetPreviewLink(ctx, port)
	if err != nil {
		return Preview{}, "", fmt.Errorf("daytona: preview link for port %d: %w", port, err)
	}
	if link == nil || link.URL == "" {
		return Preview{}, "", fmt.Errorf("daytona: preview link for port %d was empty", port)
	}
	return Preview{URL: link.URL, Port: port, Signed: false}, link.Token, nil
}
