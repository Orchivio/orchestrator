package contract_test

import (
	"testing"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestAgentEventRoundTrip(t *testing.T) {
	in := &pluginv1.AgentEvent{Type: "tool.called", ToolName: "Bash"}
	b, err := protojson.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := &pluginv1.AgentEvent{}
	if err := protojson.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("round trip mismatch: %s", b)
	}
}
