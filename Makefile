.PHONY: all build build-linux build-windows build-all test test-coverage clean check

# 默认构建 Linux (无 CGO，纯 Go 原生静态编译)
all: build-linux

build: build-linux

build-linux:
	@mkdir -p bin/linux
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/linux/sft ./cmd/sft
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/linux/sftd ./cmd/sftd

# 交叉编译 Windows x64 可执行程序 (输出到 bin/windows/)
build-windows:
	@mkdir -p bin/windows
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o bin/windows/sft.exe ./cmd/sft
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o bin/windows/sftd.exe ./cmd/sftd

# 构建所有平台产物
build-all: build-linux build-windows

test:
	go test -v -race ./...

test-coverage:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

check:
	go vet ./...

clean:
	rm -rf bin/ dist/ coverage.out
