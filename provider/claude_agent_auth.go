package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeAgentStatus reports what the local Claude Code binary is signed into.
// The host reads this instead of touching Claude credentials: the binary owns
// the login, and every request it makes is authenticated by itself.
type ClaudeAgentStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	Email            string `json:"email"`
	OrgName          string `json:"orgName"`
	SubscriptionType string `json:"subscriptionType"`
	ConfigDirectory  string `json:"configDirectory"`
}

// PlanSignedIn reports whether the binary is authenticated with a Claude
// subscription rather than an API key from the environment.
func (s ClaudeAgentStatus) PlanSignedIn() bool {
	return s.LoggedIn && (s.AuthMethod == "claude.ai" || s.AuthMethod == "oauth_token")
}

// Describe renders a short human-readable summary of the login.
func (s ClaudeAgentStatus) Describe() string {
	if !s.LoggedIn {
		return "Claude Code is not signed in"
	}
	account := strings.TrimSpace(s.Email)
	if org := strings.TrimSpace(s.OrgName); org != "" && org != account {
		account = strings.Join([]string{account, org}, " · ")
	}
	plan := strings.TrimSpace(s.SubscriptionType)
	if plan == "" {
		plan = s.AuthMethod
	}
	if account == "" {
		return plan
	}
	return plan + " · " + account
}

// CheckClaudeAgent asks the Claude Code binary about its own login state.
func CheckClaudeAgent(ctx context.Context, command string) (ClaudeAgentStatus, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		command = DefaultClaudeCommand
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return ClaudeAgentStatus{}, fmt.Errorf("%s was not found on PATH; install Claude Code first", command)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "auth", "status")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A signed-out binary exits non-zero, which is a status rather than a failure.
	_ = cmd.Run()

	var status ClaudeAgentStatus
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &status); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		return ClaudeAgentStatus{}, fmt.Errorf("claude auth status: %s", detail)
	}
	return status, nil
}
