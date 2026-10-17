# slog-lambda

[![current](https://img.shields.io/github/v/tag/angryboat/slog-lambda.svg)](https://github.com/angryboat/slog-lambda/releases)
[![codecov](https://codecov.io/gh/angryboat/slog-lambda/graph/badge.svg?token=m4y4KRMuAu)](https://codecov.io/gh/angryboat/slog-lambda)
[![Doc](https://godoc.org/github.com/angryboat/slog-lambda?status.svg)](https://pkg.go.dev/github.com/angryboat/slog-lambda)
[![License](https://img.shields.io/github/license/angryboat/slog-lambda)](./LICENSE)

## AWS Lambda `slog.Handler`

A [`log/slog`](https://pkg.go.dev/log/slog) handler for [AWS Lambda advanced logging controls](https://docs.aws.amazon.com/lambda/latest/dg/monitoring-cloudwatchlogs-advanced.html).

### Features

- Auto-configures log format and level from Lambda environment variables (`AWS_LAMBDA_LOG_FORMAT`, `AWS_LAMBDA_LOG_LEVEL`)
- JSON and text output formats
- Includes Lambda metadata (function name, version, request ID) in log output
- Supports custom log levels: TRACE and FATAL
- Source code location in log records

### Install

```sh
go get github.com/angryboat/slog-lambda
```

### Usage

```go
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	sloglambda "github.com/angryboat/slog-lambda"
)

func init() {
	logger := slog.New(sloglambda.NewHandler(os.Stdout))
	slog.SetDefault(logger)
}

func main() {
	lambda.Start(func(ctx context.Context, event any) error {
		slog.InfoContext(ctx, "Lambda Invoked", slog.Any("event", event))

		return nil
	})
}
```

By default, `NewHandler` reads `AWS_LAMBDA_LOG_FORMAT` and `AWS_LAMBDA_LOG_LEVEL` to configure itself. Use options to override:

```go
handler := sloglambda.NewHandler(os.Stdout,
	sloglambda.WithJSON(),
	sloglambda.WithLevel(slog.LevelDebug),
	sloglambda.WithSource(),
	sloglambda.WithType("api.request"),
)
```

### Options

| Option                    | Description                                    |
| ------------------------- | ---------------------------------------------- |
| `WithJSON()`              | Output in JSON format                          |
| `WithText()`              | Output in text format                          |
| `WithLevel(slog.Leveler)` | Set the minimum log level                      |
| `WithSource()`            | Include source file, function, and line number |
| `WithType(string)`        | Set the `type` field (default: `"app.log"`)    |
| `WithoutTime()`           | Omit the timestamp                             |

### Output

When using `WithJSON()`, log records look like:

```json
{
  "level": "INFO",
  "msg": "Lambda Invoked",
  "record": {
    "functionName": "my-func",
    "version": "$LATEST",
    "requestId": "abc-123"
  },
  "type": "app.log"
}
```

When using `WithText()`:

```
level="INFO" msg="Lambda Invoked" record.functionName="my-func" record.version="$LATEST" record.requestId="abc-123" type="app.log"
```

`record` and `type` are reserved. If your attrs or groups use either key at the top level, the handler's value wins and yours moves to the first unused key with underscores appended (`type_`, then `type__`, and so on).

### Key Names and Lambda Conventions

Lambda documents two JSON shapes. [System logs](https://docs.aws.amazon.com/lambda/latest/dg/monitoring-cloudwatchlogs-logformat.html#monitoring-cloudwatchlogs-JSON-system) (platform events Lambda emits) use `time`, `type`, and `record`. [Application logs](https://docs.aws.amazon.com/lambda/latest/dg/monitoring-cloudwatchlogs-logformat.html#monitoring-cloudwatchlogs-JSON-application) from the managed runtimes' built-in loggers use `timestamp`, `level`, `message`, and `requestId`.

This handler mixes the two:

| Key      | Source                                                                    |
| -------- | ------------------------------------------------------------------------- |
| `level`  | Lambda application-log key, used for log-level filtering                  |
| `msg`    | slog's default `slog.MessageKey` (Lambda's runtimes use `message`)        |
| `time`   | slog's default `slog.TimeKey`, matching the system-log shape              |
| `type`   | Borrowed from the system-log shape (default `app.log`)                    |
| `record` | Borrowed from the system-log shape; holds function name, version, request ID |

For [log-level filtering](https://docs.aws.amazon.com/lambda/latest/dg/monitoring-cloudwatchlogs-log-level.html), Lambda only requires a `level` key on a JSON object written to stdout or stderr, and the function's log format set to JSON. AWS says the timestamp must be RFC 3339 and its key should "follow the conventions of the runtime", without listing which key names it accepts. `time` values are RFC 3339 (`time.RFC3339Nano`).

There is no option to rename `time`. To emit `timestamp` instead, pass `WithoutTime()` and add the attr in a wrapping handler:

```go
type timestampHandler struct{ slog.Handler }

func (h timestampHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(slog.Time("timestamp", r.Time))
	return h.Handler.Handle(ctx, r)
}

func (h timestampHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return timestampHandler{h.Handler.WithAttrs(attrs)}
}

func (h timestampHandler) WithGroup(name string) slog.Handler {
	return timestampHandler{h.Handler.WithGroup(name)}
}

logger := slog.New(timestampHandler{sloglambda.NewHandler(os.Stdout, sloglambda.WithoutTime())})
```

`WithAttrs` and `WithGroup` must be overridden, or the wrapper drops out of any derived logger. Record attrs land in the innermost group, so on a logger built with `WithGroup("g")` the key becomes `g.timestamp`, not a top-level `timestamp`.

### Log Levels

The handler maps AWS Lambda log levels to `slog.Level` values:

| Lambda Level | slog Level                 |
| ------------ | -------------------------- |
| TRACE        | `slog.LevelDebug - 4`      |
| DEBUG        | `slog.LevelDebug`          |
| INFO         | `slog.LevelInfo` (default) |
| WARN         | `slog.LevelWarn`           |
| ERROR        | `slog.LevelError`          |
| FATAL        | `slog.LevelError + 4`      |

Only these six names are guaranteed to work with Lambda's log-level filtering. A level between them renders as the nearest lower name plus an offset, e.g. `slog.LevelInfo + 2` becomes `INFO+2` and `slog.LevelDebug - 8` becomes `TRACE-4`. AWS says an invalid or missing `level` is treated as INFO, but doesn't say whether `INFO+2` counts as invalid. Don't rely on Lambda-side filtering for custom offsets; `WithLevel` filtering in the handler itself is unaffected.
