package sloglambda_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	sloglambda "github.com/angryboat/slog-lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValueTypes(t *testing.T) {
	buffer := new(bytes.Buffer)
	newHandler := func(t testing.TB) slog.Handler {
		t.Cleanup(buffer.Reset)

		return sloglambda.NewHandler(buffer, sloglambda.WithLevel(slog.LevelDebug), sloglambda.WithText())
	}

	t.Run("http.Request", func(t *testing.T) {
		logger := slog.New(newHandler(t))
		req, _ := http.NewRequest("GET", "/foo/bar", nil)
		logger.InfoContext(t.Context(), t.Name(), slog.Any("request", req))
		assert.Contains(t, buffer.String(), ` request="&{GET /foo/bar HTTP/1.1 `)
		assert.True(t, strings.HasSuffix(buffer.String(), `}"`+"\n"), buffer.String())
	})

	t.Run("http.Request in Group", func(t *testing.T) {
		logger := slog.New(newHandler(t))
		req, _ := http.NewRequest("GET", "/foo/bar", nil)
		logger.InfoContext(t.Context(), t.Name(), slog.Group("http", slog.Any("request", req)))
		assert.Contains(t, buffer.String(), ` http.request="&{GET /foo/bar HTTP/1.1 `)
		assert.True(t, strings.HasSuffix(buffer.String(), `}"`+"\n"), buffer.String())
	})

	t.Run("http.Request JSON", func(t *testing.T) {
		buffer := new(bytes.Buffer)
		logger := slog.New(sloglambda.NewHandler(buffer, sloglambda.WithJSON()))
		req, _ := http.NewRequest("GET", "/foo/bar", nil)
		logger.InfoContext(t.Context(), t.Name(), slog.Any("request", req))

		var result map[string]any
		require.NoError(t, json.Unmarshal(buffer.Bytes(), &result))
		assert.Equal(t, t.Name(), result["msg"])
		assert.True(t, strings.HasPrefix(result["request"].(string), "!ERROR:"), result["request"])
	})

	t.Run("LogValuer", func(t *testing.T) {
		t.Run("top level attr", func(t *testing.T) {
			logger := slog.New(newHandler(t))
			logger.InfoContext(t.Context(), t.Name(), "value", testLogValuer{"Foo Bar"})
			assert.Contains(t, buffer.String(), `value="Foo Bar"`)
		})

		t.Run("group level attr", func(t *testing.T) {
			logger := slog.New(newHandler(t))
			logger.InfoContext(t.Context(), t.Name(), slog.Group("group", "value", testLogValuer{"Foo Bar"}))
			assert.Contains(t, buffer.String(), `group.value="Foo Bar"`)
		})

		t.Run("nested top level attr", func(t *testing.T) {
			logger := slog.New(newHandler(t))
			logger.InfoContext(t.Context(), t.Name(), "value", nestedTestLogValuer{testLogValuer{"Foo Bar"}})
			assert.Contains(t, buffer.String(), `value="Foo Bar"`)
		})

		t.Run("nested group level attr", func(t *testing.T) {
			logger := slog.New(newHandler(t))
			logger.InfoContext(t.Context(), t.Name(), slog.Group("group", "value", nestedTestLogValuer{testLogValuer{"Foo Bar"}}))
			assert.Contains(t, buffer.String(), `group.value="Foo Bar"`)
		})
	})

	t.Run("LogValuer resolving to group", func(t *testing.T) {
		t.Run("top level attr", func(t *testing.T) {
			logger := slog.New(newHandler(t))
			logger.InfoContext(t.Context(), t.Name(), "value", groupTestLogValuer{})
			assert.Contains(t, buffer.String(), `value.foo="bar"`)
		})

		t.Run("group level attr", func(t *testing.T) {
			logger := slog.New(newHandler(t))
			logger.InfoContext(t.Context(), t.Name(), slog.Group("group", "value", groupTestLogValuer{}))
			assert.Contains(t, buffer.String(), `group.value.foo="bar"`)
		})
	})
}

type groupTestLogValuer struct{}

func (groupTestLogValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("foo", "bar"), slog.Int("num", 42))
}

type nestedTestLogValuer struct {
	Value testLogValuer
}

func (lv nestedTestLogValuer) LogValue() slog.Value {
	return slog.AnyValue(lv.Value)
}

type testLogValuer struct {
	Value string
}

func (lv testLogValuer) LogValue() slog.Value {
	return slog.StringValue(lv.Value)
}

var _ slog.LogValuer = (*testLogValuer)(nil)
