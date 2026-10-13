package provider

import (
	"fmt"
	"strings"
	"time"
)

func FormatUsageDollars(amount float64) string {
	if amount > 0 && amount < 0.01 {
		return fmt.Sprintf("$%.4f", amount)
	}
	return fmt.Sprintf("$%.2f", amount)
}

func UsageAvailability(info UsageInfo) (string, bool) {
	if info.AccountCredits != nil && *info.AccountCredits <= 0 {
		return "out of credits", true
	}
	now := time.Now()
	var parts []string
	var blocking UsageWindowReset
	blocked := false
	for _, window := range info.Resets {
		parts = append(parts, fmt.Sprintf("%s %d%% used", window.Window, window.UsedPercent))
		if window.UsedPercent >= 100 && (window.ResetAt == 0 || time.Unix(window.ResetAt, 0).After(now)) {
			if !blocked || window.ResetAt == 0 || (blocking.ResetAt != 0 && window.ResetAt > blocking.ResetAt) {
				blocking = window
			}
			blocked = true
		}
	}
	if info.UsableRequests != nil {
		parts = append(parts, fmt.Sprintf("%d requests left", *info.UsableRequests))
		blocked = blocked || *info.UsableRequests <= 0
	}
	if info.Credits != nil {
		label := info.CreditsLabel
		if label == "" {
			label = "credits left"
		}
		amount := fmt.Sprintf("%.2f", *info.Credits)
		if info.CreditsLabel != "" {
			amount = FormatUsageDollars(*info.Credits)
		}
		parts = append(parts, amount+" "+label)
		blocked = blocked || *info.Credits <= 0
	}
	if blocked {
		if blocking.ResetAt > 0 {
			return fmt.Sprintf("out of usage · %s resets %s", blocking.Window, time.Unix(blocking.ResetAt, 0).Local().Format("Jan 2 15:04")), true
		}
		return "out of usage", true
	}
	if info.AccountCredits != nil {
		parts = append(parts, FormatUsageDollars(*info.AccountCredits)+" balance")
	}
	if len(parts) == 0 {
		return info.Label, false
	}
	return strings.Join(parts, " · "), false
}
