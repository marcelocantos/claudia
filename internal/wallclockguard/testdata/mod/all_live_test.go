package fixture

import (
	"context"
	"time"
)

// Every test in a *_live_test.go file is live by the old filename rule.
func useClock() {
	_, _ = context.WithTimeout(context.Background(), time.Minute)
}
