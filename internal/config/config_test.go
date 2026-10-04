package config

import (
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

func TestUnverifiedProviderSelectionsAreDropped(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := store.Get()
	settings.Models = []models.Entry{
		{ID: "legacy", UpstreamID: "cline-pass/legacy", Providers: []string{"history-provider"}},
		{ID: "verified", UpstreamID: "cline-pass/verified", Providers: []string{"alibaba"}, ProviderPipeline: "planner"},
	}
	updated, err := store.Update(settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Models[0].Providers) != 0 {
		t.Fatalf("unverified providers survived normalization: %#v", updated.Models[0].Providers)
	}
	if len(updated.Models[1].Providers) != 1 || updated.Models[1].Providers[0] != "alibaba" {
		t.Fatalf("verified providers were dropped: %#v", updated.Models[1].Providers)
	}
}
