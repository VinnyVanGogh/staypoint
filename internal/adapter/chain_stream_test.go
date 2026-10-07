package adapter

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// chainParser runs ParseChainStreamDelta through the ProviderAdapter shape
// parseFixture expects.
type chainParser struct{ ClaudeAdapter }

func (chainParser) ParseStreamDelta(line []byte) ([]StreamDelta, error) {
	return ParseChainStreamDelta(line)
}

// A failover chain run (gemini > claude) must parse whichever CLI answered.
// Parsing agy output as Claude dropped every event, so a Gemini run that did
// real work showed an empty timeline (STA-775).
func TestChainStreamParsesEitherProvider(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		native  ProviderAdapter
	}{
		{"claude_stream", ClaudeAdapter{}},
		{"agy_stream_success", AgyAdapter{}},
		{"agy_stream_quota_error", AgyAdapter{}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			want := parseFixture(t, tc.native, tc.fixture)
			got := parseFixture(t, chainParser{}, tc.fixture)
			if len(want) == 0 {
				t.Fatal("fixture parsed to no deltas")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("chain parse differs from %T:\n got %+v\nwant %+v", tc.native, got, want)
			}
		})
	}

	// The Claude parser on agy output is the bug: it errors or reads nothing.
	data, err := os.ReadFile(filepath.Join("testdata", "agy_stream_success.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		deltas, _ := ClaudeAdapter{}.ParseStreamDelta(line)
		if res := findKind(deltas, DeltaResult); len(res) != 0 {
			t.Fatalf("precondition: Claude parser unexpectedly read agy result %+v", res)
		}
	}
}

// Claude partial-message lines carry an "event" object, not a string; they
// must stay on the Claude parser.
func TestChainStreamClaudeStreamEventStaysClaude(t *testing.T) {
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta"},"session_id":"s"}`)
	want, wantErr := ClaudeAdapter{}.ParseStreamDelta(line)
	got, err := ParseChainStreamDelta(line)
	if (err != nil) != (wantErr != nil) || !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, %v; want %+v, %v", got, err, want, wantErr)
	}
}
