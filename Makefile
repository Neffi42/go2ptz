.PHONY: check fmt fmt-check vet test

# Empty probes for -race: ThreadSanitizer aborts on 39-bit VA arm64 kernels.
# RACE=-race forces it, RACE=- disables it.
RACE ?=

# Run by the Dockerfile test stage.
check: fmt-check vet test

fmt:
	gofmt -w .

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

vet:
	go vet ./...

test:
	@race='$(RACE)'; \
	if [ -z "$$race" ]; then \
		d=$$(mktemp -d); printf 'package main\nfunc main() {}\n' > $$d/main.go; \
		if go run -race $$d/main.go >/dev/null 2>&1; then race=-race; \
		else race=-; echo "warning: race detector unsupported on this host, testing without -race"; fi; \
		rm -rf $$d; \
	fi; \
	[ "$$race" = - ] && race=; \
	echo go test $$race ./...; go test $$race ./...
