package claudia

import (
	"encoding/json"
	"testing"
)

func TestProtectedPathRuleUnmarshalLegacyGrantKeys(t *testing.T) {
	var r ProtectedPathRule
	if err := json.Unmarshal([]byte(`{"Pattern":"**/bullseye.yaml","Reason":"T546"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Pattern != "**/bullseye.yaml" || r.Reason != "T546" {
		t.Fatalf("got pattern=%q reason=%q", r.Pattern, r.Reason)
	}
}
