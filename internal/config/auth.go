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
