package provider

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/oauth2/google"
)

const googleCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// GoogleTokenSource returns a TokenSource that mints and refreshes Google
// OAuth access tokens. credentials is service-account (or authorized-user)
// JSON, or the path of such a file; when empty, Application Default
// Credentials are used.
func GoogleTokenSource(credentials string) (TokenSource, error) {
	credentials = strings.TrimSpace(credentials)
	var data []byte
	switch {
	case credentials == "":
	case strings.HasPrefix(credentials, "{"):
		data = []byte(credentials)
	default:
		if rest, ok := strings.CutPrefix(credentials, "~/"); ok {
			if home, err := os.UserHomeDir(); err == nil {
				credentials = home + "/" + rest
			}
		}
		raw, err := os.ReadFile(credentials)
		if err != nil {
			return nil, fmt.Errorf("read google credentials: %w", err)
		}
		data = raw
	}

	ctx := context.Background()
	var creds *google.Credentials
	var err error
	if data != nil {
		creds, err = google.CredentialsFromJSON(ctx, data, googleCloudPlatformScope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, googleCloudPlatformScope)
	}
	if err != nil {
		return nil, fmt.Errorf("load google credentials: %w", err)
	}

	source := creds.TokenSource
	return func(context.Context) (string, error) {
		token, err := source.Token()
		if err != nil {
			return "", fmt.Errorf("refresh google access token: %w", err)
		}
		return token.AccessToken, nil
	}, nil
}
