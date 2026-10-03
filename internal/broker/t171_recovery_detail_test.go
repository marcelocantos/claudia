// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"errors"
	"testing"
)

func TestT171DetailedRecoveryWireCompatibility(t *testing.T) {
	// The old response schema is unchanged and still rejects extra fields.
	if _, err := ParseResponse([]byte(`{"v":1,"type":"auth_recovered","body":{"name":"anthropic","outcome":"refreshed"}}`)); err == nil {
		t.Fatal("legacy response schema widened")
	}
	if _, err := ParseRequest([]byte(`{"v":1,"type":"auth_recover_detail","body":{"name":"anthropic","login":true}}`)); err == nil {
		t.Fatal("new operation accepted interactive login")
	}
	raw := []byte(`{"v":1,"type":"auth_recover_detail","body":{"name":"anthropic"}}`)
	// Model the previous dispatch table, whose only difference is no new op.
	spec := requestSpecs[TypeAuthRecoverDetail]
	delete(requestSpecs, TypeAuthRecoverDetail)
	defer func() { requestSpecs[TypeAuthRecoverDetail] = spec }()
	_, err := ParseRequest(raw)
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Code != CodeUnknownType {
		t.Fatalf("old daemon: %v", err)
	}
}
