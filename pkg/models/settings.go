package models

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
)

const (
	DefaultApiUrl  = "https://api.hotdata.dev"
	DefaultDialect = "postgres"
)

type PluginSettings struct {
	ApiUrl            string                `json:"apiUrl"`
	WorkspaceID       string                `json:"workspaceId"`
	DefaultDatabaseID string                `json:"defaultDatabaseId"`
	DefaultDialect    string                `json:"defaultDialect"`
	Secrets           *SecretPluginSettings `json:"-"`
}

type SecretPluginSettings struct {
	ApiKey string `json:"apiKey"`
}

func LoadPluginSettings(source backend.DataSourceInstanceSettings) (*PluginSettings, error) {
	settings := PluginSettings{}
	err := json.Unmarshal(source.JSONData, &settings)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal PluginSettings json: %w", err)
	}

	if settings.ApiUrl == "" {
		settings.ApiUrl = DefaultApiUrl
	}
	// The CLI convention is a base URL that includes /v1; the client adds /v1
	// itself, so accept either form.
	settings.ApiUrl = strings.TrimSuffix(strings.TrimSuffix(settings.ApiUrl, "/"), "/v1")

	// The ConfigEditor shows PostgreSQL as the default but only persists it if
	// the user changes the field, so default it here to match the UI and SPEC.
	if settings.DefaultDialect == "" {
		settings.DefaultDialect = DefaultDialect
	}

	settings.Secrets = loadSecretPluginSettings(source.DecryptedSecureJSONData)

	return &settings, nil
}

func loadSecretPluginSettings(source map[string]string) *SecretPluginSettings {
	return &SecretPluginSettings{
		ApiKey: source["apiKey"],
	}
}
