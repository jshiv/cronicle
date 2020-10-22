## hot: git hooks, go binaries, dependency checks
hot:
	CompileDaemon -command="go test ./..."
## test: run test suite
test:
	go test ./...
## build: build docker containers
build:
	goreleaser build
## builds popular binaries for attaching to release tag
release:
	## brew install goreleaser/tap/goreleaser
	goreleaser release


	
