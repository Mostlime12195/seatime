package server

import (
	"fmt"

	"seatime/pkg/ledger"
	"seatime/pkg/promptcheck"
)

// darkFlowWhy says why AI time was not counted, under the submission's rule.
func darkFlowWhy(scoreVer string, r *promptcheck.Rules) string {
	if scoreVer == ledger.CapVersion && r != nil {
		return "no prompt the AI check passed covers it"
	}
	return fmt.Sprintf("more than %s after the person's last heartbeat", mins(ledger.Window.Minutes()))
}

func mins(m float64) string {
	if m == float64(int(m)) {
		return fmt.Sprintf("%d min", int(m))
	}
	return fmt.Sprintf("%.1f min", m)
}
