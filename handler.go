// Package sloglambda is a [slog.Handler] for AWS Lambda functions.
//
// It auto-configures log format (JSON or text) and level from Lambda's
// advanced logging environment variables. Options override env values.
//
// Fields are written in a fixed order, matching [slog.JSONHandler] and
// [slog.TextHandler]: "time", "level", "source", "msg", then the handler's
// "type" and "record", then attrs and groups in the order they were added.
// Values that cannot be encoded as JSON are written as "!ERROR:<reason>"
// strings so a record is never dropped.
//
// The handler reserves two top-level keys: "record" (Lambda function name,
// version, and request ID) and "type" (see [WithType]). When the handler sets
// one of these and an attr or group uses the same key at the top level, the
// handler's value wins and the caller's value moves to the first unused key
// formed by appending underscores ("record_", "type_", "type__", ...).
//
// See https://docs.aws.amazon.com/lambda/latest/dg/monitoring-logs.html
package sloglambda

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/lambdacontext"
)

const (
	lambdaEnvLogLevel        = "AWS_LAMBDA_LOG_LEVEL"
	lambdaEnvLogFormat       = "AWS_LAMBDA_LOG_FORMAT"
	lambdaEnvFunctionName    = "AWS_LAMBDA_FUNCTION_NAME"
	lambdaEnvFunctionVersion = "AWS_LAMBDA_FUNCTION_VERSION"

	traceLevelDebugOffset = 4
	fatalLevelErrorOffset = 4
)

const (
	kLambdaRecord          = "record"
	kLambdaFunctionName    = "functionName"
	kLambdaFunctionVersion = "version"
	kLambdaRequestId       = "requestId"
	kLambdaLogType         = "type"
)

// Handler implements [slog.Handler] and writes structured log records to an
// [io.Writer]. Use [NewHandler] to create one.
type Handler struct {
	out         io.Writer
	logType     string
	mu          *sync.Mutex
	level       slog.Leveler
	json        bool
	source      bool
	excludeTime bool
	gattr       []groupOrAttrs

	functionName    string
	functionVersion string
}

// Option configures a [Handler]. Pass options to [NewHandler].
type Option func(*Handler)

// WithLevel sets the minimum log level. Messages below this level are dropped.
// Defaults to the value of AWS_LAMBDA_LOG_LEVEL, or INFO if unset. A nil
// level means INFO, matching [slog.HandlerOptions].
func WithLevel(level slog.Leveler) Option {
	if level == nil {
		level = slog.LevelInfo
	}
	return func(h *Handler) {
		h.level = level
	}
}

// WithJSON sets the output format to JSON.
func WithJSON() Option {
	return func(h *Handler) {
		h.json = true
	}
}

// WithText sets the output format to key=value text.
func WithText() Option {
	return func(h *Handler) {
		h.json = false
	}
}

// WithSource adds caller information (function, file, line) to each log record.
func WithSource() Option {
	return func(h *Handler) {
		h.source = true
	}
}

// WithType sets the "type" field included in every log record.
// Defaults to "app.log".
func WithType(logType string) Option {
	return func(h *Handler) {
		h.logType = logType
	}
}

// WithFunctionName sets the function name included in the "record" field.
// Defaults to the value of AWS_LAMBDA_FUNCTION_NAME. An empty name omits it.
func WithFunctionName(name string) Option {
	return func(h *Handler) {
		h.functionName = name
	}
}

// WithFunctionVersion sets the function version included in the "record"
// field. Defaults to the value of AWS_LAMBDA_FUNCTION_VERSION. An empty version
// omits it.
func WithFunctionVersion(version string) Option {
	return func(h *Handler) {
		h.functionVersion = version
	}
}

// WithoutTime omits the timestamp from log output. Useful for testing or when
// CloudWatch already provides timestamps.
func WithoutTime() Option {
	return func(h *Handler) {
		h.excludeTime = true
	}
}

// NewHandler returns a [Handler] that writes to w.
//
// By default it reads AWS_LAMBDA_LOG_LEVEL (TRACE, DEBUG, INFO, WARN, ERROR,
// FATAL), AWS_LAMBDA_LOG_FORMAT (json, text), AWS_LAMBDA_FUNCTION_NAME, and
// AWS_LAMBDA_FUNCTION_VERSION from the environment once, at construction. Any
// provided options take precedence over environment values.
func NewHandler(w io.Writer, options ...Option) *Handler {
	h := &Handler{
		out:             w,
		mu:              new(sync.Mutex),
		level:           loggerLevelFromLambdaEnv(),
		json:            loggerIsJSON(),
		source:          false,
		logType:         "app.log",
		functionName:    os.Getenv(lambdaEnvFunctionName),
		functionVersion: os.Getenv(lambdaEnvFunctionVersion),
	}

	for _, opt := range options {
		opt(h)
	}

	return h
}

func loggerLevelFromLambdaEnv() slog.Level {
	return loggerLevelFromString(os.Getenv(lambdaEnvLogLevel))
}

func loggerLevelFromString(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "trace":
		return slog.LevelDebug - traceLevelDebugOffset
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "fatal":
		return slog.LevelError + fatalLevelErrorOffset
	default:
		return slog.LevelInfo
	}
}

func lambdaLoggerLevelString(l slog.Level) string {
	str := func(base string, val slog.Level) string {
		if val == 0 {
			return base
		}
		return fmt.Sprintf("%s%+d", base, val)
	}

	switch {
	case l < slog.LevelDebug:
		return str("TRACE", l-(slog.LevelDebug-traceLevelDebugOffset))
	case l < slog.LevelInfo:
		return str("DEBUG", l-slog.LevelDebug)
	case l < slog.LevelWarn:
		return str("INFO", l-slog.LevelInfo)
	case l < slog.LevelError:
		return str("WARN", l-slog.LevelWarn)
	case l < slog.LevelError+fatalLevelErrorOffset:
		return str("ERROR", l-slog.LevelError)
	default:
		return str("FATAL", l-(slog.LevelError+fatalLevelErrorOffset))
	}
}

func loggerIsJSON() bool {
	env := os.Getenv(lambdaEnvLogFormat)
	return strings.ToLower(strings.TrimSpace(env)) == "json"
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *Handler) WithAttrs(attr []slog.Attr) slog.Handler {
	if len(attr) == 0 {
		return h
	}
	return h.copy(groupOrAttrs{attrs: attr})
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.copy(groupOrAttrs{group: name})
}

func (h *Handler) copy(g groupOrAttrs) *Handler {
	c := *h
	c.gattr = make([]groupOrAttrs, len(h.gattr)+1)
	copy(c.gattr, h.gattr)
	c.gattr[len(c.gattr)-1] = g
	return &c
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	s := newHandleState(h, record)
	defer s.free()

	s.begin()

	if !record.Time.IsZero() && !h.excludeTime {
		s.appendKey(slog.TimeKey)
		s.appendTime(record.Time)
	}

	s.appendKey(slog.LevelKey)
	s.appendString(lambdaLoggerLevelString(record.Level))

	if record.PC != 0 && h.source {
		frames := runtime.CallersFrames([]uintptr{record.PC})
		frame, _ := frames.Next()

		g := s.openGroup(slog.SourceKey)
		s.appendKey("function")
		s.appendString(frame.Function)
		s.appendKey("file")
		s.appendString(frame.File)
		s.appendKey("line")
		s.buf = strconv.AppendInt(s.buf, int64(frame.Line), 10)
		s.closeGroup(g)
	}

	s.appendKey(slog.MessageKey)
	s.appendString(record.Message)

	if h.logType != "" {
		s.appendKey(kLambdaLogType)
		s.appendString(h.logType)
	}

	lc, _ := lambdacontext.FromContext(ctx)
	s.hasRecord = h.functionName != "" || h.functionVersion != "" || lc != nil
	if s.hasRecord {
		g := s.openGroup(kLambdaRecord)
		if h.functionName != "" {
			s.appendKey(kLambdaFunctionName)
			s.appendString(h.functionName)
		}
		if h.functionVersion != "" {
			s.appendKey(kLambdaFunctionVersion)
			s.appendString(h.functionVersion)
		}
		if lc != nil {
			s.appendKey(kLambdaRequestId)
			s.appendString(lc.AwsRequestID)
		}
		s.closeGroup(g)
	}

	s.appendGroupsAndAttrs(h.gattr)
	s.end()

	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := h.out.Write(s.buf)
	return err
}

var _ slog.Handler = (*Handler)(nil)

type groupOrAttrs struct {
	group string      // group name if non-empty
	attrs []slog.Attr // attrs if non-empty
}

type handleState struct {
	h      *Handler
	record slog.Record
	buf    []byte
	sep    bool
	depth  int

	prefix      []byte
	prefixQuote bool

	hasRecord bool
	taken     map[string]bool
}

type groupMark struct {
	start       int
	opened      int
	sep         bool
	prefixLen   int
	prefixQuote bool
}

const maxPooledBufferSize = 16 << 10

var statePool = sync.Pool{
	New: func() any {
		return &handleState{buf: make([]byte, 0, 1024)}
	},
}

func newHandleState(h *Handler, record slog.Record) *handleState {
	s := statePool.Get().(*handleState)
	s.h = h
	s.record = record
	return s
}

func (s *handleState) free() {
	if cap(s.buf) > maxPooledBufferSize {
		return
	}
	*s = handleState{buf: s.buf[:0], prefix: s.prefix[:0]}
	statePool.Put(s)
}

func (s *handleState) begin() {
	if s.h.json {
		s.buf = append(s.buf, '{')
	}
}

func (s *handleState) end() {
	if s.h.json {
		s.buf = append(s.buf, '}')
	}
	s.buf = append(s.buf, '\n')
}

func (s *handleState) appendGroupsAndAttrs(gattr []groupOrAttrs) {
	for i, ga := range gattr {
		if ga.group != "" {
			g := s.openGroup(s.userKey(ga.group))
			s.appendGroupsAndAttrs(gattr[i+1:])
			s.closeGroup(g)
			return
		}
		for _, a := range ga.attrs {
			s.appendAttr(a)
		}
	}

	s.record.Attrs(func(a slog.Attr) bool {
		s.appendAttr(a)
		return true
	})
}

func (s *handleState) appendAttr(a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}

	if a.Value.Kind() != slog.KindGroup {
		s.appendKey(s.userKey(a.Key))
		s.appendValue(a.Value)
		return
	}

	if a.Key == "" {
		for _, ga := range a.Value.Group() {
			s.appendAttr(ga)
		}
		return
	}

	g := s.openGroup(s.userKey(a.Key))
	for _, ga := range a.Value.Group() {
		s.appendAttr(ga)
	}
	s.closeGroup(g)
}

// openGroup starts a nested group. closeGroup drops it again if nothing was
// written inside, since slog requires empty groups to be omitted.
func (s *handleState) openGroup(name string) groupMark {
	g := groupMark{
		start:       len(s.buf),
		sep:         s.sep,
		prefixLen:   len(s.prefix),
		prefixQuote: s.prefixQuote,
	}

	if s.h.json {
		s.appendKey(name)
		s.buf = append(s.buf, '{')
		s.sep = false
	} else {
		s.prefix = append(append(s.prefix, name...), '.')
		s.prefixQuote = s.prefixQuote || textNeedsQuoting(name)
	}

	s.depth++
	g.opened = len(s.buf)
	return g
}

func (s *handleState) closeGroup(g groupMark) {
	s.depth--
	s.prefix = s.prefix[:g.prefixLen]
	s.prefixQuote = g.prefixQuote

	if len(s.buf) == g.opened {
		s.buf = s.buf[:g.start]
		s.sep = g.sep
		return
	}

	if s.h.json {
		s.buf = append(s.buf, '}')
	}
}

func (s *handleState) userKey(key string) string {
	if s.depth > 0 || !s.isReserved(key) {
		return key
	}

	if s.taken == nil {
		s.taken = s.topLevelUserKeys()
	}

	fallback := key + "_"
	for s.taken[fallback] {
		fallback += "_"
	}
	return fallback
}

func (s *handleState) isReserved(key string) bool {
	switch key {
	case kLambdaLogType:
		return s.h.logType != ""
	case kLambdaRecord:
		return s.hasRecord
	default:
		return false
	}
}

func (s *handleState) topLevelUserKeys() map[string]bool {
	keys := make(map[string]bool)

	for _, ga := range s.h.gattr {
		if ga.group != "" {
			keys[ga.group] = true
			return keys
		}
		for _, a := range ga.attrs {
			addTopLevelKey(keys, a)
		}
	}

	s.record.Attrs(func(a slog.Attr) bool {
		addTopLevelKey(keys, a)
		return true
	})
	return keys
}

func addTopLevelKey(keys map[string]bool, a slog.Attr) {
	if a.Key != "" {
		keys[a.Key] = true
		return
	}

	if v := a.Value.Resolve(); v.Kind() == slog.KindGroup {
		for _, ga := range v.Group() {
			addTopLevelKey(keys, ga)
		}
	}
}

func (s *handleState) appendKey(key string) {
	if s.sep {
		if s.h.json {
			s.buf = append(s.buf, ',')
		} else {
			s.buf = append(s.buf, ' ')
		}
	}
	s.sep = true

	if s.h.json {
		s.buf = appendJSONString(s.buf, key)
		s.buf = append(s.buf, ':')
		return
	}

	if s.prefixQuote || textNeedsQuoting(key) {
		s.buf = strconv.AppendQuote(s.buf, string(s.prefix)+key)
	} else {
		s.buf = append(s.buf, s.prefix...)
		s.buf = append(s.buf, key...)
	}
	s.buf = append(s.buf, '=')
}

func (s *handleState) appendValue(v slog.Value) {
	switch v.Kind() {
	case slog.KindString:
		s.appendString(v.String())
	case slog.KindInt64:
		s.buf = strconv.AppendInt(s.buf, v.Int64(), 10)
	case slog.KindUint64:
		s.buf = strconv.AppendUint(s.buf, v.Uint64(), 10)
	case slog.KindBool:
		s.buf = strconv.AppendBool(s.buf, v.Bool())
	case slog.KindFloat64:
		s.appendFloat(v.Float64())
	case slog.KindDuration:
		s.appendString(v.Duration().String())
	case slog.KindTime:
		s.appendTime(v.Time())
	case slog.KindAny:
		s.appendAny(v.Any())
	default:
		s.appendString(v.String())
	}
}

func (s *handleState) appendString(str string) {
	if s.h.json {
		s.buf = appendJSONString(s.buf, str)
	} else {
		s.buf = strconv.AppendQuote(s.buf, str)
	}
}

func (s *handleState) appendTime(t time.Time) {
	s.buf = append(s.buf, '"')
	s.buf = t.AppendFormat(s.buf, time.RFC3339Nano)
	s.buf = append(s.buf, '"')
}

func (s *handleState) appendFloat(f float64) {
	if !s.h.json {
		s.buf = strconv.AppendFloat(s.buf, f, 'g', -1, 64)
		return
	}

	if math.IsNaN(f) || math.IsInf(f, 0) {
		s.appendError(&json.UnsupportedValueError{Str: strconv.FormatFloat(f, 'g', -1, 64)})
		return
	}

	s.buf = appendJSONFloat(s.buf, f)
}

func (s *handleState) appendAny(v any) {
	switch v := v.(type) {
	case nil:
		if s.h.json {
			s.buf = append(s.buf, "null"...)
		} else {
			s.buf = append(s.buf, "<nil>"...)
		}
		return
	case error:
		s.appendString(v.Error())
		return
	case json.Marshaler:
		s.appendJSONMarshal(v)
		return
	}

	if s.h.json {
		s.appendJSONMarshal(v)
		return
	}

	str := fmt.Sprintf("%v", v)
	if textNeedsQuoting(str) {
		s.buf = strconv.AppendQuote(s.buf, str)
	} else {
		s.buf = append(s.buf, str...)
	}
}

func (s *handleState) appendError(err error) {
	s.appendString("!ERROR:" + err.Error())
}

type jsonEncoder struct {
	buf bytes.Buffer
	enc *json.Encoder
}

var jsonEncoderPool = sync.Pool{
	New: func() any {
		j := new(jsonEncoder)
		j.enc = json.NewEncoder(&j.buf)
		j.enc.SetEscapeHTML(false)
		return j
	},
}

func (s *handleState) appendJSONMarshal(v any) {
	j := jsonEncoderPool.Get().(*jsonEncoder)
	defer func() {
		if j.buf.Cap() > maxPooledBufferSize {
			return
		}
		j.buf.Reset()
		jsonEncoderPool.Put(j)
	}()

	if err := j.enc.Encode(v); err != nil {
		s.appendError(err)
		return
	}
	s.buf = append(s.buf, bytes.TrimSuffix(j.buf.Bytes(), []byte("\n"))...)
}

const hexDigits = "0123456789abcdef"

func appendJSONString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' {
				i++
				continue
			}
			buf = append(buf, s[start:i]...)
			switch b {
			case '"', '\\':
				buf = append(buf, '\\', b)
			case '\n':
				buf = append(buf, '\\', 'n')
			case '\r':
				buf = append(buf, '\\', 'r')
			case '\t':
				buf = append(buf, '\\', 't')
			default:
				buf = append(buf, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf = append(buf, s[start:i]...)
			buf = append(buf, `\ufffd`...)
			i += size
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			buf = append(buf, s[start:i]...)
			buf = append(buf, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	buf = append(buf, s[start:]...)
	return append(buf, '"')
}

// appendJSONFloat formats f the way encoding/json does.
func appendJSONFloat(buf []byte, f float64) []byte {
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}

	buf = strconv.AppendFloat(buf, f, format, -1, 64)
	if format == 'e' {
		if n := len(buf); n >= 4 && buf[n-4] == 'e' && buf[n-3] == '-' && buf[n-2] == '0' {
			buf[n-2] = buf[n-1]
			buf = buf[:n-1]
		}
	}
	return buf
}

func textNeedsQuoting(s string) bool {
	if len(s) == 0 {
		return true
	}
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if b == ' ' || b == '=' || b == '"' || b < 0x20 || b == 0x7f {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return true
		}
		i += size
	}
	return false
}
