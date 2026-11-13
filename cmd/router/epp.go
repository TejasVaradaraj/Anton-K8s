package main

// The router as a Gateway API Inference Extension endpoint picker (EPP).
//
// A gateway (Envoy Gateway, kgateway, GKE) streams each inference request to
// the EPP over Envoy's external-processing gRPC protocol. The EPP reads the
// request (session and prompt), makes Anton's placement decision (affinity,
// the cost-based affinity break, wake-on-request, KV restore before the first
// token) and tells the gateway where to send it:
//
//	x-gateway-destination-endpoint: <ip:port>   (request header, and the same
//	value under dynamic metadata envoy.lb, per the EPP protocol)
//
// The request body is processed in full-duplex streamed mode, as the
// protocol requires: chunks are collected, the decision is made at the end of
// the body, then the headers and the body are released to the gateway.
// Requests addressed to /s/{session}/v1/... are rewritten to /v1/... with the
// session moved to X-Session-ID, so any model server works unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/TejasVaradaraj/anton-platform/internal/openai"
)

const (
	destinationHeader  = "x-gateway-destination-endpoint"
	destinationMetaKey = "envoy.lb"
)

var errNoEndpoints = errors.New("no ready inference replicas")

// picker makes the placement decision for one request: the session (may be
// empty for session-less calls such as GET /v1/models), the request body,
// and returns the chosen endpoint as ip:port.
type picker func(ctx context.Context, session string, body []byte) (string, error)

type eppServer struct {
	extproc.UnimplementedExternalProcessorServer
	pick picker
}

func headerValue(h *corev3.HeaderValue) string {
	if len(h.RawValue) > 0 {
		return string(h.RawValue)
	}
	return h.Value
}

func setHeader(key, value string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: key, RawValue: []byte(value)}}
}

func (s *eppServer) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	var (
		path, session string
		body          []byte
		haveHeaders   bool
	)
	send := func(r *extproc.ProcessingResponse) error { return stream.Send(r) }

	// decide runs once the whole request is known and releases it.
	decide := func(withBody bool) error {
		endpoint, err := s.pick(ctx, session, body)
		if err != nil {
			code := typev3.StatusCode_ServiceUnavailable
			log.Printf("epp: no endpoint for session %q: %v", session, err)
			return send(&extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extproc.ImmediateResponse{Status: &typev3.HttpStatus{Code: code}, Body: []byte(err.Error())},
			}})
		}
		mutation := &extproc.HeaderMutation{SetHeaders: []*corev3.HeaderValueOption{setHeader(destinationHeader, endpoint)}}
		if sid, rest, ok := openai.SessionFromPath(path); ok {
			mutation.SetHeaders = append(mutation.SetHeaders, setHeader(":path", rest), setHeader(openai.SessionHeader, sid))
		}
		meta, _ := structpb.NewStruct(map[string]any{destinationMetaKey: map[string]any{destinationHeader: endpoint}})
		if err := send(&extproc.ProcessingResponse{
			Response: &extproc.ProcessingResponse_RequestHeaders{RequestHeaders: &extproc.HeadersResponse{
				Response: &extproc.CommonResponse{HeaderMutation: mutation, ClearRouteCache: true},
			}},
			DynamicMetadata: meta,
		}); err != nil {
			return err
		}
		if !withBody {
			return nil
		}
		return send(&extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_RequestBody{RequestBody: &extproc.BodyResponse{
			Response: &extproc.CommonResponse{BodyMutation: &extproc.BodyMutation{Mutation: &extproc.BodyMutation_StreamedResponse{
				StreamedResponse: &extproc.StreamedBodyResponse{Body: body, EndOfStream: true},
			}}},
		}}})
	}

	for {
		req, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		switch r := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			haveHeaders = true
			for _, h := range r.RequestHeaders.GetHeaders().GetHeaders() {
				switch strings.ToLower(h.Key) {
				case ":path":
					path = headerValue(h)
				case strings.ToLower(openai.SessionHeader):
					session = headerValue(h)
				}
			}
			if sid, _, ok := openai.SessionFromPath(path); ok {
				session = sid
			}
			if r.RequestHeaders.EndOfStream { // no body (e.g. GET /v1/models)
				if err := decide(false); err != nil {
					return err
				}
			}
		case *extproc.ProcessingRequest_RequestBody:
			body = append(body, r.RequestBody.Body...)
			if r.RequestBody.EndOfStream {
				if !haveHeaders {
					return fmt.Errorf("epp: body before headers")
				}
				if err := decide(true); err != nil {
					return err
				}
			}
		case *extproc.ProcessingRequest_RequestTrailers:
			if err := send(&extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_RequestTrailers{RequestTrailers: &extproc.TrailersResponse{}}}); err != nil {
				return err
			}
		case *extproc.ProcessingRequest_ResponseHeaders:
			if err := send(&extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_ResponseHeaders{ResponseHeaders: &extproc.HeadersResponse{}}}); err != nil {
				return err
			}
		case *extproc.ProcessingRequest_ResponseBody:
			// Streamed mode: every response chunk is handed back unchanged
			// (token streams flow through as they are produced).
			if err := send(&extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_ResponseBody{ResponseBody: &extproc.BodyResponse{
				Response: &extproc.CommonResponse{BodyMutation: &extproc.BodyMutation{Mutation: &extproc.BodyMutation_StreamedResponse{
					StreamedResponse: &extproc.StreamedBodyResponse{Body: r.ResponseBody.Body, EndOfStream: r.ResponseBody.EndOfStream},
				}}},
			}}}); err != nil {
				return err
			}
		case *extproc.ProcessingRequest_ResponseTrailers:
			if err := send(&extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_ResponseTrailers{ResponseTrailers: &extproc.TrailersResponse{}}}); err != nil {
				return err
			}
		}
	}
}

// eppPick is the router's placement logic behind the EPP interface: the same
// decision the HTTP proxy makes, without the proxying.
func (rt *router) eppPick(ctx context.Context, session string, body []byte) (string, error) {
	var in openai.Request
	_ = json.Unmarshal(body, &in)
	mode := rt.mode(ctx)
	if session == "" { // session-less call: any ready replica
		pods := rt.eligible(ctx, mode, map[string]bool{})
		if len(pods) == 0 {
			return "", errNoEndpoints
		}
		return fmt.Sprintf("%s:%d", pods[0].Status.PodIP, replicaPort), nil
	}
	view := requestView(session, &in)
	rt.awaitActive(ctx, session)
	target, _, decision, err := rt.pick(ctx, mode, &view, map[string]bool{})
	if err != nil {
		return "", err
	}
	observeDecision(mode, decision)
	rt.touch(session, view.ContextTokens)
	return fmt.Sprintf("%s:%d", target.Status.PodIP, replicaPort), nil
}
