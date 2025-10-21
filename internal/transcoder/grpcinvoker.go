package transcoder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bytedance/sonic"
	"github.com/fullstorydev/grpcurl"
	"github.com/jhump/protoreflect/desc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/descriptorpb"
)

type GRPCInvoker struct {
	conn             *grpc.ClientConn
	descriptorSource grpcurl.DescriptorSource
	address          string
}

// NewGRPCInvoker 创建一个GRPCInvoker实例
func NewGRPCInvoker(address string, fds *descriptorpb.FileDescriptorSet) (*GRPCInvoker, []*desc.FileDescriptor, error) {
	conn, err := grpc.NewClient(
		address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: false,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(24*1024*1024),
			grpc.MaxCallSendMsgSize(24*1024*1024),
		),
		grpc.WithDefaultServiceConfig(`{
			"loadBalancingPolicy": "round_robin",
            "methodConfig": [{
                "name": [{"service": ""}],
                "waitForReady": true,
                "retryPolicy": {
                    "maxAttempts": 3,
                    "initialBackoff": "0.1s",
                    "maxBackoff": "1s",
                    "backoffMultiplier": 1.3,
                    "retryableStatusCodes": ["UNAVAILABLE", "RESOURCE_EXHAUSTED", "INTERNAL"]
				}
			}]
		}`),
		grpc.WithConnectParams(grpc.ConnectParams{MinConnectTimeout: 10 * time.Second}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to %s: %w", address, err)
	}

	// 将FileDescriptorSet转换为FileDescriptor
	filesMap, err := desc.CreateFileDescriptorsFromSet(fds)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create file descriptors: %w", err)
	}

	// 将map转换为slice
	files := make([]*desc.FileDescriptor, 0, len(filesMap))
	for _, fd := range filesMap {
		files = append(files, fd)
	}

	descSource, err := grpcurl.DescriptorSourceFromFileDescriptors(files...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create descriptor source: %w", err)
	}

	return &GRPCInvoker{
		conn:             conn,
		descriptorSource: descSource,
		address:          address,
	}, files, nil
}

// InvokeMethod 调用gRPC方法
func (inv *GRPCInvoker) InvokeMethod(ctx context.Context, fullMethod string, jsonInput []byte) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	// 构造请求读取器，避免重复编解码
	var requestReader io.Reader
	if len(jsonInput) > 0 {
		requestReader = bytes.NewReader(jsonInput)
	} else {
		requestReader = bytes.NewReader([]byte("{}"))
	}

	// 创建输出缓冲区
	var output bytes.Buffer

	// 创建事件处理器
	handler := &grpcurlEventHandler{
		output: &output,
	}

	// 从上下文中获取gRPC元数据
	md, _ := metadata.FromOutgoingContext(ctx)
	headers := make([]string, 0)
	for k, v := range md {
		for _, val := range v {
			headers = append(headers, fmt.Sprintf("%s: %s", k, val))
		}
	}

	// 创建请求解析器和格式化器
	rf, _, err := grpcurl.RequestParserAndFormatter(
		grpcurl.FormatJSON,
		inv.descriptorSource,
		requestReader,
		grpcurl.FormatOptions{},
	)
	if err != nil {
		return nil, err
	}

	// 执行gRPC调用
	err = grpcurl.InvokeRPC(
		ctx,
		inv.descriptorSource,
		inv.conn,
		fullMethod,
		headers,
		handler,
		rf.Next,
	)

	if err != nil {
		return nil, err
	}

	if handler.err != nil {
		return nil, handler.err
	}

	return bytes.TrimRight(output.Bytes(), "\n"), nil
}

// Close 关闭gRPC连接
func (inv *GRPCInvoker) Close() error {
	return inv.conn.Close()
}

// BuildRequestJSON 构建请求JSON数据
func BuildRequestJSON(r *http.Request, pathParams map[string]string, bodyField string) ([]byte, error) {
	requestMap := make(map[string]any)

	// 添加请求路径参数
	for key, value := range pathParams {
		requestMap[key] = value
	}

	// 添加查询参数
	for key, values := range r.URL.Query() {
		if len(values) == 1 {
			requestMap[key] = values[0]
		} else {
			requestMap[key] = values
		}
	}

	// 添加请求体参数
	if r.Method != "GET" && r.Method != "DELETE" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read body: %w", err)
		}
		defer r.Body.Close()

		if len(body) > 0 {
			if bodyField == "*" {
				// protobuf中定义的body为 * 就需要合并所有参数
				var bodyMap map[string]any
				if err := sonic.Unmarshal(body, &bodyMap); err != nil {
					return nil, fmt.Errorf("failed to unmarshal body: %w", err)
				}
				// body、路径参数、查询参数合并 body 优先覆盖
				for k, v := range bodyMap {
					requestMap[k] = v
				}
			} else if bodyField != "" {
				// protobuf中定义的body为 特定字段名 就需要合并到特定字段中
				var bodyData any
				if err := sonic.Unmarshal(body, &bodyData); err != nil {
					return nil, fmt.Errorf("failed to unmarshal body: %w", err)
				}
				requestMap[bodyField] = bodyData
			}
		}
	}

	return sonic.Marshal(requestMap)
}
