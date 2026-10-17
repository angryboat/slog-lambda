package sloglambda_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
	"time"

	sloglambda "github.com/angryboat/slog-lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandler(t *testing.T) {
	t.Run("slogtest", func(t *testing.T) {
		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			newHandler := func(t *testing.T) slog.Handler {
				t.Cleanup(buffer.Reset)

				return sloglambda.NewHandler(buffer, sloglambda.WithLevel(slog.LevelDebug), sloglambda.WithJSON())
			}

			result := func(t *testing.T) map[string]any {
				result := make(map[string]any)
				require.NoError(t, json.Unmarshal(buffer.Bytes(), &result))
				return result
			}

			slogtest.Run(t, newHandler, result)
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			newHandler := func(t *testing.T) slog.Handler {
				t.Cleanup(buffer.Reset)

				return sloglambda.NewHandler(buffer, sloglambda.WithLevel(slog.LevelDebug), sloglambda.WithText())
			}

			result := func(t *testing.T) map[string]any {
				result := make(map[string]any)

				unquote := func(s string) string {
					if len(s) == 0 || s[0] != '"' {
						return s
					}

					s, err := strconv.Unquote(s)
					require.NoError(t, err)
					return s
				}

				parts := strings.Split(strings.TrimSpace(buffer.String()), " ")
				for _, entry := range parts {
					parts := strings.SplitN(entry, "=", 2)
					path := strings.Split(parts[0], ".")
					if len(path) == 1 {
						result[path[0]] = unquote(parts[1])
						continue
					}

					v := result

					for i := 0; i < len(path)-1; i++ {
						if _, ok := v[path[i]]; !ok {
							v[path[i]] = make(map[string]any)
						}
						v = v[path[i]].(map[string]any)
					}

					v[path[len(path)-1]] = unquote(parts[1])
				}

				return result
			}

			slogtest.Run(t, newHandler, result)
		})
	})

	t.Run("WithLevel nil", func(t *testing.T) {
		buffer := new(bytes.Buffer)
		logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithLevel(nil)))

		logger.Debug("dropped")
		logger.Info(t.Name())

		assert.NotContains(t, buffer.String(), `"dropped"`)
		assert.Contains(t, buffer.String(), `"level":"INFO"`)
	})

	t.Run("empty WithAttrs and WithGroup return the receiver", func(t *testing.T) {
		handler := sloglambda.NewHandler(new(bytes.Buffer))

		assert.Same(t, handler, handler.WithAttrs(nil))
		assert.Same(t, handler, handler.WithGroup(""))
	})

	t.Run("WithLevel nil reused concurrently", func(t *testing.T) {
		opt := sloglambda.WithLevel(nil)

		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				sloglambda.NewHandler(io.Discard, opt)
			})
		}
		wg.Wait()
	})

	t.Run("Enabled", func(t *testing.T) {
		tests := []struct {
			env      string
			enabled  slog.Level
			disabled slog.Level
			output   string
		}{
			{"TRACE", slog.LevelDebug - 4, slog.LevelDebug - 5, "TRACE"},
			{"DEBUG", slog.LevelDebug, slog.LevelDebug - 1, "DEBUG"},
			{"INFO", slog.LevelInfo, slog.LevelDebug, "INFO"},
			{"WARN", slog.LevelWarn, slog.LevelInfo, "WARN"},
			{"ERROR", slog.LevelError, slog.LevelWarn, "ERROR"},
			{"FATAL", slog.LevelError + 4, slog.LevelError + 3, "FATAL"},
			{"bogus", slog.LevelInfo, slog.LevelDebug, "INFO"},
			{"", slog.LevelInfo, slog.LevelDebug, "INFO"},
		}

		for _, tt := range tests {
			t.Run("AWS_LAMBDA_LOG_LEVEL="+tt.env, func(t *testing.T) {
				t.Setenv("AWS_LAMBDA_LOG_LEVEL", tt.env)

				buffer := new(bytes.Buffer)
				handler := sloglambda.NewHandler(buffer, sloglambda.WithJSON())
				logger := slog.New(handler)

				assert.True(t, handler.Enabled(t.Context(), tt.enabled))
				assert.False(t, handler.Enabled(t.Context(), tt.disabled))

				logger.Log(t.Context(), tt.disabled, "dropped")
				logger.Log(t.Context(), tt.enabled, "kept")

				var result map[string]any
				require.NoError(t, json.Unmarshal(buffer.Bytes(), &result))
				assert.Equal(t, tt.output, result["level"])
				assert.Equal(t, "kept", result["msg"])
			})
		}

		t.Run("WithLevel drops records below threshold", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithLevel(slog.LevelWarn)))

			logger.Info("dropped")
			logger.Warn("kept")

			var result map[string]any
			require.NoError(t, json.Unmarshal(buffer.Bytes(), &result))
			assert.Equal(t, "kept", result["msg"])
		})

		t.Run("WithLevel overrides env", func(t *testing.T) {
			t.Setenv("AWS_LAMBDA_LOG_LEVEL", "ERROR")

			handler := sloglambda.NewHandler(io.Discard, sloglambda.WithLevel(slog.LevelDebug))

			assert.True(t, handler.Enabled(t.Context(), slog.LevelDebug))
		})

		t.Run("WithLevel tracks a LevelVar", func(t *testing.T) {
			level := new(slog.LevelVar)
			handler := sloglambda.NewHandler(io.Discard, sloglambda.WithLevel(level))

			assert.True(t, handler.Enabled(t.Context(), slog.LevelInfo))

			level.Set(slog.LevelError)

			assert.False(t, handler.Enabled(t.Context(), slog.LevelInfo))
			assert.False(t, handler.WithGroup("g").Enabled(t.Context(), slog.LevelInfo))
		})
	})

	t.Run("concurrent writes", func(t *testing.T) {
		const goroutines = 50
		const perGoroutine = 20

		for _, format := range []struct {
			name   string
			option sloglambda.Option
			check  func(testing.TB, string)
		}{
			{"JSON", sloglambda.WithJSON(), func(t testing.TB, line string) {
				var result map[string]any
				require.NoError(t, json.Unmarshal([]byte(line), &result), line)
				assert.Equal(t, "concurrent", result["msg"])
			}},
			{"Text", sloglambda.WithText(), func(t testing.TB, line string) {
				assert.True(t, strings.HasPrefix(line, `level="INFO" msg="concurrent" type="app.log" `), line)
				assert.Regexp(t, ` goroutine=\d+ g\.j=\d+ g\.pad="x+"$`, line)
			}},
		} {
			t.Run(format.name, func(t *testing.T) {
				buffer := new(bytes.Buffer)
				root := slog.New(sloglambda.NewHandler(buffer, format.option, sloglambda.WithoutTime()))

				var wg sync.WaitGroup
				for i := range goroutines {
					logger := root.With("goroutine", i).WithGroup("g")
					wg.Go(func() {
						for j := range perGoroutine {
							logger.Info("concurrent", "j", j, "pad", strings.Repeat("x", 256))
						}
					})
				}
				wg.Wait()

				lines := strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
				require.Len(t, lines, goroutines*perGoroutine)
				for _, line := range lines {
					format.check(t, line)
				}
			})
		}
	})

	t.Run("field order", func(t *testing.T) {
		ctx := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{
			AwsRequestID: "abc-123",
		})

		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

			logger.With("zzz", 1).WithGroup("g").InfoContext(ctx, "hi", "yyy", 2, "aaa", 3)

			assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log","record":{"functionName":"test-function","version":"$LATEST","requestId":"abc-123"},"zzz":1,"g":{"yyy":2,"aaa":3}}`+"\n", buffer.String())
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

			logger.With("zzz", 1).WithGroup("g").InfoContext(ctx, "hi", "yyy", 2, "aaa", 3)

			assert.Equal(t, `level="INFO" msg="hi" type="app.log" record.functionName="test-function" record.version="$LATEST" record.requestId="abc-123" zzz=1 g.yyy=2 g.aaa=3`+"\n", buffer.String())
		})

		t.Run("time and source lead", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithSource()))

			logger.Info("hi")

			assert.Regexp(t, `^\{"time":"[^"]+","level":"INFO","source":\{"function":"[^"]+","file":"[^"]+","line":\d+\},"msg":"hi","type":"app.log",`, buffer.String())
		})
	})

	t.Run("WithoutTime", func(t *testing.T) {
		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

			logger.Info(t.Name())

			assert.NotContains(t, buffer.String(), `"time"`)
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

			logger.Info(t.Name())

			assert.NotContains(t, buffer.String(), `time=`)
		})
	})

	t.Run("WithSource", func(t *testing.T) {
		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithSource()))

			logger.Info(t.Name())

			assert.Contains(t, buffer.String(), `"source":{`)
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithSource()))

			logger.Info(t.Name())

			assert.Contains(t, buffer.String(), `source.function=`)
			assert.Contains(t, buffer.String(), `source.file=`)
			assert.Contains(t, buffer.String(), `source.line=`)
		})
	})

	t.Run("WithType", func(t *testing.T) {
		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithType(t.Name())))

			logger.Info(t.Name())

			assert.Contains(t, buffer.String(), `"type":"`+t.Name()+`"`)
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithType(t.Name())))

			logger.Info(t.Name())

			assert.Contains(t, buffer.String(), `type="`+t.Name()+`"`)
		})
	})

	t.Run("WithFunctionName and WithFunctionVersion", func(t *testing.T) {
		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithFunctionName("custom-function"), sloglambda.WithFunctionVersion("42")))

			logger.Info(t.Name())

			assert.Contains(t, buffer.String(), `"record":{"functionName":"custom-function","version":"42"}`)
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithFunctionName("custom-function"), sloglambda.WithFunctionVersion("42")))

			logger.Info(t.Name())

			assert.Contains(t, buffer.String(), `record.functionName="custom-function" record.version="42"`)
		})

		t.Run("empty values omit record", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithFunctionName(""), sloglambda.WithFunctionVersion("")))

			logger.Info(t.Name())

			assert.NotContains(t, buffer.String(), `"record"`)
		})

		t.Run("carried through WithAttrs and WithGroup", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithFunctionName("custom-function"), sloglambda.WithFunctionVersion("42")))

			logger.With("a", 1).WithGroup("g").Info(t.Name(), "b", 2)

			assert.Contains(t, buffer.String(), `"record":{"functionName":"custom-function","version":"42"}`)
		})
	})

	t.Run("env is read once at construction", func(t *testing.T) {
		buffer := new(bytes.Buffer)
		logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON()))

		t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "changed")
		t.Setenv("AWS_LAMBDA_FUNCTION_VERSION", "changed")

		logger.Info(t.Name())

		assert.Contains(t, buffer.String(), `"record":{"functionName":"test-function","version":"$LATEST"}`)
	})

	t.Run("given a lambda context", func(t *testing.T) {
		ctx := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{
			AwsRequestID: "abc-123",
		})

		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON()))

			logger.InfoContext(ctx, t.Name())

			assert.Contains(t, buffer.String(), `"requestId":"abc-123"`)
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText()))

			logger.InfoContext(ctx, t.Name())

			assert.Contains(t, buffer.String(), `record.requestId="abc-123"`)
		})
	})

	t.Run("given a json.Marshaler value", func(t *testing.T) {
		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON()))

			logger.Info("hi", "payload", json.RawMessage(`{"a":1}`))

			assert.Contains(t, buffer.String(), `"payload":{"a":1}`)
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText()))

			logger.Info("hi", "payload", json.RawMessage(`{"a":1}`))

			assert.Contains(t, buffer.String(), `payload={"a":1}`)
		})

		t.Run("Text indented", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText()))

			logger.Info("hi", "payload", json.RawMessage("{\n  \"a\": 1\n}"), "next", 2)

			assert.Contains(t, buffer.String(), `payload={"a":1} next=2`)
		})

		t.Run("Text nil", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText()))

			logger.Info("hi", "payload", json.RawMessage(nil))

			assert.Contains(t, buffer.String(), `payload=null`)
		})
	})

	t.Run("given reserved keys", func(t *testing.T) {
		ctx := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{
			AwsRequestID: "abc-123",
		})

		t.Run("WithGroup record", func(t *testing.T) {
			t.Run("JSON", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

				logger.WithGroup("record").InfoContext(ctx, "hi", "x", 1)

				assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log","record":{"functionName":"test-function","version":"$LATEST","requestId":"abc-123"},"record_":{"x":1}}`+"\n", buffer.String())
			})

			t.Run("Text", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

				logger.WithGroup("record").InfoContext(ctx, "hi", "x", 1)

				assert.Equal(t, `level="INFO" msg="hi" type="app.log" record.functionName="test-function" record.version="$LATEST" record.requestId="abc-123" record_.x=1`+"\n", buffer.String())
			})
		})

		t.Run("attr type", func(t *testing.T) {
			t.Run("JSON", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

				logger.Info("hi", "type", "nope")

				assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log","record":{"functionName":"test-function","version":"$LATEST"},"type_":"nope"}`+"\n", buffer.String())
			})

			t.Run("Text", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

				logger.Info("hi", "type", "nope")

				assert.Equal(t, `level="INFO" msg="hi" type="app.log" record.functionName="test-function" record.version="$LATEST" type_="nope"`+"\n", buffer.String())
			})
		})

		t.Run("attr type and type_", func(t *testing.T) {
			t.Run("JSON", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

				logger.Info("hi", "type", "a", "type_", "b")

				assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log","record":{"functionName":"test-function","version":"$LATEST"},"type__":"a","type_":"b"}`+"\n", buffer.String())
			})

			t.Run("Text", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

				logger.Info("hi", "type", "a", "type_", "b")

				assert.Equal(t, `level="INFO" msg="hi" type="app.log" record.functionName="test-function" record.version="$LATEST" type__="a" type_="b"`+"\n", buffer.String())
			})
		})

		t.Run("WithAttrs type", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

			logger.With("type", "nope").Info("hi")

			assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log","record":{"functionName":"test-function","version":"$LATEST"},"type_":"nope"}`+"\n", buffer.String())
		})

		t.Run("inline group keys are taken", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

			logger.Info("hi", "type", "a", slog.Group("", "type_", "b"))

			assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log","record":{"functionName":"test-function","version":"$LATEST"},"type__":"a","type_":"b"}`+"\n", buffer.String())
		})

		t.Run("empty groups are not taken", func(t *testing.T) {
			tests := []struct {
				name string
				log  func(*slog.Logger)
			}{
				{"record attr", func(l *slog.Logger) { l.Info("hi", "type", "a", slog.Group("type_")) }},
				{"nested record attr", func(l *slog.Logger) { l.Info("hi", "type", "a", slog.Group("type_", slog.Group("x"))) }},
				{"WithAttrs", func(l *slog.Logger) { l.With("type", "a", slog.Group("type_")).Info("hi") }},
				{"trailing WithGroup", func(l *slog.Logger) { l.With("type", "a").WithGroup("type_").Info("hi") }},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					buffer := new(bytes.Buffer)
					tt.log(slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime())))

					assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log","record":{"functionName":"test-function","version":"$LATEST"},"type_":"a"}`+"\n", buffer.String())
				})
			}
		})

		t.Run("non-empty groups are taken", func(t *testing.T) {
			tests := []struct {
				name string
				log  func(*slog.Logger)
				want string
			}{
				{"record attr", func(l *slog.Logger) { l.Info("hi", "type", "a", slog.Group("type_", "x", 1)) }, `"type__":"a","type_":{"x":1}}`},
				{"WithGroup", func(l *slog.Logger) { l.With("type", "a").WithGroup("type_").With("x", 1).Info("hi") }, `"type__":"a","type_":{"x":1}}`},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					buffer := new(bytes.Buffer)
					tt.log(slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime())))

					assert.True(t, strings.HasSuffix(buffer.String(), tt.want+"\n"), buffer.String())
				})
			}
		})

		t.Run("unnamed LogValuers resolve once", func(t *testing.T) {
			tests := []struct {
				name string
				log  func(*slog.Logger, slog.LogValuer)
				want string
			}{
				{"after collision", func(l *slog.Logger, v slog.LogValuer) { l.Info("hi", "type", "a", slog.Any("", v)) }, `"type_":"a","first":1}`},
				{"before collision", func(l *slog.Logger, v slog.LogValuer) { l.Info("hi", slog.Any("", v), "type", "a") }, `"first":1,"type_":"a"}`},
				{"WithAttrs", func(l *slog.Logger, v slog.LogValuer) { l.With(slog.Any("", v)).Info("hi", "type", "a") }, `"first":1,"type_":"a"}`},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					buffer := new(bytes.Buffer)
					valuer := &countingValuer{}
					tt.log(slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime())), valuer)

					assert.Equal(t, 1, valuer.calls)
					assert.True(t, strings.HasSuffix(buffer.String(), tt.want+"\n"), buffer.String())
				})
			}
		})

		t.Run("nested group keys are untouched", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

			logger.WithGroup("g").Info("hi", "type", "nope", slog.Group("record", "x", 1))

			assert.Contains(t, buffer.String(), `"g":{"type":"nope","record":{"x":1}}`)
		})

		t.Run("empty WithType leaves type alone", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithType("")))

			logger.Info("hi", "type", "mine")

			assert.Contains(t, buffer.String(), `"type":"mine"`)
			assert.NotContains(t, buffer.String(), `"type_"`)
		})

		t.Run("request ID alone reserves record", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime(), sloglambda.WithFunctionName(""), sloglambda.WithFunctionVersion("")))

			logger.InfoContext(ctx, "hi", "record", 1)
			logger.Info("hi", "record", 1)

			assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log","record":{"requestId":"abc-123"},"record_":1}`+"\n"+
				`{"level":"INFO","msg":"hi","type":"app.log","record":1}`+"\n", buffer.String())
		})

		t.Run("WithAttrs and record keys share the fallback space", func(t *testing.T) {
			tests := []struct {
				name string
				log  func(*slog.Logger)
				want string
			}{
				{"chain type_ then record type", func(l *slog.Logger) { l.With("type_", 1).Info("hi", "type", 2) }, `"type_":1,"type__":2}`},
				{"chain type then record type_", func(l *slog.Logger) { l.With("type", 1).Info("hi", "type_", 2) }, `"type__":1,"type_":2}`},
				{"chain inline group record", func(l *slog.Logger) { l.With(slog.Group("", "record", 1)).Info("hi") }, `"record_":1}`},
				{"chain record then WithGroup", func(l *slog.Logger) { l.With("record", 1).WithGroup("g").Info("hi", "b", 2) }, `"record_":1,"g":{"b":2}}`},
				{"nested chain record is untouched", func(l *slog.Logger) { l.With(slog.Group("x", "record", 1)).Info("hi") }, `"x":{"record":1}}`},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					buffer := new(bytes.Buffer)
					tt.log(slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime())))

					assert.True(t, strings.HasSuffix(buffer.String(), tt.want+"\n"), buffer.String())
				})
			}
		})
	})

	t.Run("given a WithAttrs and WithGroup chain", func(t *testing.T) {
		tests := []struct {
			name string
			log  func(*slog.Logger)
			json string
			text string
		}{
			{
				"pending group with no record attrs",
				func(l *slog.Logger) { l.WithGroup("g").Info("hi") },
				``, ``,
			},
			{
				"open group then pending group with no record attrs",
				func(l *slog.Logger) { l.WithGroup("g").With("a", 1).WithGroup("h").Info("hi") },
				`,"g":{"a":1}`, ` g.a=1`,
			},
			{
				"open group then pending group with record attrs",
				func(l *slog.Logger) { l.WithGroup("g").With("a", 1).WithGroup("h").Info("hi", "b", 2) },
				`,"g":{"a":1,"h":{"b":2}}`, ` g.a=1 g.h.b=2`,
			},
			{
				"attrs then pending group with no record attrs",
				func(l *slog.Logger) { l.With("a", 1).WithGroup("g").Info("hi") },
				`,"a":1`, ` a=1`,
			},
			{
				"nested pending groups",
				func(l *slog.Logger) { l.WithGroup("g").WithGroup("h").Info("hi", "b", 2) },
				`,"g":{"h":{"b":2}}`, ` g.h.b=2`,
			},
			{
				"nested pending groups with only an empty record group",
				func(l *slog.Logger) { l.WithGroup("g").WithGroup("h").Info("hi", slog.Group("x")) },
				``, ``,
			},
			{
				"group name needing quotes",
				func(l *slog.Logger) { l.WithGroup("g h").With("a", 1).Info("hi", "b", 2) },
				`,"g h":{"a":1,"b":2}`, ` "g h.a"=1 "g h.b"=2`,
			},
			{
				"WithAttrs of only empty groups does not open pending groups",
				func(l *slog.Logger) { l.With(slog.Group("z")).WithGroup("g").Info("hi", "b", 2) },
				`,"g":{"b":2}`, ` g.b=2`,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Run("JSON", func(t *testing.T) {
					buffer := new(bytes.Buffer)
					tt.log(slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime(), sloglambda.WithFunctionName(""), sloglambda.WithFunctionVersion(""))))

					assert.Equal(t, `{"level":"INFO","msg":"hi","type":"app.log"`+tt.json+"}\n", buffer.String())
				})

				t.Run("Text", func(t *testing.T) {
					buffer := new(bytes.Buffer)
					tt.log(slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime(), sloglambda.WithFunctionName(""), sloglambda.WithFunctionVersion(""))))

					assert.Equal(t, `level="INFO" msg="hi" type="app.log"`+tt.text+"\n", buffer.String())
				})
			})
		}

		t.Run("LogValuers resolve once in WithAttrs", func(t *testing.T) {
			for _, format := range []struct {
				name string
				opt  sloglambda.Option
			}{{"JSON", sloglambda.WithJSON()}, {"Text", sloglambda.WithText()}} {
				t.Run(format.name, func(t *testing.T) {
					buffer := new(bytes.Buffer)
					named := &countingValuer{}
					unnamed := &countingValuer{}
					nested := &countingValuer{}
					logger := slog.New(sloglambda.NewHandler(buffer, format.opt, sloglambda.WithoutTime())).
						With(slog.Any("named", named), slog.Any("", unnamed), slog.Group("g", slog.Any("nested", nested)))

					logger.Info("one")
					logger.Info("two")
					logger.Info("three", "type", "a")

					assert.Equal(t, 1, named.calls)
					assert.Equal(t, 1, unnamed.calls)
					assert.Equal(t, 1, nested.calls)
					assert.Equal(t, 9, strings.Count(buffer.String(), "first"))
					assert.NotContains(t, buffer.String(), "type_=2")
					assert.NotContains(t, buffer.String(), `"type_":2`)
				})
			}
		})
	})

	t.Run("given a typed nil Stringer", func(t *testing.T) {
		buffer := new(bytes.Buffer)
		logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText()))

		require.NotPanics(t, func() {
			logger.Info(t.Name(), "ip", (*net.IP)(nil))
		})

		assert.Contains(t, buffer.String(), "ip=<nil>")
	})

	t.Run("given unencodable values", func(t *testing.T) {
		tests := []struct {
			name  string
			value any
			text  string
		}{
			{"NaN", math.NaN(), "r=NaN"},
			{"+Inf", math.Inf(1), "r=+Inf"},
			{"chan", make(chan int), "r=0x"},
			{"nested failing marshaler", struct{ F failingMarshaler }{}, "r={"},
			{"invalid raw JSON", json.RawMessage(`{`), `r="!ERROR:`},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Run("JSON", func(t *testing.T) {
					buffer := new(bytes.Buffer)
					logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON()))

					logger.Info("ratio", "r", tt.value, "ok", 1)

					var result map[string]any
					require.NoError(t, json.Unmarshal(buffer.Bytes(), &result))
					assert.Equal(t, "INFO", result["level"])
					assert.Equal(t, "ratio", result["msg"])
					assert.Equal(t, float64(1), result["ok"])
					assert.Contains(t, result, "record")
					assert.IsType(t, "", result["r"])
					assert.True(t, strings.HasPrefix(result["r"].(string), "!ERROR:"), result["r"])
				})

				t.Run("Text", func(t *testing.T) {
					buffer := new(bytes.Buffer)
					logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText()))

					logger.Info("ratio", "r", tt.value, "ok", 1)

					assert.Contains(t, buffer.String(), `msg="ratio"`)
					assert.Contains(t, buffer.String(), "ok=1")
					assert.Contains(t, buffer.String(), tt.text)
				})
			})
		}
	})

	t.Run("given oversized records", func(t *testing.T) {
		big := strings.Repeat("a", 20<<10)

		t.Run("JSON", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

			logger.Info(big, "m", map[string]string{"k": big})
			logger.Info("small")

			lines := strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
			require.Len(t, lines, 2)
			assert.Contains(t, lines[0], `"m":{"k":"`+big+`"}`)
			assert.True(t, strings.HasPrefix(lines[1], `{"level":"INFO","msg":"small"`), lines[1])
		})

		t.Run("Text", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

			logger.Info("hi", slog.Group(big, "x", 1))
			logger.Info("small", "y", 2)

			lines := strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
			require.Len(t, lines, 2)
			assert.True(t, strings.HasSuffix(lines[0], " "+big+".x=1"), "missing big group key")
			assert.True(t, strings.HasSuffix(lines[1], " y=2"), lines[1])
		})
	})

	t.Run("given a failing writer", func(t *testing.T) {
		t.Run("JSON", func(t *testing.T) {
			handler := sloglambda.NewHandler(failingWriter{}, sloglambda.WithJSON())

			err := handler.Handle(t.Context(), slog.NewRecord(time.Now(), slog.LevelInfo, "hi", 0))

			assert.ErrorIs(t, err, errWrite)
		})

		t.Run("Text", func(t *testing.T) {
			handler := sloglambda.NewHandler(failingWriter{}, sloglambda.WithText())

			err := handler.Handle(t.Context(), slog.NewRecord(time.Now(), slog.LevelInfo, "hi", 0))

			assert.ErrorIs(t, err, errWrite)
		})
	})
}

var errWrite = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errWrite
}

type countingValuer struct {
	calls int
}

func (v *countingValuer) LogValue() slog.Value {
	v.calls++
	if v.calls == 1 {
		return slog.GroupValue(slog.Int("first", 1))
	}
	return slog.GroupValue(slog.Int("type_", 2))
}

type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New(`bad "quoted" value`)
}

func BenchmarkJSON(b *testing.B) {
	logger := slog.New(sloglambda.NewHandler(io.Discard, sloglambda.WithJSON())).WithGroup("benchmark").With("format", "json")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		logger.Info("test", "count", i)
	}
}

func BenchmarkText(b *testing.B) {
	logger := slog.New(sloglambda.NewHandler(io.Discard, sloglambda.WithText())).WithGroup("benchmark").With("format", "text")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		logger.Info("test", "count", i)
	}
}

func BenchmarkJSONGroups(b *testing.B) {
	benchmarkGroups(b, sloglambda.WithJSON())
}

func BenchmarkTextGroups(b *testing.B) {
	benchmarkGroups(b, sloglambda.WithText())
}

func benchmarkGroups(b *testing.B, format sloglambda.Option) {
	ctx := lambdacontext.NewContext(context.Background(), &lambdacontext.LambdaContext{
		AwsRequestID: "abc-123",
	})
	logger := slog.New(sloglambda.NewHandler(io.Discard, format)).
		With("service", "orders", "region", "us-east-1").
		WithGroup("request").
		With("method", "POST", "path", "/orders")

	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		logger.InfoContext(ctx, "handled",
			"status", 201,
			"duration", 42*time.Millisecond,
			slog.Group("user", "id", i, "admin", false, slog.Group("plan", "name", "pro", "seats", 5)),
			slog.Group("empty"),
			"ratio", 0.75,
		)
	}
}
