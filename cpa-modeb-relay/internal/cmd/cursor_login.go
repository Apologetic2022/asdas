package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	log "github.com/sirupsen/logrus"
)

// DoCursorLogin triggers Cursor PKCE login and saves tokens into the auth directory.
func DoCursorLogin(cfg *config.Config, options *LoginOptions) {
	if options == nil {
		options = &LoginOptions{}
	}

	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{},
		Prompt:    options.Prompt,
	}

	record, savedPath, err := manager.Login(context.Background(), "cursor", cfg, authOpts)
	if err != nil {
		log.Errorf("Cursor authentication failed: %v", err)
		return
	}

	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	fmt.Println("Cursor authentication successful!")
}

// DoCursorAPIKey stores a Cursor user API key in the cursor-api-key config block.
// The relay exchanges the key for short-lived Agent Connect JWTs at runtime, so
// this is the non-interactive alternative to DoCursorLogin.
func DoCursorAPIKey(cfg *config.Config, configFilePath, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		log.Error("Cursor API key is empty")
		return
	}
	if cfg == nil {
		log.Error("no configuration loaded; cannot store the Cursor API key")
		return
	}

	for i := range cfg.CursorKey {
		if strings.TrimSpace(cfg.CursorKey[i].APIKey) == apiKey && strings.TrimSpace(cfg.CursorKey[i].BaseURL) == "" {
			fmt.Printf("Cursor API key already present in %s\n", configFilePath)
			return
		}
	}

	cfg.CursorKey = append(cfg.CursorKey, config.CursorKey{APIKey: apiKey})
	cfg.SanitizeCursorKeys()

	if err := config.SaveConfigPreserveComments(configFilePath, cfg); err != nil {
		log.Errorf("failed to save the Cursor API key: %v", err)
		return
	}
	fmt.Printf("Cursor API key saved to %s (%d configured)\n", configFilePath, len(cfg.CursorKey))
}
