# Developer shortcuts. Go may be wrapped: make cover GO="sh /path/to/dev.sh go".
GO     ?= go
COVER  ?= cover.out
FLOORS ?= coverage-floors.json

.PHONY: cover cover-check ratchet

# Same measurement CI uses: race detector on, profile for every package.
cover:
	$(GO) test -race -coverprofile=$(COVER) ./...

# Fail if any package is below its floor, or is new and has no floor.
cover-check:
	$(GO) run ./tools/coverfloors check -profile $(COVER) -floors $(FLOORS)

# Run the tests with coverage, then raise floors to current coverage (never lowers).
ratchet: cover
	$(GO) run ./tools/coverfloors ratchet -profile $(COVER) -floors $(FLOORS)
