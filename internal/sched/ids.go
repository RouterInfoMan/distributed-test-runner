package sched

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var idSanitizer = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// NewRegressionID mints a sortable, filesystem- and Nomad-safe identifier.
func NewRegressionID(name string) string {
	slug := strings.ToLower(idSanitizer.ReplaceAllString(name, "-"))
	slug = strings.Trim(slug, "-")
	if len(slug) > 24 {
		slug = slug[:24]
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	if slug == "" {
		return "reg-" + stamp + "-" + randHex(3)
	}
	return fmt.Sprintf("reg-%s-%s-%s", stamp, slug, randHex(3))
}

// SanitizeID normalizes a user-supplied regression id.
func SanitizeID(id string) string {
	return strings.Trim(idSanitizer.ReplaceAllString(id, "-"), "-")
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewToken mints the runner's callback bearer token.
func NewToken() string { return randHex(24) }

// NewRunID is unique per suite attempt.
func NewRunID() string { return "run-" + randHex(8) }
