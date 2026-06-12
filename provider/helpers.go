package provider

import (
	"net/url"
	"strconv"
	"strings"
)

// ParseLocalID splits a namespaced source-local ID of the form "<kind>:<raw>"
// into its kind and raw components.
//
// Returns (kind, raw, ok). ok is false when the input lacks a ":" separator,
// starts with ":", or ends with ":".
func ParseLocalID(id string) (kind, raw string, ok bool) {
	idx := strings.Index(id, ":")
	if idx <= 0 || idx == len(id)-1 {
		return "", "", false
	}
	return id[:idx], id[idx+1:], true
}

// EncodeAbsolute prepares an absolute URL for storage as a source-relative
// image path. The whole URL is percent-escaped so the aggregator can safely
// wrap it in adult://<slug>/<role>/<path> without the embedded "://" and "/"
// breaking DecodeImagePath.
//
// An empty input returns an empty string.
func EncodeAbsolute(absURL string) string {
	if absURL == "" {
		return ""
	}
	return url.PathEscape(absURL)
}

// YearFromDate extracts the 4-digit year prefix from an ISO-style date string
// (e.g. "2021-05-09"). Returns 0 when the string is too short or the first
// four characters are not all digits.
func YearFromDate(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return y
}

// NamesToGenres maps a slice of arbitrary tag-like values to a de-duplicated
// list of non-empty genre names using the supplied name accessor. Source
// packages call this with their own tag DTO type and a "func(tag) string".
func NamesToGenres[T any](tags []T, name func(T) string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if n := name(t); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// SanitizeImageURL validates a fully-resolved image URL before it is handed
// back to the host to fetch or proxy. It is the last line of defence against a
// compromised or misbehaving upstream coaxing the host into fetching an
// arbitrary (e.g. internal) URL.
//
// The URL must parse, be absolute, carry a non-empty host, and use one of the
// allowed schemes. Anything else returns "". Pass httpsOnly=true for upstreams
// whose images are always served over TLS (e.g. ThePornDB CDNs); pass false
// for self-hosted upstreams that may legitimately be plain HTTP on a LAN
// (e.g. Stash).
func SanitizeImageURL(raw string, httpsOnly bool) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return raw
	case "http":
		if httpsOnly {
			return ""
		}
		return raw
	default:
		return ""
	}
}
