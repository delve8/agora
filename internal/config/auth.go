package config

import (
	"fmt"
	"net"
	"strings"
)

const (
	AuthModeLocal = "local"
	AuthModeLogto = "logto"
)

type ServerAuthConfig struct {
	Mode         string
	Issuer       string
	Audience     string
	Provisioning bool
}

// LogtoClientConfig is the part of a Logto deployment the Web UI needs. The
// Server serves it from /api/config, so one built bundle can serve any tenant
// instead of inlining the settings at build time.
type LogtoClientConfig struct {
	Endpoint string
	AppID    string
	Audience string
}

// ResolveLogtoClientConfig derives the client settings from the server side
// environment. The SPA endpoint defaults to the issuer without its /oidc suffix,
// which is Logto's convention, so most deployments only need to provide the
// issuer, the API audience and the SPA application id.
func ResolveLogtoClientConfig(issuer, endpoint, appID, audience string) LogtoClientConfig {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
		endpoint = strings.TrimSuffix(issuer, "/oidc")
	}
	return LogtoClientConfig{
		Endpoint: endpoint,
		AppID:    strings.TrimSpace(appID),
		Audience: strings.TrimSpace(audience),
	}
}

// Complete reports whether the Web UI has everything it needs to start a Logto
// sign-in. Without an application id there is no client to authenticate with.
func (c LogtoClientConfig) Complete() bool {
	return c.Endpoint != "" && c.AppID != ""
}

func ResolveServerAuthConfig(mode, issuer, audience, provisioning string) (ServerAuthConfig, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = AuthModeLocal
	}
	if mode != AuthModeLocal && mode != AuthModeLogto {
		return ServerAuthConfig{}, fmt.Errorf("unsupported AGORA_AUTH_MODE %q", mode)
	}
	cfg := ServerAuthConfig{Mode: mode, Issuer: strings.TrimSpace(issuer), Audience: strings.TrimSpace(audience), Provisioning: strings.EqualFold(strings.TrimSpace(provisioning), "enabled")}
	if mode == AuthModeLogto && (cfg.Issuer == "" || cfg.Audience == "") {
		return ServerAuthConfig{}, fmt.Errorf("AGORA_LOGTO_ISSUER and AGORA_LOGTO_AUDIENCE are required in logto mode")
	}
	return cfg, nil
}

func IsLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr)
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ValidateServerAddress(addr, mode string) error {
	if strings.EqualFold(strings.TrimSpace(mode), AuthModeLocal) && !IsLoopbackAddress(addr) {
		return fmt.Errorf("local auth mode requires a loopback listen address")
	}
	return nil
}
