// Package util holds the tiny string helpers several packages used to
// define privately (orElse/trunc/envOr/truncate). One copy, no imports.
package util

// Or returns v unless it is empty, in which case def.
func Or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// Trunc shortens s to n bytes (not runes) and appends an ellipsis — for
// log lines that must never carry unbounded upstream payloads.
func Trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
