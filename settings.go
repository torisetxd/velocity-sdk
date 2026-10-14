// Package sdk holds process-wide settings shared by the SDK's packages.
package sdk

import "sync/atomic"

// Settings identifies the host application. Call Configure once at startup,
// before making requests. Empty strings fall back to Defaults().
type Settings struct {
	// AppName and AppURL attribute requests to the host app on providers that
	// support it (OpenRouter's X-Title and HTTP-Referer). Defaults send none.
	AppName string
	AppURL  string
	// CacheKeyPrefix prefixes session prompt cache keys.
	CacheKeyPrefix string
	// TempPrefix prefixes temporary files and directories the SDK creates.
	TempPrefix string
	// DebugEnv names the environment variable enabling stream debug traces
	// (and error payload dumps); DebugEnv+"_RAW" enables raw payload traces.
	DebugEnv string
	// DisableDebugEnv ignores DebugEnv entirely.
	DisableDebugEnv bool
}

// Defaults returns the settings used when Configure is never called.
func Defaults() Settings {
	return Settings{
		CacheKeyPrefix: "velocity-session-",
		TempPrefix:     "velocity-",
		DebugEnv:       "VELOCITY_STREAM_DEBUG",
	}
}

var current atomic.Pointer[Settings]

// Configure replaces the process-wide settings.
func Configure(s Settings) {
	d := Defaults()
	if s.CacheKeyPrefix == "" {
		s.CacheKeyPrefix = d.CacheKeyPrefix
	}
	if s.TempPrefix == "" {
		s.TempPrefix = d.TempPrefix
	}
	if s.DebugEnv == "" {
		s.DebugEnv = d.DebugEnv
	}
	current.Store(&s)
}

// Current returns the active settings.
func Current() Settings {
	if s := current.Load(); s != nil {
		return *s
	}
	return Defaults()
}

// DebugValue returns the value of the debug environment variable with the
// given suffix ("" or "_RAW"), or "" when debug env lookup is disabled.
func (s Settings) DebugValue(lookup func(string) string, suffix string) string {
	if s.DisableDebugEnv {
		return ""
	}
	return lookup(s.DebugEnv + suffix)
}
