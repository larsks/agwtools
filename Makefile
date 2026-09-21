prefix=/usr
bindir=$(prefix)/bin

GO=go
INSTALL=install

GOARCH=$(shell go env GOARCH)
GOOS=$(shell go env GOOS)

export GOARCH
export GOOS

# We use ?= here so that these values can be provided via environment variables
# e.g. in a container build workflow, which might not have access to git.
VERSION?=devel
COMMIT?=$(shell git describe --always --dirty=-dev --abbrev=12 --exclude '*' 2>/dev/null || echo unknown)
BUILD_DATE?=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Expand once now, so every binary in a build gets the same values.
COMMIT:=$(COMMIT)
BUILD_DATE:=$(BUILD_DATE)

MODULE:=$(shell $(GO) list -m)
LDFLAGS=\
				-X $(MODULE)/internal/version.Version=$(VERSION) \
				-X $(MODULE)/internal/version.Commit=$(COMMIT) \
				-X $(MODULE)/internal/version.Date=$(BUILD_DATE)

AGWLISTEN=agwlisten-$(GOOS)-$(GOARCH)
AGWCONNECT=agwconnect-$(GOOS)-$(GOARCH)
COMMON=internal/*/*.go

.PHONY: all tidy install clean

all: $(AGWLISTEN) $(AGWCONNECT)

tidy:
	go mod tidy

$(AGWLISTEN): $(wildcard cmd/agwlisten/*.go $(COMMON))
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/agwlisten

$(AGWCONNECT): $(wildcard cmd/agwconnect/*.go $(COMMON))
	$(GO) build -ldflags '$(LDFLAGS)' -o $@ ./cmd/agwconnect

install: $(AGWLISTEN) $(AGWCONNECT)
	$(INSTALL) -m 755 -d $(DESTDIR)$(bindir)
	$(INSTALL) -m 755 $(AGWLISTEN) $(DESTDIR)$(bindir)/agwlisten
	$(INSTALL) -m 755 $(AGWCONNECT) $(DESTDIR)$(bindir)/agwconnect

clean:
	rm -f $(AGWLISTEN) $(AGWCONNECT)
