// Package affinity selects a credential for a request.
//
// Selection must be stable: a warm prompt cache is bound to the credential that
// served the previous turn, so changing credentials mid-conversation discards
// the cache and multiplies time-to-first-token.
package affinity

import (
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/credentials"
)

// Decision records why a credential was chosen, for diagnostics.
type Decision struct {
	CredentialID string `json:"credential_id"`
	Label        string `json:"label"`
	AffinityKey  string `json:"affinity_key"`
	Reason       string `json:"reason"`
}

// Reasons reported by Select.
const (
	ReasonSessionHash = "session-hash"
	ReasonRoundRobin  = "round-robin"
	ReasonSingle      = "single-credential"
	ReasonSticky      = "sticky"
	ReasonCooldown    = "cooldown-skip"
)

// Selector chooses a credential index within a credential slice.
type Selector struct {
	mu       sync.Mutex
	sticky   map[string]string // affinity key -> credential ID
	stickyAt map[string]time.Time
}

// NewSelector builds a selector with sticky state.
func NewSelector() *Selector {
	return &Selector{sticky: map[string]string{}, stickyAt: map[string]time.Time{}}
}

// Select returns the index of the credential to use, or -1 when none is usable.
// ignore excludes credential IDs already attempted for this request.
func (s *Selector) Select(mode, affinityKey string, list []credentials.Record, rotor uint64, ignore map[string]bool) (int, Decision) {
	candidates := make([]int, 0, len(list))
	for i, record := range list {
		if ignore != nil && ignore[record.ID] {
			continue
		}
		candidates = append(candidates, i)
	}
	if len(candidates) == 0 {
		return -1, Decision{Reason: ReasonRoundRobin}
	}

	affinityKey = strings.TrimSpace(affinityKey)
	now := time.Now()

	// Cooled-down credentials stay in the pool, but rank last so a rate-limited
	// credential is not retried while a healthy one is available.
	preferred := make([]int, 0, len(candidates))
	cooled := make([]int, 0)
	for _, index := range candidates {
		if until := list[index].CooldownUntil; !until.IsZero() && until.After(now) {
			cooled = append(cooled, index)
			continue
		}
		preferred = append(preferred, index)
	}
	pool := preferred
	reason := ReasonSessionHash
	if len(pool) == 0 {
		pool = cooled
		reason = ReasonCooldown
	}

	if len(pool) == 1 {
		index := pool[0]
		return index, Decision{
			CredentialID: list[index].ID,
			Label:        list[index].Label,
			AffinityKey:  affinityKey,
			Reason:       ReasonSingle,
		}
	}

	switch mode {
	case "round-robin":
		index := pool[int(rotor%uint64(len(pool)))]
		return index, Decision{
			CredentialID: list[index].ID,
			Label:        list[index].Label,
			AffinityKey:  affinityKey,
			Reason:       ReasonRoundRobin,
		}
	case "sticky":
		if affinityKey != "" {
			s.mu.Lock()
			id := s.sticky[affinityKey]
			s.mu.Unlock()
			for _, index := range pool {
				if list[index].ID == id {
					return index, Decision{CredentialID: id, Label: list[index].Label, AffinityKey: affinityKey, Reason: ReasonSticky}
				}
			}
			index := pool[0]
			s.remember(affinityKey, list[index].ID)
			return index, Decision{CredentialID: list[index].ID, Label: list[index].Label, AffinityKey: affinityKey, Reason: ReasonSticky}
		}
		index := pool[int(rotor%uint64(len(pool)))]
		return index, Decision{CredentialID: list[index].ID, Label: list[index].Label, Reason: ReasonRoundRobin}
	default: // session
		if affinityKey != "" {
			index := pool[hashIndex(affinityKey, len(pool))]
			return index, Decision{CredentialID: list[index].ID, Label: list[index].Label, AffinityKey: affinityKey, Reason: reason}
		}
		index := pool[int(rotor%uint64(len(pool)))]
		return index, Decision{CredentialID: list[index].ID, Label: list[index].Label, Reason: ReasonRoundRobin}
	}
}

func (s *Selector) remember(key, credentialID string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sticky[key] = credentialID
	s.stickyAt[key] = time.Now()
	const maxSticky = 20000
	if len(s.sticky) > maxSticky {
		cutoff := time.Now().Add(-6 * time.Hour)
		for existing, at := range s.stickyAt {
			if at.Before(cutoff) {
				delete(s.sticky, existing)
				delete(s.stickyAt, existing)
			}
		}
	}
}

func hashIndex(key string, size int) int {
	if size <= 1 {
		return 0
	}
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(key))
	return int(hasher.Sum64() % uint64(size))
}
