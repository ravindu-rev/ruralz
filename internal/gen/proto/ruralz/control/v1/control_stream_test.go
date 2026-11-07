// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlv1

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// field is one documented field of the Control Stream
// (docs/architecture/04-control-plane-and-gitops.md "Control Stream"):
// its number never changes within ruralz.control.v1, and its JSON name is
// the name the documents use.
type field struct {
	message, name string
	number        protoreflect.FieldNumber
	typ           string // as describe prints it
	jsonName      string
	oneof         string
}

// documentedFields lists every field of package ruralz.control.v1.
func documentedFields() []field {
	const (
		ts     = "google.protobuf.Timestamp"
		dur    = "google.protobuf.Duration"
		levels = "map<string, uint32>"
		pkg    = "ruralz.control.v1."
		// The oneof of StreamRequest and StreamResponse.
		up, down = "message", "message"
	)
	return []field{
		{"EnrollRequest", "token", 1, "string", "token", ""},
		{"EnrollRequest", "node_id", 2, "string", "nodeId", ""},
		{"EnrollRequest", "version", 3, "string", "version", ""},
		{"EnrollRequest", "schema_levels", 4, levels, "schemaLevels", ""},

		{"EnrollResponse", "cluster", 1, "string", "cluster", ""},
		{"EnrollResponse", "environment", 2, "string", "environment", ""},

		{"StreamRequest", "hello", 1, pkg + "Hello", "hello", up},
		{"StreamRequest", "ack", 2, pkg + "Ack", "ack", up},
		{"StreamRequest", "nack", 3, pkg + "Nack", "nack", up},
		{"StreamRequest", "heartbeat", 4, pkg + "Heartbeat", "heartbeat", up},
		{"StreamRequest", "renew_request", 5, pkg + "RenewRequest", "renewRequest", up},

		{"StreamResponse", "snapshot", 1, pkg + "Snapshot", "snapshot", down},
		{"StreamResponse", "delta", 2, pkg + "Delta", "delta", down},
		{"StreamResponse", "heartbeat_reply", 3, pkg + "HeartbeatReply", "heartbeatReply", down},
		{"StreamResponse", "trust_update", 4, pkg + "TrustUpdate", "trustUpdate", down},
		{"StreamResponse", "revocation_update", 5, pkg + "RevocationUpdate", "revocationUpdate", down},
		{"StreamResponse", "renew_response", 6, pkg + "RenewResponse", "renewResponse", down},
		{"StreamResponse", "reconnect", 7, pkg + "Reconnect", "reconnect", down},

		{"Hello", "active_digest", 1, "string", "activeDigest", ""},
		{"Hello", "lkg_digest", 2, "string", "lkgDigest", ""},
		{"Hello", "binary_version", 3, "string", "binaryVersion", ""},
		{"Hello", "schema_levels", 4, levels, "schemaLevels", ""},
		{"Hello", "last_nonce", 5, "bytes", "lastNonce", ""},
		{"Hello", "store_epoch", 6, ts, "storeEpoch", ""},
		{"Hello", "assignment_seq", 7, "uint64", "assignmentSeq", ""},
		{"Hello", "anchor_set_version", 8, ts, "anchorSetVersion", ""},
		{"Hello", "revocation_seq", 9, "uint64", "revocationSeq", ""},

		{"Snapshot", "nonce", 1, "bytes", "nonce", ""},
		{"Snapshot", "offset", 2, "uint64", "offset", ""},
		{"Snapshot", "total_bytes", 3, "uint64", "totalBytes", ""},
		{"Snapshot", "chunk", 4, "bytes", "chunk", ""},

		{"Delta", "nonce", 1, "bytes", "nonce", ""},
		{"Delta", "base_digest", 2, "string", "baseDigest", ""},
		{"Delta", "ops", 3, "repeated " + pkg + "DeltaOp", "ops", ""},

		{"DeltaOp", "kind", 1, "string", "kind", ""},
		{"DeltaOp", "name", 2, "string", "name", ""},
		{"DeltaOp", "replace", 3, "bytes", "replace", "change"},
		{"DeltaOp", "delete", 4, "google.protobuf.Empty", "delete", "change"},

		{"Ack", "nonce", 1, "bytes", "nonce", ""},
		{"Ack", "digest", 2, "string", "digest", ""},
		{"Ack", "store_epoch", 3, ts, "storeEpoch", ""},
		{"Ack", "assignment_seq", 4, "uint64", "assignmentSeq", ""},

		{"Nack", "nonce", 1, "bytes", "nonce", ""},
		{"Nack", "digest", 2, "string", "digest", ""},
		{"Nack", "code", 3, "string", "code", ""},
		{"Nack", "class", 4, "enum " + pkg + "NackClass", "class", ""},
		{"Nack", "detail", 5, "string", "detail", ""},

		{"Heartbeat", "active_digest", 1, "string", "activeDigest", ""},
		{"Heartbeat", "lkg_digest", 2, "string", "lkgDigest", ""},
		{"Heartbeat", "schema_levels", 3, levels, "schemaLevels", ""},
		{"Heartbeat", "ready", 4, "bool", "ready", ""},
		{"Heartbeat", "draining", 5, "bool", "draining", ""},
		{"Heartbeat", "revocation_seq", 6, "uint64", "revocationSeq", ""},

		{"HeartbeatReply", "cluster_node_count", 1, "optional uint32", "clusterNodeCount", ""},
		{"HeartbeatReply", "heartbeat_interval", 2, dur, "heartbeatInterval", ""},

		{"TrustUpdate", "nonce", 1, "bytes", "nonce", ""},

		{"RevocationUpdate", "nonce", 1, "bytes", "nonce", ""},

		{"Reconnect", "retry_after", 1, dur, "retryAfter", ""},
		{"Reconnect", "reason", 2, "enum " + pkg + "ReconnectReason", "reason", ""},
	}
}

// documentedMessages lists every message of the package, those without
// fields included.
func documentedMessages() []string {
	return []string{
		"EnrollRequest", "EnrollResponse", "StreamRequest", "StreamResponse", "Hello", "Snapshot", "Delta",
		"DeltaOp", "Ack", "Nack", "Heartbeat", "HeartbeatReply", "TrustUpdate", "RevocationUpdate",
		"RenewRequest", "RenewResponse", "Reconnect",
	}
}

// describe prints a field's type the way documentedFields spells it.
func describe(fd protoreflect.FieldDescriptor) string {
	scalar := func(fd protoreflect.FieldDescriptor) string {
		switch fd.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			return string(fd.Message().FullName())
		case protoreflect.EnumKind:
			return "enum " + string(fd.Enum().FullName())
		default:
			return fd.Kind().String()
		}
	}
	switch {
	case fd.IsMap():
		return fmt.Sprintf("map<%s, %s>", scalar(fd.MapKey()), scalar(fd.MapValue()))
	case fd.IsList():
		return "repeated " + scalar(fd)
	case fd.HasOptionalKeyword():
		return "optional " + scalar(fd)
	default:
		return scalar(fd)
	}
}

func TestDocumentedFieldNumbers(t *testing.T) {
	file := File_ruralz_control_v1_control_stream_proto
	if file.Package() != "ruralz.control.v1" {
		t.Fatalf("package = %s, want ruralz.control.v1", file.Package())
	}
	var gotMessages []string
	for i := range file.Messages().Len() {
		gotMessages = append(gotMessages, string(file.Messages().Get(i).Name()))
	}
	if want := documentedMessages(); !slices.Equal(gotMessages, want) {
		t.Errorf("messages = %q, want %q", gotMessages, want)
	}
	want := map[string]field{}
	for _, f := range documentedFields() {
		want[f.message+"."+f.name] = f
	}
	for i := range file.Messages().Len() {
		md := file.Messages().Get(i)
		for j := range md.Fields().Len() {
			fd := md.Fields().Get(j)
			key := string(md.Name()) + "." + string(fd.Name())
			w, ok := want[key]
			if !ok {
				t.Errorf("field %s is not documented: add it to the Control Stream message table and this test", key)
				continue
			}
			delete(want, key)
			oneof := ""
			if od := fd.ContainingOneof(); od != nil && !od.IsSynthetic() {
				oneof = string(od.Name())
			}
			got := field{w.message, w.name, fd.Number(), describe(fd), fd.JSONName(), oneof}
			if got != w {
				t.Errorf("field %s = %+v, want %+v", key, got, w)
			}
		}
	}
	for key := range want {
		t.Errorf("documented field %s is missing", key)
	}
}

func TestDocumentedEnumValues(t *testing.T) {
	want := map[string][]string{
		"NackClass":       {"NACK_CLASS_UNSPECIFIED=0", "NACK_CLASS_DETERMINISTIC=1", "NACK_CLASS_TRANSIENT=2"},
		"ReconnectReason": {"RECONNECT_REASON_UNSPECIFIED=0", "RECONNECT_REASON_DRAIN=1", "RECONNECT_REASON_SHED=2", "RECONNECT_REASON_REBALANCE=3"},
	}
	enums := File_ruralz_control_v1_control_stream_proto.Enums()
	if enums.Len() != len(want) {
		t.Errorf("the package has %d enums, want %d", enums.Len(), len(want))
	}
	for i := range enums.Len() {
		ed := enums.Get(i)
		var got []string
		for j := range ed.Values().Len() {
			v := ed.Values().Get(j)
			got = append(got, fmt.Sprintf("%s=%d", v.Name(), v.Number()))
		}
		if w := want[string(ed.Name())]; !slices.Equal(got, w) {
			t.Errorf("enum %s = %q, want %q", ed.Name(), got, w)
		}
	}
}

func TestService(t *testing.T) {
	services := File_ruralz_control_v1_control_stream_proto.Services()
	if services.Len() != 1 || services.Get(0).FullName() != "ruralz.control.v1.ControlStream" {
		t.Fatalf("services = %v, want the one service ruralz.control.v1.ControlStream", services)
	}
	var got []string
	methods := services.Get(0).Methods()
	for i := range methods.Len() {
		m := methods.Get(i)
		got = append(got, fmt.Sprintf("%s(%s client-stream=%t) %s server-stream=%t",
			m.Name(), m.Input().Name(), m.IsStreamingClient(), m.Output().Name(), m.IsStreamingServer()))
	}
	want := []string{
		"Enroll(EnrollRequest client-stream=false) EnrollResponse server-stream=false",
		"Stream(StreamRequest client-stream=true) StreamResponse server-stream=true",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ControlStream RPCs = %q, want %q", got, want)
	}
}

// populate sets every field of m to a value other than its default; of
// each oneof it sets the member with index choice modulo the member count.
func populate(t *testing.T, m protoreflect.Message, choice int) {
	t.Helper()
	// The well-known types are set by hand, within the ranges their JSON
	// forms allow.
	switch v := m.Interface().(type) {
	case *timestamppb.Timestamp:
		v.Seconds, v.Nanos = 1793232000+int64(choice), 5
		return
	case *durationpb.Duration:
		v.Seconds, v.Nanos = 15+int64(choice), 500000000
		return
	}
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if od := fd.ContainingOneof(); od != nil && !od.IsSynthetic() {
			if od.Fields().Get(choice%od.Fields().Len()) != fd {
				continue
			}
		}
		switch {
		case fd.IsMap():
			mp := m.Mutable(fd).Map()
			mp.Set(scalarValue(t, fd.MapKey(), 0).MapKey(), scalarValue(t, fd.MapValue(), 0))
			mp.Set(scalarValue(t, fd.MapKey(), 1).MapKey(), scalarValue(t, fd.MapValue(), 1))
		case fd.IsList():
			list := m.Mutable(fd).List()
			for n := range 2 {
				if fd.Message() != nil {
					elem := list.NewElement()
					populate(t, elem.Message(), n)
					list.Append(elem)
				} else {
					list.Append(scalarValue(t, fd, n))
				}
			}
		case fd.Message() != nil:
			populate(t, m.Mutable(fd).Message(), choice)
		default:
			m.Set(fd, scalarValue(t, fd, 0))
		}
		if !m.Has(fd) {
			t.Fatalf("%s: populate left %s unset", m.Descriptor().FullName(), fd.Name())
		}
	}
}

// scalarValue is a non-default value of a scalar or enum field; n, 0 or 1,
// tells two values apart.
func scalarValue(t *testing.T, fd protoreflect.FieldDescriptor, n int) protoreflect.Value {
	t.Helper()
	n %= 2
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.EnumKind:
		values := fd.Enum().Values()
		return protoreflect.ValueOfEnum(values.Get(values.Len() - 1 - n%(values.Len()-1)).Number())
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32([2]int32{-7, -8}[n])
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64([2]int64{-1 << 40, -1<<40 - 1}[n])
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32([2]uint32{7, 8}[n])
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64([2]uint64{1 << 40, 1<<40 + 1}[n])
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32([2]float32{1.5, 2.5}[n])
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64([2]float64{2.5, 3.5}[n])
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(fmt.Sprintf("%s-%d", fd.Name(), n))
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([2][]byte{{0, 1, 0xfe, 0xff}, {0xff, 0}}[n])
	default:
		t.Fatalf("%s: no test value for kind %s", fd.FullName(), fd.Kind())
		return protoreflect.Value{}
	}
}

func TestEveryMessageRoundTrips(t *testing.T) {
	messages := File_ruralz_control_v1_control_stream_proto.Messages()
	for i := range messages.Len() {
		md := messages.Get(i)
		mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
		if err != nil {
			t.Fatal(err)
		}
		// One variant per member of the message's oneof, if it has one.
		variants := 1
		for j := range md.Oneofs().Len() {
			if od := md.Oneofs().Get(j); !od.IsSynthetic() {
				variants = max(variants, od.Fields().Len())
			}
		}
		for v := range variants {
			t.Run(fmt.Sprintf("%s/%d", md.Name(), v), func(t *testing.T) {
				msg := mt.New()
				populate(t, msg, v)
				want := msg.Interface()

				wire, err := proto.Marshal(want)
				if err != nil {
					t.Fatalf("proto.Marshal: %v", err)
				}
				got := mt.New().Interface()
				if err := proto.Unmarshal(wire, got); err != nil {
					t.Fatalf("proto.Unmarshal: %v", err)
				}
				if !proto.Equal(got, want) {
					t.Errorf("binary round trip = %v, want %v", got, want)
				}

				text, err := protojson.Marshal(want)
				if err != nil {
					t.Fatalf("protojson.Marshal: %v", err)
				}
				got = mt.New().Interface()
				if err := protojson.Unmarshal(text, got); err != nil {
					t.Fatalf("protojson.Unmarshal(%s): %v", text, err)
				}
				if !proto.Equal(got, want) {
					t.Errorf("JSON round trip of %s = %v, want %v", text, got, want)
				}
			})
		}
	}
}

// TestClusterNodeCountPresence: a withheld clusterNodeCount stays absent
// through the runtime, apart from a published count of 0, so a Node keeps
// its last count only when Ruralz Control withholds it.
func TestClusterNodeCountPresence(t *testing.T) {
	zero := uint32(0)
	for _, tt := range []struct {
		name  string
		reply *HeartbeatReply
		want  bool
	}{
		{"withheld", &HeartbeatReply{HeartbeatInterval: durationpb.New(15e9)}, false},
		{"published zero", &HeartbeatReply{ClusterNodeCount: &zero}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire, err := proto.Marshal(tt.reply)
			if err != nil {
				t.Fatal(err)
			}
			var got HeartbeatReply
			if err := proto.Unmarshal(wire, &got); err != nil {
				t.Fatal(err)
			}
			if present := got.ClusterNodeCount != nil; present != tt.want {
				t.Errorf("after the round trip, clusterNodeCount present = %t, want %t", present, tt.want)
			}
			text, err := protojson.Marshal(tt.reply)
			if err != nil {
				t.Fatal(err)
			}
			if inJSON := strings.Contains(string(text), `"clusterNodeCount"`); inJSON != tt.want {
				t.Errorf("JSON %s names clusterNodeCount = %t, want %t", text, inJSON, tt.want)
			}
		})
	}
}
