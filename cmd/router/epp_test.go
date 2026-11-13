package main

import (
	"context"
	"errors"
	"net"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type pickCall struct {
	session string
	body    string
}

// dialEPP runs an EPP with a fake picker over an in-memory connection and
// returns a stream, as Envoy would open one per request.
func dialEPP(t *testing.T, pick picker) extproc.ExternalProcessor_ProcessClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	extproc.RegisterExternalProcessorServer(gs, &eppServer{pick: pick})
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	stream, err := extproc.NewExternalProcessorClient(conn).Process(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func headers(eos bool, kv ...string) *extproc.ProcessingRequest {
	var hs []*corev3.HeaderValue
	for i := 0; i < len(kv); i += 2 {
		hs = append(hs, &corev3.HeaderValue{Key: kv[i], RawValue: []byte(kv[i+1])})
	}
	return &extproc.ProcessingRequest{Request: &extproc.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extproc.HttpHeaders{Headers: &corev3.HeaderMap{Headers: hs}, EndOfStream: eos}}}
}

func bodyChunk(b string, eos bool) *extproc.ProcessingRequest {
	return &extproc.ProcessingRequest{Request: &extproc.ProcessingRequest_RequestBody{
		RequestBody: &extproc.HttpBody{Body: []byte(b), EndOfStream: eos}}}
}

func setHeaders(r *extproc.ProcessingResponse) map[string]string {
	out := map[string]string{}
	for _, h := range r.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders() {
		out[h.Header.Key] = string(h.Header.RawValue)
	}
	return out
}

func TestEPPRoutesAChatRequestAndRewritesTheSessionPath(t *testing.T) {
	var got pickCall
	stream := dialEPP(t, func(_ context.Context, session string, body []byte) (string, error) {
		got = pickCall{session, string(body)}
		return "10.0.0.7:8000", nil
	})
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(stream.Send(headers(false, ":method", "POST", ":path", "/s/sess-1/v1/chat/completions")))
	must(stream.Send(bodyChunk(`{"model":"q","messages":[{"role":"user",`, false)))
	must(stream.Send(bodyChunk(`"content":"hi"}]}`, true)))

	hr, err := stream.Recv()
	must(err)
	set := setHeaders(hr)
	if set[destinationHeader] != "10.0.0.7:8000" || set[":path"] != "/v1/chat/completions" || set["X-Session-ID"] != "sess-1" {
		t.Fatalf("header mutation %v", set)
	}
	meta := hr.GetDynamicMetadata().GetFields()[destinationMetaKey].GetStructValue().GetFields()[destinationHeader].GetStringValue()
	if meta != "10.0.0.7:8000" {
		t.Fatalf("dynamic metadata %q must match the header", meta)
	}
	br, err := stream.Recv()
	must(err)
	sb := br.GetRequestBody().GetResponse().GetBodyMutation().GetStreamedResponse()
	want := `{"model":"q","messages":[{"role":"user","content":"hi"}]}`
	if string(sb.GetBody()) != want || !sb.GetEndOfStream() {
		t.Fatalf("body released %q eos=%v", sb.GetBody(), sb.GetEndOfStream())
	}
	if got.session != "sess-1" || got.body != want {
		t.Fatalf("picker saw %+v", got)
	}

	// Response chunks flow back unchanged.
	must(stream.Send(&extproc.ProcessingRequest{Request: &extproc.ProcessingRequest_ResponseBody{
		ResponseBody: &extproc.HttpBody{Body: []byte("data: tok\n\n"), EndOfStream: false}}}))
	rb, err := stream.Recv()
	must(err)
	if b := rb.GetResponseBody().GetResponse().GetBodyMutation().GetStreamedResponse().GetBody(); string(b) != "data: tok\n\n" {
		t.Fatalf("response chunk %q", b)
	}
}

func TestEPPSessionFromHeaderAndBodylessRequests(t *testing.T) {
	var got pickCall
	stream := dialEPP(t, func(_ context.Context, session string, body []byte) (string, error) {
		got = pickCall{session, string(body)}
		return "10.0.0.8:8000", nil
	})
	if err := stream.Send(headers(true, ":method", "GET", ":path", "/v1/models", "x-session-id", "s9")); err != nil {
		t.Fatal(err)
	}
	r, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if setHeaders(r)[destinationHeader] != "10.0.0.8:8000" || got.session != "s9" || got.body != "" {
		t.Fatalf("got %+v, headers %v", got, setHeaders(r))
	}
	if _, rewritten := setHeaders(r)[":path"]; rewritten {
		t.Fatal("a plain /v1 path must not be rewritten")
	}
}

func TestEPPAnswers503WhenNoReplicaCanServe(t *testing.T) {
	stream := dialEPP(t, func(context.Context, string, []byte) (string, error) { return "", errors.New("no ready replicas") })
	stream.Send(headers(false, ":path", "/v1/chat/completions", "x-session-id", "s"))
	stream.Send(bodyChunk(`{"messages":[{"role":"user","content":"x"}]}`, true))
	r, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ir := r.GetImmediateResponse(); ir == nil || ir.GetStatus().GetCode() != typev3.StatusCode_ServiceUnavailable {
		t.Fatalf("want an immediate 503, got %v", r)
	}
}
