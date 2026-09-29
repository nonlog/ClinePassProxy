package affinity

import (
	"fmt"
	"testing"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/credentials"
)

func TestSessionAffinitySurvivesPoolChange(t *testing.T) {
	full := []credentials.Record{
		{ID: "a", Label: "a", Enabled: true},
		{ID: "b", Label: "b", Enabled: true},
		{ID: "c", Label: "c", Enabled: true},
		{ID: "d", Label: "d", Enabled: true},
	}
	// The same pool after "c" is disabled or deleted.
	reduced := []credentials.Record{full[0], full[1], full[3]}

	selector := NewSelector()
	moved := 0
	const total = 300
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("prompt-cache-key-%d", i)
		beforeIndex, before := selector.Select("session", key, full, 0, nil)
		afterIndex, after := selector.Select("session", key, reduced, 0, nil)
		if before.CredentialID == after.CredentialID {
			continue
		}
		moved++
		if full[beforeIndex].ID != "c" {
			t.Fatalf("session %s moved off a healthy credential: %s -> %s", key, before.CredentialID, after.CredentialID)
		}
		if reduced[afterIndex].ID == "c" {
			t.Fatalf("removed credential was still selected for %s", key)
		}
	}
	if moved == 0 || moved > total/2 {
		t.Fatalf("pool change remapped %d of %d sessions", moved, total)
	}
}

func farFuture() time.Time { return time.Now().Add(time.Hour) }

func TestSessionAffinityIsStable(t *testing.T) {
	list := []credentials.Record{
		{ID: "a", Label: "a", Enabled: true},
		{ID: "b", Label: "b", Enabled: true},
		{ID: "c", Label: "c", Enabled: true},
	}
	selector := NewSelector()
	firstIndex, first := selector.Select("session", "prompt-cache-key-42", list, 1, nil)
	if first.CredentialID == "" {
		t.Fatal("expected a credential to be selected")
	}
	for i := 0; i < 50; i++ {
		index, decision := selector.Select("session", "prompt-cache-key-42", list, uint64(i+2), nil)
		if index != firstIndex || decision.CredentialID != first.CredentialID {
			t.Fatalf("affinity drifted: %v -> %v", first.CredentialID, decision.CredentialID)
		}
	}
}

func TestCooldownDeprioritizes(t *testing.T) {
	list := []credentials.Record{
		{ID: "a", Label: "a", Enabled: true, CooldownUntil: farFuture()},
		{ID: "b", Label: "b", Enabled: true},
	}
	selector := NewSelector()
	index, decision := selector.Select("session", "any", list, 0, nil)
	if list[index].ID != "b" {
		t.Fatalf("expected cooled credential to rank last, got %s via %s", decision.CredentialID, decision.Reason)
	}
}

func TestRoundRobinRotates(t *testing.T) {
	list := []credentials.Record{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}}
	selector := NewSelector()
	seen := map[string]bool{}
	for rotor := uint64(0); rotor < 4; rotor++ {
		index, _ := selector.Select("round-robin", "", list, rotor, nil)
		seen[list[index].ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("round robin did not rotate: %v", seen)
	}
}
