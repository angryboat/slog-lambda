module github.com/angryboat/slog-lambda

go 1.25.0

toolchain go1.25.4

require (
	github.com/aws/aws-lambda-go v1.50.0
	github.com/stretchr/testify v1.12.1
	go.uber.org/goleak v1.3.0
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect
