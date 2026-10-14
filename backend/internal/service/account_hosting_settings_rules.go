package service

import (
	"math"
	"strings"
)

func hostingIntegerBetween(value any, min, max float64) bool {
	n, ok := value.(float64)
	return ok && !math.IsNaN(n) && n >= min && n <= max && math.Trunc(n) == n
}

func validHostingTempUnschedulableRules(value any) bool {
	rules, ok := value.([]any)
	if !ok || len(rules) > 20 {
		return false
	}
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok || !hostingIntegerBetween(rule["error_code"], 400, 599) || !hostingIntegerBetween(rule["duration_minutes"], 1, 10080) {
			return false
		}
		for key := range rule {
			if key != "error_code" && key != "duration_minutes" && key != "keywords" && key != "description" {
				return false
			}
		}
		keywords, ok := rule["keywords"].([]any)
		if !ok || len(keywords) == 0 || len(keywords) > 20 {
			return false
		}
		for _, raw := range keywords {
			keyword, ok := raw.(string)
			if !ok || strings.TrimSpace(keyword) == "" || len(keyword) > 256 {
				return false
			}
		}
		if raw, exists := rule["description"]; exists {
			text, ok := raw.(string)
			if !ok || len([]rune(text)) > 256 {
				return false
			}
		}
	}
	return true
}
