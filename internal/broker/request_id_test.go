// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"encoding/json"
	"testing"
)

func TestSendRequestIDIsNotRPCResponseID(t *testing.T) {
	in := Request{ID: "rpc-response-1", Type: TypeSend, Send: &SendRequest{Name: "seat", Text: "hello", RequestID: "logical-host-request-2"}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Request
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.Send == nil || out.Send.RequestID != in.Send.RequestID {
		t.Fatalf("correlation identities changed: %+v", out)
	}
	if err := out.Send.Validate(); err != nil {
		t.Fatal(err)
	}
	if out.Send.RequestID != "logical-host-request-2" {
		t.Fatalf("validation rewrote host identity: %+v", out.Send)
	}
}
