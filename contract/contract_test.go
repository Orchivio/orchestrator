package contract_test

import (
	"slices"
	"testing"
	"time"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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

func TestV04FieldNumbersAreStable(t *testing.T) {
	cases := []struct {
		msg   protoreflect.MessageDescriptor
		field protoreflect.Name
		num   protoreflect.FieldNumber
	}{
		{(&pluginv1.Actor{}).ProtoReflect().Descriptor(), "username", 4},
		{(&pluginv1.Capabilities{}).ProtoReflect().Descriptor(), "connect_fields", 8},
		{(&pluginv1.Capabilities{}).ProtoReflect().Descriptor(), "supports_review_events", 9},
		{(&pluginv1.CompleteConnectRequest{}).ProtoReflect().Descriptor(), "fields", 3},
		{(&pluginv1.RepoCredentials{}).ProtoReflect().Descriptor(), "tokens", 5},
		{(&pluginv1.RepoCredentials{}).ProtoReflect().Descriptor(), "credential_id", 6},
	}
	for _, c := range cases {
		f := c.msg.Fields().ByName(c.field)
		if f == nil || f.Number() != c.num {
			t.Errorf("%s.%s: got %v, want number %d", c.msg.Name(), c.field, f, c.num)
		}
	}
}

func TestV04ServicesHaveNewRPCs(t *testing.T) {
	tracker := pluginv1.File_orchivio_plugin_v1_tracker_proto.Services().ByName("TrackerAdapter")
	if tracker.Methods().ByName("SetStatus") == nil {
		t.Error("TrackerAdapter.SetStatus missing")
	}
	host := pluginv1.File_orchivio_plugin_v1_codehost_proto.Services().ByName("CodeHostAdapter")
	if m := host.Methods().ByName("StreamEvents"); m == nil || !m.IsStreamingServer() {
		t.Error("CodeHostAdapter.StreamEvents must be server-streaming")
	}
	if host.Methods().ByName("RevokeRepoCredentials") == nil {
		t.Error("CodeHostAdapter.RevokeRepoCredentials missing")
	}
}
