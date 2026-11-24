package contract_test

import (
	"slices"
	"testing"
	"time"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
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

func TestConnectMessagesRoundTrip(t *testing.T) {
	in := &pluginv1.CompleteConnectResponse{
		Credential: &pluginv1.Credential{Data: []byte("x"), RefreshAfter: timestamppb.New(time.Unix(1700000000, 0))},
		Label:      "Acme",
		ExternalId: "org-1",
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := &pluginv1.CompleteConnectResponse{}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, out) {
		t.Fatalf("round trip mismatch: %v != %v", in, out)
	}
}

func TestRequestsCarryCredentials(t *testing.T) {
	cred := func(s string) *pluginv1.Credential { return &pluginv1.Credential{Data: []byte(s)} }
	stream := &pluginv1.StreamEventsRequest{Credential: cred("a")}
	issue := &pluginv1.IssueRepoCredentialsRequest{Credentials: []*pluginv1.Credential{cred("a"), cred("b")}}
	for _, in := range []proto.Message{stream, issue} {
		b, err := proto.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		out := in.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(in, out) {
			t.Fatalf("round trip mismatch: %v != %v", in, out)
		}
	}
	if n := len(issue.GetCredentials()); n != 2 {
		t.Fatalf("credentials = %d", n)
	}
}
