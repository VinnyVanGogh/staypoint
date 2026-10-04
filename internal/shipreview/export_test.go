package shipreview

import "time"

// Test-only access to unexported cleanup helpers (STA-648).

var DeleteRemoteBranchAt = deleteRemoteBranchAt

// SetDevSetupWait overrides how long cleanup waits for a canceled dev setup
// and returns a func restoring the previous value.
func SetDevSetupWait(d time.Duration) (restore func()) {
	old := devSetupWait
	devSetupWait = d
	return func() { devSetupWait = old }
}
