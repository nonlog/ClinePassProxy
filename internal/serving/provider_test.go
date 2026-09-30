package serving

import (
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/admin"
)

func TestObserveChunkProvider(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name:   "nested stream final provider",
			chunks: []string{`{"choices":[{"delta":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"deepseek"}}}}}]}`},
			want:   "deepseek",
		},
		{
			name:   "wrapped nonstream final provider",
			chunks: []string{`{"success":true,"data":{"choices":[{"message":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"baseten"}}}}}]}}`},
			want:   "baseten",
		},
		{
			name:   "top-level final wins over serving provider",
			chunks: []string{`{"provider":"alibaba","provider_metadata":{"gateway":{"routing":{"finalProvider":"deepseek"}}},"choices":[]}`},
			want:   "deepseek",
		},
		{
			name:   "choice final precedes top-level final",
			chunks: []string{`{"provider":"alibaba","provider_metadata":{"gateway":{"routing":{"finalProvider":"top-level"}}},"choices":[{"delta":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"deepseek"}}}}}]}`},
			want:   "deepseek",
		},
		{
			name:   "serving provider fallback",
			chunks: []string{`{"provider":"Crusoe","choices":[{"delta":{"provider":"Relace"}}]}`},
			want:   "Crusoe",
		},
		{
			name:   "delta serving provider fallback",
			chunks: []string{`{"choices":[{"delta":{"provider":"Relace"}}]}`},
			want:   "Relace",
		},
		{
			name:   "message serving provider fallback",
			chunks: []string{`{"choices":[{"message":{"provider":"baseten"}}]}`},
			want:   "baseten",
		},
		{
			name: "later final overrides earlier serving provider",
			chunks: []string{
				`{"provider":"alibaba","choices":[{"delta":{}}]}`,
				`{"choices":[{"delta":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"deepseek"}}}}}]}`,
				`{"provider":"Crusoe","choices":[{"delta":{}}]}`,
			},
			want: "deepseek",
		},
		{
			name: "later serving does not overwrite fallback",
			chunks: []string{
				`{"provider":"Crusoe","choices":[]}`,
				`{"provider":"Relace","choices":[]}`,
			},
			want: "Crusoe",
		},
		{
			name:   "object provider does not hide final",
			chunks: []string{`{"provider":{"id":"not-a-provider"},"provider_metadata":{"gateway":{"routing":{"finalProvider":"deepseek"}}}}`},
			want:   "deepseek",
		},
		{
			name: "empty candidate and object fields are not actual providers",
			chunks: []string{
				`{}`,
				`{"provider":"","provider_metadata":{"gateway":{"routing":{"resolvedProvider":"deepseek","fallbacksAvailable":["baseten"]}}}}`,
				`{"provider":{"id":"Crusoe"},"choices":[{"delta":{"provider":{"id":"Relace"},"provider_metadata":{"gateway":{"routing":{"finalProvider":["deepseek"]}}}}}]}`,
			},
		},
		{
			name:   "provider strings trimmed",
			chunks: []string{`{"provider":"  ","choices":[{"delta":{"provider":"  Crusoe  "}}]}`},
			want:   "Crusoe",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := &requestContext{tracker: admin.NewTracker()}
			server := &Server{}
			for _, chunk := range test.chunks {
				server.observeChunk(request, []byte(chunk))
			}
			if got := request.tracker.Record().Provider; got != test.want {
				t.Fatalf("provider=%q, want %q", got, test.want)
			}
		})
	}
}
