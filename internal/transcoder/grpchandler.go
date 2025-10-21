package transcoder

import (
	"fmt"
	"io"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/jhump/protoreflect/desc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// grpcurlEventHandler gRPC调用事件处理器
type grpcurlEventHandler struct {
	output    io.Writer
	err       error
	marshaler *jsonpb.Marshaler
}

func (h *grpcurlEventHandler) OnResolveMethod(md *desc.MethodDescriptor) {}

func (h *grpcurlEventHandler) OnSendHeaders(md metadata.MD) {}

func (h *grpcurlEventHandler) OnReceiveHeaders(md metadata.MD) {}

func (h *grpcurlEventHandler) OnReceiveResponse(msg proto.Message) {
	if h.marshaler == nil {
		h.marshaler = &jsonpb.Marshaler{
			EmitDefaults: true,
			OrigName:     true,
		}
	}
	jsonStr, err := h.marshaler.MarshalToString(msg)
	if err != nil {
		h.err = fmt.Errorf("failed to marshal response: %w", err)
		return
	}
	if _, werr := io.WriteString(h.output, jsonStr); werr != nil && h.err == nil {
		h.err = fmt.Errorf("failed to write response: %w", werr)
		return
	}
	if _, werr := io.WriteString(h.output, "\n"); werr != nil && h.err == nil {
		h.err = fmt.Errorf("failed to write response: %w", werr)
	}
}

func (h *grpcurlEventHandler) OnReceiveTrailers(stat *status.Status, md metadata.MD) {
	if stat.Code() != codes.OK && h.err == nil {
		h.err = stat.Err()
	}
}
