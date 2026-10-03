VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REGISTRY ?= 123456789012.dkr.ecr.ap-southeast-4.amazonaws.com
IMAGE   ?= $(REGISTRY)/kube-distribution-network:$(VERSION)

# Either builder works; buildah needs no daemon.
BUILDER ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo buildah)

AWS_REGION ?= ap-southeast-4

.PHONY: build test vet fmt lint image push run clean

build:
	CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/kdn ./cmd/kdn

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Build and test what CI should gate on.
lint: fmt vet test

image:
	$(BUILDER) build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# Log in to ECR and push. Requires AWS credentials for the registry account.
push: image
	aws ecr get-login-password --region $(AWS_REGION) 	  | $(BUILDER) login --username AWS --password-stdin $(REGISTRY)
	$(BUILDER) push $(IMAGE)

# Run against the cluster in your current kubeconfig context via `kubectl proxy`
# on :8001, so you can exercise the proxy without deploying it.
run: build
	./bin/kdn -config config.example.yaml -log-level debug

clean:
	rm -rf bin/
