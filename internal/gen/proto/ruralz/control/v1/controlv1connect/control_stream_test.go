// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlv1connect_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	controlv1 "github.com/ravindu-rev/ruralz/internal/gen/proto/ruralz/control/v1"
	"github.com/ravindu-rev/ruralz/internal/gen/proto/ruralz/control/v1/controlv1connect"
)

// handler is a ControlStream server for the test: Enroll echoes the request
// into its response, and Stream answers a Hello with a Reconnect, then
// reads until the Node closes its side.
type handler struct {
	controlv1connect.UnimplementedControlStreamHandler
	hellos chan *controlv1.Hello
}

func (h *handler) Enroll(_ context.Context, req *connect.Request[controlv1.EnrollRequest]) (*connect.Response[controlv1.EnrollResponse], error) {
	if err := requireGRPC(req.Peer(), req.Header()); err != nil {
		return nil, err
	}
	if req.Msg.GetToken() != "one-time-token" || req.Msg.GetNodeId() != "01J9ZK3M7Q8R2T4V6X8Y0A1B2C" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected EnrollRequest"))
	}
	return connect.NewResponse(&controlv1.EnrollResponse{Cluster: "prod-eu-west", Environment: "prod"}), nil
}

func (h *handler) Stream(_ context.Context, stream *connect.BidiStream[controlv1.StreamRequest, controlv1.StreamResponse]) error {
	if err := requireGRPC(stream.Peer(), stream.RequestHeader()); err != nil {
		return err
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the first message is not a Hello"))
	}
	h.hellos <- hello
	err = stream.Send(&controlv1.StreamResponse{Message: &controlv1.StreamResponse_Reconnect{Reconnect: &controlv1.Reconnect{
		RetryAfter: durationpb.New(time.Second), Reason: controlv1.ReconnectReason_RECONNECT_REASON_SHED,
	}}})
	if err != nil {
		return err
	}
	for {
		if _, err := stream.Receive(); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
	}
}

// requireGRPC fails a call that did not arrive in gRPC mode, the only mode
// of the Control Stream (ADR-0007).
func requireGRPC(peer connect.Peer, header http.Header) error {
	if peer.Protocol != connect.ProtocolGRPC || !strings.HasPrefix(header.Get("Content-Type"), "application/grpc") {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("not gRPC: "+peer.Protocol+" "+header.Get("Content-Type")))
	}
	return nil
}

// TestEnrollAndStreamInGRPCMode runs the generated connect client against
// the generated handler over HTTP/2 with TLS, in gRPC mode at both ends.
func TestEnrollAndStreamInGRPCMode(t *testing.T) {
	h := &handler{hellos: make(chan *controlv1.Hello, 1)}
	path, svc := controlv1connect.NewControlStreamHandler(h)
	if path != "/ruralz.control.v1.ControlStream/" {
		t.Errorf("handler path = %q, want /ruralz.control.v1.ControlStream/", path)
	}
	mux := http.NewServeMux()
	mux.Handle(path, svc)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := controlv1connect.NewControlStreamClient(srv.Client(), srv.URL, connect.WithGRPC())

	resp, err := client.Enroll(t.Context(), connect.NewRequest(&controlv1.EnrollRequest{
		Token: "one-time-token", NodeId: "01J9ZK3M7Q8R2T4V6X8Y0A1B2C", Version: "0.2.0",
		SchemaLevels: map[string]uint32{"ruralz/v1alpha1": 1},
	}))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if resp.Msg.GetCluster() != "prod-eu-west" || resp.Msg.GetEnvironment() != "prod" {
		t.Errorf("Enroll = %v, want Cluster prod-eu-west in Environment prod", resp.Msg)
	}

	stream := client.Stream(t.Context())
	hello := &controlv1.Hello{
		ActiveDigest:  "sha256:" + strings.Repeat("ab", 32),
		BinaryVersion: "0.2.0",
		SchemaLevels:  map[string]uint32{"ruralz/v1alpha1": 1},
		AssignmentSeq: 7,
	}
	if err := stream.Send(&controlv1.StreamRequest{Message: &controlv1.StreamRequest_Hello{Hello: hello}}); err != nil {
		t.Fatalf("Stream: send Hello: %v", err)
	}
	reply, err := stream.Receive()
	if err != nil {
		t.Fatalf("Stream: receive: %v", err)
	}
	rc := reply.GetReconnect()
	if rc.GetReason() != controlv1.ReconnectReason_RECONNECT_REASON_SHED || rc.GetRetryAfter().AsDuration() != time.Second {
		t.Errorf("Stream reply = %v, want Reconnect shed after 1s", reply)
	}
	if got := <-h.hellos; !proto.Equal(got, hello) {
		t.Errorf("the handler received Hello %v, want %v", got, hello)
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("Stream: close the request side: %v", err)
	}
	if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
		t.Errorf("Stream: receive after the handler returned = %v, want io.EOF", err)
	}
	if err := stream.CloseResponse(); err != nil {
		t.Errorf("Stream: close the response side: %v", err)
	}
}

func TestProcedures(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{controlv1connect.ControlStreamName, "ruralz.control.v1.ControlStream"},
		{controlv1connect.ControlStreamEnrollProcedure, "/ruralz.control.v1.ControlStream/Enroll"},
		{controlv1connect.ControlStreamStreamProcedure, "/ruralz.control.v1.ControlStream/Stream"},
	} {
		if tt.got != tt.want {
			t.Errorf("procedure = %q, want %q", tt.got, tt.want)
		}
	}
}
