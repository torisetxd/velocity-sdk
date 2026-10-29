package inference

import (
	"strings"

	"velocitylabs.cc/sdk/provider"
)

type Config struct {
	Type                    string               `yaml:"type" json:"type"`
	APIKey                  string               `yaml:"api_key" json:"api_key"`
	APIURL                  string               `yaml:"api_url" json:"api_url"`
	ExtraBody               map[string]any       `yaml:"extra_body" json:"extra_body"`
	UseExplicitCacheMarkers bool                 `yaml:"use_explicit_cache_markers" json:"use_explicit_cache_markers"`
	ReasoningField          string               `yaml:"reasoning_field" json:"reasoning_field"`
	Project                 string               `yaml:"project" json:"project"`
	Location                string               `yaml:"location" json:"location"`
	TokenSource             provider.TokenSource `yaml:"-" json:"-"`
}

func (c Config) Normalized() Config {
	c.APIURL = strings.TrimSpace(c.APIURL)
	c.Type = DetectType(c.Type, c.APIURL)
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.ReasoningField = strings.TrimSpace(c.ReasoningField)
	c.Project = strings.TrimSpace(c.Project)
	c.Location = strings.TrimSpace(c.Location)
	c.ExtraBody = provider.CopyMapAny(c.ExtraBody)
	return c
}
