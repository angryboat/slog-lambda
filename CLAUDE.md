# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A Go `slog.Handler` for AWS Lambda functions that auto-configures log format (JSON/text) and level from Lambda environment variables. Module path is `github.com/angryboat/slog-lambda`, import alias is `sloglambda`. Requires Go 1.25+.

## Commands

### Testing
- `make test` - Run all tests with race detection
- `go test -v -race -run TestHandler/slogtest ./...` - Run a specific test
- `go test -coverprofile=coverage.txt ./...` - Generate coverage report

### Benchmarking
- `make benchmark` - Run `BenchmarkJSON` and `BenchmarkText` for 10 seconds with memory stats

### Code Quality (all enforced in CI)
- `gofmt -s -l .` - Check formatting (must have zero output)
- `nilaway ./...` - Nil safety analysis
- `gosec ./...` - Security scanning
- `govulncheck ./...` - Vulnerability checking

## Architecture

Single-file handler in `handler.go`. The `Handler` struct implements `slog.Handler` and writes to an `io.Writer`.

Key design decisions:
- **Functional options pattern** - `Option` type (`func(*Handler)`) for configuration: `WithJSON()`, `WithText()`, `WithLevel()`, `WithSource()`, `WithType()`, `WithoutTime()`
- **Environment auto-detection** - `NewHandler` reads `AWS_LAMBDA_LOG_LEVEL` and `AWS_LAMBDA_LOG_FORMAT` at construction time; options override env values. Unknown level strings fall back to INFO; anything other than `json` (case-insensitive) is text
- **Custom level mapping** - TRACE = `slog.LevelDebug - 4`, FATAL = `slog.LevelError + 4`; `lambdaLoggerLevelString` renders these back for output, with `+N`/`-N` suffixes for in-between levels
- **`WithAttrs`/`WithGroup`** - `copy()` appends a `groupOrAttrs` entry to an immutable chain; `Handle()` drops trailing empty groups when the record has no attrs (slogtest requires this)
- **`logRecord` (map[string]any)** - Internal representation built in `Handle()`, supports nested groups; `clean()` removes empty sub-records before serialization
- **Value normalization** - `normalizeValue` resolves `slog.LogValuer` and converts typed values (times to RFC3339Nano, durations to strings); `normalizeAnyValue` handles `error` and `json.Marshaler`
- **JSON encode fallback** - If encoding fails, `replaceUnencodable()` swaps bad values for `!ERROR:<msg>` strings and retries. If that still fails, `writeJSONFallback` emits a minimal ERROR line and returns the joined errors. Never drop a log record
- **Text output** - `writeTextRecord` sorts keys and flattens groups into dotted paths (`record.functionName="..."`); strings are quoted with `strconv.Quote`
- **Buffer pooling** - `sync.Pool` with 1KB initial alloc, 16KB max retention
- **Lambda metadata** - Injects function name, version from env, and request ID from `lambdacontext` into a `record` group; `type` field (default `app.log`) added at top level
- **Locking** - `mu` is a `*sync.Mutex` shared across `copy()`-derived handlers so all writers to the same `io.Writer` serialize

## Testing

- External tests (`handler_test.go`) use `slogtest.Run` for `slog.Handler` conformance in both JSON and text formats, plus JSON fallback coverage and benchmarks
- Internal tests (`handler_internal_test.go`) cover unexported helpers: level parsing, level string rendering, `logRecord` operations, text serialization, value normalization
- `example_test.go` holds runnable `Example*` functions whose `// Output:` comments assert exact log lines; they are kept in release tarballs for pkg.go.dev
- `main_test.go` sets Lambda env vars (`AWS_LAMBDA_FUNCTION_NAME`, `AWS_LAMBDA_FUNCTION_VERSION`, `AWS_LAMBDA_LOG_FORMAT=JSON`) and runs `goleak.VerifyTestMain` for goroutine leak detection. Example outputs depend on these values
- `value_test.go` covers `slog.LogValuer` resolution and complex types like `http.Request`
- `testify` is available for assertions

## CI

- `ci.yml` (push to main, PRs): test and build across Go 1.25/1.26 on ubuntu and windows, coverage to Codecov, gofmt, nilaway
- `go.yml` (push to main, PRs, weekly schedule): gosec, govulncheck
- `release.yml` (manual `workflow_dispatch` with a version input): runs both workflows, deletes all `*_test.go` except `example_test.go`, runs `go mod tidy`, commits, tags, and opens a draft GitHub release. Only `maddiesch` can trigger it

`*.local.md` and `*.local.json` are gitignored, so `CLAUDE.local.md` is safe for personal notes.
