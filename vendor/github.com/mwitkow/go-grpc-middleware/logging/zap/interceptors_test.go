// Copyright 2017 Michal Witkowski. All Rights Reserved.
// See LICENSE for licensing terms.

package grpc_zap_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	"github.com/grpc-ecosystem/go-grpc-middleware/tags"
	"github.com/grpc-ecosystem/go-grpc-middleware/testing"
	pb_testproto "github.com/grpc-ecosystem/go-grpc-middleware/testing/testproto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

var (
	goodPing = &pb_testproto.PingRequest{Value: "something", SleepTimeMs: 9999}
)

type loggingPingService struct {
	pb_testproto.TestServiceServer
}

func customCodeToLevel(c codes.Code) zapcore.Level {
	if c == codes.Unauthenticated {
		// Make this a special case for tests, and an error.
		return zap.DPanicLevel
	}
	level := grpc_zap.DefaultCodeToLevel(c)
	return level
}

func (s *loggingPingService) Ping(ctx context.Context, ping *pb_testproto.PingRequest) (*pb_testproto.PingResponse, error) {
	grpc_ctxtags.Extract(ctx).Set("custom_tags.string", "something").Set("custom_tags.int", 1337)
	grpc_zap.Extract(ctx).Info("some ping")
	return s.TestServiceServer.Ping(ctx, ping)
}

func (s *loggingPingService) PingError(ctx context.Context, ping *pb_testproto.PingRequest) (*pb_testproto.Empty, error) {
	return s.TestServiceServer.PingError(ctx, ping)
}

func (s *loggingPingService) PingList(ping *pb_testproto.PingRequest, stream pb_testproto.TestService_PingListServer) error {
	grpc_ctxtags.Extract(stream.Context()).Set("custom_tags.string", "something").Set("custom_tags.int", 1337)
	grpc_zap.Extract(stream.Context()).Info("some pinglist")
	return s.TestServiceServer.PingList(ping, stream)
}

func (s *loggingPingService) PingEmpty(ctx context.Context, empty *pb_testproto.Empty) (*pb_testproto.PingResponse, error) {
	return s.TestServiceServer.PingEmpty(ctx, empty)
}

type bufferWriteSyncer struct {
	*bytes.Buffer
}

func (*bufferWriteSyncer) Sync() error {
	return nil // no op
}

func TestZapLoggingSuite(t *testing.T) {
	if strings.HasPrefix(runtime.Version(), "go1.7") {
		t.Skipf("Skipping due to json.RawMessage incompatibility with go1.7")
		return
	}
	b := &bufferWriteSyncer{&bytes.Buffer{}}
	zap.NewDevelopmentConfig()
	jsonEncoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.EpochTimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
	})
	core := zapcore.NewCore(jsonEncoder, b, zap.LevelEnablerFunc(func(zapcore.Level) bool { return true }))
	log := zap.New(core)
	opts := []grpc_zap.Option{
		grpc_zap.WithLevels(customCodeToLevel),
	}
	s := &ZapLoggingSuite{
		log:    log,
		buffer: b.Buffer,
		InterceptorTestSuite: &grpc_testing.InterceptorTestSuite{
			TestService: &loggingPingService{&grpc_testing.TestPingService{T: t}},
			ServerOpts: []grpc.ServerOption{
				grpc_middleware.WithStreamServerChain(
					grpc_ctxtags.StreamServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
					grpc_zap.StreamServerInterceptor(log, opts...)),
				grpc_middleware.WithUnaryServerChain(
					grpc_ctxtags.UnaryServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
					grpc_zap.UnaryServerInterceptor(log, opts...)),
			},
		},
	}
	suite.Run(t, s)
}

type ZapLoggingSuite struct {
	*grpc_testing.InterceptorTestSuite
	buffer *bytes.Buffer
	log    *zap.Logger
}

func (s *ZapLoggingSuite) SetupTest() {
	s.buffer.Reset()
}

func (s *ZapLoggingSuite) getOutputJSONs() []string {
	ret := []string{}
	dec := json.NewDecoder(s.buffer)
	for {
		var val map[string]json.RawMessage
		err := dec.Decode(&val)
		if err == io.EOF {
			break
		}
		if err != nil {
			s.T().Fatalf("failed decoding output from ZAP JSON: %v", err)
		}
		out, _ := json.MarshalIndent(val, "", "  ")
		ret = append(ret, string(out))
	}
	return ret
}

func (s *ZapLoggingSuite) TestPing_WithCustomTags() {
	_, err := s.Client.Ping(s.SimpleCtx(), goodPing)
	assert.NoError(s.T(), err, "there must be not be an on a successful call")
	msgs := s.getOutputJSONs()
	assert.Len(s.T(), msgs, 2, "two log statements should be logged")
	for _, m := range msgs {
		s.T()
		assert.Contains(s.T(), m, `grpc.service": "mwitkow.testproto.TestService"`, "all lines must contain service name")
		assert.Contains(s.T(), m, `grpc.method": "Ping"`, "all lines must contain method name")
		assert.Contains(s.T(), m, `"custom_tags.string": "something"`, "all lines must contain `custom_tags.string` set by AddFields")
		assert.Contains(s.T(), m, `"custom_tags.int": 1337`, "all lines must contain `custom_tags.int` set by AddFields")
		// request field extraction
		assert.Contains(s.T(), m, `"grpc.request.value": "something"`, "all lines must contain fields extracted from goodPing because of test.manual_extractfields.pb")
	}
	assert.Contains(s.T(), msgs[0], `"msg": "some ping"`, "handler's message must contain user message")
	assert.Contains(s.T(), msgs[1], `"msg": "finished unary call"`, "interceptor message must contain string")
	assert.Contains(s.T(), msgs[1], `"level": "info"`, "OK error codes must be logged on info level.")
	assert.Contains(s.T(), msgs[1], `grpc.time_ms":`, "interceptor log statement should contain execution time")
}

func (s *ZapLoggingSuite) TestPingError_WithCustomLevels() {
	for _, tcase := range []struct {
		code  codes.Code
		level zapcore.Level
		msg   string
	}{
		{
			code:  codes.Internal,
			level: zap.ErrorLevel,
			msg:   "Internal must remap to ErrorLevel in DefaultCodeToLevel",
		},
		{
			code:  codes.NotFound,
			level: zap.InfoLevel,
			msg:   "NotFound must remap to InfoLevel in DefaultCodeToLevel",
		},
		{
			code:  codes.FailedPrecondition,
			level: zap.WarnLevel,
			msg:   "FailedPrecondition must remap to WarnLevel in DefaultCodeToLevel",
		},
		{
			code:  codes.Unauthenticated,
			level: zap.DPanicLevel,
			msg:   "Unauthenticated is overwritten to DPanicLevel with customCodeToLevel override, which probably didn't work",
		},
	} {
		s.buffer.Reset()
		_, err := s.Client.PingError(
			s.SimpleCtx(),
			&pb_testproto.PingRequest{Value: "something", ErrorCodeReturned: uint32(tcase.code)})
		assert.Error(s.T(), err, "each call here must return an error")
		msgs := s.getOutputJSONs()
		require.Len(s.T(), msgs, 1, "only the interceptor log message is printed in PingErr")
		m := msgs[0]
		assert.Contains(s.T(), m, `grpc.service": "mwitkow.testproto.TestService"`, "all lines must contain service name")
		assert.Contains(s.T(), m, `grpc.method": "PingError"`, "all lines must contain method name")
		assert.Contains(s.T(), m, fmt.Sprintf(`grpc.code": "%s"`, tcase.code.String()), "all lines must contain method name")
		assert.Contains(s.T(), m, fmt.Sprintf(`"level": "%s"`, tcase.level.String()), tcase.msg)
	}
}

func (s *ZapLoggingSuite) TestPingList_WithCustomTags() {
	stream, err := s.Client.PingList(s.SimpleCtx(), goodPing)
	require.NoError(s.T(), err, "should not fail on establishing the stream")
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(s.T(), err, "reading stream should not fail")
	}
	msgs := s.getOutputJSONs()
	assert.Len(s.T(), msgs, 2, "two log statements should be logged")
	for _, m := range msgs {
		s.T()
		assert.Contains(s.T(), m, `grpc.service": "mwitkow.testproto.TestService"`, "all lines must contain service name")
		assert.Contains(s.T(), m, `grpc.method": "PingList"`, "all lines must contain method name")
		assert.Contains(s.T(), m, `"custom_tags.string": "something"`, "all lines must contain `custom_tags.string` set by AddFields")
		assert.Contains(s.T(), m, `"custom_tags.int": 1337`, "all lines must contain `custom_tags.int` set by AddFields")
		// request field extraction
		assert.Contains(s.T(), m, `"grpc.request.value": "something"`, "all lines must contain fields extracted from goodPing because of test.manual_extractfields.pb")
	}
	assert.Contains(s.T(), msgs[0], `"msg": "some pinglist"`, "handler's message must contain user message")
	assert.Contains(s.T(), msgs[1], `"msg": "finished streaming call"`, "interceptor message must contain string")
	assert.Contains(s.T(), msgs[1], `"level": "info"`, "OK error codes must be logged on info level.")
	assert.Contains(s.T(), msgs[1], `grpc.time_ms":`, "interceptor log statement should contain execution time")
}
