package cliproxy

import (
	"context"
	"strings"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func (s *Service) fetchCursorModelsForAuth(ctx context.Context, auth *coreauth.Auth) []*registry.ModelInfo {
	if auth == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	creds := cursorlib.CredentialsFromMetadata(auth.Metadata)
	if strings.TrimSpace(creds.AccessToken) == "" {
		// API-key credentials have no stored token yet: exchange eagerly so
		// the live model catalog (not just the builtin fallback) registers.
		apiKey := cursorAuthAPIKey(auth)
		if apiKey == "" {
			return nil
		}
		exchangeCtx, cancelExchange := context.WithTimeout(ctx, 25*time.Second)
		refreshed, err := cursorauth.NewAuthService().RefreshToken(exchangeCtx, apiKey, "", creds.BaseURL)
		cancelExchange()
		if err != nil {
			log.WithError(err).Warn("cursor: api key exchange failed; using builtin model list")
			return nil
		}
		if auth.Metadata == nil {
			auth.Metadata = map[string]any{}
		}
		auth.Metadata["access_token"] = refreshed.AccessToken
		if !refreshed.ExpiresAt.IsZero() {
			auth.Metadata["expired"] = refreshed.ExpiresAt.UTC().Format(time.RFC3339)
		}
		creds = cursorlib.CredentialsFromMetadata(auth.Metadata)
		log.Infof("cursor: exchanged api key for agent token (auth %s)", auth.ID)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	models, err := cursorlib.FetchAvailableModels(fetchCtx, creds)
	if err != nil {
		log.WithError(err).Warn("cursor: AvailableModels fetch failed; using builtin model list")
		return nil
	}
	if len(models) == 0 {
		log.Warn("cursor: AvailableModels returned empty catalog; using builtin model list")
		return nil
	}
	log.Infof("cursor: AvailableModels loaded %d models for auth %s", len(models), auth.ID)
	return cursorlib.CatalogToModelInfos(models)
}

// cursorAuthAPIKey returns the configured Cursor user API key for an auth.
func cursorAuthAPIKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
			return key
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["api_key"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
