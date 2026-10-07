package gates

import (
	"strconv"
)

// Settings keys in settings_kv (Board-only writes via /api/settings/security-gate).
const (
	// SettingAdvisorEnabled is the kill switch for the per-request Together
	// advisory. Default on.
	SettingAdvisorEnabled = "gates.advisor_enabled"
	// SettingPasskeyGraceMinutes is how long one Touch ID covers further gate
	// actions (0 = off, max 5). Default 2.
	SettingPasskeyGraceMinutes = "gates.passkey_grace_minutes"
)

// DefaultGraceMinutes and MaxGraceMinutes bound the passkey grace window.
const (
	DefaultGraceMinutes = 2
	MaxGraceMinutes     = 5
)

func getSetting(db Execer, key string) (string, bool) {
	var v string
	if err := db.QueryRow(`SELECT value FROM settings_kv WHERE key = ?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

// AdvisorEnabled reports the advisory kill switch (on unless set to "false").
func AdvisorEnabled(db Execer) bool {
	v, ok := getSetting(db, SettingAdvisorEnabled)
	return !ok || v != "false"
}

// GraceMinutes returns the passkey grace window in minutes, clamped to 0..5.
func GraceMinutes(db Execer) int {
	v, ok := getSetting(db, SettingPasskeyGraceMinutes)
	if !ok {
		return DefaultGraceMinutes
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	if n > MaxGraceMinutes {
		return MaxGraceMinutes
	}
	return n
}
