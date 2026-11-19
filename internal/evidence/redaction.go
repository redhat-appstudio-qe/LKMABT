package evidence

import (
	"encoding/json"
	"strings"
)

const redactedValue = "[REDACTED]"

var sensitiveFragments = []string{"token", "password", "secret", "authorization", "credential"}

// IsSensitiveKey reports whether key contains a credential-related fragment,
// matched case-insensitively as a substring. Values are never inspected.
func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range sensitiveFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// Redact recursively replaces values under sensitive map keys with a redaction
// marker. It traverses only map[string]any and []any values, so callers holding
// typed structs should use RedactedJSON, which normalizes them through JSON
// first.
func Redact(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if IsSensitiveKey(key) {
				result[key] = redactedValue
			} else {
				result[key] = Redact(item)
			}
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = Redact(item)
		}
		return result
	default:
		return value
	}
}

// RedactedJSON marshals value as two-space-indented JSON after redacting
// sensitive map keys. Matching uses JSON key names only; free-form values such
// as condition messages or log text are not inspected. The result has no
// trailing newline.
func RedactedJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, err
	}
	return json.MarshalIndent(Redact(generic), "", "  ")
}
