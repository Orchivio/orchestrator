.PHONY: gen test
gen:
	protoc -I proto --go_out=gen --go_opt=paths=source_relative --go-grpc_out=gen --go-grpc_opt=paths=source_relative $$(find proto -name '*.proto')
test:
	go test ./...
