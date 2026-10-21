package server

import (
	"context"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errInternal is the only message clients see for unexpected failures.
var errInternal = status.Error(codes.Internal, "internal error")

// logUnary logs one line per RPC. Health checks are logged at debug level
// because probes call them every few seconds.
func logUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)

		code := status.Code(err)
		level := slog.LevelInfo
		switch {
		case isServerFault(code):
			level = slog.LevelError
		case strings.HasPrefix(info.FullMethod, "/grpc.health."):
			level = slog.LevelDebug
		}
		attrs := []slog.Attr{
			slog.String("method", info.FullMethod),
			slog.String("code", code.String()),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		}
		if err != nil {
			attrs = append(attrs, slog.String("error", err.Error()))
		}
		log.LogAttrs(ctx, level, "rpc", attrs...)
		return resp, err
	}
}

// recoverUnary turns a panicking handler into an Internal error, so one bad
// request cannot crash the process.
func recoverUnary(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if p := recover(); p != nil {
				logPanic(ctx, log, info.FullMethod, p)
				err = errInternal
			}
		}()
		return handler(ctx, req)
	}
}

// recoverStream is recoverUnary for streaming RPCs.
func recoverStream(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if p := recover(); p != nil {
				logPanic(ss.Context(), log, info.FullMethod, p)
				err = errInternal
			}
		}()
		return handler(srv, ss)
	}
}

func logPanic(ctx context.Context, log *slog.Logger, method string, p any) {
	log.ErrorContext(ctx, "panic in gRPC handler",
		slog.String("method", method),
		slog.Any("panic", p),
		slog.String("stack", string(debug.Stack())),
	)
}

// isServerFault reports whether code means git-svc failed, as opposed to
// the caller sending a bad or impossible request.
func isServerFault(code codes.Code) bool {
	switch code {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable, codes.Unimplemented:
		return true
	default:
		return false
	}
}
