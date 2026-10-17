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

`record` and `type` are reserved. If your attrs or groups use either key at the top level, the handler's value wins and yours moves to `record_` or `type_`.

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
