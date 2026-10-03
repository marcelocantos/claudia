// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func (d *Daemon) handleRecoveryDetail(c *broker.ClientConn, req *broker.Request) {
	provider := req.AuthDetail.Name
	result := claudia.OMPRecoveryResult{Provider: provider, Outcome: "failure", Classification: "unsupported_provider"}
	if claudia.IsOMPPlan(provider) {
		result.Classification = "busy"
		if d.reauthMu.TryLock() {
			defer d.reauthMu.Unlock()
			ctx, cancel := context.WithTimeout(d.ctx, 3*time.Minute)
			defer cancel()
			recover := d.authRecoverDetail
			if recover == nil {
				recover = claudia.RecoverOMPPlanDetailed
			}
			result = recover(ctx, provider)
		}
	}
	reply := &broker.AuthRecoveryDetailResponse{Provider: provider, Outcome: result.Outcome, Classification: result.Classification}
	if reply.Validate() != nil {
		reply.Outcome = "failure"
		reply.Classification = "refresh_failed"
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeAuthRecoveryDetail, AuthDetail: reply})
}
