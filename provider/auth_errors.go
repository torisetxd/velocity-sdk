package provider

import "strings"

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") || (strings.Contains(msg, "token") && strings.Contains(msg, "expired"))
}
