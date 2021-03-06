########################################################################################################################
# Copyright (c) 2018 IoTeX
# This is an alpha (internal) release and is not suitable for production. This source code is provided ‘as is’ and no
# warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
# permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
# License 2.0 that can be found in the LICENSE file.
########################################################################################################################

# Go parameters
GOCMD=go
GOLINT=golint
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
BUILD_TARGET_SERVER=server
BUILD_TARGET_ACTINJ=actioninjector
SKIP_GLIDE=false

# Docker parameters
DOCKERCMD=docker

all: clean build test
.PHONY: build
build:
	$(GOBUILD) -o ./bin/$(BUILD_TARGET_SERVER) -v ./$(BUILD_TARGET_SERVER)
	$(GOBUILD) -o ./bin/$(BUILD_TARGET_ACTINJ) -v ./tools/actioninjector

.PHONY: fmt
fmt:
	$(GOCMD) fmt ./...

.PHONY: lint
lint:
	go list ./... | grep -v /vendor/ | xargs $(GOLINT)

.PHONY: test
test: fmt
	$(GOTEST) -short ./...

.PHONY: mockgen
mockgen:
	@./misc/scripts/mockgen.sh

.PHONY: stringer
stringer:
	sh ./misc/scripts/stringer.sh

.PHONY: clean
clean:
	$(GOCLEAN)
	rm -f ./bin/$(BUILD_TARGET_SERVER)
	rm -f ./bin/$(BUILD_TARGET_ACTINJ)
	rm -f chain.db
	rm -f trie.db

.PHONY: run
run:
	$(GOBUILD) -o ./bin/$(BUILD_TARGET_SERVER) -v ./$(BUILD_TARGET_SERVER)
	export LD_LIBRARY_PATH=$LD_LIBRARY_PATH:$(PWD)/crypto/lib
	./bin/$(BUILD_TARGET_SERVER) -config=e2etests/config_local_delegate.yaml -log-level=debug

.PHONY: docker
docker:
	$(DOCKERCMD) build -t $(USER)/iotex-go:latest --build-arg SKIP_GLIDE=$(SKIP_GLIDE) .
