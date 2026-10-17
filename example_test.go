package sloglambda_test

import (
	"log/slog"
	"os"

	sloglambda "github.com/angryboat/slog-lambda"
)

func ExampleWithJSON() {
	handler := sloglambda.NewHandler(os.Stdout, sloglambda.WithJSON(), sloglambda.WithoutTime())
	logger := slog.New(handler)

	slog.SetDefault(logger)

	slog.Info("Hello, world!")
	// Output: {"level":"INFO","msg":"Hello, world!","type":"app.log","record":{"functionName":"test-function","version":"$LATEST"}}
}

func ExampleWithText() {
	handler := sloglambda.NewHandler(os.Stdout, sloglambda.WithText(), sloglambda.WithoutTime())
	logger := slog.New(handler)

	slog.SetDefault(logger)

	slog.Info("Hello, world!")
	// Output: level="INFO" msg="Hello, world!" type="app.log" record.functionName="test-function" record.version="$LATEST"
}

func ExampleNewHandler() {
	handler := sloglambda.NewHandler(os.Stdout)
	logger := slog.New(handler)

	slog.SetDefault(logger)

	slog.Info("Hello, world!")
}
