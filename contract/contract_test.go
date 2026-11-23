package contract_test

import (
	"slices"
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

func TestStreamEventsRequestCarriesProjectKeys(t *testing.T) {
	req := &pluginv1.StreamEventsRequest{ProjectKeys: []string{"ORC", "RES"}}
	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got pluginv1.StreamEventsRequest
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.GetProjectKeys(), []string{"ORC", "RES"}) {
		t.Fatalf("keys = %v", got.GetProjectKeys())
	}
}
