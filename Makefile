prefix=/usr
bindir=$(prefix)/bin

GO=go
INSTALL=install

GOARCH=$(shell go env GOARCH)
GOOS=$(shell go env GOOS)

export GOARCH
export GOOS

AGWLISTEN=agwlisten-$(GOOS)-$(GOARCH)
AGWCONNECT=agwconnect-$(GOOS)-$(GOARCH)
COMMON=internal/*/*.go

.PHONY: all tidy install clean

all: $(AGWLISTEN) $(AGWCONNECT)

tidy:
	go mod tidy

$(AGWLISTEN): $(wildcard cmd/agwlisten/*.go $(COMMON))
	$(GO) build -o $@ ./cmd/agwlisten

$(AGWCONNECT): $(wildcard cmd/agwconnect/*.go $(COMMON))
	$(GO) build -o $@ ./cmd/agwconnect

install: $(AGWLISTEN) $(AGWCONNECT)
	$(INSTALL) -m 755 -d $(DESTDIR)$(bindir)
	$(INSTALL) -m 755 $(AGWLISTEN) $(DESTDIR)$(bindir)/agwlisten
	$(INSTALL) -m 755 $(AGWCONNECT) $(DESTDIR)$(bindir)/agwconnect

clean:
	rm -f $(AGWLISTEN) $(AGWCONNECT)
