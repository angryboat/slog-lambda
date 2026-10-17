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
- `make benchmark` - Run all benchmarks (`BenchmarkJSON`, `BenchmarkText`, `BenchmarkJSONGroups`, `BenchmarkTextGroups`) for 10 seconds with memory stats

### Code Quality (all enforced in CI)
- `gofmt -s -l .` - Check formatting (must have zero output)
- `nilaway ./...` - Nil safety analysis
- `gosec ./...` - Security scanning
- `govulncheck ./...` - Vulnerability checking

## Architecture

Single-file handler in `handler.go`. The `Handler` struct implements `slog.Handler` and writes to an `io.Writer`.

Key design decisions:
- **Functional options pattern** - `Option` type (`func(*Handler)`) for configuration: `WithJSON()`, `WithText()`, `WithLevel()`, `WithSource()`, `WithType()`, `WithFunctionName()`, `WithFunctionVersion()`, `WithoutTime()`
- **Environment auto-detection** - `NewHandler` reads `AWS_LAMBDA_LOG_LEVEL` and `AWS_LAMBDA_LOG_FORMAT` at construction time; options override env values. Unknown level strings fall back to INFO; anything other than `json` (case-insensitive) is text
- **Custom level mapping** - TRACE = `slog.LevelDebug - 4`, FATAL = `slog.LevelError + 4`; `lambdaLoggerLevelString` renders these back for output, with `+N`/`-N` suffixes for in-between levels
- **`WithAttrs`/`WithGroup`** - `copy()` appends a `groupOrAttrs` entry to an immutable chain (empty attrs or group name return the receiver, like stdlib). `WithAttrs` resolves `LogValuer`s once and pre-serializes the chain into `preformatted` (bytes, separator state, text prefix, and which groups are already open), so `Handle` copies bytes, opens any still-pending groups lazily, appends record attrs, and closes the open groups. If a top-level chain key is `record`, or `type` while a log type is set, `pre` is nil and `Handle` falls back to walking the chain per record so `userKey` can rename against the record's keys
- **Direct buffer appends** - `handleState` (pooled, 1KB initial buffer, 16KB max retention) appends JSON or text straight into a byte slice, mirroring `slog.JSONHandler`. No intermediate map. Field order is stdlib's: `time`, `level`, `source`, `msg`, then `type`, `record`, then attrs in call order
- **Empty groups** - `openGroup` returns a `groupMark`; `closeGroup` truncates the buffer back if nothing was written inside (slogtest requires empty groups, including nested ones, to be omitted)
- **Values** - `appendValue` resolves `slog.LogValuer`, writes times as RFC3339Nano and durations as strings. `error` uses `Error()`, `json.Marshaler` and (in JSON) other `KindAny` values go through a pooled `json.Encoder` with HTML escaping off. Anything unencodable (NaN, channels, failing marshalers) becomes an `!ERROR:<msg>` string in place. Never drop a log record
- **Text output** - groups flatten into dotted paths (`record.functionName="..."`); keys are quoted only when needed, strings always with `strconv.Quote`
- **Lambda metadata** - Injects function name and version (read from env once in `NewHandler`, overridable via `WithFunctionName()`/`WithFunctionVersion()`) and request ID from `lambdacontext` into a `record` group; `type` field (default `app.log`) added at top level
- **Reserved keys** - `record` and `type` are written before user attrs; `userKey` renames a colliding top-level user key to the first unused `record_`/`type_`/`type__`... key, scanning top-level user keys lazily only on collision
- **Locking** - `mu` is a `*sync.Mutex` shared across `copy()`-derived handlers so all writers to the same `io.Writer` serialize

## Testing

- External tests (`handler_test.go`) use `slogtest.Run` for `slog.Handler` conformance in both JSON and text formats, plus field order, unencodable values, and benchmarks (`BenchmarkJSONGroups`/`BenchmarkTextGroups` cover nested groups)
- Internal tests (`handler_internal_test.go`) cover unexported helpers: level parsing, level string rendering, JSON string/float encoding, value rendering, key quoting, group elision
- `example_test.go` holds runnable `Example*` functions whose `// Output:` comments assert exact log lines; they are kept in release tarballs for pkg.go.dev
- `main_test.go` sets Lambda env vars (`AWS_LAMBDA_FUNCTION_NAME`, `AWS_LAMBDA_FUNCTION_VERSION`, `AWS_LAMBDA_LOG_FORMAT=JSON`) and runs `goleak.VerifyTestMain` for goroutine leak detection. Example outputs depend on these values
- `value_test.go` covers `slog.LogValuer` resolution and complex types like `http.Request`
- `testify` is available for assertions

## CI

- `ci.yml` (push to main, PRs): test across Go 1.25/1.26/1.27/stable on ubuntu x64 and arm64, coverage to Codecov, gofmt, nilaway
- `go.yml` (push to main, PRs, weekly schedule): gosec, govulncheck
- `release.yml` (manual `workflow_dispatch` with a version input): runs both workflows, deletes all `*_test.go` except `example_test.go`, runs `go mod tidy`, commits, tags, and opens a draft GitHub release. Only `maddiesch` can trigger it

`*.local.md` and `*.local.json` are gitignored, so `CLAUDE.local.md` is safe for personal notes.
