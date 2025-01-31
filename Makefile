.PHONY: test bin docker debug stacktest
default:
	$(MAKE) all
test:
	CGO_ENABLED=1 go test -race -short ./...
test-all:
	CGO_ENABLED=1 go test -race ./...

stacktest:
	# count=1 forces uncached runs
	# not using stacktest/... here because Go would run them all in parallel,
	# or at least the TestMain's, and the stacks would conflict with each other
	go test -count=1 -v ./stacktest/tests/chaos_cluster
	go test -count=1 -v ./stacktest/tests/end2end_carbon
	go test -count=1 -v ./stacktest/tests/end2end_carbon_bigtable

check:
	$(MAKE) test
bin:
	./scripts/build.sh
bin-race:
	./scripts/build.sh -race
docker:
	./scripts/build_docker.sh
qa: bin qa-common

#debug versions for remote debugging with delve
bin-debug:
	./scripts/build.sh -debug
docker-debug:
	./scripts/build_docker.sh -debug
qa-debug: bin-debug qa-common

qa-common:
	# regular qa steps (can run directly on code)
	scripts/qa/gofmt.sh
	scripts/qa/go-generate.sh
	scripts/qa/ineffassign.sh
	scripts/qa/misspell.sh
	scripts/qa/gitignore.sh
	scripts/qa/unused.sh
	scripts/qa/vendor.sh
	scripts/qa/vet-high-confidence.sh
	# qa-post-build steps minus stack tests
	scripts/qa/docs.sh

all:
	$(MAKE) bin
	$(MAKE) docker
	$(MAKE) qa

debug:
	$(MAKE) bin-debug
	$(MAKE) docker-debug
	$(MAKE) qa-debug

clean:
	rm build/*
	rm scripts/build/*
