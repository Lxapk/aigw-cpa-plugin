package main

import (
	"net/http"
	"net/url"
	"strings"
)

// Small helpers for the management HTTP surface.

// parseFormBody decodes an application/x-www-form-urlencoded body, which is what
// an HTML form posts. If the body looks like JSON it is left alone (the JSON
// endpoints parse it directly).
func parseFormBody(body []byte, headers http.Header) url.Values {
	values := url.Values{}
	if len(body) == 0 {
		return values
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		// Not a form submission; callers using JSON read Body themselves.
		return values
	}
	parsed, errParse := url.ParseQuery(trimmed)
	if errParse != nil {
		return values
	}
	for key, vals := range parsed {
		for _, v := range vals {
			values.Add(key, v)
		}
	}
	return values
}

// atoiDefault parses s as an int, returning fallback when it is empty or invalid.
func atoiDefault(s string, fallback int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	n := 0
	neg := false
	for i, r := range s {
		if i == 0 && (r == '-' || r == '+') {
			neg = r == '-'
			continue
		}
		if r < '0' || r > '9' {
			return fallback
		}
		n = n*10 + int(r-'0')
	}
	if neg {
		n = -n
	}
	return n
}
