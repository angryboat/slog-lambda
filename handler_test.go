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
				assert.True(t, strings.HasPrefix(line, `g.j=`), line)
				assert.Contains(t, line, ` level="INFO" msg="concurrent" `)
				assert.True(t, strings.HasSuffix(line, ` type="app.log"`), line)
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

			assert.Contains(t, buffer.String(), `next=2 payload={"a":1}`)
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

				assert.JSONEq(t, `{"level":"INFO","msg":"hi","record":{"functionName":"test-function","version":"$LATEST","requestId":"abc-123"},"record_":{"x":1},"type":"app.log"}`, buffer.String())
			})

			t.Run("Text", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

				logger.WithGroup("record").InfoContext(ctx, "hi", "x", 1)

				assert.Equal(t, `level="INFO" msg="hi" record.functionName="test-function" record.requestId="abc-123" record.version="$LATEST" record_.x=1 type="app.log"`+"\n", buffer.String())
			})
		})

		t.Run("attr type", func(t *testing.T) {
			t.Run("JSON", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

				logger.Info("hi", "type", "nope")

				assert.JSONEq(t, `{"level":"INFO","msg":"hi","record":{"functionName":"test-function","version":"$LATEST"},"type":"app.log","type_":"nope"}`, buffer.String())
			})

			t.Run("Text", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

				logger.Info("hi", "type", "nope")

				assert.Contains(t, buffer.String(), `type="app.log" type_="nope"`)
			})
		})

		t.Run("attr type and type_", func(t *testing.T) {
			t.Run("JSON", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

				logger.Info("hi", "type", "a", "type_", "b")

				assert.JSONEq(t, `{"level":"INFO","msg":"hi","record":{"functionName":"test-function","version":"$LATEST"},"type":"app.log","type_":"b","type__":"a"}`, buffer.String())
			})

			t.Run("Text", func(t *testing.T) {
				buffer := new(bytes.Buffer)
				logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithText(), sloglambda.WithoutTime()))

				logger.Info("hi", "type", "a", "type_", "b")

				assert.Contains(t, buffer.String(), `type="app.log" type_="b" type__="a"`)
			})
		})

		t.Run("WithAttrs type", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

			logger.With("type", "nope").Info("hi")

			assert.Contains(t, buffer.String(), `"type":"app.log","type_":"nope"`)
		})

		t.Run("nested group keys are untouched", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithoutTime()))

			logger.WithGroup("g").Info("hi", "type", "nope", slog.Group("record", "x", 1))

			assert.Contains(t, buffer.String(), `"g":{"record":{"x":1},"type":"nope"}`)
		})

		t.Run("empty WithType leaves type alone", func(t *testing.T) {
			buffer := new(bytes.Buffer)
			logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON(), sloglambda.WithType("")))

			logger.Info("hi", "type", "mine")

			assert.Contains(t, buffer.String(), `"type":"mine"`)
			assert.NotContains(t, buffer.String(), `"type_"`)
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
			{"invalid raw JSON", json.RawMessage(`{`), "r={"},
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

	t.Run("given a value that fails to encode on retry", func(t *testing.T) {
		buffer := new(bytes.Buffer)
		handler := sloglambda.NewHandler(buffer, sloglambda.WithJSON())

		record := slog.NewRecord(time.Now(), slog.LevelInfo, "ratio", 0)
		record.AddAttrs(slog.Any("r", struct{ F *flakyMarshaler }{new(flakyMarshaler)}))

		err := handler.Handle(t.Context(), record)

		require.ErrorContains(t, err, `bad "quoted" value`)
		assert.Equal(t, 1, strings.Count(buffer.String(), "\n"))

		var result map[string]any
		require.NoError(t, json.Unmarshal(buffer.Bytes(), &result))
		assert.Equal(t, "ERROR", result["level"])
		assert.Contains(t, result["msg"], `bad "quoted" value`)
	})

	t.Run("given a value that fails to encode on retry and a failing writer", func(t *testing.T) {
		handler := sloglambda.NewHandler(failingWriter{}, sloglambda.WithJSON())

		record := slog.NewRecord(time.Now(), slog.LevelInfo, "ratio", 0)
		record.AddAttrs(slog.Any("r", struct{ F *flakyMarshaler }{new(flakyMarshaler)}))

		err := handler.Handle(t.Context(), record)

		require.ErrorIs(t, err, errWrite)
		assert.ErrorContains(t, err, `bad "quoted" value`)
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

type flakyMarshaler struct{ calls int }

func (m *flakyMarshaler) MarshalJSON() ([]byte, error) {
	m.calls++
	if m.calls%2 == 1 {
		return nil, errors.New(`bad "quoted" value`)
	}
	return []byte(`"ok"`), nil
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
