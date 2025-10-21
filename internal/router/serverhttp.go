package router

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path"
	"strings"

	"pilot/internal/transcoder"

	"github.com/bytedance/sonic"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Result struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

func (r *HTTPRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// 路由匹配
	pathKey := normalizePath(strings.ToUpper(req.Method), req.URL.Path)
	matchedRoute, pathParams, ok := r.routerTree.Lookup(pathKey)
	if !ok || matchedRoute == nil {
		writeJSON(w, http.StatusNotFound, Result{
			Code: http.StatusNotFound,
			Msg:  fmt.Sprintf("No route found for %s %s", req.Method, req.URL.Path),
			Data: nil,
		})
		return
	}

	// 选择服务实例
	pool, ok := r.servicePools[matchedRoute.ServiceName]
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, Result{
			Code: http.StatusServiceUnavailable,
			Msg:  fmt.Sprintf("Service %s not available", matchedRoute.ServiceName),
			Data: nil,
		})
		return
	}
	invoker, err := pool.getNextInvoker()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, Result{
			Code: http.StatusServiceUnavailable,
			Msg:  "No available service instances",
			Data: nil,
		})
		return
	}

	// 构建请求参数
	requestJSON, err := buildRequestPayload(req, pathParams, matchedRoute.HttpRule.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Result{
			Code: http.StatusBadRequest,
			Msg:  fmt.Sprintf("Failed to build request: %v", err),
			Data: nil,
		})
		return
	}

	// 附带 HTTP Header -> gRPC Metadata
	ctxWithMD := metadata.NewOutgoingContext(req.Context(), buildOutgoingMD(req))

	// gRPC 调用
	responseJSON, err := invoker.InvokeMethod(
		ctxWithMD,
		matchedRoute.FullMethod,
		requestJSON,
	)
	if err != nil {
		statusCode, res := mapErrorToHTTP(err)
		writeJSON(w, statusCode, res)
		return
	}

	data := decodeResponseJSON(responseJSON)
	writeJSON(w, http.StatusOK, Result{
		Code: int(codes.OK),
		Msg:  "success",
		Data: data,
	})
}

// NormalizePath 统一规范路由注册路径
// 生成的格式固定为：/[METHOD]/cleanedPath
func normalizePath(method string, p string) string {
	m := strings.ToUpper(strings.TrimSpace(method))
	clean := strings.TrimSpace(p)
	// 去除前导斜杠，统一用 path.Clean 再补齐
	clean = strings.TrimPrefix(clean, "/")
	clean = path.Clean("/" + clean)
	return fmt.Sprintf("/[%s]%s", m, clean)
}

// mapGRPCCodeToHTTP 将 gRPC 错误码映射为 HTTP 状态码
func mapGRPCCodeToHTTP(code codes.Code) int {
	switch code {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return http.StatusRequestTimeout
	case codes.Unknown:
		return http.StatusInternalServerError
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusRequestTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.Aborted:
		return http.StatusConflict
	case codes.OutOfRange:
		return http.StatusBadRequest
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Internal:
		return http.StatusInternalServerError
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	case codes.DataLoss:
		return http.StatusInternalServerError
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	default:
		return http.StatusOK
	}
}

// writeJSON 统一 JSON 响应输出
func writeJSON(w http.ResponseWriter, status int, res Result) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := sonic.ConfigDefault.NewEncoder(w).Encode(res); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

// buildRequestPayload 构建转码后的请求负载
func buildRequestPayload(req *http.Request, pathParams map[string]string, bodyField string) ([]byte, error) {
	return transcoder.BuildRequestJSON(req, pathParams, bodyField)
}

// buildOutgoingMD 将 HTTP Header 写入 gRPC Metadata
func buildOutgoingMD(req *http.Request) metadata.MD {
	md := metadata.New(nil)
	blocked := map[string]struct{}{
		":authority": {}, ":method": {}, ":path": {}, ":scheme": {},
		"host": {}, "connection": {}, "keep-alive": {}, "proxy-connection": {},
		"transfer-encoding": {}, "upgrade": {}, "upgrade-insecure-requests": {},
		"content-length": {}, "content-type": {}, "user-agent": {},
		"accept": {}, "accept-encoding": {}, "accept-language": {}, "origin": {}, "referer": {},
		"te": {},
	}
	for k, vals := range req.Header {
		lk := strings.ToLower(k)
		if _, banned := blocked[lk]; banned {
			continue
		}
		for _, v := range vals {
			md.Append(lk, v)
		}
	}
	return md
}

// mapErrorToHTTP 将 gRPC/内部错误映射为 HTTP 响应
func mapErrorToHTTP(err error) (int, Result) {
	if st, ok := status.FromError(err); ok {
		code := st.Code()
		return mapGRPCCodeToHTTP(code), Result{Code: int(code), Msg: st.Message(), Data: nil}
	}
	return http.StatusInternalServerError, Result{Code: -1, Msg: err.Error(), Data: nil}
}

// decodeResponseJSON 尝试解码响应体为结构化数据
func decodeResponseJSON(b []byte) any {
	var data any
	if err := json.Unmarshal(b, &data); err != nil {
		return json.RawMessage(b)
	}
	return data
}
