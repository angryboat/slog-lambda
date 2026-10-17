package sloglambda

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func Test_loggerLevelFromString(t *testing.T) {
	cases := map[string]slog.Level{
		"TRACE":  slog.LevelDebug - 4,
		"DEBUG":  slog.LevelDebug,
		"INFO":   slog.LevelInfo,
		"WARN":   slog.LevelWarn,
		"ERROR":  slog.LevelError,
		"FATAL":  slog.LevelError + 4,
		"trace":  slog.LevelDebug - 4,
		"debug":  slog.LevelDebug,
		"info":   slog.LevelInfo,
		"Warn":   slog.LevelWarn,
		" error": slog.LevelError,
		" info ": slog.LevelInfo,
		"":       slog.LevelInfo,
	}

	for str, level := range cases {
		t.Run(fmt.Sprintf("%s=%s", str, &level), func(t *testing.T) {
			assert.Equal(t, level, loggerLevelFromString(str))
		})
	}
}

func Test_lambdaLoggerLevelString(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug - 8: "TRACE-4",
		slog.LevelDebug - 4: "TRACE",
		slog.LevelDebug:     "DEBUG",
		slog.LevelInfo:      "INFO",
		slog.LevelWarn:      "WARN",
		slog.LevelError:     "ERROR",
		slog.LevelError + 4: "FATAL",
		slog.LevelError + 8: "FATAL+4",
	}

	for level, str := range cases {
		t.Run(fmt.Sprintf("%s=%s", level, str), func(t *testing.T) {
			assert.Equal(t, str, lambdaLoggerLevelString(level))
		})
	}
}

func Test_textNeedsQuoting(t *testing.T) {
	cases := map[string]bool{
		"":           true,
		"plain":      false,
		"a.b_c-1":    false,
		`back\slash`: false,
		"a b":        true,
		"a=b":        true,
		`a"b`:        true,
		"a\nb":       true,
		"a\tb":       true,
		"a\x7fb":     true,
		"\xff":       true,
		"héllo":      false,
		"a\u00a0b":   true,
	}

	for input, want := range cases {
		assert.Equal(t, want, textNeedsQuoting(input), "input %q", input)
	}
}

func Test_appendJSONString(t *testing.T) {
	cases := map[string]string{
		"":                         `""`,
		"plain":                    `"plain"`,
		`quote " and backslash \`:  `"quote \" and backslash \\"`,
		"newline\n tab\t return\r": `"newline\n tab\t return\r"`,
		"control \x00 \x1f \x7f":   "\"control \\u0000 \\u001f \x7f\"",
		"html <b>&amp;</b>":        `"html <b>&amp;</b>"`,
		"héllo 世界 🎉":               `"héllo 世界 🎉"`,
		"sep \u2028 \u2029":        `"sep \u2028 \u2029"`,
	}

	for input, want := range cases {
		assert.Equal(t, want, string(appendJSONString(nil, input)), "input %q", input)
	}

	t.Run("invalid UTF-8", func(t *testing.T) {
		var got string
		assert.NoError(t, json.Unmarshal(appendJSONString(nil, "a\xffb\xc3"), &got))
		assert.Equal(t, "a\ufffdb\ufffd", got)
	})
}

func Test_appendJSONFloat(t *testing.T) {
	inputs := []float64{0, 1, -1, 1.5, 1e-6, 1e-7, 1.23e-9, 1e20, 1e21, -1e21, 1e100, math.MaxFloat64, math.SmallestNonzeroFloat64}

	for _, input := range inputs {
		want, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}

		assert.Equal(t, string(want), string(appendJSONFloat(nil, input)), "input %v", input)
	}
}

func Test_appendValue(t *testing.T) {
	ts := time.Date(2026, 10, 16, 1, 2, 3, 4, time.UTC)

	cases := []struct {
		name  string
		value slog.Value
		json  string
		text  string
	}{
		{"string", slog.StringValue("a b"), `"a b"`, `"a b"`},
		{"int64", slog.Int64Value(-42), `-42`, `-42`},
		{"uint64", slog.Uint64Value(42), `42`, `42`},
		{"bool", slog.BoolValue(true), `true`, `true`},
		{"float64", slog.Float64Value(1.23), `1.23`, `1.23`},
		{"NaN", slog.Float64Value(math.NaN()), `"!ERROR:json: unsupported value: NaN"`, `NaN`},
		{"+Inf", slog.Float64Value(math.Inf(1)), `"!ERROR:json: unsupported value: +Inf"`, `+Inf`},
		{"duration", slog.DurationValue(time.Second), `"1s"`, `"1s"`},
		{"time", slog.TimeValue(ts), `"2026-10-16T01:02:03.000000004Z"`, `"2026-10-16T01:02:03.000000004Z"`},
		{"nil", slog.AnyValue(nil), `null`, `<nil>`},
		{"error", slog.AnyValue(errors.New("boom")), `"boom"`, `"boom"`},
		{"json.Marshaler", slog.AnyValue(jsonMarshalerSuccess{}), `"JSON Marshal"`, `"JSON Marshal"`},
		{"indented json.Marshaler", slog.AnyValue(json.RawMessage("{\n  \"a\": 1\n}")), `{"a":1}`, `{"a":1}`},
		{"stringer", slog.AnyValue(stringerValue{}), `{}`, `stringerValue`},
		{"struct", slog.AnyValue(struct {
			A string
			B int
		}{"x y", 1}), `{"A":"x y","B":1}`, `"{x y 1}"`},
		{"html", slog.AnyValue(map[string]string{"a": "<b>"}), `{"a":"<b>"}`, `map[a:<b>]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, format := range []struct {
				json bool
				want string
			}{{true, tc.json}, {false, tc.text}} {
				s := &handleState{h: &Handler{json: format.json}}
				s.appendValue(tc.value)
				assert.Equal(t, format.want, string(s.buf), "json=%v", format.json)
			}
		})
	}
}

func Test_appendValue_failingMarshaler(t *testing.T) {
	for _, json := range []bool{true, false} {
		s := &handleState{h: &Handler{json: json}}
		s.appendValue(slog.AnyValue(jsonMarshalerFail{}))

		assert.True(t, strings.HasPrefix(string(s.buf), `"!ERROR:json: error calling MarshalJSON`), string(s.buf))
		assert.Contains(t, string(s.buf), assert.AnError.Error())
	}
}

func Test_handleState_appendKey(t *testing.T) {
	cases := []struct {
		prefix string
		key    string
		want   string
	}{
		{"", "plain", `plain=`},
		{"", "a b", `"a b"=`},
		{"", "a=c", `"a=c"=`},
		{"a", "b c", `"a.b c"=`},
		{"a b", "c", `"a b.c"=`},
		{"a", "b", `a.b=`},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s", tc.prefix, tc.key), func(t *testing.T) {
			s := &handleState{h: &Handler{}}
			if tc.prefix != "" {
				s.openGroup(tc.prefix)
			}
			s.appendKey(tc.key)
			assert.Equal(t, tc.want, string(s.buf))
		})
	}
}

func Test_handleState_groups(t *testing.T) {
	t.Run("empty groups are dropped", func(t *testing.T) {
		for _, json := range []bool{true, false} {
			s := &handleState{h: &Handler{json: json}}
			s.begin()
			s.appendKey("a")
			s.appendValue(slog.IntValue(1))
			outer := s.openGroup("G")
			inner := s.openGroup("H")
			s.closeGroup(inner)
			s.closeGroup(outer)
			s.appendKey("b")
			s.appendValue(slog.IntValue(2))
			s.end()

			if json {
				assert.Equal(t, "{\"a\":1,\"b\":2}\n", string(s.buf))
			} else {
				assert.Equal(t, "a=1 b=2\n", string(s.buf))
			}
		}
	})

	t.Run("inline groups flatten", func(t *testing.T) {
		s := &handleState{h: &Handler{json: true}}
		s.begin()
		s.appendAttr(slog.Group("", slog.String("foo", "bar")))
		s.end()

		assert.Equal(t, "{\"foo\":\"bar\"}\n", string(s.buf))
	})

	t.Run("LogValuer resolving to group", func(t *testing.T) {
		s := &handleState{h: &Handler{}}
		s.appendAttr(slog.Any("v", groupLogValuer{}))

		assert.Equal(t, `v.foo="bar"`, string(s.buf))
	})
}

type stringerValue struct{}

func (s stringerValue) String() string {
	return "stringerValue"
}

type groupLogValuer struct{}

func (groupLogValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("foo", "bar"))
}

type jsonMarshalerSuccess struct{}

func (jsonMarshalerSuccess) MarshalJSON() ([]byte, error) {
	return []byte(`"JSON Marshal"`), nil
}

var _ json.Marshaler = (*jsonMarshalerSuccess)(nil)

type jsonMarshalerFail struct{}

func (jsonMarshalerFail) MarshalJSON() ([]byte, error) {
	return nil, assert.AnError
}

var _ json.Marshaler = (*jsonMarshalerFail)(nil)
